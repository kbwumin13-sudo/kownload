package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
	"go.etcd.io/bbolt"
)

// Actual scoped new-M, HTTP, target fd and Bolt writer. No task/ledger version
// is hand-labelled and no RAM authorization/cache is injected by this fixture.
func TestSharedRecoveryHTTPShort429KeepsRunAndChargesOnce(t *testing.T) {
	payload := bytes.Repeat([]byte("shared-v4-body"), 32768)
	firstRange := make(chan struct{})
	releaseFailure := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFailure) }) }
	var ranges atomic.Int32
	var refusedAt atomic.Int64
	var resumedAt atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"fixed-shared-v4"`)
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			// Header-only metadata response: body is fetched by a real Range.
			return
		}
		if ranges.Add(1) == 1 {
			close(firstRange)
			select {
			case <-releaseFailure:
			case <-r.Context().Done():
				return
			}
			refusedAt.Store(time.Now().UnixNano())
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		resumedAt.CompareAndSwap(0, time.Now().UnixNano())
		begin, end := 0, len(payload)-1
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end); err != nil || begin < 0 || end >= len(payload) || end < begin {
			t.Errorf("bad actual range %q", r.Header.Get("Range"))
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[begin : end+1])
	}))
	defer server.Close()
	dir := sharedActualWriterDir(t, "coverage-short429")
	d := coverageRuntimeDownloader(t, true, dir)
	defer d.Close()
	defer release()
	started := make(chan string, 2)
	d.Listener(func(event *Event) {
		if event.Key == EventKeyStart {
			select {
			case started <- event.Task.ID:
			default:
			}
		}
	})
	request := operationRequest(server.URL+"/fixed", dir)
	request.OperationID = "shared-short429"
	request.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 1}
	created, err := d.CreateBatchOperation(context.Background(), request)
	if err != nil || len(created.TaskIDs) != 1 {
		t.Fatalf("new M: %+v %v", created, err)
	}
	id := created.TaskIDs[0]
	select {
	case <-firstRange:
	case <-time.After(5 * time.Second):
		t.Fatalf("first real Range did not enter: %+v", d.GetTask(id))
	}
	select {
	case got := <-started:
		if got != id {
			t.Fatal("wrong Start task")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start missing")
	}
	before := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Version == 4 })
	release()
	waiting := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Version == 4 && r.DeferralUsed == 1 })
	if d.GetTask(id).Status != base.DownloadStatusRunning {
		t.Fatal("short cooldown yielded its running slot")
	}
	if waiting.Episode != before.Episode || waiting.Generation != before.Generation || waiting.TransportUsed != 0 || waiting.TotalUsed != 1 || len(waiting.Actions) != 1 {
		t.Fatalf("waiting ledger: %+v", waiting)
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	state, err := d.QueryRecoveryForDataset(identity.DatasetID, id)
	if err != nil || state.Management != "managed" || state.Reason != "cooldown" || state.RetryAllowed {
		t.Fatalf("cooldown read: %+v %v", state, err)
	}
	waitStatus(t, d, id, base.DownloadStatusDone, 8*time.Second)
	final := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return len(r.Actions) == 1 && r.Actions[0].State == "finished" })
	if final.Version != 4 || final.Episode != before.Episode || final.Generation != before.Generation || final.TotalUsed != 1 || final.DeferralUsed != 1 || final.TransportUsed != 0 || len(final.Actions[0].Finished) != len(final.Actions[0].Members) {
		t.Fatalf("admitted ledger: %+v", final)
	}
	if ranges.Load() != 2 || resumedAt.Load()-refusedAt.Load() < int64(850*time.Millisecond) {
		t.Fatalf("source retried early/extra: requests=%d waited=%v", ranges.Load(), time.Duration(resumedAt.Load()-refusedAt.Load()))
	}
	task := d.GetTask(id)
	actual, err := os.ReadFile(task.Meta.SingleFilepath())
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("same Task body mismatch: bytes=%d sha=%x err=%v", len(actual), sha256.Sum256(actual), err)
	}
	beforeRequests := ranges.Load()
	replayed, err := d.CreateBatchOperation(context.Background(), request)
	if err != nil || len(replayed.TaskIDs) != 1 || replayed.TaskIDs[0] != id || ranges.Load() != beforeRequests {
		t.Fatalf("original M replay: %+v %v", replayed, err)
	}
	sharedArchiveWriter(t, d, dir, id, "coverage-short429", payload)
}

// The environment path belongs only to this test harness; no product option,
// REST route or writer hook is introduced to retain actual-writer evidence.
func sharedActualWriterDir(t *testing.T, name string) string {
	t.Helper()
	root := os.Getenv("K15_SHARED_READER_EVIDENCE")
	if root == "" {
		return t.TempDir()
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, name+"-")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
func sharedArchiveWriter(t *testing.T, d *Downloader, dir, id, label string, body []byte) {
	t.Helper()
	if os.Getenv("K15_SHARED_READER_EVIDENCE") == "" {
		return
	}
	task := d.GetTask(id)
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	db, err := bbolt.Open(filepath.Join(dir, "db", dbFile), 0600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var record recoveryRecord
	var required recoveryRequirement
	var stored Task
	err = db.View(func(tx *bbolt.Tx) error {
		if e := decodeStoredJSON(tx.Bucket([]byte(bucketTask)).Get([]byte(id)), &stored); e != nil {
			return e
		}
		if e := decodeStoredJSON(tx.Bucket([]byte(bucketTaskRecovery)).Get([]byte(id)), &record); e != nil {
			return e
		}
		return decodeStoredJSON(tx.Bucket([]byte(bucketRecoveryRequired)).Get([]byte(id)), &required)
	})
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("archive read: %v %v", err, closeErr)
	}
	actual, err := os.ReadFile(task.Meta.SingleFilepath())
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatal("archive output changed", err)
	}
	manifest := struct {
		DatasetID string              `json:"datasetId"`
		TaskID    string              `json:"taskId"`
		StoreDir  string              `json:"storeDir"`
		Output    string              `json:"output"`
		SHA256    string              `json:"sha256"`
		Label     string              `json:"label"`
		Status    base.Status         `json:"taskStatus"`
		Record    recoveryRecord      `json:"record"`
		Required  recoveryRequirement `json:"required"`
	}{identity.DatasetID, id, filepath.Join(dir, "db"), task.Meta.SingleFilepath(), fmt.Sprintf("%x", sha256.Sum256(actual)), label, stored.Status, record, required}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "writer-manifest.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("actual writer archive", path)
}

func TestSharedRecoveryHTTPLong503UsesExistingQueue(t *testing.T) {
	payload := bytes.Repeat([]byte("plain-shared-v4"), 32768)
	firstRange := make(chan struct{})
	releaseFailure := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releaseFailure) }) }
	var ranges atomic.Int32
	var refusedAt, resumedAt atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes") // No strong ETag: plain variant is real, not relabelled.
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			return
		}
		if ranges.Add(1) == 1 {
			close(firstRange)
			select {
			case <-releaseFailure:
			case <-r.Context().Done():
				return
			}
			refusedAt.Store(time.Now().UnixNano())
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(503)
			return
		}
		resumedAt.CompareAndSwap(0, time.Now().UnixNano())
		begin, end := 0, len(payload)-1
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end); err != nil || begin < 0 || end >= len(payload) || end < begin {
			t.Errorf("bad range %q", r.Header.Get("Range"))
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
		w.WriteHeader(206)
		_, _ = w.Write(payload[begin : end+1])
	}))
	defer server.Close()
	otherURL, otherHits := localOperationSource(t)
	dir := sharedActualWriterDir(t, "plain-long503")
	d := coverageRuntimeDownloader(t, true, dir)
	defer d.Close()
	defer release()
	cfg, err := d.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	owned := *cfg
	owned.MaxRunning = 1
	if err := d.PutConfig(&owned); err != nil {
		t.Fatal(err)
	}
	waits := make(chan string, 8)
	d.Listener(func(e *Event) {
		if e.Key == EventKeyProgress && e.Task.getStatus() == base.DownloadStatusWait {
			select {
			case waits <- e.Task.ID:
			default:
			}
		}
	})
	request := operationRequest(server.URL+"/fixed", dir)
	request.OperationID = "shared-long503"
	request.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 1}
	created, err := d.CreateBatchOperation(context.Background(), request)
	if err != nil || len(created.TaskIDs) != 1 {
		t.Fatal("new M", err)
	}
	id := created.TaskIDs[0]
	select {
	case <-firstRange:
	case <-time.After(5 * time.Second):
		t.Fatalf("first Range missing %+v", d.GetTask(id))
	}
	before := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Version == 4 })
	otherRequest := operationRequest(otherURL, dir)
	otherRequest.OperationID = "shared-queued-other"
	otherRequest.Reqs[0].Opts.Name = "other.bin"
	other, err := d.CreateBatchOperation(context.Background(), otherRequest)
	if err != nil || len(other.TaskIDs) != 1 {
		t.Fatal(err)
	}
	if d.GetTask(other.TaskIDs[0]).Status != base.DownloadStatusWait || otherHits.Load() != 0 {
		t.Fatal("other source was not actually queued")
	}
	release()
	select {
	case got := <-waits:
		if got != id {
			t.Fatalf("wrong waiting task %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("long cooldown did not yield %+v", d.GetTask(id))
	}
	waiting := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.DeferralUsed == 1 })
	if waiting.Coverage != nil || waiting.Episode != before.Episode || waiting.TotalUsed != 1 || waiting.TransportUsed != 0 {
		t.Fatalf("plain wait record: %+v", waiting)
	}
	waitStatus(t, d, other.TaskIDs[0], base.DownloadStatusDone, 5*time.Second)
	if otherHits.Load() == 0 {
		t.Fatal("queued independent source did not run in released slot")
	}
	waitStatus(t, d, id, base.DownloadStatusDone, 8*time.Second)
	final := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return len(r.Actions) == 1 && r.Actions[0].State == "finished" })
	if final.Version != 4 || final.Coverage != nil || final.Episode != before.Episode || final.Generation <= before.Generation || final.TotalUsed != 1 || final.TransportUsed != 0 || final.DeferralUsed != 1 {
		t.Fatalf("queue admission: %+v", final)
	}
	if ranges.Load() != 2 || resumedAt.Load()-refusedAt.Load() < int64(2800*time.Millisecond) {
		t.Fatalf("Until/source requests: hits=%d elapsed=%v", ranges.Load(), time.Duration(resumedAt.Load()-refusedAt.Load()))
	}
	task := d.GetTask(id)
	actual, err := os.ReadFile(task.Meta.SingleFilepath())
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatal("plain original Task output mismatch", err)
	}
	replayed, err := d.CreateBatchOperation(context.Background(), request)
	if err != nil || len(replayed.TaskIDs) != 1 || replayed.TaskIDs[0] != id || ranges.Load() != 2 {
		t.Fatal("M replay created IO or task", err)
	}
	sharedArchiveWriter(t, d, dir, id, "plain-long503", payload)
}

// This decorator changes only the return ACK of the real atomic store. Owned
// read views remain on the actual Bolt source captured by Core, not a fake DB.
type sharedRuntimeAckGate struct {
	AtomicStorage
	armed      atomic.Bool
	entered    chan struct{}
	release    chan struct{}
	unknown    bool
	matchUntil string // optional: distinguish explicit Extend from an ordinary checkpoint save
}
type sharedRuntimeAckTx struct {
	StorageTransaction
	deferral   *bool
	matchUntil string
}

func (tx sharedRuntimeAckTx) Put(bucket, key string, value any) error {
	if bucket == bucketTaskRecovery {
		if r, ok := value.(*recoveryRecord); ok && r.Version == 4 && r.DeferralUsed == 1 && len(r.Actions) > 0 && r.Actions[len(r.Actions)-1].State == "charged" && (tx.matchUntil == "" || tx.matchUntil == r.NotBefore) {
			*tx.deferral = true
		}
	}
	return tx.StorageTransaction.Put(bucket, key, value)
}
func (gate *sharedRuntimeAckGate) UpdateAtomic(fn func(StorageTransaction) error) error {
	deferral := false
	err := gate.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error { return fn(sharedRuntimeAckTx{tx, &deferral, gate.matchUntil}) })
	if err == nil && deferral && gate.armed.CompareAndSwap(true, false) {
		close(gate.entered)
		<-gate.release
		if gate.unknown {
			return errors.New("owned test lost storage commit ACK")
		}
	}
	return err
}

func TestSharedRecoveryHTTPAckUnknownAndCancelledLateWave(t *testing.T) {
	for _, scenario := range []struct {
		name                   string
		strong, cancel, expiry bool
	}{{"plain-unknown", false, false, false}, {"coverage-cancelled-late-ack", true, true, false}, {"plain-unknown-expiry", false, false, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("failed-wave"), 16384)
			entered := make(chan struct{})
			failureRelease := make(chan struct{})
			var releaseOnce sync.Once
			releaseFailure := func() { releaseOnce.Do(func() { close(failureRelease) }) }
			var ranges atomic.Int32
			var resumed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario.strong {
					w.Header().Set("ETag", `"ack-wave-fixed"`)
				}
				w.Header().Set("Accept-Ranges", "bytes")
				if r.Header.Get("Range") == "" {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					return
				}
				if ranges.Add(1) == 1 {
					close(entered)
					select {
					case <-failureRelease:
					case <-r.Context().Done():
						return
					}
					wait := "30"
					if scenario.expiry {
						wait = "3"
					}
					w.Header().Set("Retry-After", wait)
					w.WriteHeader(503)
					return
				}
				if scenario.expiry && resumed.Load() {
					begin, end := 0, len(payload)-1
					if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end); err != nil || begin < 0 || end >= len(payload) || end < begin {
						t.Error("invalid resumed range")
						w.WriteHeader(400)
						return
					}
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
					w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
					w.WriteHeader(206)
					_, _ = w.Write(payload[begin : end+1])
					return
				}
				t.Error("unknown/cancelled wave sent another request")
				w.WriteHeader(500)
			}))
			defer server.Close()
			dir := sharedActualWriterDir(t, scenario.name)
			d := coverageRuntimeDownloader(t, true, dir)
			defer d.Close()
			defer releaseFailure()
			paused := make(chan string, 4)
			d.Listener(func(e *Event) {
				if e.Key == EventKeyPause {
					select {
					case paused <- e.Task.ID:
					default:
					}
				}
			})
			request := operationRequest(server.URL+"/ack", dir)
			request.OperationID = "shared-" + scenario.name
			request.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 1}
			created, err := d.CreateBatchOperation(context.Background(), request)
			if err != nil || len(created.TaskIDs) != 1 {
				t.Fatal(err)
			}
			id := created.TaskIDs[0]
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatalf("actual failure request missing %+v", d.GetTask(id))
			}
			before := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Version == 4 })
			identity, err := d.DatasetIdentity()
			if err != nil {
				t.Fatal(err)
			}
			path := d.GetTask(id).Meta.SingleFilepath()
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			owner := d.getTask(id).recoveryOwner.Load()
			if owner == nil {
				t.Fatal("actual owner missing")
			}
			gate := &sharedRuntimeAckGate{entered: make(chan struct{}), release: make(chan struct{}), unknown: !scenario.cancel}
			gate.armed.Store(true)
			owner.mu.Lock()
			gate.AtomicStorage = owner.store
			owner.store = gate
			owner.mu.Unlock()
			var ackOnce sync.Once
			releaseAck := func() { ackOnce.Do(func() { close(gate.release) }) }
			defer releaseAck()
			releaseFailure()
			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("real deferral transaction did not reach ACK gate")
			}
			// Commit is visible, but neither RAM continuation nor next IO exists.
			if owner.sharedContinuation.Load() != nil || ranges.Load() != 1 {
				t.Fatal("RAM/IO preceded commit ACK")
			}
			_, committed, err := d.sharedRecoveryExpected(identity.DatasetID, id)
			if err != nil || committed.Record.DeferralUsed != 1 {
				t.Fatal("real committed record missing", err)
			}
			if scenario.cancel {
				if err := d.Pause(&TaskFilter{IDs: []string{id}}); err != nil {
					t.Fatal(err)
				}
			}
			releaseAck()
			if scenario.cancel {
				select {
				case got := <-paused:
					if got != id {
						t.Fatal("wrong paused task")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("cancelled wave did not finish pause")
				}
			} else {
				waitStatus(t, d, id, base.DownloadStatusError, 5*time.Second)
			}
			if owner.sharedContinuation.Load() != nil || owner.active.Load() || ranges.Load() != 1 {
				t.Fatal("unknown/late ACK resurrected a wave")
			}
			after := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.DeferralUsed == 1 })
			if after.Episode != before.Episode || after.TotalUsed != 1 || after.TransportUsed != 0 || after.Actions[len(after.Actions)-1].State != "charged" {
				t.Fatalf("uncertain attempt was refunded/replayed %+v", after)
			}
			actual, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(actual, original) {
				t.Fatal("failed wave changed target bytes", err)
			}
			if !scenario.expiry {
				sharedArchiveWriter(t, d, dir, id, scenario.name, original)
			}
			d.Close()
			// Preserve the original non-Done writer DB for the old-reader owner.
			// Only this byte-for-byte copy is reopened by the new runtime below.
			reopenedDir := t.TempDir()
			copyStore := filepath.Join(reopenedDir, "db")
			if err := os.MkdirAll(copyStore, 0700); err != nil {
				t.Fatal(err)
			}
			rawDB, err := os.ReadFile(filepath.Join(dir, "db", dbFile))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(copyStore, dbFile), rawDB, 0600); err != nil {
				t.Fatal(err)
			}
			d2 := operationDownloader(t, NewBoltStorage(copyStore), reopenedDir)
			defer d2.Close()
			state, err := d2.QueryRecoveryForDataset(identity.DatasetID, id)
			if err != nil || state.Management != "managed" || state.Reason != "cooldown" || state.RetryAllowed {
				t.Fatalf("reopened Until read %+v %v", state, err)
			}
			if err := d2.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			if ranges.Load() != 1 || d2.getTask(id).fetcher != nil || d2.getTask(id).recoveryOwner.Load() != nil {
				t.Fatal("reload created owner/restore/IO before Until")
			}
			_, reload, err := d2.sharedRecoveryExpected(identity.DatasetID, id)
			if err != nil || reload.Record.TotalUsed != 1 || reload.Record.Episode != before.Episode || reload.Record.NotBefore != after.NotBefore {
				t.Fatal("reload reset the charged budget/Until", err)
			}
			if scenario.expiry {
				// This is a separate private case, not either archived old-reader
				// sample. Preserve its pre-expiry DB and output before resuming.
				if err := os.WriteFile(filepath.Join(reopenedDir, "before-until.db"), rawDB, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(reopenedDir, "before-until.body"), original, 0600); err != nil {
					t.Fatal(err)
				}
				if err := d2.Pause(&TaskFilter{IDs: []string{id}}); err != nil {
					t.Fatal(err)
				}
				waitStatus(t, d2, id, base.DownloadStatusPause, 3*time.Second)
				until, err := time.Parse(time.RFC3339Nano, after.NotBefore)
				if err != nil {
					t.Fatal(err)
				}
				if delay := time.Until(until) + 20*time.Millisecond; delay > 0 {
					time.Sleep(delay)
				}
				if ranges.Load() != 1 || d2.getTask(id).recoveryOwner.Load() != nil {
					t.Fatal("cold runtime reconstructed RAM before admission")
				}
				resumed.Store(true)
				if err := d2.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
					t.Fatal(err)
				}
				waitStatus(t, d2, id, base.DownloadStatusDone, 5*time.Second)
				final := coverageRuntimeRecord(t, d2, id, func(r recoveryRecord) bool { return r.TransportUsed == 1 })
				if final.Episode != before.Episode || final.Generation <= after.Generation || final.TotalUsed != 2 || final.DeferralUsed != 1 || final.NotBefore != after.NotBefore || len(final.Actions) != 2 || final.Actions[0].State != "finished" || ranges.Load() != 2 {
					t.Fatalf("cold expiry admission was free/refunded or changed E/Until: %+v ranges=%d", final, ranges.Load())
				}
				body, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(body, payload) {
					t.Fatal("cold same Task final body", err)
				}
				t.Logf("cold expiry sameTask=%s generation=%d->%d transport=%d deferral=%d total=%d fullSHA=%x", id, after.Generation, final.Generation, final.TransportUsed, final.DeferralUsed, final.TotalUsed, sha256.Sum256(body))
			}
			d2.Close()
			preserved, err := os.ReadFile(filepath.Join(dir, "db", dbFile))
			if err != nil || !bytes.Equal(preserved, rawDB) {
				t.Fatal("old-reader original database changed", err)
			}
		})
	}
}

// The original new-M qualification is produced by CreateBatchOperation and
// consumed by ensureTaskRecovery. Neither RAM qualification nor schema is set
// by this test. The cold case loses that evidence through normal Close/Setup.
func TestSharedRecoveryG0CurrentCheckpointUpgradeAndBegin(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-new-M-RAM", true: "cold-no-RAM"}[cold], func(t *testing.T) {
			blockEntered := make(chan struct{})
			targetEntered := make(chan struct{})
			var blockOnce, targetOnce sync.Once
			var targetHits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/target" {
					targetHits.Add(1)
					targetOnce.Do(func() { close(targetEntered) })
					<-r.Context().Done() // Hold before headers: no later checkpoint can hide G0 preservation.
					return
				}
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("Content-Length", "2097152")
				if r.Header.Get("Range") == "" {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					return
				}
				blockOnce.Do(func() { close(blockEntered) })
				<-r.Context().Done()
			}))
			defer server.Close()
			dir := t.TempDir()
			d := coverageRuntimeDownloader(t, true, dir)
			defer d.Close()
			cfg, err := d.GetConfig()
			if err != nil {
				t.Fatal(err)
			}
			cfg.MaxRunning = 1
			if err = d.PutConfig(cfg); err != nil {
				t.Fatal(err)
			}
			block := operationRequest(server.URL+"/block", dir)
			block.OperationID = "g0-block"
			block.Reqs[0].Opts.Name = "block.bin"
			block.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 1}
			blockResult, err := d.CreateBatchOperation(context.Background(), block)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-blockEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("blocker did not occupy real slot")
			}
			req := operationRequest(server.URL+"/target", dir)
			req.OperationID = "g0-target"
			req.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 1}
			created, err := d.CreateBatchOperation(context.Background(), req)
			if err != nil || len(created.TaskIDs) != 1 {
				t.Fatal(err)
			}
			id := created.TaskIDs[0]
			task := d.getTask(id)
			task.lock.Lock()
			prepareErr := func() error {
				if task.getStatus() != base.DownloadStatusWait || !task.initialHTTPExecution.Load() {
					return fmt.Errorf("new M was not queued with its original restore evidence: status=%s initial=%v", task.getStatus(), task.initialHTTPExecution.Load())
				}
				if err := d.restoreTask(task); err != nil {
					return err
				}
				if !task.recoveryInitialEligible.Load() {
					return fmt.Errorf("normal Restore did not transfer original M evidence")
				}
				capture, ok := task.fetcher.(interface {
					CaptureRecoveryContext(*base.Request) (*transfer.HTTPExecutionContext, bool, error)
				})
				if !ok {
					return fmt.Errorf("HTTP capture missing")
				}
				snapshot, supported, err := capture.CaptureRecoveryContext(task.Meta.Req)
				if err != nil {
					return err
				}
				if !supported {
					return fmt.Errorf("HTTP capture unsupported")
				}
				if _, err = d.ensureTaskRecovery(task, snapshot.DefaultsHash()); err != nil {
					return err
				}
				return d.saveTask(task)
			}()
			task.lock.Unlock()
			if prepareErr != nil {
				t.Fatal(prepareErr)
			}
			identity, err := d.DatasetIdentity()
			if err != nil {
				t.Fatal(err)
			}
			_, prior, err := d.sharedRecoveryExpected(identity.DatasetID, id)
			if err != nil || prior.Record.Version != 1 || prior.Record.Generation != 0 || prior.Record.InitialAttempt.State != "ungranted" || prior.Record.InitialAttempt.CheckpointHash != "" || !prior.CheckpointPresent || prior.CheckpointHash == "" || targetHits.Load() != 0 {
				t.Fatalf("actual G0 CURRENT checkpoint: %+v err=%v hits=%d", prior, err, targetHits.Load())
			}
			owner := task.recoveryOwner.Load()
			if owner == nil || !owner.sharedInitialEligible.Load() {
				t.Fatal("constructor lost original new-M evidence")
			}
			active := d
			if cold {
				d.Close()
				active = operationDownloader(t, NewBoltStorage(filepath.Join(dir, "db")), dir)
				defer active.Close()
				if active.getTask(id).recoveryInitialEligible.Load() || active.getTask(id).recoveryOwner.Load() != nil {
					t.Fatal("Setup reconstructed RAM qualification")
				}
				if err = active.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err = d.Pause(&TaskFilter{IDs: blockResult.TaskIDs}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-targetEntered:
			case <-time.After(5 * time.Second):
				t.Fatalf("actual upgraded begin did not request: %+v", active.GetTask(id))
			}
			_, admitted, err := active.sharedRecoveryExpected(identity.DatasetID, id)
			want := 0
			if cold {
				want = 1
			}
			if err != nil || admitted.Record.Version != 4 || admitted.Record.Generation == 0 || admitted.Record.Episode != prior.Record.Episode || int(admitted.Record.TotalUsed) != want || int(admitted.Record.TransportUsed) != want || admitted.Record.DeferralUsed != 0 || admitted.Record.InitialAttempt.State != "issued" || admitted.CheckpointPresent != prior.CheckpointPresent || admitted.CheckpointHash != prior.CheckpointHash {
				t.Fatalf("G0 upgrade/begin qualification or CURRENT checkpoint changed: cold=%v prior=%+v next=%+v err=%v", cold, prior, admitted, err)
			}
			t.Logf("real G0 upgrade cold=%v sameE=true G=%d used=%d CURRENT=%s hits=%d", cold, admitted.Record.Generation, admitted.Record.TotalUsed, admitted.CheckpointHash, targetHits.Load())
			active.Close()
		})
	}
}

func TestSharedRecoveryHTTPConcurrentRange429OneWave(t *testing.T) {
	payload := bytes.Repeat([]byte("two-owned-ranges"), 262144) // 4 MiB: a real slow-start split.
	var phase atomic.Int32
	var warmCount, refused, resumed atomic.Int32
	warmReady, refuseReady := make(chan struct{}), make(chan struct{})
	failureRelease, bodyFinish := make(chan struct{}), make(chan struct{})
	var failureOnce, finishOnce sync.Once
	releaseFailure := func() { failureOnce.Do(func() { close(failureRelease) }) }
	finishBodies := func() { finishOnce.Do(func() { close(bodyFinish) }) }
	var earliest atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"two-owned-v4"`)
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			return
		}
		begin, end := 0, len(payload)-1
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end); err != nil || begin < 0 || end >= len(payload) || end < begin {
			t.Errorf("bad range %q", r.Header.Get("Range"))
			w.WriteHeader(400)
			return
		}
		if phase.Load() == 0 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
			w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
			w.WriteHeader(206)
			w.(http.Flusher).Flush()
			if warmCount.Add(1) == 2 {
				close(warmReady)
			}
			<-r.Context().Done()
			return
		}
		if phase.Load() == 1 {
			if refused.Add(1) == 2 {
				close(refuseReady)
			}
			select {
			case <-failureRelease:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			w.(http.Flusher).Flush()
			return
		}
		earliest.CompareAndSwap(0, time.Now().UnixNano())
		if n := resumed.Add(1); n > 2 {
			t.Error("controlled workset issued an extra Range")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
		w.WriteHeader(206)
		// Both real ranges reach their final small tail before either ends,
		// keeping unrelated work-stealing out of this wave admission test.
		cut := end + 1 - 256*1024
		if cut < begin {
			cut = begin
		}
		_, _ = w.Write(payload[begin:cut])
		w.(http.Flusher).Flush()
		select {
		case <-bodyFinish:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(payload[cut : end+1])
	}))
	defer server.Close()
	dir := t.TempDir()
	d := coverageRuntimeDownloader(t, true, dir)
	defer d.Close()
	defer releaseFailure()
	defer finishBodies()
	paused := make(chan string, 2)
	d.Listener(func(e *Event) {
		if e.Key == EventKeyPause {
			select {
			case paused <- e.Task.ID:
			default:
			}
		}
	})
	req := operationRequest(server.URL+"/two", dir)
	req.OperationID = "shared-real-two-range-wave"
	req.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 2}
	created, err := d.CreateBatchOperation(context.Background(), req)
	if err != nil || len(created.TaskIDs) != 1 {
		t.Fatal(err)
	}
	id := created.TaskIDs[0]
	select {
	case <-warmReady:
	case <-time.After(5 * time.Second):
		t.Fatal("two actual owned ranges never formed")
	}
	if err = d.Pause(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-paused:
		if got != id {
			t.Fatal("wrong pause")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("two worker join missing")
	}
	phase.Store(1)
	if err = d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refuseReady:
	case <-time.After(5 * time.Second):
		t.Fatal("two resumed Range workers did not both reach source")
	}
	before := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Version == 4 && r.Generation > 1 })
	owner := d.getTask(id).recoveryOwner.Load()
	gate := &sharedRuntimeAckGate{entered: make(chan struct{}), release: make(chan struct{})}
	gate.armed.Store(true)
	owner.mu.Lock()
	gate.AtomicStorage = owner.store
	owner.store = gate
	owner.mu.Unlock()
	var ackOnce sync.Once
	releaseAck := func() { ackOnce.Do(func() { close(gate.release) }) }
	defer releaseAck()
	releaseFailure()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("two refusals did not drain/reserve without self join")
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	_, pending, err := d.sharedRecoveryExpected(identity.DatasetID, id)
	if err != nil {
		t.Fatal(err)
	}
	action := pending.Record.Actions[len(pending.Record.Actions)-1]
	if pending.Record.DeferralUsed != 1 || pending.Record.TotalUsed != before.TotalUsed+1 || pending.Record.TransportUsed != before.TransportUsed || len(action.Members) != 2 || action.Members[0] != "worker/0" || action.Members[1] != "worker/1" || action.State != "charged" || len(action.Finished) != 0 || refused.Load() != 2 || resumed.Load() != 0 || owner.sharedContinuation.Load() != nil {
		t.Fatalf("two-worker pre-ACK wave: %+v refused=%d resumed=%d", pending.Record, refused.Load(), resumed.Load())
	}
	body, err := os.ReadFile(d.GetTask(id).Meta.SingleFilepath())
	if err != nil || !bytes.Equal(body, make([]byte, len(payload))) {
		t.Fatal("target payload preceded wave ACK/Until", err)
	}
	until, err := time.Parse(time.RFC3339Nano, pending.Record.NotBefore)
	if err != nil {
		t.Fatal(err)
	}
	phase.Store(2)
	releaseAck()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if time.Now().Before(until) && resumed.Load() != 0 {
			t.Fatal("a real worker requested before Until")
		}
		task := d.getTask(id)
		task.lock.Lock()
		f := task.fetcher
		task.lock.Unlock()
		if f != nil && f.Progress().TotalDownloaded() >= int64(len(payload)-512*1024) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if resumed.Load() != 2 {
		t.Fatalf("ACK workset was not admitted exactly once each: %d", resumed.Load())
	}
	finishBodies()
	waitStatus(t, d, id, base.DownloadStatusDone, 5*time.Second)
	final := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool {
		return r.DeferralUsed == 1 && r.Actions[len(r.Actions)-1].State == "finished"
	})
	last := final.Actions[len(final.Actions)-1]
	if final.Generation != before.Generation || final.Episode != before.Episode || final.TotalUsed != before.TotalUsed+1 || final.TransportUsed != before.TransportUsed || len(last.Members) != 2 || len(last.Finished) != 2 || resumed.Load() != 2 || earliest.Load() < until.UnixNano() {
		t.Fatalf("two-worker final admission: %+v resumed=%d", final, resumed.Load())
	}
	body, err = os.ReadFile(d.GetTask(id).Meta.SingleFilepath())
	if err != nil || !bytes.Equal(body, payload) {
		t.Fatal("two-worker final bytes", err)
	}
	t.Logf("actual two Range 429: members=%v finished=%v total=%d->%d transport=%d unchangedG=%d requests=2+2 fullSHA=%x", last.Members, last.Finished, before.TotalUsed, final.TotalUsed, final.TransportUsed, final.Generation, sha256.Sum256(body))
}

func TestSharedRecoveryHTTPResolve429UsesOriginalDeferredPath(t *testing.T) {
	payload := bytes.Repeat([]byte("resolve-deferral"), 8192)
	first := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var requests atomic.Int32
	var resumed atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(first)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		resumed.CompareAndSwap(0, time.Now().UnixNano())
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			return
		}
		http.ServeContent(w, r, "resolve.bin", time.Time{}, bytes.NewReader(payload))
	}))
	defer server.Close()
	dir := t.TempDir()
	d := coverageRuntimeDownloader(t, true, dir)
	defer d.Close()
	defer unblock()
	req := operationRequest(server.URL+"/resolve", dir)
	req.OperationID = "shared-resolve-429"
	req.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 1}
	created, err := d.CreateBatchOperation(context.Background(), req)
	if err != nil || len(created.TaskIDs) != 1 {
		t.Fatal(err)
	}
	id := created.TaskIDs[0]
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("real Resolve request missing")
	}
	before := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Version == 4 })
	unblock()
	pending := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.DeferralUsed == 1 })
	waitStatus(t, d, id, base.DownloadStatusWait, 2*time.Second)
	if pending.TotalUsed != 1 || pending.TransportUsed != 0 || pending.Episode != before.Episode || requests.Load() != 1 {
		t.Fatalf("Resolve deferred without durable one-charge: %+v requests=%d", pending, requests.Load())
	}
	if _, err = os.Stat(filepath.Join(dir, "one.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Resolve-only deferral wrote a target", err)
	}
	until, err := time.Parse(time.RFC3339Nano, pending.NotBefore)
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, id, base.DownloadStatusDone, 6*time.Second)
	final := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.DeferralUsed == 1 && r.Actions[0].State == "finished" })
	if final.Generation <= before.Generation || final.Episode != before.Episode || final.TransportUsed != 0 || final.TotalUsed != 1 || len(final.Actions[0].Finished) != len(final.Actions[0].Members) || resumed.Load() < until.UnixNano() || requests.Load() != 3 {
		t.Fatalf("Resolve deferred continuation: %+v requests=%d", final, requests.Load())
	}
	body, err := os.ReadFile(d.GetTask(id).Meta.SingleFilepath())
	if err != nil || !bytes.Equal(body, payload) {
		t.Fatal("Resolve deferred final body", err)
	}
	t.Logf("real Resolve429 directDeferred members=%v total=%d sameE G=%d->%d requests=%d SHA=%x", final.Actions[0].Members, final.TotalUsed, before.Generation, final.Generation, requests.Load(), sha256.Sum256(body))
}

// The real deferral transaction commits, then its ACK is held while the
// concrete store's domain changes. A foreign domain cannot acquire a RAM
// continuation even though a later wave-admission guard also rejects IO.
func TestSharedRecoveryHTTPDeferralACKDomainChangeDoesNotGrantRAM(t *testing.T) {
	payload := bytes.Repeat([]byte("domain-fenced-body"), 8192)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var ranges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			return
		}
		if ranges.Add(1) != 1 {
			t.Error("foreign domain received another actual request")
			w.WriteHeader(500)
			return
		}
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(429)
	}))
	defer server.Close()
	dir := t.TempDir()
	d := coverageRuntimeDownloader(t, true, dir)
	defer d.Close()
	defer unblock()
	req := operationRequest(server.URL+"/domain", dir)
	req.OperationID = "shared-ack-domain"
	req.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 1}
	created, err := d.CreateBatchOperation(context.Background(), req)
	if err != nil || len(created.TaskIDs) != 1 {
		t.Fatal(err)
	}
	id := created.TaskIDs[0]
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("actual first Range missing")
	}
	owner := d.getTask(id).recoveryOwner.Load()
	if owner == nil {
		t.Fatal("real owner missing")
	}
	gate := &sharedRuntimeAckGate{entered: make(chan struct{}), release: make(chan struct{})}
	gate.armed.Store(true)
	owner.mu.Lock()
	gate.AtomicStorage = owner.store
	owner.store = gate
	owner.mu.Unlock()
	var ackOnce sync.Once
	releaseACK := func() { ackOnce.Do(func() { close(gate.release) }) }
	defer releaseACK()
	unblock()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("real committed deferral ACK not gated")
	}
	old, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	foreign := "ffffffffffffffffffffffffffffffff"
	if foreign == old.DatasetID {
		foreign = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	}
	if err = d.storage.Put(bucketConfig, datasetIdentityKey, &datasetRecord{Version: 1, DatasetID: foreign}); err != nil {
		t.Fatal(err)
	}
	releaseACK()
	deadline := time.Now().Add(750 * time.Millisecond)
	for owner.active.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if owner.active.Load() || owner.sharedContinuation.Load() != nil || ranges.Load() != 1 {
		t.Fatalf("ACK from replaced domain minted RAM/IO: active=%v continuation=%+v requests=%d", owner.active.Load(), owner.sharedContinuation.Load(), ranges.Load())
	}
	var record recoveryRecord
	if ok, err := d.storage.Get(bucketTaskRecovery, id, &record); err != nil || !ok || record.DeferralUsed != 1 || record.TotalUsed != 1 || record.TransportUsed != 0 || record.Actions[0].State != "charged" {
		t.Fatalf("domain loss refunded actual action: %+v err=%v", record, err)
	}
	t.Logf("actual ACK-domain fence: original=%s foreign=%s charged=%d RAM=0 nextRange=0", old.DatasetID, foreign, record.DeferralUsed)
}

// Reserve is driven by real HTTP. The explicit extension then uses the same
// typed owner seam as the coordinator, with its real transaction ACK gated.
func TestSharedRecoveryHTTPExtendACKDomainChangeRevokesExistingRAM(t *testing.T) {
	var ranges atomic.Int32
	payload := bytes.Repeat([]byte("extend-domain"), 8192)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			return
		}
		if ranges.Add(1) != 1 {
			t.Error("extended foreign domain received another request")
		}
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(429)
	}))
	defer server.Close()
	dir := t.TempDir()
	d := coverageRuntimeDownloader(t, true, dir)
	defer d.Close()
	req := operationRequest(server.URL+"/extend", dir)
	req.OperationID = "shared-extend-ack-domain"
	req.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 1}
	created, err := d.CreateBatchOperation(context.Background(), req)
	if err != nil || len(created.TaskIDs) != 1 {
		t.Fatal(err)
	}
	id := created.TaskIDs[0]
	coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.DeferralUsed == 1 })
	owner := d.getTask(id).recoveryOwner.Load()
	if owner == nil {
		t.Fatal("real owner missing")
	}
	deadline := time.Now().Add(2 * time.Second)
	for owner.sharedContinuation.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	grant := owner.sharedContinuation.Load()
	if grant == nil {
		t.Fatal("known real Reserve ACK did not produce its RAM candidate")
	}
	until, err := time.Parse(time.RFC3339Nano, grant.Until)
	if err != nil {
		t.Fatal(err)
	}
	gate := &sharedRuntimeAckGate{entered: make(chan struct{}), release: make(chan struct{}), matchUntil: until.Add(time.Second).UTC().Format(time.RFC3339Nano)}
	gate.armed.Store(true)
	owner.mu.Lock()
	gate.AtomicStorage = owner.store
	owner.store = gate
	token, ctx := owner.token, owner.runContext
	owner.mu.Unlock()
	var once sync.Once
	releaseACK := func() { once.Do(func() { close(gate.release) }) }
	defer releaseACK()
	type extensionResult struct {
		until time.Time
		err   error
	}
	done := make(chan extensionResult, 1)
	exited := make(chan struct{})
	extensionCtx, cancelExtension := context.WithCancel(ctx)
	go func() {
		defer close(exited)
		u, e := owner.ExtendRecoveryDeferral(extensionCtx, token, grant.ActionID, grant.Members[0], grant.Members, until.Add(time.Second))
		done <- extensionResult{u, e}
	}()
	defer func() {
		cancelExtension()
		releaseACK()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Error("explicit Extend worker did not exit")
		}
	}()
	select {
	case <-gate.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("real explicit Extend did not reach ACK gate")
	}
	old, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	foreign := "dddddddddddddddddddddddddddddddd"
	if foreign == old.DatasetID {
		foreign = "cccccccccccccccccccccccccccccccc"
	}
	if err = d.storage.Put(bucketConfig, datasetIdentityKey, &datasetRecord{Version: 1, DatasetID: foreign}); err != nil {
		t.Fatal(err)
	}
	releaseACK()
	select {
	case result := <-done:
		if result.err == nil || !result.until.IsZero() {
			t.Fatalf("foreign Extend ACK returned success: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Extend observer did not return")
	}
	if owner.active.Load() || owner.sharedContinuation.Load() != nil || ranges.Load() != 1 {
		t.Fatal("domain change retained previous RAM or sent IO")
	}
	var record recoveryRecord
	if ok, err := d.storage.Get(bucketTaskRecovery, id, &record); err != nil || !ok || record.DeferralUsed != 1 || record.TotalUsed != 1 || record.TransportUsed != 0 || record.NotBefore != until.Add(time.Second).UTC().Format(time.RFC3339Nano) || record.Actions[0].State != "charged" {
		t.Fatalf("Extend changed debit or lost confirmed Until: %+v err=%v", record, err)
	}
	t.Logf("explicit Extend real ACK domain fence: charged=%d Until advanced RAM=0 extraRange=0", record.DeferralUsed)
}

type sharedCapturedStoreManager struct {
	fetcher.FetcherManager
	armed              atomic.Bool
	sampler            bool
	capturedDownloaded atomic.Int64
	entered, release   chan struct{}
}

func (manager *sharedCapturedStoreManager) Store(f fetcher.Fetcher) (any, error) {
	value, err := manager.FetcherManager.Store(f)
	current := f.Progress().TotalDownloaded()
	if err == nil && (!manager.sampler || current > 0) && manager.armed.CompareAndSwap(true, false) {
		manager.capturedDownloaded.Store(current)
		close(manager.entered)
		<-manager.release
	}
	return value, err
}

// The manager delegates real HTTP Store, then holds the captured owned value
// before original persistence. It never fakes Progress or skips the sampler.
// No wait-for-Start fixture shortcut hides
// the startup checkpoint/first 429 ordering.
func TestSharedRecoveryHTTPCheckpointFrontierDrainsPendingStores(t *testing.T) {
	for _, sampler := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup-store", true: "captured-sampler-store"}[sampler], func(t *testing.T) {
			payload := bytes.Repeat([]byte("frontier"), 524288)
			metadata, metadataRelease := make(chan struct{}), make(chan struct{})
			failureReady, failureRelease, refused := make(chan struct{}), make(chan struct{}), make(chan struct{})
			prefixRelease := make(chan struct{})
			var prefixOnce sync.Once
			releasePrefix := func() { prefixOnce.Do(func() { close(prefixRelease) }) }
			var metaOnce, failOnce sync.Once
			releaseMeta := func() { metaOnce.Do(func() { close(metadataRelease) }) }
			releaseFailure := func() { failOnce.Do(func() { close(failureRelease) }) }
			var ranges, after atomic.Int32
			var phase atomic.Bool
			var earliest atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Accept-Ranges", "bytes")
				if r.Header.Get("Range") == "" {
					close(metadata)
					select {
					case <-metadataRelease:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					return
				}
				n := ranges.Add(1)
				begin, end := 0, len(payload)-1
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end); err != nil || begin < 0 || end >= len(payload) || end < begin {
					t.Error("bad frontier range")
					w.WriteHeader(400)
					return
				}
				if !phase.Load() && sampler && n == 1 {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
					w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
					w.WriteHeader(206)
					w.(http.Flusher).Flush()
					select {
					case <-prefixRelease:
					case <-r.Context().Done():
						return
					}
					_, _ = w.Write(payload[begin : begin+1024])
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				if !phase.Load() {
					close(failureReady)
					select {
					case <-failureRelease:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(429)
					w.(http.Flusher).Flush()
					close(refused)
					return
				}
				after.Add(1)
				earliest.CompareAndSwap(0, time.Now().UnixNano())
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
				w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
				w.WriteHeader(206)
				_, _ = w.Write(payload[begin : end+1])
			}))
			defer server.Close()
			dir := t.TempDir()
			gate := &sharedCapturedStoreManager{FetcherManager: &httpProtocol.FetcherManager{}, entered: make(chan struct{}), release: make(chan struct{}), sampler: sampler}
			gate.armed.Store(true)
			store := NewBoltStorage(filepath.Join(dir, "db"))
			if err := InitializeDatasetIdentity(store); err != nil {
				t.Fatal(err)
			}
			d := NewDownloader(&DownloaderConfig{Storage: store, StorageDir: dir, FetchManagers: []fetcher.FetcherManager{gate}})
			if err := d.Setup(); err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			defer releaseMeta()
			defer releaseFailure()
			defer releasePrefix()
			started := make(chan string, 1)
			d.Listener(func(e *Event) {
				if e.Key == EventKeyStart {
					select {
					case started <- e.Task.ID:
					default:
					}
				}
			})
			req := operationRequest(server.URL+"/frontier", dir)
			req.OperationID = "shared-store-frontier"
			connections := 1
			if sampler {
				connections = 2
			}
			req.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: connections}
			created, err := d.CreateBatchOperation(context.Background(), req)
			if err != nil || len(created.TaskIDs) != 1 {
				t.Fatal(err)
			}
			id := created.TaskIDs[0]
			select {
			case <-metadata:
			case <-time.After(5 * time.Second):
				t.Fatal("real metadata gate missing")
			}
			owner := d.getTask(id).recoveryOwner.Load()
			if owner == nil {
				t.Fatal("real owner missing before Resolve headers")
			}
			var ackOnce sync.Once
			releaseACK := func() { ackOnce.Do(func() { close(gate.release) }) }
			defer releaseACK()
			releaseMeta()
			if sampler {
				// Only this case withholds its real prefix until startup Store has
				// completed, making the held capture belong to the actual sampler.
				select {
				case got := <-started:
					if got != id {
						t.Fatal("wrong Start")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("sampler setup Start missing")
				}
				releasePrefix()
			}
			select {
			case <-failureReady:
			case <-time.After(5 * time.Second):
				t.Fatal("actual rate-limited worker missing")
			}
			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("actual captured Store missing")
			}
			releaseFailure()
			select {
			case <-refused:
			case <-time.After(2 * time.Second):
				t.Fatal("source did not actually return429 while owned Store was pending")
			}
			// The real HTTP Store value is captured; its original caller retains
			// Task.lock until the delegate returns and persistence ACKs.
			// Give the independent coordinator a bounded observation interval.
			time.Sleep(100 * time.Millisecond)
			var pending recoveryRecord
			var saved Task
			var checkpoint json.RawMessage
			present, err := d.storage.Get(bucketSave, id, &checkpoint)
			if err != nil || present != sampler {
				t.Fatalf("captured pending Store checkpoint presence=%v sampler=%v err=%v", present, sampler, err)
			}
			if ok, err := d.storage.Get(bucketTask, id, &saved); err != nil || !ok {
				t.Fatal(err)
			}
			if sampler && gate.capturedDownloaded.Load() != 1024 {
				t.Fatalf("sampler Store did not capture real prefix: %d", gate.capturedDownloaded.Load())
			}
			if ok, err := d.storage.Get(bucketTaskRecovery, id, &pending); err != nil || !ok || pending.DeferralUsed != 0 || pending.TotalUsed != 0 || after.Load() != 0 {
				t.Fatalf("action passed pending captured Store: %+v err=%v", pending, err)
			}
			digest := ""
			if present {
				digest, err = checkpointDigest(checkpoint)
				if err != nil {
					t.Fatal(err)
				}
			}
			phase.Store(true)
			releaseACK()
			reserved := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.DeferralUsed == 1 })
			until, err := time.Parse(time.RFC3339Nano, reserved.NotBefore)
			if err != nil {
				t.Fatal(err)
			}
			waitStatus(t, d, id, base.DownloadStatusDone, 8*time.Second)
			final := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.DeferralUsed == 1 && r.Actions[0].State == "finished" })
			if final.TotalUsed != 1 || final.TransportUsed != 0 || final.Generation != pending.Generation || earliest.Load() < until.UnixNano() {
				t.Fatalf("pending Store broke wave budget/Until: %+v", final)
			}
			body, err := os.ReadFile(d.GetTask(id).Meta.SingleFilepath())
			if err != nil || !bytes.Equal(body, payload) {
				t.Fatal("Store frontier final body", err)
			}
			t.Logf("actual captured Store sampler=%v capturedDownloaded=%d priorCURRENT=%s action1/sameG fullSHA=%x", sampler, gate.capturedDownloaded.Load(), digest, sha256.Sum256(body))
		})
	}
}
