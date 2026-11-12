package download

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func TestPathFreezeHTTPResponseMIMEAndReservedNames(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fallback, videos := filepath.Join(dir, "fallback"), filepath.Join(dir, "videos")
	for _, root := range []string{fallback, videos} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	payload := bytes.Repeat([]byte("same-name actual HTTP bytes\n"), 32768)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4; charset=binary")
		w.Header().Set("ETag", `"same-object"`)
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		http.ServeContent(w, r, "payload.bin", time.Unix(1700000000, 0), bytes.NewReader(payload))
	}))
	defer server.Close()
	d := operationDownloader(t, NewBoltStorage(filepath.Join(dir, "db")), dir)
	defer d.Close()
	cfg, _ := d.GetConfig()
	cfg.MaxRunning = 2
	cfg.Extra, err = pathpolicy.WithConfig(cfg.Extra, pathpolicy.Config{Enabled: true, Rules: []pathpolicy.Rule{{ID: "actual-video", Kind: pathpolicy.MIMERule, Match: pathpolicy.Match{MIME: "video/*"}, Root: videos}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PutConfig(cfg); err != nil {
		t.Fatal(err)
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	requests := make([]HTTPPathCreateRequest, 2)
	ids := make([]string, 2)
	for i, op := range []string{"mime-name-first", "mime-name-second"} {
		requests[i] = HTTPPathCreateRequest{Version: 1, OperationID: op, RequiredCapabilities: []string{HTTPPathRequiredCapability}, PathIntent: PathIntent{Version: 1, Path: fallback, Origin: pathpolicy.Default}, Request: &base.CreateTaskBatchItem{Req: &base.Request{URL: server.URL + "/payload.bin"}, Opts: &base.Options{Path: fallback, Name: "payload.bin", Extra: &httpModel.OptsExtra{Connections: 1}}}}
		result, err := d.CreatePathOperationForDataset(context.Background(), identity.DatasetID, HTTPPathCreateKind, requests[i])
		if err != nil || len(result.Operation.TaskIDs) != 1 {
			t.Fatalf("create %d: %+v %v", i, result, err)
		}
		ids[i] = result.Operation.TaskIDs[0]
	}
	frozen := make([]TaskPathFreeze, 2)
	for i, id := range ids {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			task := d.GetTask(id)
			if task.Status == base.DownloadStatusError {
				t.Fatalf("execution: %+v", task)
			}
			if task.PathFreeze != nil && task.PathFreeze.Phase == PathFreezeFrozen && task.PathFreeze.Output != nil && task.PathFreeze.Output.Phase == MCPWriteOutputStaging {
				frozen[i] = *task.PathFreeze
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if frozen[i].Decision == nil || frozen[i].Decision.RuleID != "actual-video" || frozen[i].Decision.Root != videos {
			t.Fatalf("actual response MIME not frozen: %+v", frozen[i])
		}
	}
	if frozen[0].Layout.Members[0].RelativePath == frozen[1].Layout.Members[0].RelativePath {
		t.Fatal("two incomplete operations reserved the same final name")
	}
	for _, state := range frozen {
		if _, err := os.Stat(filepath.Join(videos, state.Layout.Members[0].RelativePath)); !os.IsNotExist(err) {
			t.Fatalf("final existed before source released: %v", err)
		}
	}
	cfg.Extra, err = pathpolicy.WithConfig(cfg.Extra, pathpolicy.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PutConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for i, request := range requests {
		result, err := d.CreatePathOperationForDataset(context.Background(), identity.DatasetID, HTTPPathCreateKind, request)
		if err != nil || len(result.Operation.TaskIDs) != 1 || result.Operation.TaskIDs[0] != ids[i] || !reflect.DeepEqual(d.GetTask(ids[i]).PathFreeze, &frozen[i]) {
			t.Fatal("replay re-evaluated MIME or renamed frozen target", err)
		}
	}
	close(release)
	for i, id := range ids {
		waitStatus(t, d, id, base.DownloadStatusDone, 8*time.Second)
		body, err := os.ReadFile(filepath.Join(videos, frozen[i].Layout.Members[0].RelativePath))
		if err != nil || !bytes.Equal(body, payload) {
			t.Fatal("independent published bytes", err)
		}
	}
}

// The first F uses the real kernel/commit-lost-ACK seam while its known M is
// paused. It returns no fresh value or IO permission. Subsequent creation and
// cold continuation use the actual Core, concrete Bolt and HTTP writer.
func TestPathFreezeUnknownACKReservesNameAcrossReopen(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "output")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("durable F unknown ACK keeps its name\n"), 8192)
	release := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("ETag", `"unknown-f-ack"`)
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		http.ServeContent(w, r, "same.bin", time.Unix(1700000000, 0), bytes.NewReader(payload))
	}))
	defer server.Close()
	db := filepath.Join(dir, "db")
	store := NewBoltStorage(db)
	d := operationDownloader(t, store, dir)
	defer func() {
		if d != nil {
			_ = d.Close()
		}
	}()
	cfg, _ := d.GetConfig()
	cfg.MaxRunning = 0 // Hold real Q before any execution; no fabricated lease.
	if err := d.PutConfig(cfg); err != nil {
		t.Fatal(err)
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	request := HTTPPathCreateRequest{Version: 1, OperationID: "unknown-f-first", RequiredCapabilities: []string{HTTPPathRequiredCapability}, PathIntent: PathIntent{Version: 1, Path: root, Origin: pathpolicy.Default}, Request: &base.CreateTaskBatchItem{Req: &base.Request{URL: server.URL + "/same.bin"}, Opts: &base.Options{Path: root, Name: "same.bin", Extra: &httpModel.OptsExtra{Connections: 1}}}}
	first, err := d.CreatePathOperationForDataset(context.Background(), identity.DatasetID, HTTPPathCreateKind, request)
	if err != nil || len(first.Operation.TaskIDs) != 1 {
		t.Fatal("first known M", err)
	}
	firstID := first.Operation.TaskIDs[0]
	if err := d.Pause(&TaskFilter{IDs: []string{firstID}}); err != nil {
		t.Fatal(err)
	}
	live := d.getTask(firstID)
	live.statusLock.Lock()
	paused := live.pauseDone
	live.statusLock.Unlock()
	if paused != nil {
		select {
		case <-paused:
		case <-time.After(3 * time.Second):
			t.Fatal("initial pause not persisted")
		}
	}
	_, expected, err := d.readPathExpected(identity.DatasetID, firstID)
	if err != nil {
		t.Fatal(err)
	}
	prepared := pathK12Prepared(t, expected, root, "same.bin")
	prepared.ResolvedMeta.Res.Range = true
	prepared.ResolvedMeta.Res.Size = int64(len(payload))
	prepared.ResolvedMeta.Res.Files[0].Size = int64(len(payload))
	prepared.StagingName = "." + firstID + ".path-part"
	owned, fresh, err := ConfirmPathFreezeTree(d4k12WriteFailure{AtomicStorage: store, unknown: true}, identity.DatasetID, []PathFreezeExpected{expected}, []PathFreezePrepared{prepared})
	if err == nil || fresh || owned != nil {
		t.Fatal("unknown F ACK returned a grant", err)
	}
	if live.PathFreeze.Phase != PathFreezePending || calls.Load() != 0 {
		t.Fatal("unknown F ACK applied live F or started IO")
	}
	var original TaskPathFreeze
	if err := withDatasetReadView(store, identity.DatasetID, func(reader datasetReader) error {
		_, state, err := ReadPathFreeze(reader, firstID)
		original = state
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if original.Phase != PathFreezeFrozen || original.Layout.Members[0].RelativePath != "same.bin" {
		t.Fatal("lost ACK did not actually commit F")
	}
	cfg.MaxRunning = 1
	if err := d.PutConfig(cfg); err != nil {
		t.Fatal(err)
	}
	secondRequest, err := cloneOperationJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest.OperationID = "unknown-f-second"
	second, err := d.CreatePathOperationForDataset(context.Background(), identity.DatasetID, HTTPPathCreateKind, secondRequest)
	if err != nil || len(second.Operation.TaskIDs) != 1 {
		t.Fatal("second M", err)
	}
	secondID := second.Operation.TaskIDs[0]
	var secondF *TaskPathFreeze
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		task := d.GetTask(secondID)
		if task.Status == base.DownloadStatusError {
			t.Fatalf("second failed: %+v", task)
		}
		if task.PathFreeze != nil && task.PathFreeze.Phase == PathFreezeFrozen && task.PathFreeze.Output != nil && task.PathFreeze.Output.Phase == MCPWriteOutputStaging {
			secondF = task.PathFreeze
			break
		}
	}
	if secondF == nil || secondF.Layout.Members[0].RelativePath == "same.bin" {
		t.Fatal("persistent F missing from occupancy")
	}
	if live.PathFreeze.Phase != PathFreezePending {
		t.Fatal("occupancy read reconstructed first live F")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d = nil
	d = operationDownloader(t, NewBoltStorage(db), dir)
	if !reflect.DeepEqual(d.GetTask(firstID).PathFreeze, &original) || !reflect.DeepEqual(d.GetTask(secondID).PathFreeze, secondF) {
		t.Fatal("normal reopen changed frozen reservations")
	}
	close(release)
	if err := d.ContinueBatch(&TaskFilter{IDs: []string{firstID, secondID}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{firstID, secondID} {
		waitStatus(t, d, id, base.DownloadStatusDone, 8*time.Second)
		task := d.GetTask(id)
		body, err := os.ReadFile(task.Meta.SingleFilepath())
		if err != nil || !bytes.Equal(body, payload) {
			t.Fatal("reopened target bytes", err)
		}
	}
}
