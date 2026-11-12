// Package mcpauthority builds immutable local-policy decision values. It does
// not load files, verify or pin output roots, inject Core authority, or enable
// MCP write capabilities. Only a trusted startup caller may construct a value.
package mcpauthority

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
)

var ErrInvalidAuthority = errors.New("invalid MCP authority")

var datasetPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
var networkPattern = regexp.MustCompile(`^mcp-network/v1:[0-9a-f]{64}$`)

const authorityDomain = "kownload-mcp-authority/v1\x00"
const authorityPrefix = "mcp-authority-v1:"

// VerifiedRootBinding is the trusted caller's claim of prior same-fd root
// verification. New only checks these values against the policy: the type name
// does not prove verification, ownership, permissions or a live pinned handle.
// Never populate it from an HTTP request or model-provided policy/identity.
type VerifiedRootBinding struct {
	RootID   string
	Path     string
	Identity transfer.CompletionFileIdentity
}

// Authority owns immutable decisions for one dataset. Its ID identifies the
// configuration, not a credential or a substitute for execution-time guards.
// A nil/zero Authority denies everything. No caller-owned slice is retained.
type Authority struct {
	id      string
	dataset string
	creates map[string]mcpwritepolicy.CreateGrant
	tasks   map[string]taskGrant
}

type taskGrant struct{ pause, resume bool }

type canonicalRoot struct {
	RootID string `json:"rootId"`
	Path   string `json:"path"`
	Device string `json:"device"`
	Inode  string `json:"inode"`
}

type canonicalAuthorityV1 struct {
	Version         int                             `json:"version"`
	Policy          mcpwritepolicy.PolicySnapshotV1 `json:"policy"`
	DatasetID       string                          `json:"datasetId"`
	NetworkPolicyID string                          `json:"networkPolicyId"`
	Roots           []canonicalRoot                 `json:"roots"`
	EffectsVersion  string                          `json:"effectsVersion"`
}

// New performs no IO. networkPolicyID must come from the actual immutable
// network policy, and every configured policy root needs a trusted binding.
// Matching strings alone do not establish either provenance. Callers must not
// mutate input concurrently with construction; later mutations are isolated.
func New(p *mcpwritepolicy.Policy, dataset, networkPolicyID string, roots []VerifiedRootBinding) (*Authority, error) {
	v, err := p.SnapshotV1()
	if err != nil || !datasetPattern.MatchString(dataset) || !networkPattern.MatchString(networkPolicyID) || len(roots) != len(v.Roots) {
		return nil, ErrInvalidAuthority
	}
	byID := make(map[string]VerifiedRootBinding, len(roots))
	for _, root := range roots {
		if _, exists := byID[root.RootID]; exists || root.Identity.Inode == 0 {
			return nil, ErrInvalidAuthority
		}
		byID[root.RootID] = root
	}
	bound := make([]canonicalRoot, 0, len(v.Roots))
	for _, spec := range v.Roots {
		root, exists := byID[spec.ID]
		if !exists || root.Path != spec.Path {
			return nil, ErrInvalidAuthority
		}
		bound = append(bound, canonicalRoot{spec.ID, spec.Path, strconv.FormatUint(root.Identity.Device, 10), strconv.FormatUint(root.Identity.Inode, 10)})
	}
	// Fixed struct field order, sorted sets, explicit empty arrays, no whitespace
	// or trailing LF. No raw input formatting or floating-point identities enter P.
	encoded, err := json.Marshal(canonicalAuthorityV1{1, v, dataset, networkPolicyID, bound, "effects/v1"})
	if err != nil {
		return nil, ErrInvalidAuthority
	}
	sum := sha256.Sum256(append([]byte(authorityDomain), encoded...))
	a := &Authority{id: authorityPrefix + hex.EncodeToString(sum[:]), dataset: dataset,
		creates: make(map[string]mcpwritepolicy.CreateGrant), tasks: make(map[string]taskGrant)}
	// Capture decisions through the original policy methods. Do not keep the
	// caller's Policy pointer or reimplement its scope/permission semantics.
	for _, scope := range v.Datasets {
		if scope.ID != dataset {
			continue
		}
		for _, root := range scope.RootIDs {
			if grant, ok := p.AllowCreate(dataset, root); ok {
				grant.Effects = append([]string{}, grant.Effects...)
				a.creates[root] = grant
			}
		}
		for _, task := range scope.TaskIDs {
			a.tasks[task] = taskGrant{p.AllowTask("pause", dataset, task), p.AllowTask("resume", dataset, task)}
		}
	}
	return a, nil
}

func (a *Authority) PolicyID() string {
	if a == nil {
		return ""
	}
	return a.id
}

func (a *Authority) AllowCreate(policyID, dataset, rootID string) (mcpwritepolicy.CreateGrant, bool) {
	if a == nil || a.id == "" || policyID != a.id || dataset != a.dataset {
		return mcpwritepolicy.CreateGrant{}, false
	}
	grant, ok := a.creates[rootID]
	if !ok {
		return mcpwritepolicy.CreateGrant{}, false
	}
	grant.Effects = append([]string{}, grant.Effects...)
	return grant, true
}

func (a *Authority) AllowTask(policyID, dataset, operation, taskID string) bool {
	if a == nil || a.id == "" || policyID != a.id || dataset != a.dataset {
		return false
	}
	grant := a.tasks[taskID]
	switch operation {
	case "pause":
		return grant.pause
	case "resume":
		return grant.resume
	default:
		return false
	}
}
