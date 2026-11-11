package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/download"
)

type typedVerifiedCoreFixture struct {
	capability download.VerifiedHTTPCapability
	result     download.VerifiedHTTPOperationResult
	err        error
	request    download.VerifiedHTTPRequest
	domain     string
	calls      []string
}

func (f *typedVerifiedCoreFixture) VerifiedHTTPCapabilityForDataset(domain string) (download.VerifiedHTTPCapability, error) {
	f.domain, f.calls = domain, append(f.calls, "capability")
	return f.capability, f.err
}
func (f *typedVerifiedCoreFixture) CreateVerifiedHTTPOperationForDataset(_ context.Context, domain string, request download.VerifiedHTTPRequest) (download.VerifiedHTTPOperationResult, error) {
	f.domain, f.request, f.calls = domain, request, append(f.calls, "create")
	return f.result, f.err
}
func (f *typedVerifiedCoreFixture) QueryVerifiedHTTPOperationForDataset(domain, id string) (download.VerifiedHTTPOperationResult, error) {
	f.domain, f.calls = domain, append(f.calls, "query:"+id)
	return f.result, f.err
}
func (f *typedVerifiedCoreFixture) CancelVerifiedHTTPOperationForDataset(domain, id string) (download.VerifiedHTTPOperationResult, error) {
	f.domain, f.calls = domain, append(f.calls, "cancel:"+id)
	return f.result, f.err
}

func TestVerifiedHTTPAdapterTypedRoutesAndIntent(t *testing.T) {
	domain := strings.Repeat("a", 32)
	root := "/api/v1/datasets/" + domain + "/verified-http/v1"
	for _, action := range []string{"capability", "create", "query", "cancel"} {
		t.Run(action, func(t *testing.T) {
			fixture := &typedVerifiedCoreFixture{capability: download.VerifiedHTTPCapability{Version: 1, Capability: verifiedHTTPCapability}, result: download.VerifiedHTTPOperationResult{Operation: scopeFixtureResult()}}
			method, path, body, call := "GET", root+"/capability", "", action
			switch action {
			case "create":
				method, path, body = "POST", root+"/operations", verifiedFixtureBody()
			case "query":
				path, call = root+"/operations/example-op", "query:example-op"
			case "cancel":
				method, path, call = "POST", root+"/operations/example-op/cancel", "cancel:example-op"
			}
			status, reply := verifiedFixtureCall(t, verifiedHTTPAdapter{fixture}, method, path, body)
			if status != 200 || reply.Code != 0 || fixture.domain != domain || !reflect.DeepEqual(fixture.calls, []string{call}) {
				t.Fatalf("typed route: %d %+v %v", status, reply, fixture.calls)
			}
			if action == "create" {
				r := fixture.request
				want := download.VerifiedHTTPIntegrityIntent{Version: 1, RequiredCapability: verifiedHTTPCapability, Algorithm: "sha256", ExpectedSHA256: strings.Repeat("a", 64)}
				if r.Version != 1 || r.OperationID != "example-op" || r.Integrity != want || r.Request.Req.Extra.(map[string]any)["large"] != json.Number("900719925474099312345") || r.Request.Opts.Extra.(map[string]any)["autoExtract"] != nil {
					t.Fatal("outer capability/version or raw options changed")
				}
			}
		})
	}
}

func TestVerifiedHTTPAdapterUnknownCapabilityFailsClosed(t *testing.T) {
	path := "/api/v1/datasets/" + strings.Repeat("a", 32) + "/verified-http/v1/capability"
	for _, capability := range []download.VerifiedHTTPCapability{{Version: 2, Capability: verifiedHTTPCapability, Supported: true}, {Version: 1, Capability: "other-capability", Supported: true}, {Supported: true}} {
		fixture := &typedVerifiedCoreFixture{capability: capability}
		status, reply := verifiedFixtureCall(t, verifiedHTTPAdapter{fixture}, "GET", path, "")
		if status != 501 || reply.Msg != "verified_http_unsupported" || string(reply.Data) != "null" || !reflect.DeepEqual(fixture.calls, []string{"capability"}) {
			t.Fatalf("unknown Core version presented as known: %d %+v", status, reply)
		}
	}
}

func TestVerifiedHTTPAdapterTypedErrorsAndProjection(t *testing.T) {
	domain := strings.Repeat("a", 32)
	path := "/api/v1/datasets/" + domain + "/verified-http/v1/operations/example-op"
	for _, item := range []struct {
		err     error
		status  int
		message string
	}{
		{download.ErrVerifiedHTTPInvalid, 400, "invalid_verified_http_request"},
		{download.ErrVerifiedHTTPUnsupportedOption, 400, "unsupported_option"},
		{download.ErrVerifiedHTTPUnsupported, 501, "verified_http_unsupported"},
		{download.ErrDatasetMismatch, 409, "dataset_mismatch"},
	} {
		fixture := &typedVerifiedCoreFixture{err: item.err}
		status, reply := verifiedFixtureCall(t, verifiedHTTPAdapter{fixture}, "GET", path, "")
		if status != item.status || reply.Msg != item.message || string(reply.Data) != "null" {
			t.Fatalf("typed error: %d %+v", status, reply)
		}
	}
	match := true
	view := download.TaskCompletionView{Version: 1, ScopeVersion: 1, DatasetID: domain, TaskID: "example-task", CompletionID: "example-completion", Available: true, Phase: "verified", RecordedMatch: &match}
	result := download.VerifiedHTTPOperationResult{Operation: verifiedCreatedReceipt(), CompletionViews: []download.TaskCompletionView{view}}
	owned := adaptVerifiedHTTPResult(domain, result)
	match = false
	if owned.Views[0].RecordedMatch == nil || !*owned.Views[0].RecordedMatch {
		t.Fatal("adapter retained shared pointer")
	}
	match = true
	fixture := &typedVerifiedCoreFixture{result: result, err: download.ErrOperationUnavailable}
	status, reply := verifiedFixtureCall(t, verifiedHTTPAdapter{fixture}, "GET", path, "")
	if status != 503 || !bytes.Contains(reply.Data, []byte(`"recordedMatch":true`)) || !bytes.Contains(reply.Data, []byte(`"phase":"verified"`)) {
		t.Fatalf("typed partial lost evidence: %d %s", status, reply.Data)
	}
	fixture.result.CompletionViews[0].DatasetID = strings.Repeat("b", 32)
	_, reply = verifiedFixtureCall(t, verifiedHTTPAdapter{fixture}, "GET", path, "")
	if bytes.Contains(reply.Data, []byte(strings.Repeat("b", 32))) || bytes.Contains(reply.Data, []byte(`"recordedMatch":true`)) || !bytes.Contains(reply.Data, []byte(`"completionId":null`)) {
		t.Fatal("adapter relabeled invalid projection", string(reply.Data))
	}
	fixture.result.CompletionViews[0] = download.TaskCompletionView{Version: 1, ScopeVersion: 1, DatasetID: domain, TaskID: "example-task", ErrorCategory: "integrity_mismatch"}
	_, reply = verifiedFixtureCall(t, verifiedHTTPAdapter{fixture}, "GET", path, "")
	if !bytes.Contains(reply.Data, []byte(`"recordedMatch":null`)) || !bytes.Contains(reply.Data, []byte(`"phase":null`)) || !bytes.Contains(reply.Data, []byte(`"errorCategory":"integrity_mismatch"`)) {
		t.Fatal("unavailable typed category changed", string(reply.Data))
	}
}
