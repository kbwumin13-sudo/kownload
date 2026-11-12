package download

import "github.com/GopeedLab/gopeed/internal/transfer"

const MCPWriteNamespace = "mcp-write/v1"

type MCPWriteKind string

const (
	MCPWriteCreate MCPWriteKind = "create"
	MCPWritePause  MCPWriteKind = "pause"
	MCPWriteResume MCPWriteKind = "resume"
)

type MCPWriteErrorCode string

const (
	MCPWriteInvalidRequest        MCPWriteErrorCode = "invalid_request"
	MCPWritePermissionDenied      MCPWriteErrorCode = "permission_denied"
	MCPWriteCapabilityUnavailable MCPWriteErrorCode = "capability_unavailable"
	MCPWriteDatasetMismatch       MCPWriteErrorCode = "dataset_mismatch"
	MCPWritePolicyMismatch        MCPWriteErrorCode = "policy_mismatch"
	MCPWriteOperationConflict     MCPWriteErrorCode = "operation_conflict"
	MCPWriteTaskUnavailable       MCPWriteErrorCode = "task_unavailable"
	MCPWriteStaleTarget           MCPWriteErrorCode = "stale_target"
	MCPWriteNetworkDenied         MCPWriteErrorCode = "network_denied"
	MCPWriteOutputConflict        MCPWriteErrorCode = "output_conflict"
	MCPWriteStorageUnavailable    MCPWriteErrorCode = "storage_unavailable"
	MCPWriteExecutionFailed       MCPWriteErrorCode = "execution_failed"
	MCPWriteCancelled             MCPWriteErrorCode = "cancelled"
)

// MCPWriteError exposes a fixed category, never a URL, filesystem path or cause.
type MCPWriteError struct{ Code MCPWriteErrorCode }

func (e *MCPWriteError) Error() string {
	if e == nil {
		return "storage_unavailable"
	}
	return string(e.Code)
}

func (c MCPWriteErrorCode) Valid() bool {
	switch c {
	case MCPWriteInvalidRequest, MCPWritePermissionDenied, MCPWriteCapabilityUnavailable,
		MCPWriteDatasetMismatch, MCPWritePolicyMismatch, MCPWriteOperationConflict,
		MCPWriteTaskUnavailable, MCPWriteStaleTarget, MCPWriteNetworkDenied,
		MCPWriteOutputConflict, MCPWriteStorageUnavailable, MCPWriteExecutionFailed, MCPWriteCancelled:
		return true
	}
	return false
}

type MCPWriteTarget struct {
	Episode        string `json:"episode"`
	Generation     uint64 `json:"generation"`
	SourceRevision uint64 `json:"sourceRevision"`
}

type MCPWriteCapabilityRequest struct {
	Version   int    `json:"version"`
	DatasetID string `json:"datasetId"`
	TaskID    string `json:"taskId,omitempty"`
}
type MCPWriteOperations struct {
	Create bool `json:"create"`
	Pause  bool `json:"pause"`
	Resume bool `json:"resume"`
}
type MCPWriteTaskCapability struct {
	TaskID   string         `json:"taskId"`
	Expected MCPWriteTarget `json:"expected"`
}
type MCPWriteCapability struct {
	Version         int                     `json:"version"`
	DatasetID       string                  `json:"datasetId"`
	PolicyID        *string                 `json:"policyId"`
	NetworkPolicyID *string                 `json:"networkPolicyId"`
	Operations      MCPWriteOperations      `json:"operations"`
	OutputRootIDs   []string                `json:"outputRootIds"`
	Task            *MCPWriteTaskCapability `json:"task"`
	Reason          *MCPWriteErrorCode      `json:"reason"`
}
type MCPWriteCreateRequest struct {
	URL          string `json:"url"`
	OutputRootID string `json:"outputRootId"`
	FileName     string `json:"fileName"`
}
type MCPWriteActionRequest struct {
	TaskID   string         `json:"taskId"`
	Expected MCPWriteTarget `json:"expected"`
}
type MCPWriteRequest struct {
	Version     int                    `json:"version"`
	DatasetID   string                 `json:"datasetId"`
	PolicyID    string                 `json:"policyId"`
	OperationID string                 `json:"operationId"`
	Kind        MCPWriteKind           `json:"kind"`
	Create      *MCPWriteCreateRequest `json:"create,omitempty"`
	Action      *MCPWriteActionRequest `json:"action,omitempty"`
}
type MCPWriteQuery struct {
	Version     int    `json:"version"`
	DatasetID   string `json:"datasetId"`
	PolicyID    string `json:"policyId"`
	OperationID string `json:"operationId"`
}

type MCPWriteReceiptState string

const (
	MCPWriteUnknown     MCPWriteReceiptState = "unknown"
	MCPWriteNotRecorded MCPWriteReceiptState = "not_recorded"
	MCPWritePending     MCPWriteReceiptState = "pending"
	MCPWriteSettled     MCPWriteReceiptState = "settled"
)

type MCPWriteOutcome string

const (
	MCPWriteCreated  MCPWriteOutcome = "created"
	MCPWriteApplied  MCPWriteOutcome = "applied"
	MCPWriteRejected MCPWriteOutcome = "rejected"
)

type MCPWriteResult struct {
	TaskID    *string            `json:"taskId"`
	Outcome   MCPWriteOutcome    `json:"outcome"`
	ErrorCode *MCPWriteErrorCode `json:"errorCode"`
	Expected  *MCPWriteTarget    `json:"expected"`
	Accepted  *MCPWriteTarget    `json:"accepted"`
}
type MCPWriteReceipt struct {
	Version     int    `json:"version"`
	DatasetID   string `json:"datasetId"`
	PolicyID    string `json:"policyId"`
	OperationID string `json:"operationId"`
	// Unknown/not_recorded queries cannot invent the command's kind.
	Kind          *MCPWriteKind        `json:"kind"`
	State         MCPWriteReceiptState `json:"state"`
	Result        *MCPWriteResult      `json:"result"`
	QueryRequired bool                 `json:"queryRequired"`
}

// MCPExecutionBinding is a trusted Core snapshot, not a wire command or IO permit.
// For GUI actions PreserveGUI is true: no MCP output/effect relabelling occurs.
type MCPExecutionBinding struct {
	Version            int                  `json:"version"`
	RequiredCapability string               `json:"requiredCapability"`
	DatasetID          string               `json:"datasetId"`
	PolicyID           string               `json:"policyId"`
	NetworkPolicyID    string               `json:"networkPolicyId"`
	PreserveGUI        bool                 `json:"preserveGui"`
	Root               *MCPWriteRootBinding `json:"root"`
	FileName           string               `json:"fileName"`
	StagingName        string               `json:"stagingName"`
	EffectsVersion     string               `json:"effectsVersion"`
	Effects            []string             `json:"effects"`
}
type MCPWriteRootBinding struct {
	RootID   string                          `json:"rootId"`
	Path     string                          `json:"path"`
	Identity transfer.CompletionFileIdentity `json:"identity"`
}
type MCPWriteOutputPhase string

const (
	MCPWriteOutputPlanned    MCPWriteOutputPhase = "planned"
	MCPWriteOutputStaging    MCPWriteOutputPhase = "staging"
	MCPWriteOutputPublishing MCPWriteOutputPhase = "publishing"
	MCPWriteOutputPublished  MCPWriteOutputPhase = "published"
	MCPWriteOutputUncertain  MCPWriteOutputPhase = "uncertain"
)

type MCPWriteOutputState struct {
	Phase   MCPWriteOutputPhase        `json:"phase"`
	Binding *transfer.MCPOutputBinding `json:"binding"`
}

// MCPTaskState is installed only for MCP-created tasks. GUI action receipts do
// not install it. CreationBinding is immutable; resume replaces only Current.
type MCPTaskState struct {
	Version                 int                 `json:"version"`
	RequiredCapability      string              `json:"requiredCapability"`
	OperationID             string              `json:"operationId"`
	CreationBinding         MCPExecutionBinding `json:"creationBinding"`
	CurrentExecutionBinding MCPExecutionBinding `json:"currentExecutionBinding"`
	Output                  MCPWriteOutputState `json:"output"`
}
