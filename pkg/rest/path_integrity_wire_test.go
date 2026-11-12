package rest

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/gorilla/mux"
)

func TestPathReceiverActualUIIntegrityEnvelope(t *testing.T) {
	// These exact bytes were exported by HttpPathCreateRequest.toJson in the
	// Dart serializer regression, rather than reconstructed from Go structs.
	raw, err := os.ReadFile("testdata/http_path_ui_sha256.json")
	if err != nil {
		t.Fatal(err)
	}
	f := new(pathReceiverFixture)
	router := mux.NewRouter()
	registerPathOperationRoutes(router, f)
	post := func(body []byte) int {
		r := httptest.NewRequest("POST", "/api/v1/datasets/"+strings.Repeat("a", 32)+"/http-path/v1/operations", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w.Code
	}
	if post(raw) != 200 || f.creates != 1 {
		t.Fatal("actual UI SHA request did not reach the strict create adapter")
	}
	intent := f.request.Integrity
	if intent == nil || intent.Version != 1 || intent.RequiredCapability != verifiedHTTPCapability || intent.Algorithm != "sha256" || intent.ExpectedSHA256 != strings.Repeat("a", 64) {
		t.Fatal("UI integrity intent was incomplete or rewritten")
	}
	var wire map[string]json.RawMessage
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	var item map[string]json.RawMessage
	if err = json.Unmarshal(wire["request"], &item); err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(map[string]any{
		"action": "prepare", "operationId": f.request.OperationID,
		"datasetId": strings.Repeat("a", 32), "clientId": "captured-client", "browser": "edge",
		"req": item["req"], "opts": item["opts"], "pathCreate": json.RawMessage(raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/browser/handoff", bytes.NewReader(outer))
	r.Header.Set("Content-Type", "application/json")
	parsed, typed, ok := readBrowserHandoff(httptest.NewRecorder(), r)
	if !ok || !typed || parsed.PathCreate == nil || parsed.OperationID != f.request.OperationID || !reflect.DeepEqual(parsed.PathCreate.Integrity, intent) || !reflect.DeepEqual(parsed.Req, f.request.Request.Req) || !reflect.DeepEqual(parsed.Opts, f.request.Request.Opts) {
		t.Fatal("browser wrapper changed the actual UI SHA intent or original request")
	}
	var incomplete map[string]json.RawMessage
	if err = json.Unmarshal(wire["integrity"], &incomplete); err != nil {
		t.Fatal(err)
	}
	delete(incomplete, "version")
	delete(incomplete, "requiredCapability")
	wire["integrity"], _ = json.Marshal(incomplete)
	old, _ := json.Marshal(wire)
	if post(old) != 400 || f.creates != 1 {
		t.Fatal("former incomplete integrity shape reached the Core")
	}
	if !reflect.DeepEqual(f.request.RequiredCapabilities, []string{download.HTTPPathRequiredCapability, verifiedHTTPCapability}) {
		t.Fatal("ordered capabilities were rewritten")
	}
}
