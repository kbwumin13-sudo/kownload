package download

import (
	"context"
	"errors"

	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
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

// Capability describes this complete receiver/runner, not a permission to skip
// request-specific options, resource identity, recovery or filesystem guards.
// Reads neither enroll Tasks nor probe/create a filesystem path.
func (d *Downloader) VerifiedHTTPCapabilityForDataset(dataset string) (VerifiedHTTPCapability, error) {
	if d.closed.Load() {
		return VerifiedHTTPCapability{}, ErrOperationUnavailable
	}
	if err := withDatasetReadView(d.storage, dataset, func(datasetReader) error { return nil }); err != nil {
		return VerifiedHTTPCapability{}, err
	}
	if d.cfg == nil {
		return VerifiedHTTPCapability{1, verifiedHTTPCapability, false}, nil
	}
	_, atomic := d.storage.(AtomicStorage)
	directHTTP := false
	for _, manager := range d.cfg.FetchManagers {
		// No Build/Setup/Restore is executed by this read. A different manager
		// using the same name cannot self-certify these private HTTP run hooks.
		if _, ok := manager.(*httpProtocol.FetcherManager); ok {
			directHTTP = true
			break
		}
	}
	d.configSnapshotMu.RLock()
	configured := d.cfg.DownloaderStoreConfig != nil
	d.configSnapshotMu.RUnlock()
	supported := configured && atomic && directHTTP && d.verifiedCompleter != nil && d.verifiedDone != nil && transfer.VerifiedCompletionPlatformSupported
	return VerifiedHTTPCapability{1, verifiedHTTPCapability, supported}, nil
}

func (d *Downloader) requireVerifiedHTTPReceiver(dataset string) error {
	capability, err := d.VerifiedHTTPCapabilityForDataset(dataset)
	if err != nil {
		return err
	}
	if !capability.Supported {
		return ErrVerifiedHTTPUnsupported
	}
	return nil
}

func (d *Downloader) CreateVerifiedHTTPOperationForDataset(ctx context.Context, dataset string, request VerifiedHTTPRequest) (VerifiedHTTPOperationResult, error) {
	if err := ctx.Err(); err != nil {
		return VerifiedHTTPOperationResult{}, err
	}
	if err := d.requireVerifiedHTTPReceiver(dataset); err != nil {
		return VerifiedHTTPOperationResult{}, err
	}
	return d.createVerifiedOperation(ctx, dataset, request)
}

func (d *Downloader) QueryVerifiedHTTPOperationForDataset(dataset, operationID string) (VerifiedHTTPOperationResult, error) {
	if err := d.requireVerifiedHTTPReceiver(dataset); err != nil {
		return VerifiedHTTPOperationResult{}, err
	}
	return d.queryVerifiedOperation(dataset, operationID)
}

func (d *Downloader) CancelVerifiedHTTPOperationForDataset(dataset, operationID string) (VerifiedHTTPOperationResult, error) {
	if err := d.requireVerifiedHTTPReceiver(dataset); err != nil {
		return VerifiedHTTPOperationResult{}, err
	}
	return d.cancelVerifiedOperation(dataset, operationID)
}
