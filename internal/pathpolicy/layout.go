package pathpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
)

const LayoutVersion = 1

var ErrLayout = errors.New("invalid path layout")

// LayoutV1 is a stable value projection of final paths, not an execution plan
// or permission to perform IO. The caller must supply the complete, actually
// resolved protocol layout. BT storage setup, late media sidecars, persistence,
// required markers, and execution/CAS checks remain outside these helpers.
//
// Field order, JSON tags, explicit false values and non-nil Members determine
// the compact encoding/json representation used by LayoutFingerprint.
type LayoutV1 struct {
	Version    int      `json:"version"`
	ParentRoot string   `json:"parentRoot"`
	Members    []Member `json:"members"`
}

// Member identifies a final file path relative to ParentRoot. Index is its
// position in the complete frozen resource list, including unselected members.
// Selected records the protocol's effective download selection; false does
// not guarantee that a protocol will never create or access this path.
type Member struct {
	Index        int    `json:"index"`
	RelativePath string `json:"relativePath"`
	Selected     bool   `json:"selected"`
}

// BuildLayoutV1 validates and copies the complete member list, sorting only
// the owned copy by Index. Indices must be unique and cover exactly 0..N-1.
// No path is cleaned, case folded, Unicode normalized, decoded, or resolved
// through the filesystem. Invalid inputs return a zero layout and fixed error.
func BuildLayoutV1(parentRoot string, members []Member) (LayoutV1, error) {
	if !validRoot(parentRoot) || len(members) == 0 {
		return LayoutV1{}, ErrLayout
	}
	owned := append([]Member(nil), members...)
	sort.Slice(owned, func(i, j int) bool { return owned[i].Index < owned[j].Index })
	paths := make(map[string]bool, len(owned))
	for index, member := range owned {
		if member.Index != index || !validLayoutRelative(member.RelativePath) || paths[member.RelativePath] {
			return LayoutV1{}, ErrLayout
		}
		paths[member.RelativePath] = true
	}
	return LayoutV1{Version: LayoutVersion, ParentRoot: parentRoot, Members: owned}, nil
}

// LayoutFingerprint returns lowercase SHA-256 of the validated, Index-sorted
// compact JSON value. It never mutates a caller's layout or accepts an unknown
// version. It does not authenticate raw JSON, task provenance, or source identity.
func LayoutFingerprint(layout LayoutV1) (string, error) {
	if layout.Version != LayoutVersion {
		return "", ErrUnsupportedVersion
	}
	canonical, err := BuildLayoutV1(layout.ParentRoot, layout.Members)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", ErrLayout
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validLayoutRelative(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00") {
		return false
	}
	// Reject volume-qualified Windows paths even on a Unix host. V1 relative
	// paths use slash-separated components and have no drive or UNC syntax.
	if len(value) >= 2 && value[1] == ':' &&
		(value[0] >= 'A' && value[0] <= 'Z' || value[0] >= 'a' && value[0] <= 'z') {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}
