//go:build darwin || linux

package rest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"go.etcd.io/bbolt"
)

// Discard the actual authenticated UDS response after the receiver has replied.
// This is a client ACK-loss fixture, not a storage ACK fault or a fake receiver.
type coverageRESTLostACK struct {
	transport http.RoundTripper
	status    int
	body      []byte
	calls     int
}

func (c *coverageRESTLostACK) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls++
	response, err := c.transport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	c.status = response.StatusCode
	c.body, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	return nil, errors.New("owned fixture discarded the received create ACK")
}

func TestRecoveryCoverageBuildServerUDSPrefixReplayAndCompletion(t *testing.T) {
	root, err := os.MkdirTemp("/private/tmp", "d4r-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	prefix := bytes.Repeat([]byte("x"), (1<<20)+4096)
	payload := append(append([]byte(nil), prefix...), bytes.Repeat([]byte("t"), 16384)...)
	var phase atomic.Int32
	var hits [3]atomic.Int32
	var claimed [2]atomic.Bool
	var invalidURL atomic.Bool
	var bodyWrites atomic.Int32
	firstHeaders, firstBody := make(chan struct{}), make(chan struct{})
	var headerOnce, bodyOnce sync.Once
	release := func() { bodyOnce.Do(func() { close(firstBody) }) }
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/fixed?proof=a%2Fb" {
			invalidURL.Store(true)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p := int(phase.Load())
		hits[p].Add(1)
		if p < 2 && !claimed[p].CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("ETag", `"d4-rest-fixed"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if p == 0 {
			headerOnce.Do(func() { close(firstHeaders) })
			select {
			case <-firstBody:
			case <-r.Context().Done():
				return
			}
		}
		bodyWrites.Add(1)
		if p < 2 {
			_, _ = w.Write(prefix) // Real unexpected EOF under the declared full size.
		} else {
			_, _ = w.Write(payload)
		}
	}))
	defer source.Close()
	defer release()
	f := verifiedLiveUDS(t, root) // Actual BuildServer, Bolt, UDS and global restoration.
	defer f.close()
	directory := filepath.Join(root, "files")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	control := func(method, path string, body []byte) json.RawMessage {
		t.Helper()
		status, reply := f.call(t, method, path, body)
		if status != http.StatusOK || reply.Code != 0 {
			t.Fatalf("%s %s: HTTP %d code %d data %s", method, path, status, reply.Code, reply.Data)
		}
		return reply.Data
	}
	identity := control(http.MethodGet, "/api/v1/dataset/identity", nil)
	var domain struct {
		DatasetID string `json:"datasetId"`
	}
	if err := json.Unmarshal(identity, &domain); err != nil || domain.DatasetID != f.domain {
		t.Fatal("actual identity did not match the receiver", err)
	}
	config := control(http.MethodGet, "/api/v1/config", nil)
	endpoint := "/api/v1/datasets/" + domain.DatasetID + "/task-batches/v1/operations"
	opID := "d4-rest-owned-1"
	body, err := json.Marshal(map[string]any{"version": 1, "operationId": opID, "reqs": []any{
		map[string]any{"req": map[string]any{"url": source.URL + "/fixed?proof=a%2Fb"},
			"opts": map[string]any{"path": directory, "name": "one.bin", "extra": map[string]any{"connections": 1, "autoTorrent": false, "autoExtract": false}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	decodeOperation := func(raw []byte) scopedBatchResult {
		t.Helper()
		var result scopedBatchResult
		if err := json.Unmarshal(raw, &result); err != nil || result.ScopeVersion != 1 || result.DatasetID != domain.DatasetID || result.Namespace != scopedBatchNamespace || result.Operation.OperationID != opID || len(result.Operation.TaskIDs) != 1 {
			t.Fatalf("invalid same-domain operation receipt: %s (%v)", raw, err)
		}
		return result
	}
	lost := &coverageRESTLostACK{transport: f.client.Transport}
	client := &http.Client{Transport: lost, CheckRedirect: f.client.CheckRedirect}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Token", verifiedLiveToken)
	req.Header.Set("Content-Type", "application/json")
	response, ackErr := client.Do(req)
	cancel()
	if ackErr == nil || response != nil || lost.calls != 1 || lost.status != http.StatusOK {
		t.Fatal("create ACK was not discarded after exactly one actual POST", ackErr, lost.calls, lost.status)
	}
	var received datasetReply
	if err := json.Unmarshal(lost.body, &received); err != nil || received.Code != 0 {
		t.Fatal("the discarded response was not a successful real receipt", err)
	}
	observed := decodeOperation(received.Data)
	// The client recovers the ID using only the original same-domain GET.
	recovered := decodeOperation(control(http.MethodGet, endpoint+"/"+opID, nil))
	id := recovered.Operation.TaskIDs[0]
	if observed.Operation.TaskIDs[0] != id {
		t.Fatal("GET did not recover the original M")
	}
	recoveryPath := "/api/v1/datasets/" + domain.DatasetID + "/tasks/" + id + "/recovery/v1"
	readRecovery := func() download.RecoveryTaskState {
		t.Helper()
		var reply recoveryReply
		if err := json.Unmarshal(control(http.MethodGet, recoveryPath, nil), &reply); err != nil || reply.ScopeVersion != 1 || reply.DatasetID != domain.DatasetID || reply.Recovery.Version != 1 || reply.Recovery.TaskID != id || reply.Recovery.Management != "managed" {
			t.Fatal("invalid managed recovery projection", reply, err)
		}
		return reply.Recovery
	}
	readTask := func() *download.Task {
		t.Helper()
		task := new(download.Task)
		if err := json.Unmarshal(control(http.MethodGet, "/api/v1/tasks/"+id, nil), task); err != nil || task.ID != id {
			t.Fatal("invalid original Task", err)
		}
		return task
	}
	wait := func(label string, predicate func(*download.Task, download.RecoveryTaskState) bool) (*download.Task, download.RecoveryTaskState) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			task, state := readTask(), readRecovery()
			if predicate(task, state) {
				return task, state
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s not reached: Task %s ref %+v recovery %+v hits %d/%d/%d", label, task.Status, task.Recovery, state, hits[0].Load(), hits[1].Load(), hits[2].Load())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	select {
	case <-firstHeaders:
	case <-time.After(5 * time.Second):
		t.Fatal("actual initial response did not reach the body gate")
	}
	output := filepath.Join(directory, "one.bin")
	// The concrete Bolt runner upgrades the strong-identity coverage triplet to
	// strict v4 before Start; the public recovery projection remains wire v1.
	// Ref/progress can precede the asynchronous Touch. Wait for the actual target
	// before reading it while the source still holds every payload byte.
	initialTask, initial := wait("v4 coverage and target before the first body byte", func(task *download.Task, state download.RecoveryTaskState) bool {
		info, err := os.Stat(output)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal("initial target metadata could not be read", err)
		}
		return task.Recovery != nil && task.Recovery.Version == 4 && task.Progress != nil && state.Episode != "" && state.Generation > 0 && err == nil && info.Mode().IsRegular()
	})
	// Touch can preallocate the full length before the asynchronous non-Range
	// worker truncates it. Length is not evidence of downloaded body bytes.
	select {
	case <-firstBody:
		t.Fatal("initial body gate was already released")
	default:
	}
	initialBytes, err := os.ReadFile(output)
	if err != nil || (len(initialBytes) != 0 && len(initialBytes) != len(payload)) || len(bytes.Trim(initialBytes, "\x00")) != 0 || bodyWrites.Load() != 0 || initialTask.Progress.Downloaded != 0 {
		t.Fatalf("body preceded confirmed v4 coverage: allocated=%d sourceBodyWrites=%d downloaded=%d err=%v", len(initialBytes), bodyWrites.Load(), initialTask.Progress.Downloaded, err)
	}
	t.Logf("confirmed v4 coverage before body: allocated=%d zeroFilled=true sha256=%x sourceBodyWrites=0 downloaded=0", len(initialBytes), sha256.Sum256(initialBytes))
	rawReq, err := json.Marshal(initialTask.Meta.Req)
	if err != nil {
		t.Fatal(err)
	}
	creation, err := json.Marshal(initialTask.CreationOperation)
	if err != nil {
		t.Fatal(err)
	}
	catalogPath := "/api/v1/task-batches/operations?filter=all&limit=100"
	catalog := control(http.MethodGet, catalogPath, nil)
	release()
	_, first := wait("first prefix earned an episode at a real inactive error", func(task *download.Task, state download.RecoveryTaskState) bool {
		return task.Status == base.DownloadStatusError && state.Status == base.DownloadStatusError && state.Episode != initial.Episode
	})
	if actual, err := os.ReadFile(output); err != nil || !bytes.Equal(actual, prefix) {
		t.Fatal("first real prefix was not retained", err)
	}
	beforeReads := hits[0].Load()
	for i := 0; i < 2; i++ {
		if got := decodeOperation(control(http.MethodGet, endpoint+"/"+opID, nil)); got.Operation.TaskIDs[0] != id {
			t.Fatal("query replaced the original Task")
		}
		_ = readRecovery()
	}
	if replay := decodeOperation(control(http.MethodPost, endpoint, body)); replay.Operation.TaskIDs[0] != id {
		t.Fatal("same-op replay replaced the original M")
	}
	afterReads := readRecovery()
	if hits[0].Load() != beforeReads || afterReads.Episode != first.Episode || afterReads.Generation != first.Generation || afterReads.TotalUsed != first.TotalUsed {
		t.Fatal("unknown-ACK query or same-op replay authorized another run", first, afterReads)
	}
	phase.Store(1)
	control(http.MethodPut, "/api/v1/tasks/"+id+"/continue", nil)
	_, second := wait("same prefix did not earn another episode", func(task *download.Task, state download.RecoveryTaskState) bool {
		return task.Status == base.DownloadStatusError && state.Status == base.DownloadStatusError && state.Generation > first.Generation
	})
	if second.Episode != first.Episode || second.TotalUsed <= first.TotalUsed || second.TransportUsed < first.TransportUsed {
		t.Fatal("repeated prefix reset counters or earned another episode", first, second)
	}
	if actual, err := os.ReadFile(output); err != nil || !bytes.Equal(actual, prefix) {
		t.Fatal("repeated prefix changed the retained output", err)
	}
	phase.Store(2)
	control(http.MethodPut, "/api/v1/tasks/"+id+"/continue", nil)
	finalTask, final := wait("same Task completed the full body", func(task *download.Task, state download.RecoveryTaskState) bool {
		return task.Status == base.DownloadStatusDone && state.Status == base.DownloadStatusDone && state.Generation > second.Generation
	})
	actual, err := os.ReadFile(output)
	expectedSHA, actualSHA := sha256.Sum256(payload), sha256.Sum256(actual)
	if err != nil || len(actual) != len(payload) || actualSHA != expectedSHA {
		t.Fatal("final original output size/SHA mismatch", err)
	}
	finalReq, _ := json.Marshal(finalTask.Meta.Req)
	finalCreation, _ := json.Marshal(finalTask.CreationOperation)
	if !bytes.Equal(rawReq, finalReq) || !bytes.Equal(creation, finalCreation) || finalTask.Meta.Opts.Path != directory || finalTask.Meta.Opts.Name != "one.bin" || finalTask.Meta.SingleFilepath() != output || final.Episode != first.Episode || final.TotalUsed < second.TotalUsed || finalTask.Recovery == nil || finalTask.Recovery.Version != 4 || final.LastExplicitRetry != nil {
		t.Fatal("Continue changed intent/path/creation identity or reset the episode")
	}
	var tasks []*download.Task
	if err := json.Unmarshal(control(http.MethodGet, "/api/v1/tasks", nil), &tasks); err != nil || len(tasks) != 1 || tasks[0].ID != id {
		t.Fatal("same-op recovery created another Task", err)
	}
	if !bytes.Equal(config, control(http.MethodGet, "/api/v1/config", nil)) || !bytes.Equal(identity, control(http.MethodGet, "/api/v1/dataset/identity", nil)) || !bytes.Equal(catalog, control(http.MethodGet, catalogPath, nil)) || invalidURL.Load() {
		t.Fatal("configuration, dataset, catalog or exact source URL changed")
	}

	// The public DTO intentionally does not expose B/seq. Close the real Core
	// before opening its Bolt file read-only; never open a second live writer.
	f.close()
	db, err := bbolt.Open(filepath.Join(root, "db", "gopeed.db"), 0600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stored struct {
		Version       int                         `json:"version"`
		Episode       string                      `json:"episode"`
		Generation    uint64                      `json:"generation"`
		TotalUsed     int                         `json:"totalUsed"`
		TransportUsed int                         `json:"transportUsed"`
		Coverage      *transfer.HTTPCoverageState `json:"coverage"`
	}
	if err := db.View(func(tx *bbolt.Tx) error {
		for _, bucket := range []string{"task", "task_recovery_v1", "task_recovery_required_v1"} {
			if tx.Bucket([]byte(bucket)) == nil || tx.Bucket([]byte(bucket)).Get([]byte(id)) == nil {
				return fmt.Errorf("missing persisted %s", bucket)
			}
		}
		if err := json.Unmarshal(tx.Bucket([]byte("task_recovery_v1")).Get([]byte(id)), &stored); err != nil {
			return err
		}
		var task download.Task
		var required struct {
			Version    int    `json:"version"`
			Capability string `json:"capability"`
		}
		if err := json.Unmarshal(tx.Bucket([]byte("task")).Get([]byte(id)), &task); err != nil {
			return err
		}
		if err := json.Unmarshal(tx.Bucket([]byte("task_recovery_required_v1")).Get([]byte(id)), &required); err != nil {
			return err
		}
		if task.ID != id || task.Status != base.DownloadStatusDone || task.Recovery == nil || task.Recovery.Version != 4 || required.Version != 4 || required.Capability != "http-shared-coverage-recovery/v1" {
			return errors.New("persisted Task/ref/required did not preserve strict v4 coverage and done")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	coverage := stored.Coverage
	if stored.Version != 4 || stored.Episode != final.Episode || stored.Generation != final.Generation || stored.TotalUsed != final.TotalUsed || stored.TransportUsed != final.TransportUsed || coverage == nil || coverage.SettlementSequence != 1 || len(coverage.SettledRanges) != 1 || coverage.SettledRanges[0] != (transfer.HTTPCoverageRange{Begin: 0, End: int64(len(prefix))}) || len(coverage.DurableRanges) != 1 || coverage.DurableRanges[0] != (transfer.HTTPCoverageRange{Begin: 0, End: int64(len(payload))}) {
		t.Fatalf("closed Bolt lost the one earned baseline/full durable output: %+v", stored)
	}
	t.Logf("real Bolt/UDS single M, client ACK discarded once; same Task %s; prefix=%d total=%d sha256=%x; episodes %s -> %s -> %s -> %s; used %d/%d/%d; source phase hits %d/%d/%d; final seq=%d", id, len(prefix), len(payload), expectedSHA, initial.Episode, first.Episode, second.Episode, final.Episode, first.TotalUsed, second.TotalUsed, final.TotalUsed, hits[0].Load(), hits[1].Load(), hits[2].Load(), coverage.SettlementSequence)
}
