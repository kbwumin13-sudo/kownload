package download

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func coverageRuntimeDownloader(t *testing.T, bolt bool, dir string) *Downloader {
	t.Helper()
	var store Storage = NewMemStorage()
	if bolt {
		store = NewBoltStorage(filepath.Join(dir, "db"))
	}
	if err := InitializeDatasetIdentity(store); err != nil {
		t.Fatal(err)
	}
	return operationDownloader(t, store, dir)
}
func coverageRuntimeRecord(t *testing.T, d *Downloader, id string, predicate func(recoveryRecord) bool) recoveryRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var r recoveryRecord
		ok, err := d.storage.Get(bucketTaskRecovery, id, &r)
		if err == nil && ok && predicate(r) {
			return r
		}
		time.Sleep(time.Millisecond)
	}
	var r recoveryRecord
	d.storage.Get(bucketTaskRecovery, id, &r)
	t.Fatalf("coverage record did not reach boundary: %+v", r)
	return r
}
func TestCoverageRuntimeNewMInstallBeforeFirstTargetByte(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(fmt.Sprint(bolt), func(t *testing.T) {
			payload := bytes.Repeat([]byte("owned new M"), 4096)
			gate := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(gate) }) }
			var requests atomic.Int32
			var bodyWritten atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("ETag", `"new-m-identity"`)
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				select {
				case <-gate:
					n, _ := w.Write(payload)
					bodyWritten.Add(int64(n))
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			d := coverageRuntimeDownloader(t, bolt, dir)
			defer d.Close()
			defer release()
			started := make(chan string, 1)
			d.Listener(func(e *Event) {
				if e.Key == EventKeyStart {
					select {
					case started <- e.Task.ID:
					default:
					}
				}
			})
			request := operationRequest(server.URL+"/file?exact=a%2Fb", dir)
			request.OperationID = "coverage-new-m"
			request.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 1}
			result, err := d.CreateBatchOperation(context.Background(), request)
			if err != nil || len(result.TaskIDs) != 1 {
				t.Fatal(err)
			}
			id := result.TaskIDs[0]
			select {
			case got := <-started:
				if got != id {
					t.Fatal("wrong started Task")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Start not reached after install")
			}
			record := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Version == recoverySharedVersion })
			if record.Coverage == nil || len(record.Coverage.DurableRanges) != 0 || len(record.Coverage.SettledRanges) != 0 || record.Coverage.SettlementSequence != 0 {
				t.Fatal("first baseline was not empty")
			}
			task := d.GetTask(id)
			info, err := os.Stat(task.Meta.SingleFilepath())
			if err != nil {
				t.Fatal(err)
			}
			// Touch may preallocate the declared size before the asynchronous
			// single-stream worker truncates it. Neither zero-filled allocation
			// nor file length is evidence that response payload was written.
			if info.Size() != 0 && info.Size() != int64(len(payload)) {
				t.Fatalf("unexpected initial allocation size: %d", info.Size())
			}
			initial, err := os.ReadFile(task.Meta.SingleFilepath())
			if err != nil || len(initial) > len(payload) || !bytes.Equal(initial, make([]byte, len(initial))) {
				t.Fatal("target contains payload before body gate", err)
			}
			if bodyWritten.Load() != 0 || task.Progress == nil || task.Progress.Downloaded != 0 {
				t.Fatal("source body/progress preceded body gate")
			}
			release()
			waitStatus(t, d, id, base.DownloadStatusDone, 5*time.Second)
			record = coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Coverage != nil && len(r.Coverage.DurableRanges) == 1 })
			if record.Coverage.DurableRanges[0] != (transfer.HTTPCoverageRange{Begin: 0, End: int64(len(payload))}) || len(record.Coverage.SettledRanges) != 0 || record.Coverage.SettlementSequence != 0 {
				t.Fatal("done invented a settlement")
			}
			sameContent(t, task.Meta.SingleFilepath(), payload)
			plan, err := transfer.PlanHTTPCoverageCheckpoint(*record.Coverage, record.Coverage.DurableRanges, record.Coverage.CheckpointHash, recoveryCoverageRangeLimit)
			if err != nil || plan.ThresholdReached || !plan.WholeResourceCovered || plan.Settlement == nil || plan.NetNewBytes != int64(len(payload)) {
				t.Fatal("real small whole output did not retain the mathematical candidate", err)
			}
			// A successfully done Task has no next transport boundary: the candidate
			// must not rotate its episode or issue an unused free run.
			// Exercise the original public Mem/Bolt Restore checkpoint contract, not
			// only JSON equality. Mem must retain the protocol's original dynamic type.
			live := d.getTask(id)
			checkpoint, restore := live.fetcherManager.Restore()
			if ok, err := d.storage.Get(bucketSave, id, checkpoint); err != nil || !ok {
				t.Fatal("checkpoint lost its Restore type", err)
			}
			if restored := restore(live.Meta, checkpoint); restored == nil || restored.Meta().Req.URL != request.Reqs[0].Req.URL {
				t.Fatal("checkpoint cannot restore")
			}
			before := requests.Load()
			again, err := d.CreateBatchOperation(context.Background(), request)
			if err != nil || len(again.TaskIDs) != 1 || again.TaskIDs[0] != id || requests.Load() != before {
				t.Fatal("same M replay created IO/Task", err)
			}
		})
	}
}
func TestCoverageRuntimeHistoricalTaskDoesNotInstall(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("historical same Task")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"historical-strong"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.Write(payload)
	}))
	defer server.Close()
	store := NewBoltStorage(filepath.Join(dir, "db"))
	store.Setup([]string{bucketTask})
	InitializeDatasetIdentity(store)
	task := recoveryFixtureTask(dir)
	task.Status = base.DownloadStatusPause
	task.Meta.Req.URL = server.URL + "/old"
	task.Meta.Opts.Name = "historical.bin"
	task.Meta.Opts.Extra = &httpModel.OptsExtra{Connections: 1}
	if err := store.Put(bucketTask, task.ID, task); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	d := operationDownloader(t, NewBoltStorage(filepath.Join(dir, "db")), dir)
	defer d.Close()
	if err := d.Continue(&TaskFilter{IDs: []string{task.ID}}); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, task.ID, base.DownloadStatusDone, 5*time.Second)
	record := coverageRuntimeRecord(t, d, task.ID, func(r recoveryRecord) bool { return r.Version == 1 })
	if record.Coverage != nil || record.TotalUsed != 1 {
		t.Fatal("historical Task gained new install/free attempt")
	}
	sameContent(t, filepath.Join(dir, "historical.bin"), payload)
}
func TestCoverageRuntimeRepeatedPrefixCannotEarnTwice(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(fmt.Sprint(bolt), func(t *testing.T) {
			prefix := bytes.Repeat([]byte("x"), (1<<20)+4096)
			total := len(prefix) + 16384
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := hits.Add(1)
				if n >= 3 {
					w.WriteHeader(403)
					return
				}
				w.Header().Set("ETag", `"repeat-prefix"`)
				w.Header().Set("Content-Length", fmt.Sprint(total))
				w.Write(prefix)
			}))
			defer server.Close()
			dir := t.TempDir()
			d := coverageRuntimeDownloader(t, bolt, dir)
			defer d.Close()
			id, err := d.CreateDirect(&base.Request{URL: server.URL + "/prefix"}, &base.Options{Path: dir, Name: "prefix.bin", Extra: &httpModel.OptsExtra{Connections: 1}})
			if err != nil {
				t.Fatal(err)
			}
			first := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool {
				return r.Version == recoverySharedVersion && r.Coverage != nil && r.Coverage.SettlementSequence == 1
			})
			waitStatus(t, d, id, base.DownloadStatusError, 5*time.Second)
			before := d.GetTask(id)
			if first.TotalUsed != 0 || len(first.Coverage.SettledRanges) != 1 || first.Coverage.SettledRanges[0].End != int64(len(prefix)) {
				t.Fatal("first earned baseline incorrect")
			}
			live := d.getTask(id)
			owner := live.recoveryOwner.Load()
			// The DB commit is observable before ACK returns. Wait for the same
			// production Fence to leave before asserting its RAM-only ACK result.
			d.operationMu.Lock()
			authorized := owner.coverageSettlementAuthorization.Load() != nil
			d.operationMu.Unlock()
			if !authorized {
				t.Fatal("fresh ACK did not reserve one next begin")
			}
			if err = d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
				t.Fatal(err)
			}
			second := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Generation > first.Generation && r.TotalUsed > 0 })
			waitStatus(t, d, id, base.DownloadStatusError, 5*time.Second)
			if second.Episode != first.Episode || second.Coverage.SettlementSequence != 1 || !bytes.Equal(coverageRetryCopy(t, second.Coverage.SettledRanges), coverageRetryCopy(t, first.Coverage.SettledRanges)) {
				t.Fatal("truncate/rewrite earned same bytes again")
			}
			after := d.GetTask(id)
			if after.ID != before.ID || after.Meta.Opts.Path != before.Meta.Opts.Path || after.Meta.Opts.Name != before.Meta.Opts.Name {
				t.Fatal("coverage replaced Task/path")
			}
			if owner.coverageSettlementAuthorization.Load() != nil {
				t.Fatal("same covered prefix minted a second authorization")
			}
			var raw json.RawMessage
			if err := withDatasetReadView(d.storage, mustCoverageDataset(t, d), func(reader datasetReader) error { _, err := reader.Get(bucketSave, id, &raw); return err }); err != nil {
				t.Fatal(err)
			}
			if !json.Valid(raw) {
				t.Fatal("lost actual checkpoint")
			}
		})
	}
}
func mustCoverageDataset(t *testing.T, d *Downloader) string {
	t.Helper()
	v, e := d.DatasetIdentity()
	if e != nil {
		t.Fatal(e)
	}
	return v.DatasetID
}

func TestCoverageRuntimeInstallationAuthorityIsProcessLocal(t *testing.T) {
	for _, issued := range []bool{false, true} {
		t.Run(fmt.Sprint(issued), func(t *testing.T) {
			store := NewMemStorage()
			task, owner := recoveryFixtureStore(t, store, t.TempDir())
			if issued {
				if _, err := owner.begin(1, ""); err != nil {
					t.Fatal(err)
				}
				if owner.coverageInstallAuthorization.Load() == nil {
					t.Fatal("new ACKed Task lost first installation slot")
				}
			}
			var saved Task
			if ok, err := store.Get(bucketTask, task.ID, &saved); err != nil || !ok {
				t.Fatal(err)
			}
			reloaded, err := newTaskRecoveryOwner(store, &saved)
			if err != nil {
				t.Fatal(err)
			}
			generation := uint64(1)
			if issued {
				generation = 2
			}
			if _, err = reloaded.begin(generation, ""); err != nil {
				t.Fatal(err)
			}
			if reloaded.coverageInstallAuthorization.Load() != nil || reloaded.coverageNewEnrollment.Load() {
				t.Fatal("durable initial state recreated D4 installation authority")
			}
			if issued && reloaded.record.TotalUsed != 1 {
				t.Fatal("reload issued a new free initial attempt")
			}
		})
	}
}

func TestCoverageRuntimeObservationCannotAuthorizeIO(t *testing.T) {
	task, owner := recoveryFixtureStore(t, NewMemStorage(), t.TempDir())
	lease, err := owner.begin(1, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.currentLease(context.Background()); err != nil {
		t.Fatal(err)
	}
	round := uint64(0)
	for i := 0; i < recoveryMaxTransport; i++ {
		permit, err := lease.Retry(context.Background(), round, "a", []string{"a"})
		if err != nil {
			t.Fatal(err)
		}
		if err = permit.Consume(); err != nil {
			t.Fatal(err)
		}
		if err = permit.Finish(context.Background()); err != nil {
			t.Fatal(err)
		}
		round = permit.Round
	}
	if _, err = lease.Retry(context.Background(), round, "a", []string{"a"}); err != transfer.ErrRecoveryExhausted {
		t.Fatal(err)
	}
	if lease.Check() == nil || owner.CheckRecoveryObservation(lease.Token) != nil {
		t.Fatal("known exhaustion confused observation and IO")
	}
	owner.mu.Lock()
	owner.observeUnknownWrite(fmt.Errorf("synthetic unknown storage ACK"))
	owner.mu.Unlock()
	if owner.CheckRecoveryObservation(lease.Token) == nil || owner.coverageInstallAuthorization.Load() != nil || owner.coverageSettlementAuthorization.Load() != nil {
		t.Fatal("unknown ACK retained observation/authority")
	}
	_ = task
}

// Faults decorate real AtomicStorage and its real commit boundary. They do not
// replace the domain read view or publish any fake runtime/IO observations.
type coverageRuntimeAckFault struct {
	AtomicStorage
	mode      string
	knownACK  bool
	armed     atomic.Bool
	committed chan struct{}
	release   <-chan struct{}
}
type coverageRuntimeAckTx struct {
	StorageTransaction
	mode    string
	matched *bool
}

func (tx coverageRuntimeAckTx) Put(bucket, key string, value any) error {
	if bucket == bucketTaskRecovery {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		var r recoveryRecord
		if err = decodeStoredJSON(raw, &r); err != nil {
			return err
		}
		if r.Version == recoverySharedVersion && r.Coverage != nil && ((tx.mode == "install" && r.Coverage.SettlementSequence == 0) || (tx.mode == "earned" && r.Coverage.SettlementSequence > 0)) {
			*tx.matched = true
		}
	}
	return tx.StorageTransaction.Put(bucket, key, value)
}
func (s *coverageRuntimeAckFault) UpdateAtomic(fn func(StorageTransaction) error) error {
	matched := false
	err := s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error { return fn(coverageRuntimeAckTx{tx, s.mode, &matched}) })
	if err == nil && matched && s.armed.Swap(false) {
		close(s.committed)
		if s.release != nil {
			<-s.release
		}
		if s.knownACK {
			return nil
		}
		return fmt.Errorf("synthetic coverage commit ACK unavailable")
	}
	return err
}

func TestCoverageRuntimeUnknownInstallACKNeverStartsTarget(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(fmt.Sprint(bolt), func(t *testing.T) {
			headers, ack := make(chan struct{}), make(chan struct{})
			entered := make(chan struct{})
			var headerOnce, ackOnce sync.Once
			releaseHeaders := func() { headerOnce.Do(func() { close(headers) }) }
			releaseACK := func() { ackOnce.Do(func() { close(ack) }) }
			payload := bytes.Repeat([]byte("install-ack"), 4096)
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) == 1 {
					close(entered)
				}
				select {
				case <-headers:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("ETag", `"unknown-install"`)
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
				w.Write(payload)
			}))
			defer server.Close()
			dir := t.TempDir()
			d := coverageRuntimeDownloader(t, bolt, dir)
			defer d.Close()
			defer releaseHeaders()
			defer releaseACK()
			var started atomic.Int32
			d.Listener(func(e *Event) {
				if e.Key == EventKeyStart {
					started.Add(1)
				}
			})
			request := operationRequest(server.URL+"/install", dir)
			request.OperationID = "unknown-install"
			result, err := d.CreateBatchOperation(context.Background(), request)
			if err != nil || len(result.TaskIDs) != 1 {
				t.Fatal(err)
			}
			id := result.TaskIDs[0]
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("real Resolve not entered")
			}
			live := d.getTask(id)
			owner := live.recoveryOwner.Load()
			if owner == nil {
				t.Fatal("no actual recovery owner")
			}
			fault := &coverageRuntimeAckFault{AtomicStorage: d.storage.(AtomicStorage), mode: "install", committed: make(chan struct{}), release: ack}
			fault.armed.Store(true)
			owner.mu.Lock()
			owner.store = fault
			owner.mu.Unlock()
			releaseHeaders()
			select {
			case <-fault.committed:
			case <-time.After(3 * time.Second):
				t.Fatal("actual v4 transaction not committed")
			}
			var committed recoveryRecord
			if ok, err := d.storage.Get(bucketTaskRecovery, id, &committed); err != nil || !ok || committed.Version != recoverySharedVersion {
				t.Fatal("fault is not after real commit", err)
			}
			if started.Load() != 0 {
				t.Fatal("Start before install ACK")
			}
			if _, err = os.Stat(filepath.Join(dir, "one.bin")); !os.IsNotExist(err) {
				t.Fatal("target exists before install ACK", err)
			}
			releaseACK()
			waitStatus(t, d, id, base.DownloadStatusError, 5*time.Second)
			d.operationMu.Lock()
			install := owner.coverageInstallAuthorization.Load()
			settlement := owner.coverageSettlementAuthorization.Load()
			d.operationMu.Unlock()
			if install != nil || settlement != nil || started.Load() != 0 || hits.Load() != 1 {
				t.Fatal("unknown install ACK produced authority/IO")
			}
			final := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Version == recoverySharedVersion })
			if final.Coverage == nil || final.Coverage.SettlementSequence != 0 {
				t.Fatal("unknown v4 downgraded")
			}
			var stored Task
			d.storage.Get(bucketTask, id, &stored)
			if stored.Recovery == nil || stored.Recovery.Version != recoverySharedVersion || stored.Status != base.DownloadStatusError {
				t.Fatal("unknown install did not reconcile managed error")
			}
		})
	}
}

func TestCoverageRuntimeUnknownEarnedACKReloadChargesNextBegin(t *testing.T) {
	coverageRuntimeEarnedACK(t, false)
}
func TestCoverageRuntimeEarnedACKDatasetChangeCannotAuthorize(t *testing.T) {
	coverageRuntimeEarnedACK(t, true)
}
func coverageRuntimeEarnedACK(t *testing.T, changeDataset bool) {
	dir := t.TempDir()
	headers, ack := make(chan struct{}), make(chan struct{})
	entered := make(chan struct{})
	var headerOnce, ackOnce sync.Once
	releaseHeaders := func() { headerOnce.Do(func() { close(headers) }) }
	releaseACK := func() { ackOnce.Do(func() { close(ack) }) }
	prefix := bytes.Repeat([]byte("q"), (1<<20)+4096)
	total := len(prefix) + 16384
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			close(entered)
			select {
			case <-headers:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("ETag", `"unknown-earned"`)
		w.Header().Set("Content-Length", fmt.Sprint(total))
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		if n == 1 {
			w.Write(prefix)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	d := coverageRuntimeDownloader(t, true, dir)
	defer func() {
		if d != nil {
			d.Close()
		}
	}()
	defer releaseHeaders()
	defer releaseACK()
	id, err := d.CreateDirect(&base.Request{URL: server.URL + "/earned"}, &base.Options{Path: dir, Name: "earned.bin", Extra: &httpModel.OptsExtra{Connections: 1}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("Resolve not entered")
	}
	live := d.getTask(id)
	owner := live.recoveryOwner.Load()
	if owner == nil {
		t.Fatal("no actual owner")
	}
	fault := &coverageRuntimeAckFault{AtomicStorage: d.storage.(AtomicStorage), mode: "earned", knownACK: changeDataset, committed: make(chan struct{}), release: ack}
	fault.armed.Store(true)
	owner.mu.Lock()
	owner.store = fault
	owner.mu.Unlock()
	releaseHeaders()
	select {
	case <-fault.committed:
	case <-time.After(5 * time.Second):
		t.Fatal("earned transaction not reached")
	}
	before := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Coverage != nil && r.Coverage.SettlementSequence == 1 })
	if owner.coverageSettlementAuthorization.Load() != nil {
		t.Fatal("unacknowledged earned commit produced authorization")
	}
	if changeDataset {
		setScopedDataset(t, d.storage, scopedDatasetB)
	}
	releaseACK()
	d.operationMu.Lock()
	authority := owner.coverageSettlementAuthorization.Load()
	d.operationMu.Unlock()
	if authority != nil {
		t.Fatal("unknown ACK or changed dataset reconstructed winner")
	}
	waitStatus(t, d, id, base.DownloadStatusError, 5*time.Second)
	if changeDataset {
		_, after, err := readRecovery(d.storage, id)
		if err != nil || !bytes.Equal(coverageRetryCopy(t, before), coverageRetryCopy(t, after)) {
			t.Fatal("domain failure changed committed record or budget", err)
		}
		return
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	d = nil
	d = operationDownloader(t, NewBoltStorage(filepath.Join(dir, "db")), dir)
	reloaded := d.getTask(id)
	if reloaded == nil || reloaded.Recovery == nil || reloaded.Recovery.Version != recoverySharedVersion {
		t.Fatal("lost managed Task on real reopen")
	}
	if err = d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	next := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Generation > before.Generation })
	if next.TotalUsed != 1 || next.Episode != before.Episode || next.Coverage.SettlementSequence != 1 {
		t.Fatal("reopen obtained a free earned attempt or lost baseline")
	}
}

// This helper writes the compatibility database through the real scheduled
// CreateBatchOperation -> HTTP response -> install -> seal -> earned path.
// Its separate frozen-reader executable performs the rejection check.
func TestCoverageRuntimeWriteCompatibilityDatabase(t *testing.T) {
	dir := os.Getenv("K15_COVERAGE_COMPAT_DIR")
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("compatibility directory must exist and be empty", err)
	}
	prefix := bytes.Repeat([]byte("compatibility-data"), 65536)
	total := len(prefix) + 16384
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"actual-v3-writer"`)
		w.Header().Set("Content-Length", fmt.Sprint(total))
		w.Write(prefix)
	}))
	defer server.Close()
	d := coverageRuntimeDownloader(t, true, dir)
	defer func() {
		if d != nil {
			d.Close()
		}
	}()
	request := operationRequest(server.URL+"/compat?full=URL%2Fproof", dir)
	request.OperationID = "d4-real-writer-compat"
	request.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 1}
	result, err := d.CreateBatchOperation(context.Background(), request)
	if err != nil || len(result.TaskIDs) != 1 {
		t.Fatal(err)
	}
	id := result.TaskIDs[0]
	record := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool {
		return r.Version == recoverySharedVersion && r.Coverage != nil && r.Coverage.SettlementSequence == 1
	})
	waitStatus(t, d, id, base.DownloadStatusError, 5*time.Second)
	d.operationMu.Lock()
	d.operationMu.Unlock()
	task := d.GetTask(id)
	sameContent(t, task.Meta.SingleFilepath(), prefix)
	manifest := struct {
		TaskID, DatasetID, OutputPath, ExpectedBody string
		Size                                        int
		Operation                                   BatchOperationRequest
		Record                                      recoveryRecord
	}{id, mustCoverageDataset(t, d), task.Meta.SingleFilepath(), string(prefix), len(prefix), request, record}
	// Keep expected bytes in a sibling file rather than logging task data.
	if err = os.WriteFile(filepath.Join(dir, "expected-prefix"), prefix, 0600); err != nil {
		t.Fatal(err)
	}
	manifest.ExpectedBody = "expected-prefix"
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	d = nil
	if err = os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageRuntimeCancelledFirstResolveNeverReinstalls(t *testing.T) {
	first := make(chan struct{})
	var hits atomic.Int32
	payload := []byte("same Task after cancelled initial Resolve")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(first)
			<-r.Context().Done()
			return
		}
		w.Header().Set("ETag", `"cancelled-initial"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.Write(payload)
	}))
	defer server.Close()
	dir := t.TempDir()
	d := coverageRuntimeDownloader(t, true, dir)
	defer d.Close()
	id, err := d.CreateDirect(&base.Request{URL: server.URL + "/cancelled"}, &base.Options{Path: dir, Name: "cancelled.bin", Extra: &httpModel.OptsExtra{Connections: 1}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("first Resolve not entered")
	}
	live := d.getTask(id)
	owner := live.recoveryOwner.Load()
	if owner == nil || owner.coverageInstallAuthorization.Load() == nil {
		t.Fatal("new Task did not have process-local first slot")
	}
	if err = d.Pause(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	// Continue intentionally uses the existing asynchronous Pause handshake;
	// this fixture adds no sleep/pauseDone wait to hide preparation ordering.
	if err = d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, id, base.DownloadStatusDone, 5*time.Second)
	record := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Generation > 1 })
	if record.Version != 1 || record.Coverage != nil || record.TotalUsed != 1 || owner.coverageInstallAuthorization.Load() != nil {
		t.Fatal("cancel/resume recreated installation/free initial authority")
	}
	sameContent(t, filepath.Join(dir, "cancelled.bin"), payload)
}

// This component isolates next-begin admission after a genuine RAM grant would
// have been stored. The separately gated real HTTP test covers its producer.
func TestCoverageRuntimeEarnedNextBeginRequiresSameDataset(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("bolt=%v/changed=%v", bolt, changed), func(t *testing.T) {
				var store AtomicStorage = NewMemStorage()
				if bolt {
					store = NewBoltStorage(t.TempDir())
				}
				defer store.Close()
				dataset, task, owner, _ := coverageRetryFixture(t, store)
				before := coverageRetryCopy(t, owner.record)
				r := owner.record
				owner.coverageSettlementAuthorization.Store(&coverageSettlementAuthorization{DatasetID: dataset, Episode: r.Episode, SourceHash: r.SourceHash, DefaultsHash: r.ExecutionDefaultsHash, IdentityHash: r.Coverage.IdentityHash, Sequence: r.Coverage.SettlementSequence})
				if changed {
					setScopedDataset(t, store, scopedDatasetB)
				}
				lease, err := owner.begin(r.Generation+1, "")
				_, after, readErr := readRecovery(store, task.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if changed {
					if err != transfer.ErrRecoveryHeld || lease != nil || owner.active.Load() || !bytes.Equal(before, coverageRetryCopy(t, after)) {
						t.Fatal("changed dataset consumed or rewrote earned grant", err)
					}
				} else if err != nil || lease == nil || after.TotalUsed != r.TotalUsed || after.Episode != r.Episode {
					t.Fatal("same-domain grant lost or charged", err)
				}
				if owner.coverageSettlementAuthorization.Load() != nil {
					t.Fatal("one-shot grant retained")
				}
			})
		}
	}
}
