package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// This is the real Core/fetcher/Bolt path. Only the remote byte source is
// controlled; no F, file identity, recovery token or first-run RAM is injected.
func TestPathFreezeActualHTTPFirstWriteAndPublish(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "output")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("durable path first byte\n"), 32768)
	var d *Downloader
	var id atomic.Value
	var checked atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"path-fixed"`)
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			return
		}
		taskID, ok := id.Load().(string)
		if !ok { // M ACK is already complete; the public receipt can race its queued run.
			deadline := time.Now().Add(time.Second)
			for !ok && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
				taskID, ok = id.Load().(string)
			}
		}
		if !ok {
			t.Error("request before receipt task identity")
			w.WriteHeader(500)
			return
		}
		identity, e := d.DatasetIdentity()
		if e != nil {
			t.Error(e)
			w.WriteHeader(500)
			return
		}
		e = withDatasetReadView(d.storage, identity.DatasetID, func(reader datasetReader) error {
			task, state, e := ReadPathFreeze(reader, taskID)
			if e != nil {
				return e
			}
			if state.Phase != PathFreezeFrozen || state.Output == nil || state.Output.Phase != MCPWriteOutputStaging || state.Output.Binding == nil || task.Meta.Opts.Path != root {
				return ErrPathFreezeHeld
			}
			return nil
		})
		if e != nil {
			t.Errorf("first byte before F and identity ACK: %v", e)
			w.WriteHeader(500)
			return
		}
		checked.Store(true)
		begin, end := 0, len(payload)-1
		if _, e = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end); e != nil || begin < 0 || end < begin || end >= len(payload) {
			t.Errorf("range %q", r.Header.Get("Range"))
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
		w.WriteHeader(206)
		_, _ = w.Write(payload[begin : end+1])
	}))
	defer server.Close()
	d = operationDownloader(t, NewBoltStorage(filepath.Join(dir, "db")), dir)
	defer d.Close()
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	request := HTTPPathCreateRequest{Version: 1, OperationID: "path-actual-http", RequiredCapabilities: []string{HTTPPathRequiredCapability}, PathIntent: PathIntent{Version: 1, Path: root, Origin: pathpolicy.UserSelected}, Request: &base.CreateTaskBatchItem{Req: &base.Request{URL: server.URL + "/file"}, Opts: &base.Options{Path: root, Name: "payload.bin", Extra: &httpModel.OptsExtra{Connections: 1}}}}
	result, err := d.CreatePathOperationForDataset(context.Background(), identity.DatasetID, HTTPPathCreateKind, request)
	if err != nil || len(result.Operation.TaskIDs) != 1 {
		t.Fatalf("M: %+v %v", result, err)
	}
	taskID := result.Operation.TaskIDs[0]
	id.Store(taskID)
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		task := d.GetTask(taskID)
		if task.Status == base.DownloadStatusDone {
			break
		}
		if task.Status == base.DownloadStatusError {
			t.Fatalf("execution error: %+v", task)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if task := d.GetTask(taskID); task.Status != base.DownloadStatusDone {
		t.Fatalf("no Done: %+v", task)
	}
	actual, err := os.ReadFile(filepath.Join(root, "payload.bin"))
	if err != nil || sha256.Sum256(actual) != sha256.Sum256(payload) || !checked.Load() {
		t.Fatalf("published bytes/first-write guard: len=%d err=%v checked=%v", len(actual), err, checked.Load())
	}
	_, e, err := d.readPathExpected(identity.DatasetID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if e.Record.Members[0].PathFreeze.Output.Phase != MCPWriteOutputPublished {
		t.Fatal("Done without published F")
	}
	replay, err := d.CreatePathOperationForDataset(context.Background(), identity.DatasetID, HTTPPathCreateKind, request)
	if err != nil || len(replay.Operation.TaskIDs) != 1 || replay.Operation.TaskIDs[0] != taskID {
		t.Fatal("same-op replay", err)
	}
}
