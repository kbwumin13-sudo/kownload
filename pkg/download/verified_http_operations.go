package download

import (
	"context"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// These internal entry points are connected to the dedicated receiver only
// once the public capability is ready. They reuse the original S2 R/M/Q and
// ACK permission, never a second creation record or legacy POST fallback.
func (d *Downloader) createVerifiedOperation(ctx context.Context, dataset string, request VerifiedHTTPRequest) (VerifiedHTTPOperationResult, error) {
	if err := ctx.Err(); err != nil {
		return VerifiedHTTPOperationResult{}, err
	}
	if err := validateVerifiedHTTPRequest(request); err != nil {
		return VerifiedHTTPOperationResult{}, err
	}
	integrity := request.Integrity
	batch := BatchOperationRequest{Version: 1, OperationID: request.OperationID, Reqs: []*base.CreateTaskBatchItem{request.Request}, integrity: &integrity}
	_, createErr := d.createBatchOperationInDataset(ctx, batch, dataset)
	// Never label a receipt using an earlier identity preflight. Failed write
	// readback and successful replies both require this fresh same-domain view.
	result, readErr := d.queryVerifiedOperation(dataset, request.OperationID)
	if readErr != nil {
		return VerifiedHTTPOperationResult{}, readErr
	}
	return result, createErr
}

func (d *Downloader) cancelVerifiedOperation(dataset, operationID string) (VerifiedHTTPOperationResult, error) {
	// The transaction also requires typed integrity; this preflight is not the
	// write guard and cannot authorize mutation of a legacy creation operation.
	if _, err := d.queryVerifiedOperation(dataset, operationID); err != nil {
		return VerifiedHTTPOperationResult{}, err
	}
	_, cancelErr := d.cancelBatchOperationInDataset(operationID, dataset, true)
	result, readErr := d.queryVerifiedOperation(dataset, operationID)
	if readErr != nil {
		return VerifiedHTTPOperationResult{}, readErr
	}
	return result, cancelErr
}
