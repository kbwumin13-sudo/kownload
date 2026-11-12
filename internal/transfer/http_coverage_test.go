package transfer

import (
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

const coverageTestIdentity = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const coverageTestCheckpoint = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const coverageTestCheckpoint2 = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
const coverageTestMaxInt64 = int64(^uint64(0) >> 1)

func coverageTestState(t *testing.T, size int64) HTTPCoverageState {
	t.Helper()
	s, err := NewHTTPCoverageState(coverageTestIdentity, size)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func coverageTestClone(s HTTPCoverageState) HTTPCoverageState {
	raw, _ := json.Marshal(s)
	var copy HTTPCoverageState
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func coverageTestPlan(t *testing.T, prior HTTPCoverageState, current []HTTPCoverageRange) HTTPCoveragePlan {
	t.Helper()
	p, err := PlanHTTPCoverageCheckpoint(prior, current, coverageTestCheckpoint, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHTTPCoverageIdentityGoldenAndExactBytes(t *testing.T) {
	const address = "https://files.example/a.bin?sig=a%2Fb&part=1"
	const etag = `"tag-1"`
	got, err := HTTPCoverageIdentityHash(address, etag, 12345)
	// Independently calculated from separate URL/tag SHA-256 and the fixed
	// version,urlHash,etagHash,size compact JSON, not concatenated raw values.
	const expected = "60bfd874c37135525a100a97998e0bad641a5380a1d059beb92d0a6e01a0ceda"
	if err != nil || got != expected {
		t.Fatalf("%q %v", got, err)
	}
	for _, tc := range []struct {
		url, etag string
		size      int64
	}{
		{"https://files.example/a.bin?part=1&sig=a%2Fb", etag, 12345},
		{"https://files.example/a.bin?sig=a%2fb&part=1", etag, 12345},
		{"https://files.example/a.bin?sig=a/b&part=1", etag, 12345},
		{address + "&part=1", etag, 12345},
		{strings.Replace(address, "files.example", "FILES.example", 1), etag, 12345},
		{address, `"TAG-1"`, 12345}, {address, etag, 12346},
	} {
		other, err := HTTPCoverageIdentityHash(tc.url, tc.etag, tc.size)
		if err != nil || other == got {
			t.Fatalf("identity bytes collapsed: %q %v", other, err)
		}
	}
	for _, tag := range []string{`""`, `"a\b"`, "\"\x80\xff\"", `"标签"`} {
		if _, err := HTTPCoverageIdentityHash(address, tag, 1); err != nil {
			t.Fatalf("legal opaque tag rejected: %v", err)
		}
	}
}

func TestHTTPCoverageIdentityRejectsUnprovableInputs(t *testing.T) {
	for _, address := range []string{"", "/relative", "//files.example/a", "ftp://files.example/a", "https:opaque", "https:///a", "https://", "https://files.example/%zz", "https://files.example:bad/a", "https://files.example/a b", "https://files.example/a\n"} {
		got, err := HTTPCoverageIdentityHash(address, `"tag"`, 10)
		if got != "" || !errors.Is(err, ErrHTTPCoverageInvalid) {
			t.Fatalf("URL accepted: %q %v", got, err)
		}
	}
	for _, tag := range []string{"", "tag", `W/"tag"`, `w/"tag"`, " \"tag\"", "\"tag\" ", `"a"b"`, "\"a b\"", "\"a\tb\"", "\"a\r\nb\"", "\"a\x00b\"", "\"a\x7fb\""} {
		got, err := HTTPCoverageIdentityHash("https://files.example/a?secret=private", tag, 10)
		if got != "" || !errors.Is(err, ErrHTTPCoverageInvalid) {
			t.Fatalf("ETag accepted: %q %v", got, err)
		}
		if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "files.example") {
			t.Fatal("error leaked raw identity")
		}
	}
	for _, size := range []int64{0, -1} {
		if got, err := HTTPCoverageIdentityHash("https://files.example/a", `"tag"`, size); got != "" || !errors.Is(err, ErrHTTPCoverageInvalid) {
			t.Fatalf("size: %q %v", got, err)
		}
	}
}

func TestHTTPCoverageNewStateAndPersistedPresence(t *testing.T) {
	s := coverageTestState(t, 10)
	if s.Version != 1 || s.Status != "tracking" || s.Size != 10 || s.IdentityHash != coverageTestIdentity || s.CheckpointHash != "" || s.SettlementSequence != 0 || s.DurableRanges == nil || s.SettledRanges == nil {
		t.Fatalf("%+v", s)
	}
	if err := ValidateHTTPCoverageState(s, 1); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s)
	for _, changed := range []string{
		strings.Replace(string(raw), `"durableRanges":[]`, `"durableRanges":null`, 1),
		strings.Replace(string(raw), `"settledRanges":[]`, `"settledRanges":null`, 1),
		strings.Replace(string(raw), `"durableRanges":[],`, ``, 1),
		strings.Replace(string(raw), `"settledRanges":[],`, ``, 1),
	} {
		var missing HTTPCoverageState
		if err := json.Unmarshal([]byte(changed), &missing); err != nil {
			t.Fatal(err)
		}
		if err := ValidateHTTPCoverageState(missing, 1); !errors.Is(err, ErrHTTPCoverageInvalid) {
			t.Fatalf("missing persisted range accepted: %v", err)
		}
	}
	for _, tc := range []struct {
		hash string
		size int64
	}{{"", 1}, {strings.ToUpper(coverageTestIdentity), 1}, {"0", 1}, {coverageTestIdentity, 0}, {coverageTestIdentity, -1}} {
		got, err := NewHTTPCoverageState(tc.hash, tc.size)
		if !reflect.DeepEqual(got, HTTPCoverageState{}) || !errors.Is(err, ErrHTTPCoverageInvalid) {
			t.Fatalf("constructor: %+v %v", got, err)
		}
	}
}

func TestHTTPCoverageValidateCanonicalState(t *testing.T) {
	base := coverageTestState(t, 100)
	base.CheckpointHash = coverageTestCheckpoint
	base.DurableRanges = []HTTPCoverageRange{{0, 10}}
	base.SettledRanges = []HTTPCoverageRange{{20, 30}} // history need not be a subset of current
	base.SettlementSequence = 1
	if err := ValidateHTTPCoverageState(base, 2); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*HTTPCoverageState)
		want   error
	}{
		{"version", func(s *HTTPCoverageState) { s.Version = 2 }, ErrHTTPCoverageUnsupportedVersion},
		{"identity", func(s *HTTPCoverageState) { s.IdentityHash = "bad" }, ErrHTTPCoverageInvalid},
		{"size", func(s *HTTPCoverageState) { s.Size = 0 }, ErrHTTPCoverageInvalid},
		{"status", func(s *HTTPCoverageState) { s.Status = "future" }, ErrHTTPCoverageInvalid},
		{"durable missing", func(s *HTTPCoverageState) { s.DurableRanges = nil }, ErrHTTPCoverageInvalid},
		{"settled missing", func(s *HTTPCoverageState) { s.SettledRanges = nil }, ErrHTTPCoverageInvalid},
		{"checkpoint missing", func(s *HTTPCoverageState) { s.CheckpointHash = "" }, ErrHTTPCoverageInvalid},
		{"checkpoint malformed", func(s *HTTPCoverageState) { s.CheckpointHash = strings.ToUpper(coverageTestCheckpoint) }, ErrHTTPCoverageInvalid},
		{"negative", func(s *HTTPCoverageState) { s.DurableRanges = []HTTPCoverageRange{{-1, 1}} }, ErrHTTPCoverageInvalid},
		{"zero length", func(s *HTTPCoverageState) { s.DurableRanges = []HTTPCoverageRange{{1, 1}} }, ErrHTTPCoverageInvalid},
		{"reversed", func(s *HTTPCoverageState) { s.DurableRanges = []HTTPCoverageRange{{2, 1}} }, ErrHTTPCoverageInvalid},
		{"beyond size", func(s *HTTPCoverageState) { s.DurableRanges = []HTTPCoverageRange{{0, 101}} }, ErrHTTPCoverageInvalid},
		{"unsorted", func(s *HTTPCoverageState) { s.DurableRanges = []HTTPCoverageRange{{20, 30}, {0, 10}} }, ErrHTTPCoverageInvalid},
		{"overlap", func(s *HTTPCoverageState) { s.DurableRanges = []HTTPCoverageRange{{0, 10}, {9, 20}} }, ErrHTTPCoverageInvalid},
		{"adjacent", func(s *HTTPCoverageState) { s.SettledRanges = []HTTPCoverageRange{{0, 10}, {10, 20}} }, ErrHTTPCoverageInvalid},
		{"too many", func(s *HTTPCoverageState) { s.SettledRanges = []HTTPCoverageRange{{0, 1}, {2, 3}, {4, 5}} }, ErrHTTPCoverageLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := coverageTestClone(base)
			tc.mutate(&s)
			before := coverageTestClone(s)
			err := ValidateHTTPCoverageState(s, 2)
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(s, before) {
				t.Fatalf("validation: %v", err)
			}
		})
	}
	for _, limit := range []int{0, -1} {
		if err := ValidateHTTPCoverageState(base, limit); !errors.Is(err, ErrHTTPCoverageInvalid) {
			t.Fatal(err)
		}
	}
	empty := coverageTestState(t, 1)
	empty.SettlementSequence = 1
	if err := ValidateHTTPCoverageState(empty, 1); !errors.Is(err, ErrHTTPCoverageInvalid) {
		t.Fatal("empty checkpoint with seq", err)
	}
}

func TestHTTPCoverageMergeActualWrites(t *testing.T) {
	for _, tc := range []struct {
		name      string
		before    []HTTPCoverageRange
		offset, n int64
		limit     int
		want      []HTTPCoverageRange
	}{
		{"nil short prefix", nil, 10, 3, 3, []HTTPCoverageRange{{10, 13}}},
		{"adjacent at limit", []HTTPCoverageRange{{0, 5}}, 5, 5, 1, []HTTPCoverageRange{{0, 10}}},
		{"bridge", []HTTPCoverageRange{{0, 4}, {10, 14}, {20, 24}}, 4, 16, 3, []HTTPCoverageRange{{0, 24}}},
		{"overlap", []HTTPCoverageRange{{0, 10}}, 4, 12, 2, []HTTPCoverageRange{{0, 16}}},
		{"contained retry", []HTTPCoverageRange{{0, 10}}, 2, 3, 1, []HTTPCoverageRange{{0, 10}}},
		{"out of order completion", []HTTPCoverageRange{{50, 60}}, 10, 5, 2, []HTTPCoverageRange{{10, 15}, {50, 60}}},
		{"zero at EOF", []HTTPCoverageRange{{1, 2}}, 100, 0, 1, []HTTPCoverageRange{{1, 2}}},
		{"empty zero", nil, 0, 0, 1, []HTTPCoverageRange{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]HTTPCoverageRange(nil), tc.before...)
			out, err := MergeHTTPCoverageWrite(tc.before, 100, tc.offset, tc.n, tc.limit)
			if err != nil || out == nil || !reflect.DeepEqual(out, tc.want) {
				t.Fatalf("%v %v", out, err)
			}
			if len(before) > 0 && !reflect.DeepEqual(before, tc.before) {
				t.Fatal("input changed")
			}
			if len(out) > 0 {
				out[0].End++
				if len(before) > 0 && !reflect.DeepEqual(before, tc.before) {
					t.Fatal("output aliases input")
				}
			}
		})
	}
	got, err := MergeHTTPCoverageWrite(nil, coverageTestMaxInt64, coverageTestMaxInt64-1, 1, 1)
	if err != nil || !reflect.DeepEqual(got, []HTTPCoverageRange{{coverageTestMaxInt64 - 1, coverageTestMaxInt64}}) {
		t.Fatalf("max boundary: %v %v", got, err)
	}
}

func TestHTTPCoverageMergeRejectsWithoutPartialResult(t *testing.T) {
	for _, tc := range []struct {
		current         []HTTPCoverageRange
		size, offset, n int64
		limit           int
		want            error
	}{
		{nil, 0, 0, 0, 1, ErrHTTPCoverageInvalid}, {nil, 10, -1, 0, 1, ErrHTTPCoverageInvalid},
		{nil, 10, 11, 0, 1, ErrHTTPCoverageInvalid}, {nil, 10, 0, -1, 1, ErrHTTPCoverageInvalid},
		{nil, 10, 9, 2, 1, ErrHTTPCoverageInvalid}, {nil, coverageTestMaxInt64, coverageTestMaxInt64 - 1, 2, 1, ErrHTTPCoverageInvalid},
		{nil, 10, 0, 1, 0, ErrHTTPCoverageInvalid},
		{[]HTTPCoverageRange{{0, 1}, {1, 2}}, 10, 0, 0, 2, ErrHTTPCoverageInvalid},
		{[]HTTPCoverageRange{{0, 1}, {2, 3}}, 10, 0, 0, 1, ErrHTTPCoverageLimit},
		{[]HTTPCoverageRange{{0, 1}}, 10, 3, 1, 1, ErrHTTPCoverageLimit},
	} {
		before := append([]HTTPCoverageRange(nil), tc.current...)
		out, err := MergeHTTPCoverageWrite(tc.current, tc.size, tc.offset, tc.n, tc.limit)
		if out != nil || !errors.Is(err, tc.want) || len(before) > 0 && !reflect.DeepEqual(before, tc.current) {
			t.Fatalf("failed write: %v %v", out, err)
		}
	}
}

func coverageBitmapRanges(bits []bool) []HTTPCoverageRange {
	out := make([]HTTPCoverageRange, 0)
	for i := 0; i < len(bits); {
		if !bits[i] {
			i++
			continue
		}
		begin := i
		for i < len(bits) && bits[i] {
			i++
		}
		out = append(out, HTTPCoverageRange{int64(begin), int64(i)})
	}
	return out
}

func TestHTTPCoverageBitmapOracle(t *testing.T) {
	random := rand.New(rand.NewSource(17))
	const size = 128
	bits, baseline := make([]bool, size), make([]bool, size)
	for i := range baseline {
		baseline[i] = random.Intn(3) == 0
	}
	prior := coverageTestState(t, size)
	prior.SettledRanges = coverageBitmapRanges(baseline)
	prior.CheckpointHash = coverageTestCheckpoint
	var current []HTTPCoverageRange
	for step := 0; step < 200; step++ {
		offset := random.Intn(size + 1)
		written := random.Intn(size - offset + 1)
		var err error
		current, err = MergeHTTPCoverageWrite(current, size, int64(offset), int64(written), size)
		if err != nil {
			t.Fatal(err)
		}
		for i := offset; i < offset+written; i++ {
			bits[i] = true
		}
		if want := coverageBitmapRanges(bits); !reflect.DeepEqual(current, want) {
			t.Fatalf("step %d: %v != %v", step, current, want)
		}
		plan := coverageTestPlan(t, prior, current)
		var net int64
		for i, covered := range bits {
			if covered && !baseline[i] {
				net++
			}
		}
		if plan.NetNewBytes != net {
			t.Fatalf("step %d net %d != %d", step, plan.NetNewBytes, net)
		}
	}
}

func TestHTTPCoverageThresholdAndSmallWhole(t *testing.T) {
	total := HTTPCoverageThreshold
	for _, tc := range []struct {
		name                         string
		size                         int64
		current                      []HTTPCoverageRange
		net                          int64
		threshold, whole, settlement bool
	}{
		{"below threshold", 2 * total, []HTTPCoverageRange{{0, total - 1}}, total - 1, false, false, false},
		{"threshold", 2 * total, []HTTPCoverageRange{{0, total}}, total, true, false, true},
		{"disjoint threshold", 3 * total, []HTTPCoverageRange{{0, total / 2}, {2 * total, 2*total + total/2}}, total, true, false, true},
		{"consume all", 3 * total, []HTTPCoverageRange{{0, 2*total + 9}}, 2*total + 9, true, false, true},
		{"small partial", 10, []HTTPCoverageRange{{0, 9}}, 9, false, false, false},
		{"small whole", 10, []HTTPCoverageRange{{0, 10}}, 10, false, true, true},
		{"small boundary", total - 1, []HTTPCoverageRange{{0, total - 1}}, total - 1, false, true, true},
		{"whole exact threshold", total, []HTTPCoverageRange{{0, total}}, total, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := coverageTestPlan(t, coverageTestState(t, tc.size), tc.current)
			if p.NetNewBytes != tc.net || p.ThresholdReached != tc.threshold || p.WholeResourceCovered != tc.whole || (p.Settlement != nil) != tc.settlement {
				t.Fatalf("%+v", p)
			}
			if p.Settlement != nil {
				if p.Settlement.SettlementSequence != 1 || !reflect.DeepEqual(p.Settlement.SettledRanges, tc.current) {
					t.Fatalf("not consume-all: %+v", p.Settlement)
				}
			}
		})
	}
}

func TestHTTPCoverageReplayRequiresAppliedSettlement(t *testing.T) {
	prior := coverageTestState(t, 4*HTTPCoverageThreshold)
	current := []HTTPCoverageRange{{0, 2 * HTTPCoverageThreshold}}
	first := coverageTestPlan(t, prior, current)
	same := coverageTestPlan(t, prior, current)
	if first.Settlement == nil || !reflect.DeepEqual(first, same) {
		t.Fatal("same old prior must deterministically offer same candidate")
	}
	if first.Snapshot.SettlementSequence != 0 || len(first.Snapshot.SettledRanges) != 0 {
		t.Fatal("snapshot consumed baseline")
	}
	fromSnapshot := coverageTestPlan(t, first.Snapshot, current)
	if fromSnapshot.Settlement == nil || fromSnapshot.NetNewBytes != 2*HTTPCoverageThreshold {
		t.Fatal("same checkpoint hash incorrectly spent baseline")
	}
	// Pure JSON round trip is not a DB/Sync or execution proof.
	applied := coverageTestClone(*first.Settlement)
	if err := ValidateHTTPCoverageState(applied, 1); err != nil {
		t.Fatal(err)
	}
	after := coverageTestPlan(t, applied, current)
	if after.NetNewBytes != 0 || after.ThresholdReached || after.Settlement != nil {
		t.Fatalf("repeated reward: %+v", after)
	}
	next := coverageTestPlan(t, applied, []HTTPCoverageRange{{0, 3 * HTTPCoverageThreshold}})
	if next.NetNewBytes != HTTPCoverageThreshold || next.Settlement == nil || next.Settlement.SettlementSequence != 2 || next.Settlement.SettledRanges[0].End != 3*HTTPCoverageThreshold {
		t.Fatalf("next settlement: %+v", next)
	}
}

func TestHTTPCoverageTruncatePreservesHistoricalBaseline(t *testing.T) {
	prior := coverageTestState(t, 4*HTTPCoverageThreshold)
	prior.DurableRanges = []HTTPCoverageRange{{0, 2 * HTTPCoverageThreshold}}
	prior.SettledRanges = []HTTPCoverageRange{{0, 2 * HTTPCoverageThreshold}}
	prior.CheckpointHash = coverageTestCheckpoint
	prior.SettlementSequence = 1
	before := coverageTestClone(prior)
	empty := coverageTestPlan(t, prior, nil)
	if empty.Snapshot.DurableRanges == nil || len(empty.Snapshot.DurableRanges) != 0 || !reflect.DeepEqual(empty.Snapshot.SettledRanges, prior.SettledRanges) || empty.NetNewBytes != 0 || empty.Settlement != nil {
		t.Fatalf("truncate: %+v", empty)
	}
	partial := coverageTestPlan(t, empty.Snapshot, []HTTPCoverageRange{{0, HTTPCoverageThreshold}})
	if partial.NetNewBytes != 0 || partial.Settlement != nil {
		t.Fatalf("rewrites rewarded: %+v", partial)
	}
	rebuilt := coverageTestPlan(t, partial.Snapshot, []HTTPCoverageRange{{0, 3 * HTTPCoverageThreshold}})
	if rebuilt.NetNewBytes != HTTPCoverageThreshold || rebuilt.Settlement == nil || rebuilt.Settlement.SettlementSequence != 2 {
		t.Fatalf("new suffix: %+v", rebuilt)
	}
	if !reflect.DeepEqual(prior, before) {
		t.Fatal("prior mutated")
	}
}

func TestHTTPCoverageWholeWithoutNetNewNeverRewards(t *testing.T) {
	for _, size := range []int64{1, 10, HTTPCoverageThreshold - 1, HTTPCoverageThreshold, 2 * HTTPCoverageThreshold} {
		s := coverageTestState(t, size)
		s.SettledRanges = []HTTPCoverageRange{{0, size}}
		s.CheckpointHash = coverageTestCheckpoint
		s.SettlementSequence = ^uint64(0)
		p := coverageTestPlan(t, s, []HTTPCoverageRange{{0, size}})
		if !p.WholeResourceCovered || p.NetNewBytes != 0 || p.ThresholdReached || p.Settlement != nil {
			t.Fatalf("size %d: %+v", size, p)
		}
	}
	s := coverageTestState(t, 10)
	s.SettledRanges = []HTTPCoverageRange{{0, 9}}
	s.CheckpointHash = coverageTestCheckpoint
	s.SettlementSequence = 1
	p := coverageTestPlan(t, s, []HTTPCoverageRange{{0, 10}})
	if p.NetNewBytes != 1 || p.ThresholdReached || !p.WholeResourceCovered || p.Settlement == nil {
		t.Fatalf("small mathematical candidate: %+v", p)
	}
}

func TestHTTPCoverageLimitsOnlyForNeededCandidate(t *testing.T) {
	s := coverageTestState(t, 4*HTTPCoverageThreshold)
	s.SettledRanges = []HTTPCoverageRange{{0, 1}}
	s.CheckpointHash = coverageTestCheckpoint
	s.SettlementSequence = ^uint64(0)
	// Snapshot is valid even though an unused union would exceed the limit and
	// a hypothetical seq+1 would overflow. Neither candidate is requested.
	p, err := PlanHTTPCoverageCheckpoint(s, []HTTPCoverageRange{{3, 4}}, coverageTestCheckpoint2, 1)
	if err != nil || p.NetNewBytes != 1 || p.Settlement != nil || p.Snapshot.SettlementSequence != s.SettlementSequence {
		t.Fatalf("unused candidate: %+v %v", p, err)
	}
	for _, tc := range []struct {
		name              string
		seq               uint64
		baseline, current []HTTPCoverageRange
		limit             int
	}{
		{"sequence overflow", ^uint64(0), []HTTPCoverageRange{}, []HTTPCoverageRange{{0, HTTPCoverageThreshold}}, 1},
		{"union over limit", 1, []HTTPCoverageRange{{0, HTTPCoverageThreshold}}, []HTTPCoverageRange{{2 * HTTPCoverageThreshold, 3 * HTTPCoverageThreshold}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := coverageTestState(t, 4*HTTPCoverageThreshold)
			prior.CheckpointHash = coverageTestCheckpoint
			prior.SettlementSequence = tc.seq
			prior.SettledRanges = tc.baseline
			before := coverageTestClone(prior)
			p, err := PlanHTTPCoverageCheckpoint(prior, tc.current, coverageTestCheckpoint2, tc.limit)
			if !reflect.DeepEqual(p, HTTPCoveragePlan{}) || !errors.Is(err, ErrHTTPCoverageLimit) || !reflect.DeepEqual(before, prior) {
				t.Fatalf("nonzero failed plan: %+v %v", p, err)
			}
		})
	}
}

func TestHTTPCoveragePlanRejectsBadShapeBeforeUnprovable(t *testing.T) {
	s := coverageTestState(t, 10)
	s.Status = "unprovable"
	if err := ValidateHTTPCoverageState(s, 1); err != nil {
		t.Fatal(err)
	}
	p, err := PlanHTTPCoverageCheckpoint(s, nil, coverageTestCheckpoint, 1)
	if !reflect.DeepEqual(p, HTTPCoveragePlan{}) || !errors.Is(err, ErrHTTPCoverageUnprovable) {
		t.Fatalf("unprovable: %+v %v", p, err)
	}
	for _, tc := range []struct {
		mutate func(*HTTPCoverageState)
		want   error
	}{
		{func(s *HTTPCoverageState) { s.Version = 2 }, ErrHTTPCoverageUnsupportedVersion},
		{func(s *HTTPCoverageState) { s.DurableRanges = nil }, ErrHTTPCoverageInvalid},
		{func(s *HTTPCoverageState) { s.SettledRanges = nil }, ErrHTTPCoverageInvalid},
		{func(s *HTTPCoverageState) { s.IdentityHash = "bad" }, ErrHTTPCoverageInvalid},
	} {
		bad := coverageTestClone(s)
		tc.mutate(&bad)
		p, err := PlanHTTPCoverageCheckpoint(bad, nil, coverageTestCheckpoint, 1)
		if !reflect.DeepEqual(p, HTTPCoveragePlan{}) || !errors.Is(err, tc.want) {
			t.Fatalf("shape bypass: %+v %v", p, err)
		}
	}
	tracking := coverageTestState(t, 10)
	for _, checkpoint := range []string{"", "bad", strings.ToUpper(coverageTestCheckpoint)} {
		p, err := PlanHTTPCoverageCheckpoint(tracking, nil, checkpoint, 1)
		if !reflect.DeepEqual(p, HTTPCoveragePlan{}) || !errors.Is(err, ErrHTTPCoverageInvalid) {
			t.Fatalf("checkpoint: %+v %v", p, err)
		}
	}
	for _, current := range [][]HTTPCoverageRange{{{0, 1}, {1, 2}}, {{-1, 2}}, {{0, 11}}} {
		p, err := PlanHTTPCoverageCheckpoint(tracking, current, coverageTestCheckpoint, 2)
		if !reflect.DeepEqual(p, HTTPCoveragePlan{}) || !errors.Is(err, ErrHTTPCoverageInvalid) {
			t.Fatalf("current: %+v %v", p, err)
		}
	}
}

func TestHTTPCoveragePlanOutputsHaveIndependentStorage(t *testing.T) {
	s := coverageTestState(t, 4*HTTPCoverageThreshold)
	s.DurableRanges = []HTTPCoverageRange{{0, HTTPCoverageThreshold}}
	s.SettledRanges = []HTTPCoverageRange{{0, HTTPCoverageThreshold / 2}}
	s.CheckpointHash = coverageTestCheckpoint
	s.SettlementSequence = 1
	current := []HTTPCoverageRange{{0, 2 * HTTPCoverageThreshold}}
	before := coverageTestClone(s)
	beforeCurrent := append([]HTTPCoverageRange(nil), current...)
	p := coverageTestPlan(t, s, current)
	if p.Settlement == nil {
		t.Fatal("missing candidate")
	}
	p.Snapshot.DurableRanges[0].Begin = 1
	p.Snapshot.SettledRanges[0].End = 1
	if p.Settlement.DurableRanges[0].Begin != 0 || p.Settlement.SettledRanges[0].End != 2*HTTPCoverageThreshold {
		t.Fatal("snapshot aliases settlement")
	}
	p.Settlement.DurableRanges[0].End = 2
	p.Settlement.SettledRanges[0].End = 3
	if p.Snapshot.DurableRanges[0].End != 2*HTTPCoverageThreshold || p.Snapshot.SettledRanges[0].End != 1 {
		t.Fatal("settlement aliases snapshot")
	}
	if !reflect.DeepEqual(s, before) || !reflect.DeepEqual(current, beforeCurrent) {
		t.Fatal("output aliases inputs")
	}
	current[0].End = 4
	s.SettledRanges[0].End = 5
	if p.Snapshot.DurableRanges[0].End != 2*HTTPCoverageThreshold || p.Settlement.SettledRanges[0].End != 3 {
		t.Fatal("inputs alias outputs")
	}
	empty := coverageTestPlan(t, coverageTestState(t, 10), nil)
	if empty.Snapshot.DurableRanges == nil || empty.Snapshot.SettledRanges == nil {
		t.Fatal("transient nil was persisted as nil")
	}
}

func TestHTTPCoverageMaxInt64Plan(t *testing.T) {
	s := coverageTestState(t, coverageTestMaxInt64)
	p := coverageTestPlan(t, s, []HTTPCoverageRange{{0, coverageTestMaxInt64}})
	if p.NetNewBytes != coverageTestMaxInt64 || !p.ThresholdReached || !p.WholeResourceCovered || p.Settlement == nil {
		t.Fatalf("int64 boundary: %+v", p)
	}
	after := coverageTestPlan(t, *p.Settlement, []HTTPCoverageRange{{0, coverageTestMaxInt64}})
	if after.NetNewBytes != 0 || after.Settlement != nil {
		t.Fatalf("intersection overflow: %+v", after)
	}
}
