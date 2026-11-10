package rest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

func TestBrowserHandoffRESTLostPrepareACKAndQueryRecovery(t *testing.T) {
	var hits atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte("payload")) }))
	defer source.Close()
	server, listener, err := BuildServer(&model.StartConfig{Network: "tcp", Address: "127.0.0.1:0", Storage: model.StorageMem, StorageDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	core := Downloader
	go server.Serve(listener)
	defer func() { server.Shutdown(context.Background()); core.Close(); srv = nil }()
	endpoint := "http://" + listener.Addr().String() + "/api/v1/browser/handoff"
	sum := sha256.Sum256([]byte(source.URL))
	req := download.BrowserHandoffRequest{Action: "prepare", OperationID: "lost-http-ack", ClientID: "A", Browser: "edge", Download: download.BrowserDownloadBinding{ID: 11, StartTime: "2026-11-11T00:00:00Z", URLHash: hex.EncodeToString(sum[:])}, Req: &base.Request{URL: source.URL}, Opts: &base.Options{Path: t.TempDir(), Name: "payload"}}
	// Read enough of the first request to let the real handler persist it, then
	// close its HTTP connection before the caller receives any acknowledgement.
	prepared := make(chan struct{})
	drop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rr := httptest.NewRequest("POST", endpoint, strings.NewReader(string(b)))
		BrowserHandoff(httptest.NewRecorder(), rr)
		close(prepared)
		conn, _, e := w.(http.Hijacker).Hijack()
		if e == nil {
			conn.Close()
		}
	}))
	defer drop.Close()
	encoded, _ := json.Marshal(req)
	resp, e := http.Post(drop.URL, "application/json", strings.NewReader(string(encoded)))
	if resp != nil {
		resp.Body.Close()
	}
	if e == nil {
		t.Fatal("ACK was not dropped")
	}
	<-prepared
	post := func(r download.BrowserHandoffRequest) download.BrowserHandoffResult {
		t.Helper()
		b, _ := json.Marshal(r)
		response, e := http.Post(endpoint, "application/json", strings.NewReader(string(b)))
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		var result model.Result[download.BrowserHandoffResult]
		if e = json.NewDecoder(response.Body).Decode(&result); e != nil || result.Code != 0 {
			t.Fatalf("REST result %+v %v", result, e)
		}
		return result.Data
	}
	req.Action = "query"
	v := post(req)
	if v.State != "prepared" || v.TaskID == "" || hits.Load() != 0 {
		t.Fatal("lost ACK started task or lost identity")
	}
	req.Action = "prepare"
	again := post(req)
	if again.TaskID != v.TaskID || len(core.GetTasks()) != 1 {
		t.Fatal("prepare replay duplicated")
	}
	if _, e = http.Get(endpoint); e != nil {
		t.Fatal(e)
	}
	req.Action = "commit"
	req.TaskID = v.TaskID
	req.CancelConfirmed = true
	req.BrowserState = download.BrowserDownloadState{State: "interrupted", Error: "USER_CANCELED"}
	got := post(req)
	if got.State != "committed" || got.TaskID != v.TaskID {
		t.Fatal("bad commit")
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && core.GetTask(v.TaskID).Status != base.DownloadStatusDone; time.Sleep(10 * time.Millisecond) {
	}
	if core.GetTask(v.TaskID).Status != base.DownloadStatusDone {
		t.Fatal("commit did not use existing downloader")
	}
}
