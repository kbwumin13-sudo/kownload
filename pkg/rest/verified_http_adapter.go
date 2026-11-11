package rest

import (
	"context"

	"github.com/GopeedLab/gopeed/pkg/download"
)

type verifiedHTTPCompletionCore interface {
	VerifiedHTTPCapabilityForDataset(string) (download.VerifiedHTTPCapability, error)
	CreateVerifiedHTTPOperationForDataset(context.Context, string, download.VerifiedHTTPRequest) (download.VerifiedHTTPOperationResult, error)
	QueryVerifiedHTTPOperationForDataset(string, string) (download.VerifiedHTTPOperationResult, error)
	CancelVerifiedHTTPOperationForDataset(string, string) (download.VerifiedHTTPOperationResult, error)
}

// Only translates the dedicated typed contract. Core owns same-view domain
// guards, canonical intent/options, C publication and capability activation.
type verifiedHTTPAdapter struct{ core verifiedHTTPCompletionCore }

func (a verifiedHTTPAdapter) capability(domain string) (bool, error) {
	capability, err := a.core.VerifiedHTTPCapabilityForDataset(domain)
	if err != nil {
		return false, err
	}
	if capability.Version != 1 || capability.Capability != verifiedHTTPCapability {
		return false, download.ErrVerifiedHTTPUnsupported
	}
	return capability.Supported, nil
}

func (a verifiedHTTPAdapter) create(ctx context.Context, domain string, request verifiedHTTPRequest) (verifiedHTTPResult, error) {
	result, err := a.core.CreateVerifiedHTTPOperationForDataset(ctx, domain, download.VerifiedHTTPRequest{
		Version: request.Version, OperationID: request.OperationID, Request: request.Request,
		Integrity: download.VerifiedHTTPIntegrityIntent{Version: request.Version, RequiredCapability: request.RequiredCapability,
			Algorithm: request.Integrity.Algorithm, ExpectedSHA256: request.Integrity.ExpectedSHA256},
	})
	return adaptVerifiedHTTPResult(domain, result), err
}
func (a verifiedHTTPAdapter) query(domain, id string) (verifiedHTTPResult, error) {
	result, err := a.core.QueryVerifiedHTTPOperationForDataset(domain, id)
	return adaptVerifiedHTTPResult(domain, result), err
}
func (a verifiedHTTPAdapter) cancel(domain, id string) (verifiedHTTPResult, error) {
	result, err := a.core.CancelVerifiedHTTPOperationForDataset(domain, id)
	return adaptVerifiedHTTPResult(domain, result), err
}

func optionalCompletionString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func adaptVerifiedHTTPResult(domain string, result download.VerifiedHTTPOperationResult) verifiedHTTPResult {
	// ForDataset only returns receipts from its domain-matched coherent view.
	// Do not bless results with a second identity read, or overwrite their own
	// projection association/version fields to make an invalid view look valid.
	out := verifiedHTTPResult{DatasetID: domain, Operation: result.Operation}
	for _, view := range result.CompletionViews {
		var recordedMatch *bool
		if view.RecordedMatch != nil {
			owned := *view.RecordedMatch
			recordedMatch = &owned
		}
		out.Views = append(out.Views, completionView{Version: view.Version, ScopeVersion: view.ScopeVersion,
			DatasetID: view.DatasetID, TaskID: view.TaskID, CompletionID: optionalCompletionString(view.CompletionID),
			Available: view.Available, Phase: optionalCompletionString(view.Phase), RecordedMatch: recordedMatch,
			ErrorCategory: optionalCompletionString(view.ErrorCategory)})
	}
	return out
}
