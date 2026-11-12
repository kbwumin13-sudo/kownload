//go:build darwin || linux

package rest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"go.etcd.io/bbolt"
)

// Static draft on 631f. Combine with the final immutable v4 Core before running;
// the old baseline's lack of cooldown is not a requested red test. No private
// Until or ledger reads occur until the actual server and Bolt writer close.
func TestRecoverySharedBuildServerUDSCooldownAndContinuation(t *testing.T) {
	root, err := os.MkdirTemp("/private/tmp", "r4r-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("retained failed REST fixture: %s", root)
			return
		}
		_ = os.RemoveAll(root)
	})
	payload := bytes.Repeat([]byte("rest-shared-body"), 32768)
	peerBody := bytes.Repeat([]byte("queue-peer"), 2048)
	firstRange, peerRange := make(chan struct{}), make(chan struct{})
	failureGate, peerGate := make(chan struct{}), make(chan struct{})
	var failureOnce, peerOnce, peerSeen sync.Once
	releaseFailure := func() { failureOnce.Do(func() { close(failureGate) }) }
	releasePeer := func() { peerOnce.Do(func() { close(peerGate) }) }
	var ranges atomic.Int32
	var refusedAt, resumedAt atomic.Int64
	var invalidURL atomic.Bool
	serveRange := func(w http.ResponseWriter, r *http.Request, body []byte) {
		begin, end := 0, len(body)-1
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end); err != nil || begin < 0 || end < begin || end >= len(body) {
			t.Errorf("invalid actual range %q", r.Header.Get("Range"))
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(body)))
		w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[begin : end+1])
	}
	metadata := func(w http.ResponseWriter, body []byte) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // Frozen Core fixture: metadata only, then a real Range.
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/fixed?proof=a%2Fb" {
			invalidURL.Store(true)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Accept-Ranges", "bytes") // Deliberately no ETag: real plain variant.
		if r.Header.Get("Range") == "" {
			metadata(w, payload)
			return
		}
		if ranges.Add(1) == 1 {
			close(firstRange)
			select {
			case <-failureGate:
			case <-r.Context().Done():
				return
			}
			refusedAt.Store(time.Now().UnixNano())
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		resumedAt.CompareAndSwap(0, time.Now().UnixNano())
		serveRange(w, r, payload)
	}))
	defer source.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "" {
			metadata(w, peerBody)
			return
		}
		peerSeen.Do(func() { close(peerRange) })
		select {
		case <-peerGate:
		case <-r.Context().Done():
			return
		}
		serveRange(w, r, peerBody)
	}))
	defer peer.Close()
	f := verifiedLiveUDS(t, root) // Real BuildServer/Bolt/UDS/auth and global restoration.
	defer f.close()
	defer releaseFailure()
	defer releasePeer()
	call := func(method, path string, body []byte) (int, model.Result[json.RawMessage]) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Api-Token", verifiedLiveToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var reply model.Result[json.RawMessage]
		if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, reply
	}
	control := func(method, path string, body []byte) json.RawMessage {
		t.Helper()
		status, reply := call(method, path, body)
		if status != 200 || reply.Code != model.CodeOk {
			t.Fatalf("%s %s: HTTP%d %+v", method, path, status, reply)
		}
		return reply.Data
	}
	encode := func(v any) []byte {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// Explicit fixture setup at 0 Task; preserve the resulting private config baseline.
	var cfg map[string]any
	if err := json.Unmarshal(control(http.MethodGet, "/api/v1/config", nil), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["maxRunning"] = 1
	control(http.MethodPut, "/api/v1/config", encode(cfg))
	config := control(http.MethodGet, "/api/v1/config", nil)
	identity := control(http.MethodGet, "/api/v1/dataset/identity", nil)
	directory := filepath.Join(root, "files")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	options := func(name string) map[string]any {
		return map[string]any{"path": directory, "name": name, "extra": map[string]any{"connections": 1, "autoTorrent": false, "autoExtract": false}}
	}
	endpoint := "/api/v1/datasets/" + f.domain + "/task-batches/v1/operations"
	op := "shared-rest-owned-1"
	body := encode(map[string]any{"version": 1, "operationId": op, "reqs": []any{map[string]any{"req": map[string]any{"url": source.URL + "/fixed?proof=a%2Fb"}, "opts": options("one.bin")}}})
	operation := func(raw []byte) scopedBatchResult {
		t.Helper()
		var value scopedBatchResult
		if err := json.Unmarshal(raw, &value); err != nil || value.ScopeVersion != 1 || value.DatasetID != f.domain || value.Namespace != scopedBatchNamespace || value.Operation.OperationID != op || len(value.Operation.TaskIDs) != 1 {
			t.Fatalf("invalid same-D single-M receipt %s: %v", raw, err)
		}
		return value
	}
	created := operation(control(http.MethodPost, endpoint, body))
	id := created.Operation.TaskIDs[0]
	waitSignal := func(ch <-chan struct{}, label string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("missing " + label)
		}
	}
	waitSignal(firstRange, "first real Range")
	// One ordinary peer Task supplies real queue pressure without a second scoped M.
	var peerID string
	if err := json.Unmarshal(control(http.MethodPost, "/api/v1/tasks", encode(map[string]any{"req": map[string]any{"url": peer.URL + "/peer"}, "opts": options("peer.bin")})), &peerID); err != nil || peerID == "" || peerID == id {
		t.Fatal("peer create", err)
	}
	recoveryPath := "/api/v1/datasets/" + f.domain + "/tasks/" + id + "/recovery/v1"
	readTask := func(taskID string) *download.Task {
		t.Helper()
		var task download.Task
		if err := json.Unmarshal(control(http.MethodGet, "/api/v1/tasks/"+taskID, nil), &task); err != nil || task.ID != taskID {
			t.Fatal("Task identity", err)
		}
		return &task
	}
	readRecovery := func() download.RecoveryTaskState {
		t.Helper()
		raw := control(http.MethodGet, recoveryPath, nil)
		for _, forbidden := range []string{`"notBefore":`, `"until":`, `"coverage":`, `"requiredCapability":`, `"capability":`, `"seq":`, `"B":`, `http-shared-recovery/v1`, `http-shared-coverage-recovery/v1`} {
			if bytes.Contains(raw, []byte(forbidden)) {
				t.Fatalf("internal recovery field escaped: %s", raw)
			}
		}
		var reply recoveryReply
		if err := json.Unmarshal(raw, &reply); err != nil || reply.ScopeVersion != 1 || reply.DatasetID != f.domain || reply.Recovery.Version != 1 || reply.Recovery.TaskID != id || reply.Recovery.Management != "managed" {
			t.Fatalf("invalid wire1 recovery %s: %v", raw, err)
		}
		return reply.Recovery
	}
	wait := func(label string, predicate func(*download.Task, download.RecoveryTaskState) bool) (*download.Task, download.RecoveryTaskState) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for {
			task, state := readTask(id), readRecovery()
			if predicate(task, state) {
				return task, state
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: task=%s recovery=%+v ranges=%d", label, task.Status, state, ranges.Load())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if readTask(peerID).Status != base.DownloadStatusWait {
		t.Fatal("peer did not actually queue")
	}
	releaseFailure()
	initial, cool := wait("public cooldown", func(task *download.Task, state download.RecoveryTaskState) bool {
		return task.Status == base.DownloadStatusWait && state.Reason == "cooldown"
	})
	waitSignal(peerRange, "peer claimed yielded slot")
	if cool.RetryAllowed || cool.TotalUsed != 1 || cool.TransportUsed != 0 || initial.Recovery == nil || initial.Recovery.Version != 4 || initial.Completion != nil {
		t.Fatalf("invalid deferred snapshot %+v / %+v", initial, cool)
	}
	rawReq, creation := encode(initial.Meta.Req), encode(initial.CreationOperation)
	catalogPath := "/api/v1/task-batches/operations?filter=all&limit=100"
	catalog := control(http.MethodGet, catalogPath, nil)
	for i := 0; i < 2; i++ {
		if operation(control(http.MethodGet, endpoint+"/"+op, nil)).Operation.TaskIDs[0] != id {
			t.Fatal("query changed M")
		}
		_ = readRecovery()
	}
	if operation(control(http.MethodPost, endpoint, body)).Operation.TaskIDs[0] != id {
		t.Fatal("same-op replay changed M")
	}
	control(http.MethodPut, "/api/v1/tasks/"+id+"/continue", nil)
	retry := download.RecoveryRetryRequest{Version: 1, ActionID: "shared-rest-cooldown-retry", ExpectedEpisode: cool.Episode, ExpectedSourceRevision: cool.SourceRevision, Decision: "retry"}
	status, denied := call(http.MethodPost, recoveryPath+"/retry", encode(retry))
	if status != http.StatusConflict || denied.Code != model.CodeError || denied.Msg != "recovery_held" || string(denied.Data) != "null" {
		t.Fatalf("cooldown Retry: HTTP%d %+v", status, denied)
	}
	q := url.Values{"actionId": {retry.ActionID}, "expectedEpisode": {retry.ExpectedEpisode}, "expectedSourceRevision": {fmt.Sprint(retry.ExpectedSourceRevision)}}
	status, query := call(http.MethodGet, recoveryPath+"?"+q.Encode(), nil)
	if status != http.StatusOK || query.Code != model.CodeOk {
		t.Fatalf("tuple query: HTTP%d %+v", status, query)
	}
	var queried recoveryReply
	if err := json.Unmarshal(query.Data, &queried); err != nil || queried.ScopeVersion != 1 || queried.DatasetID != f.domain || queried.Recovery.Version != 1 || queried.Recovery.TaskID != id {
		t.Fatal("tuple query shape", err)
	}
	qs := queried.Recovery
	if qs.Management != "managed" || qs.Reason != "cooldown" || qs.RetryAllowed || qs.Episode != cool.Episode || qs.Generation != cool.Generation || qs.SourceRevision != cool.SourceRevision || qs.TotalUsed != 1 || qs.TransportUsed != 0 || qs.LastExplicitRetry != nil {
		t.Fatalf("tuple query changed recovery: %+v", qs)
	}
	receipt := qs.Receipt
	if receipt == nil || receipt.Version != 1 || receipt.ActionID != retry.ActionID || receipt.ExpectedEpisode != retry.ExpectedEpisode || receipt.ExpectedSourceRevision != retry.ExpectedSourceRevision || receipt.Decision != "retry" || receipt.State != "not_recorded" || !receipt.QueryRequired || receipt.NewEpisode != "" {
		t.Fatalf("tuple query consumed or lost action: %+v", receipt)
	}
	still := readRecovery()
	if still.Reason != "cooldown" || still.RetryAllowed || still.Episode != cool.Episode || still.Generation != cool.Generation || still.SourceRevision != cool.SourceRevision || still.TotalUsed != 1 || still.TransportUsed != 0 || still.LastExplicitRetry != nil || ranges.Load() != 1 {
		t.Fatalf("query/replay/Continue/Retry changed pre-Until authority: before=%+v after=%+v ranges=%d", cool, still, ranges.Load())
	}
	if !bytes.Equal(rawReq, encode(readTask(id).Meta.Req)) || !bytes.Equal(config, control(http.MethodGet, "/api/v1/config", nil)) || !bytes.Equal(identity, control(http.MethodGet, "/api/v1/dataset/identity", nil)) || !bytes.Equal(catalog, control(http.MethodGet, catalogPath, nil)) {
		t.Fatal("pre-Until intent/config/D/catalog changed")
	}
	releasePeer()
	peerDeadline := time.Now().Add(5 * time.Second)
	for readTask(peerID).Status != base.DownloadStatusDone {
		if time.Now().After(peerDeadline) {
			t.Fatal("queue peer did not complete")
		}
		time.Sleep(5 * time.Millisecond)
	}
	peerActual, err := os.ReadFile(filepath.Join(directory, "peer.bin"))
	if err != nil || !bytes.Equal(peerActual, peerBody) {
		t.Fatalf("queue peer bytes differ: bytes=%d err=%v", len(peerActual), err)
	}
	final, state := wait("original Task complete", func(task *download.Task, state download.RecoveryTaskState) bool {
		return task.Status == base.DownloadStatusDone && state.Status == base.DownloadStatusDone
	})
	actual, err := os.ReadFile(filepath.Join(directory, "one.bin"))
	sum := sha256.Sum256(payload)
	if err != nil || len(actual) != len(payload) || sha256.Sum256(actual) != sum || ranges.Load() != 2 || resumedAt.Load()-refusedAt.Load() < int64(3*time.Second) {
		t.Fatalf("final bytes/Until: bytes=%d ranges=%d wait=%s err=%v", len(actual), ranges.Load(), time.Duration(resumedAt.Load()-refusedAt.Load()), err)
	}
	// Continue during Until preserves the main Task's one known-ACK wave.
	// It may pause and requeue the peer; peer request counts are unrestricted.
	if state.TotalUsed != 1 || state.TransportUsed != 0 || state.Episode != cool.Episode || state.SourceRevision != cool.SourceRevision || state.Generation <= cool.Generation || state.LastExplicitRetry != nil || final.Recovery == nil || final.Recovery.Version != 4 || !bytes.Equal(rawReq, encode(final.Meta.Req)) || !bytes.Equal(creation, encode(final.CreationOperation)) || final.Meta.SingleFilepath() != filepath.Join(directory, "one.bin") || invalidURL.Load() {
		t.Fatal("final identity, counters or path changed")
	}
	if !bytes.Equal(config, control(http.MethodGet, "/api/v1/config", nil)) || !bytes.Equal(identity, control(http.MethodGet, "/api/v1/dataset/identity", nil)) || !bytes.Equal(catalog, control(http.MethodGet, catalogPath, nil)) {
		t.Fatal("final config/D/catalog changed")
	}
	var tasks []*download.Task
	if err := json.Unmarshal(control(http.MethodGet, "/api/v1/tasks", nil), &tasks); err != nil || len(tasks) != 2 {
		t.Fatal("unexpected new Task", err)
	}
	seen := map[string]bool{}
	for _, task := range tasks {
		seen[task.ID] = true
	}
	if !seen[id] || !seen[peerID] {
		t.Fatal("Task IDs changed")
	}
	// Join and close this actual Core before opening its original Bolt read-only.
	f.close()
	db, err := bbolt.Open(filepath.Join(root, "db", "gopeed.db"), 0600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stored struct {
		Version    int    `json:"version"`
		Episode    string `json:"episode"`
		Generation uint64 `json:"generation"`
		Total      int    `json:"totalUsed"`
		Transport  int    `json:"transportUsed"`
		Refresh    int    `json:"refreshUsed"`
		Deferral   int    `json:"deferralUsed"`
		NotBefore  string `json:"notBefore"`
		Next       uint64 `json:"nextAction"`
		Actions    []struct {
			Kind     string   `json:"kind"`
			State    string   `json:"state"`
			Members  []string `json:"members"`
			Finished []string `json:"finished"`
		} `json:"actions"`
	}
	if err := db.View(func(tx *bbolt.Tx) error {
		decode := func(bucket string, dst any) error {
			b := tx.Bucket([]byte(bucket))
			if b == nil || b.Get([]byte(id)) == nil {
				return fmt.Errorf("missing %s", bucket)
			}
			return json.Unmarshal(b.Get([]byte(id)), dst)
		}
		var saved download.Task
		var required struct {
			Version    int    `json:"version"`
			Capability string `json:"capability"`
		}
		if err := decode("task", &saved); err != nil {
			return err
		}
		if err := decode("task_recovery_v1", &stored); err != nil {
			return err
		}
		if err := decode("task_recovery_required_v1", &required); err != nil {
			return err
		}
		if saved.ID != id || saved.Status != base.DownloadStatusDone || saved.Recovery == nil || saved.Recovery.Version != 4 || required.Version != 4 || required.Capability != "http-shared-recovery/v1" {
			return errors.New("stored triplet4 identity/variant/done differs")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	until, err := time.Parse(time.RFC3339Nano, stored.NotBefore)
	if err != nil || until.IsZero() || stored.Version != 4 || stored.Episode != state.Episode || stored.Generation != state.Generation || stored.Total != 1 || stored.Deferral != 1 || stored.Transport != 0 || stored.Refresh != 0 || stored.Next != 1 || len(stored.Actions) != 1 {
		t.Fatalf("closed ledger: %+v err=%v", stored, err)
	}
	a := stored.Actions[0]
	if a.Kind != "deferral" || a.State != "finished" || len(a.Members) != 1 || len(a.Finished) != 1 || a.Members[0] != a.Finished[0] {
		t.Fatalf("one parent not finished: %+v", a)
	}
	t.Logf("actual REST wire1 cooldown; originalM=%s Task=%s peer=%s bytes=%d SHA=%x Until=%s sourceRanges=%d", op, id, peerID, len(actual), sum, stored.NotBefore, ranges.Load())
}
