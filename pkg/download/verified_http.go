package download

import (
	"context"
	"errors"

	"github.com/GopeedLab/gopeed/pkg/base"
)

const verifiedHTTPCapability = "http-sha256-completion/v1"

var (
	ErrVerifiedHTTPInvalid           = errors.New("invalid_verified_http_request")
	ErrVerifiedHTTPUnsupported       = errors.New("verified_http_unsupported")
	ErrVerifiedHTTPUnsupportedOption = errors.New("unsupported_option")
)

type VerifiedHTTPIntegrityIntent struct {
	Version            int    `json:"version"`
	RequiredCapability string `json:"requiredCapability"`
	Algorithm          string `json:"algorithm"`
	ExpectedSHA256     string `json:"expectedSHA256"`
}

type VerifiedHTTPRequest struct {
	Version     int                         `json:"version"`
	OperationID string                      `json:"operationId"`
	Request     *base.CreateTaskBatchItem   `json:"request"`
	Integrity   VerifiedHTTPIntegrityIntent `json:"integrity"`
}

type VerifiedHTTPCapability struct {
	Version    int    `json:"version"`
	Capability string `json:"capability"`
	Supported  bool   `json:"supported"`
}

type TaskCompletionView struct {
	Version       int    `json:"version"`
	ScopeVersion  int    `json:"scopeVersion"`
	DatasetID     string `json:"datasetId"`
	TaskID        string `json:"taskId"`
	CompletionID  string `json:"completionId"`
	Available     bool   `json:"available"`
	Phase         string `json:"phase,omitempty"`
	RecordedMatch *bool  `json:"recordedMatch,omitempty"`
	ErrorCategory string `json:"errorCategory,omitempty"`
}

type VerifiedHTTPOperationResult struct {
	Operation       BatchOperationResult `json:"operation"`
	CompletionViews []TaskCompletionView `json:"completionViews"`
}

// Until the complete writer/guard/publication pipeline is connected, these
// dedicated methods fail closed. Merely compiling a filesystem adapter or a
// Task reference never constitutes an execution capability.
func (d *Downloader) VerifiedHTTPCapabilityForDataset(dataset string) (VerifiedHTTPCapability, error) {
	if err := withDatasetReadView(d.storage, dataset, func(datasetReader) error { return nil }); err != nil {
		return VerifiedHTTPCapability{}, err
	}
	return VerifiedHTTPCapability{1, verifiedHTTPCapability, false}, nil
}

func (d *Downloader) CreateVerifiedHTTPOperationForDataset(ctx context.Context, dataset string, request VerifiedHTTPRequest) (VerifiedHTTPOperationResult, error) {
	if err := ctx.Err(); err != nil {
		return VerifiedHTTPOperationResult{}, err
	}
	return d.unsupportedVerifiedHTTP(dataset)
}

func (d *Downloader) QueryVerifiedHTTPOperationForDataset(dataset, operationID string) (VerifiedHTTPOperationResult, error) {
	return d.unsupportedVerifiedHTTP(dataset)
}

func (d *Downloader) CancelVerifiedHTTPOperationForDataset(dataset, operationID string) (VerifiedHTTPOperationResult, error) {
	return d.unsupportedVerifiedHTTP(dataset)
}

func (d *Downloader) unsupportedVerifiedHTTP(dataset string) (VerifiedHTTPOperationResult, error) {
	if _, err := d.VerifiedHTTPCapabilityForDataset(dataset); err != nil {
		return VerifiedHTTPOperationResult{}, err
	}
	return VerifiedHTTPOperationResult{}, ErrVerifiedHTTPUnsupported
}
