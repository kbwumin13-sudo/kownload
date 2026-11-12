package rest

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/gorilla/mux"
)

type pathReceiverFixture struct {
	creates int
	kind    string
	request download.HTTPPathCreateRequest
}

func (f *pathReceiverFixture) PathCapabilityForDataset(d, k string, c []string) (download.HTTPPathCapability, error) {
	return download.HTTPPathCapability{ScopeVersion: 1, DatasetID: d, Kind: k, RequestVersion: 1, Supported: true, RequiredCapabilities: c, Methods: download.HTTPPathMethods{Create: "POST", Query: "GET", Cancel: "POST", Preview: "POST"}}, nil
}
func (f *pathReceiverFixture) PreviewPathForDataset(_ context.Context, _, _ string, r download.HTTPPathCreateRequest) (pathpolicy.Preparation, error) {
	return pathpolicy.PrepareSingleFile(pathpolicy.Input{Path: r.PathIntent.Path, Origin: r.PathIntent.Origin, Name: r.Request.Opts.Name}, pathpolicy.Config{})
}
func (f *pathReceiverFixture) CreatePathOperationForDataset(_ context.Context, _, k string, r download.HTTPPathCreateRequest) (download.HTTPPathOperationResult, error) {
	f.creates++
	f.kind = k
	f.request = r
	return download.HTTPPathOperationResult{RequiredCapabilities: r.RequiredCapabilities, Operation: download.BatchOperationResult{Version: 1, OperationID: r.OperationID, State: "settled", Members: []download.BatchOperationMemberResult{{Index: 0, TaskID: "owned_task", State: "created"}}, TaskIDs: []string{"owned_task"}, CreatedCount: 1}}, nil
}
func (f *pathReceiverFixture) QueryPathOperationForDataset(_, _, id string) (download.HTTPPathOperationResult, error) {
	return download.HTTPPathOperationResult{RequiredCapabilities: []string{}, Operation: download.BatchOperationResult{Version: 1, OperationID: id, State: "unknown", Members: []download.BatchOperationMemberResult{}, TaskIDs: []string{}}}, nil
}
func (f *pathReceiverFixture) CancelPathOperationForDataset(d, k, id string) (download.HTTPPathOperationResult, error) {
	return f.QueryPathOperationForDataset(d, k, id)
}

func TestPathReceiverPreservesCallerIntent(t *testing.T) {
	f := new(pathReceiverFixture)
	router := mux.NewRouter()
	registerPathOperationRoutes(router, f)
	body := `{"version":1,"requiredCapabilities":["single-http-path-freeze/v1"],"operationId":"owned-op","request":{"req":{"url":"https://example.com/a%2Fb","extra":{"large":900719925474099312345}},"opts":{"path":"/synthetic/root","name":"one.bin"}},"pathIntent":{"version":1,"path":"/synthetic/root","origin":"default"}}`
	path := "/api/v1/datasets/" + strings.Repeat("a", 32) + "/http-path/v1/operations"
	call := func(body string) int {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w.Code
	}
	if status := call(body); status != 200 || f.creates != 1 || f.kind != "http-path-create/v1" {
		t.Fatalf("actual typed forwarding %d", status)
	}
	extra, ok := f.request.Request.Req.Extra.(map[string]any)
	if !ok || extra["large"] != json.Number("900719925474099312345") || f.request.Request.Opts.Path != "/synthetic/root" || f.request.PathIntent.Origin != pathpolicy.Default {
		t.Fatal("caller raw intent changed")
	}
	bad := strings.Replace(body, `"origin":"default"`, `"origin":"rule"`, 1)
	if call(bad) != 400 || f.creates != 1 {
		t.Fatal("caller rule provenance accepted")
	}
	bad = strings.Replace(body, `"path":"/synthetic/root","origin"`, `"path":"/different","origin"`, 1)
	if call(bad) != 400 || f.creates != 1 {
		t.Fatal("raw path mismatch repaired or submitted")
	}
}

func TestPathReceiverVariantAndEmptyQuery(t *testing.T) {
	f := new(pathReceiverFixture)
	router := mux.NewRouter()
	registerPathOperationRoutes(router, f)
	root := "/api/v1/datasets/" + strings.Repeat("a", 32) + "/"
	for _, v := range pathRouteVariants {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", root+v.prefix+"/v1/operations/new-op", nil))
		var reply struct {
			Code int                `json:"code"`
			Data pathOperationReply `json:"data"`
		}
		if json.Unmarshal(w.Body.Bytes(), &reply) != nil || w.Code != 200 || reply.Code != 0 || reply.Data.Kind != v.kind || reply.Data.RequiredCapabilities == nil || len(reply.Data.RequiredCapabilities) != 0 || reply.Data.Operation.State != "unknown" {
			t.Fatal("no-R query invented a variant", v.prefix, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", root+"bt-path/v1/capability?integrity=sha256", nil))
	if w.Code != 400 || f.creates != 0 {
		t.Fatal("HTTP SHA capability borrowed by native variant")
	}
	h := pathOperationHandler{f, pathRouteVariants[0]}
	if h.validCaps([]string{verifiedHTTPCapability, "single-http-path-freeze/v1"}) {
		t.Fatal("capability sequence sorted/repaired")
	}
}

func TestPathSettingsPreserveUnsupportedValue(t *testing.T) {
	opaque := json.RawMessage(`{"version":99,"privateFutureRule":{"keep":9007199254740993}}`)
	previous := &base.DownloaderStoreConfig{Extra: map[string]any{"pathPolicyV1": opaque}}
	next := &base.DownloaderStoreConfig{Extra: map[string]any{"other": "edited"}}
	if err := preparePathPolicyConfigUpdate(previous, next); err != nil || !reflect.DeepEqual(next.Extra["pathPolicyV1"], opaque) {
		t.Fatal("unrelated save lost unsupported value", err)
	}
	next.Extra["pathPolicyV1"] = map[string]any{"version": 1, "enabled": false, "rules": []any{}}
	if preparePathPolicyConfigUpdate(previous, next) == nil || !reflect.DeepEqual(previous.Extra["pathPolicyV1"], opaque) {
		t.Fatal("unknown settings silently replaced")
	}
	next = &base.DownloaderStoreConfig{Extra: map[string]any{"pathPolicyV1": nil}}
	if preparePathPolicyConfigUpdate(&base.DownloaderStoreConfig{}, next) == nil {
		t.Fatal("explicit null became default OFF")
	}
}
