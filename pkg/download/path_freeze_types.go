package download

import (
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

const (
	HTTPPathCreateKind = "http-path-create/v1"
	HTTPPathCapability = "single-http-path-freeze/v1"
)

// PathIntent contains caller provenance, never a frozen decision or rule ID.
// Its Path must exactly equal Request.Opts.Path before defaults are applied.
type PathIntent struct {
	Version int               `json:"version"`
	Path    string            `json:"path"`
	Origin  pathpolicy.Origin `json:"origin"`
}
type HTTPPathCreateRequest struct {
	Version              int                          `json:"version"`
	RequiredCapabilities []string                     `json:"requiredCapabilities"`
	OperationID          string                       `json:"operationId"`
	Request              *base.CreateTaskBatchItem    `json:"request"`
	PathIntent           PathIntent                   `json:"pathIntent"`
	Integrity            *VerifiedHTTPIntegrityIntent `json:"integrity,omitempty"`
}

// PathPolicySnapshot is captured once at R, after effective default/placeholder
// resolution. It is separate from the original caller request fingerprint.
// Config is an owned ordered snapshot; a later global setting is not a replay.
type PathPolicySnapshot struct {
	Version              int               `json:"version"`
	Config               pathpolicy.Config `json:"config"`
	EffectiveDefaultRoot string            `json:"effectiveDefaultRoot"`
	InputRoot            string            `json:"inputRoot"`
}
type PathFreezePhase string

const (
	PathFreezePending PathFreezePhase = "pending"
	PathFreezeFrozen  PathFreezePhase = "frozen"
)

// PathOutputState is used only by ordinary no-checksum HTTP. It reuses the
// anchored filesystem binding without making it a permission or SHA record.
// Published, publishing, uncertain and unknown ACK never authorize an RW open.
type PathOutputState struct {
	Phase       MCPWriteOutputPhase        `json:"phase"`
	StagingName string                     `json:"stagingName"`
	Binding     *transfer.MCPOutputBinding `json:"binding"`
}
type TaskPathFreeze struct {
	Version            int                  `json:"version"`
	RequiredCapability string               `json:"requiredCapability"`
	Phase              PathFreezePhase      `json:"phase"`
	Decision           *pathpolicy.Decision `json:"decision"`
	Layout             *pathpolicy.LayoutV1 `json:"layout"`
	LayoutHash         string               `json:"layoutHash"`
	Output             *PathOutputState     `json:"output"`
}

// The execution token is a CURRENT F-CAS coordinate, not permanent frozen
// validity. A later legitimate run reuses the same Decision/Layout unchanged.
type PathFreezeExpected struct {
	Token                 transfer.RecoveryToken
	Record                batchOperationRecord
	Meta                  *fetcher.FetcherMeta
	SourceHash            string
	ExecutionDefaultsHash string
	CheckpointPresent     bool
	CheckpointHash        string
}
type PathFreezePrepared struct {
	ResolvedMeta *fetcher.FetcherMeta
	Decision     pathpolicy.Decision
	Layout       pathpolicy.LayoutV1
	LayoutHash   string
	StagingName  string
}

// HTTPPathCapability is produced by the active Core, never by preview alone.
type HTTPPathMethods struct {
	Create  string `json:"create"`
	Query   string `json:"query"`
	Cancel  string `json:"cancel"`
	Preview string `json:"preview"`
}
type HTTPPathCapability struct {
	ScopeVersion         int             `json:"scopeVersion"`
	DatasetID            string          `json:"datasetId"`
	Kind                 string          `json:"kind"`
	RequestVersion       int             `json:"requestVersion"`
	Supported            bool            `json:"supported"`
	RequiredCapabilities []string        `json:"requiredCapabilities"`
	Methods              HTTPPathMethods `json:"methods"`
}
type HTTPPathOperationResult struct {
	Operation            BatchOperationResult `json:"operation"`
	RequiredCapabilities []string             `json:"requiredCapabilities"`
	CompletionViews      []TaskCompletionView `json:"completionViews,omitempty"`
}
