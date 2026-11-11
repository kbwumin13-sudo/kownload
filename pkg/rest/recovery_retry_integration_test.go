//go:build !windows

package rest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/download"
)

// Reuses the existing real Bolt/UDS harness, including restoration of all
// BuildServer globals. No test cache, fake recovery owner or alternate API.
func TestRecoveryRetryBuildServerUserActionAndReplay(t *testing.T) {
	f := newMCPPublishedFixture(t)
	var allow atomic.Bool
	var requests atomic.Int32
	uiFinished := make(chan struct{})
	var finishOnce sync.Once
	payload := bytes.Repeat([]byte("explicit user action via real API\n"), 1024)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__allow" {
			allow.Store(true)
			return
		}
		if r.URL.Path == "/__finish" {
			finishOnce.Do(func() { close(uiFinished) })
			return
		}
		requests.Add(1)
		if !allow.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("ETag", `"retry-api-same-resource"`)
		http.ServeContent(w, r, "preserved.bin", time.Time{}, bytes.NewReader(payload))
	}))
	defer source.Close()
	id := f.create(t, source.URL+"/payload", "preserved.bin")
	var identity struct {
		DatasetID string `json:"datasetId"`
	}
	if err := json.Unmarshal(f.control(t, "GET", "/api/v1/dataset/identity", nil), &identity); err != nil || identity.DatasetID == "" {
		t.Fatal("missing real dataset", err)
	}
	path := "/api/v1/datasets/" + identity.DatasetID + "/tasks/" + id + "/recovery/v1"
	read := func() download.RecoveryTaskState {
		t.Helper()
		var reply recoveryReply
		if err := json.Unmarshal(f.control(t, "GET", path, nil), &reply); err != nil || reply.DatasetID != identity.DatasetID || reply.ScopeVersion != 1 || reply.Recovery.TaskID != id {
			t.Fatal("invalid authoritative recovery wrapper", err)
		}
		return reply.Recovery
	}
	state := read()
	for deadline := time.Now().Add(4 * time.Second); !state.RetryAllowed && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		state = read()
	}
	if state.Management != "managed" || !state.RetryAllowed {
		t.Fatal("real failure not durably retryable", state)
	}
	request := download.RecoveryRetryRequest{Version: 1, ActionID: "user-real-api-retry", ExpectedEpisode: state.Episode, ExpectedSourceRevision: state.SourceRevision, Decision: "retry"}
	// Explicitly enabled private fixture for the Flutter owner. Normal tests
	// take the automatic assertion path below. The UI must perform the Retry;
	// this branch never synthesizes a reset or persists a second journal.
	if ready := os.Getenv("K15_RETRY_UI_FIXTURE_JSON"); ready != "" {
		body, err := json.Marshal(map[string]any{"socket": f.socket, "apiToken": mcpPublishedToken, "datasetId": identity.DatasetID, "taskId": id, "recoveryPath": path, "request": request, "allowURL": source.URL + "/__allow", "finishURL": source.URL + "/__finish", "outputPath": filepath.Join(f.root, "preserved.bin")})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(ready, body, 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(ready)
		select {
		case <-uiFinished:
		case <-time.After(90 * time.Second):
			t.Fatal("private UI fixture was not completed")
		}
		if read().Status != "done" {
			t.Fatal("UI did not finish original task")
		}
		actual, err := os.ReadFile(filepath.Join(f.root, "preserved.bin"))
		if err != nil || !bytes.Equal(actual, payload) {
			t.Fatal("UI output mismatch", err)
		}
		return
	}
	allow.Store(true)
	var reply recoveryReply
	type result struct {
		status int
		body   []byte
	}
	results := make(chan result, 2)
	var clicks sync.WaitGroup
	for i := 0; i < 2; i++ {
		clicks.Add(1)
		go func() {
			defer clicks.Done()
			status, body := f.request(t, "POST", path+"/retry", request, false)
			results <- result{status, body}
		}()
	}
	clicks.Wait()
	close(results)
	accepted := 0
	for response := range results {
		if response.status == 409 {
			continue
		}
		var envelope struct {
			Code int           `json:"code"`
			Data recoveryReply `json:"data"`
		}
		if response.status != 200 || json.Unmarshal(response.body, &envelope) != nil || envelope.Code != 0 {
			t.Fatal("double-click response", response.status, string(response.body))
		}
		reply = envelope.Data
		if reply.Recovery.Receipt == nil || reply.Recovery.Episode == state.Episode {
			t.Fatal("missing retry receipt", reply)
		}
		switch reply.Recovery.Receipt.State {
		case "accepted":
			accepted++
		case "replayed":
		default:
			t.Fatal("unexpected receipt", reply)
		}
	}
	if accepted != 1 {
		t.Fatal("double-click fresh winner count", accepted)
	}
	if reply.DatasetID != identity.DatasetID || reply.Recovery.TaskID != id {
		t.Fatal("wrong scoped receipt", reply)
	}
	for deadline := time.Now().Add(4 * time.Second); read().Status != "done"; {
		if time.Now().After(deadline) {
			t.Fatal("retry did not complete")
		}
		time.Sleep(5 * time.Millisecond)
	}
	actual, err := os.ReadFile(filepath.Join(f.root, "preserved.bin"))
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatal("original path/body not preserved", err)
	}
	calls := requests.Load()
	if err := json.Unmarshal(f.control(t, "POST", path+"/retry", request), &reply); err != nil || reply.Recovery.Receipt == nil || reply.Recovery.Receipt.State != "replayed" {
		t.Fatal("same action not replayed", err)
	}
	query := fmt.Sprintf("?actionId=%s&expectedEpisode=%s&expectedSourceRevision=%d", request.ActionID, request.ExpectedEpisode, request.ExpectedSourceRevision)
	if err := json.Unmarshal(f.control(t, "GET", path+query, nil), &reply); err != nil || reply.Recovery.Receipt == nil || reply.Recovery.Receipt.State != "accepted" {
		t.Fatal("exact original action not queryable", err)
	}
	if requests.Load() != calls {
		t.Fatal("query/replay sent network IO")
	}
	status, raw := f.request(t, "GET", strings.Replace(path, identity.DatasetID, strings.Repeat("f", 32), 1), nil, false)
	if status != 409 || !bytes.Contains(raw, []byte(`"data":null`)) {
		t.Fatal("wrong dataset leaked receipt", status, string(raw))
	}
	// Actual authenticated router must protect the reset route too.
	r, err := http.NewRequest("GET", "http://kownload.local"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("recovery route bypassed existing auth", response.StatusCode)
	}
}
