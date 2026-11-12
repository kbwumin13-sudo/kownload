package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

const HTTPCoverageVersion = 1
const HTTPCoverageThreshold int64 = 1 << 20

var (
	ErrHTTPCoverageInvalid            = errors.New("http_coverage_invalid")
	ErrHTTPCoverageUnsupportedVersion = errors.New("http_coverage_unsupported_version")
	ErrHTTPCoverageLimit              = errors.New("http_coverage_limit")
	ErrHTTPCoverageUnprovable         = errors.New("http_coverage_unprovable")
)

type HTTPCoverageRange struct {
	Begin int64 `json:"begin"` // inclusive
	End   int64 `json:"end"`   // exclusive
}

// HTTPCoverageState describes values, not evidence of successful IO or permission
// to execute. DurableRanges is the last sealed current coverage; SettledRanges
// is the historical consumed baseline and may extend beyond current coverage.
type HTTPCoverageState struct {
	Version            int                 `json:"version"`
	IdentityHash       string              `json:"identityHash"`
	Size               int64               `json:"size"`
	Status             string              `json:"status"`
	DurableRanges      []HTTPCoverageRange `json:"durableRanges"`
	SettledRanges      []HTTPCoverageRange `json:"settledRanges"`
	CheckpointHash     string              `json:"checkpointHash"`
	SettlementSequence uint64              `json:"settlementSequence"`
}

// HTTPCoveragePlan contains independent owned values. A non-nil Settlement is
// only a mathematical candidate. The owner must separately prove writer joins,
// successful Sync, actual EOF/Complete where required, and the atomic CAS/ACK.
type HTTPCoveragePlan struct {
	Snapshot             HTTPCoverageState
	Settlement           *HTTPCoverageState
	NetNewBytes          int64
	ThresholdReached     bool
	WholeResourceCovered bool
}

// HTTPCoverageIdentityHash preserves the exact final URL and strong quoted ETag
// bytes in separate digests, then hashes fixed-order compact JSON. It neither
// normalizes query strings nor returns copies of the raw identity. The caller
// must prove these are actual matching response identities with identity coding.
func HTTPCoverageIdentityHash(finalURL, strongETag string, size int64) (string, error) {
	u, err := url.Parse(finalURL)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.Hostname() == "" ||
		(!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) || size <= 0 {
		return "", ErrHTTPCoverageInvalid
	}
	for i := range finalURL {
		if finalURL[i] <= 0x20 || finalURL[i] == 0x7f {
			return "", ErrHTTPCoverageInvalid
		}
	}
	if len(strongETag) < 2 || strongETag[0] != '"' || strongETag[len(strongETag)-1] != '"' {
		return "", ErrHTTPCoverageInvalid
	}
	for i := 1; i < len(strongETag)-1; i++ {
		// RFC opaque-tag allows obs-text and backslashes, but no internal
		// quotes, space, controls, or DEL. Quotes are not backslash-escaped.
		b := strongETag[i]
		if b == '"' || b <= 0x20 || b == 0x7f {
			return "", ErrHTTPCoverageInvalid
		}
	}
	raw, err := json.Marshal(struct {
		Version  int    `json:"version"`
		URLHash  string `json:"urlHash"`
		ETagHash string `json:"etagHash"`
		Size     int64  `json:"size"`
	}{HTTPCoverageVersion, coverageDigest([]byte(finalURL)), coverageDigest([]byte(strongETag)), size})
	if err != nil {
		return "", ErrHTTPCoverageInvalid
	}
	return coverageDigest(raw), nil
}

func NewHTTPCoverageState(identityHash string, size int64) (HTTPCoverageState, error) {
	if !coverageValidHash(identityHash) || size <= 0 {
		return HTTPCoverageState{}, ErrHTTPCoverageInvalid
	}
	return HTTPCoverageState{
		Version: HTTPCoverageVersion, IdentityHash: identityHash, Size: size, Status: "tracking",
		DurableRanges: make([]HTTPCoverageRange, 0), SettledRanges: make([]HTTPCoverageRange, 0),
	}, nil
}

// ValidateHTTPCoverageState accepts the complete shape of known tracking and
// unprovable states. Plan rejects the latter separately; validation does not
// authorize restoring it. Missing/null persisted ranges are never empty evidence.
func ValidateHTTPCoverageState(state HTTPCoverageState, rangeLimit int) error {
	if state.Version != HTTPCoverageVersion {
		return ErrHTTPCoverageUnsupportedVersion
	}
	if !coverageValidHash(state.IdentityHash) || state.Size <= 0 ||
		(state.Status != "tracking" && state.Status != "unprovable") {
		return ErrHTTPCoverageInvalid
	}
	for _, ranges := range [][]HTTPCoverageRange{state.DurableRanges, state.SettledRanges} {
		if err := coverageCheckRanges(ranges, state.Size, rangeLimit, false); err != nil {
			return err
		}
	}
	if state.CheckpointHash == "" {
		if len(state.DurableRanges) != 0 || len(state.SettledRanges) != 0 || state.SettlementSequence != 0 {
			return ErrHTTPCoverageInvalid
		}
	} else if !coverageValidHash(state.CheckpointHash) {
		return ErrHTTPCoverageInvalid
	}
	return nil
}

// MergeHTTPCoverageWrite records exactly the successful WriteAt prefix n. It
// cannot observe the IO error and must not replace it. Nil transient current is
// explicitly empty; success always returns an owned non-nil canonical slice.
func MergeHTTPCoverageWrite(current []HTTPCoverageRange, size, offset, written int64, rangeLimit int) ([]HTTPCoverageRange, error) {
	if err := coverageCheckRanges(current, size, rangeLimit, true); err != nil {
		return nil, err
	}
	if offset < 0 || offset > size || written < 0 || written > size-offset {
		return nil, ErrHTTPCoverageInvalid
	}
	if written == 0 {
		return coverageCopyRanges(current), nil
	}
	return coverageUnion(current, []HTTPCoverageRange{{Begin: offset, End: offset + written}}, rangeLimit)
}

// PlanHTTPCoverageCheckpoint uses the complete current coverage, never unions
// prior DurableRanges into it. Snapshot-only does not spend the historical B;
// repeating the same old prior can therefore return the same candidate. Only
// applying Settlement as the new prior consumes all net-new covered bytes.
func PlanHTTPCoverageCheckpoint(prior HTTPCoverageState, current []HTTPCoverageRange, checkpointHash string, rangeLimit int) (HTTPCoveragePlan, error) {
	if err := ValidateHTTPCoverageState(prior, rangeLimit); err != nil {
		return HTTPCoveragePlan{}, err
	}
	if prior.Status == "unprovable" {
		return HTTPCoveragePlan{}, ErrHTTPCoverageUnprovable
	}
	if err := coverageCheckRanges(current, prior.Size, rangeLimit, true); err != nil {
		return HTTPCoveragePlan{}, err
	}
	if !coverageValidHash(checkpointHash) {
		return HTTPCoveragePlan{}, ErrHTTPCoverageInvalid
	}
	var covered, intersection int64
	for _, r := range current {
		covered += r.End - r.Begin // canonical ranges bound the total by Size
	}
	for i, j := 0, 0; i < len(current) && j < len(prior.SettledRanges); {
		c, b := current[i], prior.SettledRanges[j]
		if begin, end := max(c.Begin, b.Begin), min(c.End, b.End); begin < end {
			intersection += end - begin
		}
		if c.End <= b.End {
			i++
		} else {
			j++
		}
	}
	snapshot := prior
	snapshot.DurableRanges = coverageCopyRanges(current)
	snapshot.SettledRanges = coverageCopyRanges(prior.SettledRanges)
	snapshot.CheckpointHash = checkpointHash
	netNew := covered - intersection
	whole := len(current) == 1 && current[0].Begin == 0 && current[0].End == prior.Size
	plan := HTTPCoveragePlan{
		Snapshot: snapshot, NetNewBytes: netNew,
		ThresholdReached: netNew >= HTTPCoverageThreshold, WholeResourceCovered: whole,
	}
	if netNew == 0 || !(plan.ThresholdReached || prior.Size < HTTPCoverageThreshold && whole) {
		return plan, nil
	}
	if prior.SettlementSequence == ^uint64(0) {
		return HTTPCoveragePlan{}, ErrHTTPCoverageLimit
	}
	settled, err := coverageUnion(prior.SettledRanges, current, rangeLimit)
	if err != nil {
		return HTTPCoveragePlan{}, err
	}
	settlement := snapshot
	settlement.DurableRanges = coverageCopyRanges(current)
	settlement.SettledRanges = settled
	settlement.SettlementSequence++
	plan.Settlement = &settlement
	return plan, nil
}

func coverageDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func coverageValidHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for i := range value {
		if !(value[i] >= '0' && value[i] <= '9' || value[i] >= 'a' && value[i] <= 'f') {
			return false
		}
	}
	return true
}

func coverageCopyRanges(value []HTTPCoverageRange) []HTTPCoverageRange {
	owned := make([]HTTPCoverageRange, len(value))
	copy(owned, value)
	return owned
}

func coverageCheckRanges(ranges []HTTPCoverageRange, size int64, limit int, allowNil bool) error {
	if size <= 0 || limit <= 0 || ranges == nil && !allowNil {
		return ErrHTTPCoverageInvalid
	}
	if len(ranges) > limit {
		return ErrHTTPCoverageLimit
	}
	for i, r := range ranges {
		if r.Begin < 0 || r.End <= r.Begin || r.End > size || i > 0 && r.Begin <= ranges[i-1].End {
			return ErrHTTPCoverageInvalid
		}
	}
	return nil
}

// Both inputs are already canonical. The output is owned and joins adjacent
// ranges as well as overlaps. No interval is dropped to satisfy the limit.
func coverageUnion(a, b []HTTPCoverageRange, limit int) ([]HTTPCoverageRange, error) {
	result := make([]HTTPCoverageRange, 0)
	for i, j := 0, 0; i < len(a) || j < len(b); {
		var next HTTPCoverageRange
		if j == len(b) || i < len(a) && a[i].Begin <= b[j].Begin {
			next = a[i]
			i++
		} else {
			next = b[j]
			j++
		}
		if n := len(result); n > 0 && next.Begin <= result[n-1].End {
			result[n-1].End = max(result[n-1].End, next.End)
		} else {
			result = append(result, next)
			if len(result) > limit {
				return nil, ErrHTTPCoverageLimit
			}
		}
	}
	return result, nil
}
