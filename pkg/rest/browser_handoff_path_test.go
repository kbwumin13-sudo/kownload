package rest

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

type browserPathReceiverFixture struct {
	requests []download.BrowserHandoffRequest
	legacy   bool
}

func (f *browserPathReceiverFixture) BrowserHandoff(r download.BrowserHandoffRequest) (download.BrowserHandoffResult, error) {
	f.requests = append(f.requests, r)
	v := download.BrowserHandoffResult{Version: 1, OperationID: r.OperationID, State: "prepared", TaskID: "same_task", TaskIDs: []string{"same_task"}}
	if !f.legacy {
		v.PathCreate = &download.BrowserPathHandoffProjection{Version: 1, DatasetID: r.DatasetID, Kind: download.HTTPPathCreateKind, RequiredCapabilities: []string{download.HTTPPathRequiredCapability}}
	}
	return v, nil
}

func (*browserPathReceiverFixture) PendingBrowserHandoffs(string, string) ([]download.BrowserHandoffResult, error) {
	return nil, nil
}

func TestBrowserPathReceiverPreservesTypedBinding(t *testing.T) {
	f := new(browserPathReceiverFixture)
	handler := browserHandoffHandler(f)
	marker := httptest.NewRecorder()
	handler(marker, httptest.NewRequest("GET", "/api/v1/browser/handoff", nil))
	var carrier model.Result[map[string]int]
	if json.Unmarshal(marker.Body.Bytes(), &carrier) != nil || carrier.Data["pathCreateVersion"] != 1 || len(f.requests) != 0 {
		t.Fatal("carrier query created an operation or omitted its version")
	}
	item := `{"req":{"url":"https://example.com/a%2Fb","extra":{"large":900719925474099312345}},"opts":{"path":"/synthetic/root","name":"one.bin"}}`
	body := `{"action":"prepare","operationId":"browser:original","datasetId":"` + strings.Repeat("a", 32) + `","clientId":"captured-client","browser":"edge","download":{"id":3,"startTime":"2026-11-12T00:00:00Z","urlHash":"` + strings.Repeat("b", 64) + `"},` + item[1:len(item)-1] + `,"pathCreate":{"version":1,"operationId":"browser:original","requiredCapabilities":["single-http-path-freeze/v1"],"request":` + item + `,"pathIntent":{"version":1,"path":"/synthetic/root","origin":"user_selected"}}}`
	call := func(body string) model.Result[download.BrowserHandoffResult] {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/v1/browser/handoff", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler(w, r)
		var response model.Result[download.BrowserHandoffResult]
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if result := call(body); result.Code != 0 || result.Data.OperationID != "browser:original" || len(f.requests) != 1 {
		t.Fatal("typed request lost original identity", result)
	}
	got := f.requests[0]
	if got.Req.Extra.(map[string]any)["large"] != json.Number("900719925474099312345") || got.PathCreate.Request.Opts.Path != got.Opts.Path || got.Download.ID != 3 {
		t.Fatal("captured request was rewritten")
	}
	bad := strings.Replace(body, `"path":"/synthetic/root","origin"`, `"path":"/other","origin"`, 1)
	if call(bad).Code == 0 || len(f.requests) != 1 {
		t.Fatal("mismatched intent reached the Core")
	}
	f.legacy = true
	if call(body).Code == 0 {
		t.Fatal("legacy receipt acknowledged a typed prepare")
	}
	query := `{"action":"query","operationId":"browser:original","datasetId":"` + strings.Repeat("a", 32) + `","clientId":"captured-client","browser":"edge"}`
	if call(query).Code != 0 || f.requests[len(f.requests)-1].Action != "query" {
		t.Fatal("legacy receipt could not be queried")
	}
}
