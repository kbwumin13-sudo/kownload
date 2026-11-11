package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func operationDownloader(t *testing.T, s Storage, dir string) *Downloader {
	t.Helper()
	d := NewDownloader(&DownloaderConfig{Storage: s, StorageDir: dir, FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	return d
}
func operationRequest(url, dir string) BatchOperationRequest {
	return BatchOperationRequest{Version: 1, OperationID: "operation-1", Reqs: []*base.CreateTaskBatchItem{{Req: &base.Request{URL: url}, Opts: &base.Options{Path: dir, Name: "one.bin"}}}}
}
func requireOperation(t *testing.T, d *Downloader, r BatchOperationRequest) BatchOperationResult {
	t.Helper()
	v, e := d.CreateBatchOperation(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func localOperationSource(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("durable-batch-payload"))
	}))
	t.Cleanup(s.Close)
	return s.URL, &hits
}
func TestBatchOperationConcurrentReplayAndCanonicalPrecision(t *testing.T) {
	url, _ := localOperationSource(t)
	dir := t.TempDir()
	d := operationDownloader(t, NewMemStorage(), dir)
	defer d.Close()
	r := operationRequest(url, dir)
	var wg sync.WaitGroup
	out := make(chan BatchOperationResult, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); v, e := d.CreateBatchOperation(context.Background(), r); out <- v; errs <- e }()
	}
	wg.Wait()
	close(out)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	id := ""
	for v := range out {
		if len(v.TaskIDs) != 1 {
			t.Fatalf("receipt %+v", v)
		}
		if id == "" {
			id = v.TaskIDs[0]
		}
		if id != v.TaskIDs[0] {
			t.Fatal("same operation changed IDs")
		}
	}
	waitStartupDone(t, d, id)
	if len(d.GetTasks()) != 1 {
		t.Fatal("concurrent replay created duplicates")
	}
	raw := `{"a":9007199254740993,"nested":{"z":2,"a":1}}`
	var extra any
	if err := decodeStoredJSON([]byte(raw), &extra); err != nil {
		t.Fatal(err)
	}
	r.OperationID = "precision-op"
	r.Reqs[0].Req.Extra = extra
	_, hash, e := canonicalBatch(r)
	if e != nil {
		t.Fatal(e)
	}
	var reordered any
	_ = decodeStoredJSON([]byte(`{"nested":{"a":1,"z":2},"a":9007199254740993}`), &reordered)
	r.Reqs[0].Req.Extra = reordered
	_, same, _ := canonicalBatch(r)
	if hash != same {
		t.Fatal("map order changed fingerprint")
	}
	var different any
	_ = decodeStoredJSON([]byte(`{"a":9007199254740992,"nested":{"a":1,"z":2}}`), &different)
	r.Reqs[0].Req.Extra = different
	_, other, _ := canonicalBatch(r)
	if hash == other {
		t.Fatal("large Extra numbers rounded into same fingerprint")
	}
}

func TestBatchOperationPendingCancelDefaultsAndNoTTL(t *testing.T) {
	url, hits := localOperationSource(t)
	dir := t.TempDir()
	d := operationDownloader(t, NewMemStorage(), dir)
	defer d.Close()
	r := operationRequest(url, dir)
	r.Reqs[0].Opts.Path = ""
	d.cfg.DownloadDir = filepath.Join(dir, "first-default")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, e := d.CreateBatchOperation(ctx, r)
	if e != nil || v.PendingCount != 1 || len(d.GetTasks()) != 0 || hits.Load() != 0 {
		t.Fatalf("reservation: %+v %v", v, e)
	}
	saved, e := d.loadOperation(r.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	if saved.Members[0].Options.Path != d.cfg.DownloadDir {
		t.Fatal("default path not frozen")
	}
	saved.CreatedAt = time.Now().Add(-48 * time.Hour)
	saved.UpdatedAt = saved.CreatedAt
	if e = d.storage.Put(bucketTaskOperations, operationKey(r.OperationID), saved); e != nil {
		t.Fatal(e)
	}
	// More than the legacy cache capacity, without allocating tasks or credentials.
	for i := 0; i < 513; i++ {
		other := operationRequest(url, dir)
		other.OperationID = fmt.Sprintf("pending-%d", i)
		if _, e = d.CreateBatchOperation(ctx, other); e != nil {
			t.Fatal(e)
		}
	}
	v, e = d.QueryBatchOperation(r.OperationID)
	if e != nil || v.PendingCount != 1 {
		t.Fatal("old pending association evicted")
	}
	d.cfg.DownloadDir = filepath.Join(dir, "changed-default")
	created := requireOperation(t, d, r)
	waitStartupDone(t, d, created.TaskIDs[0])
	if _, e = os.Stat(filepath.Join(dir, "first-default", "one.bin")); e != nil {
		t.Fatal("replay reinterpreted default path", e)
	}
	r.OperationID = "cancel-pending"
	v, e = d.CreateBatchOperation(ctx, r)
	if e != nil {
		t.Fatal(e)
	}
	cancelled, e := d.CancelBatchOperation(r.OperationID)
	if e != nil || cancelled.FailedCount != 1 || cancelled.Members[0].Error != "cancelled" {
		t.Fatalf("cancel %+v %v", cancelled, e)
	}
	again := requireOperation(t, d, r)
	if again.FailedCount != 1 || len(again.TaskIDs) != 0 || again.Members[0].TaskID != v.Members[0].TaskID {
		t.Fatal("cancelled pending resurrected")
	}
}

type operationFaultStorage struct {
	AtomicStorage
	phase     string
	fired     atomic.Bool
	denyReads atomic.Bool
}

func (s *operationFaultStorage) Get(bucket, key string, value any) (bool, error) {
	if s.denyReads.Load() {
		return false, errors.New("synthetic unavailable read after commit")
	}
	return s.AtomicStorage.Get(bucket, key, value)
}

type operationFaultTx struct {
	StorageTransaction
	owner   *operationFaultStorage
	sawTask bool
}

func (tx *operationFaultTx) Put(bucket, key string, v any) error {
	if err := tx.StorageTransaction.Put(bucket, key, v); err != nil {
		return err
	}
	if bucket == bucketTask {
		tx.sawTask = true
		if tx.owner.phase == "rollback-task" && !tx.owner.fired.Swap(true) {
			return errors.New("synthetic write failure after task")
		}
		if tx.owner.phase == "rollback-second" && v.(*Task).CreationOperation.Index == 1 && !tx.owner.fired.Swap(true) {
			return errors.New("synthetic second member failure")
		}
	}
	if bucket == bucketOperationTasks && tx.owner.phase == "rollback-link" && !tx.owner.fired.Swap(true) {
		return errors.New("synthetic write failure after reverse link")
	}
	return nil
}
func (s *operationFaultStorage) UpdateAtomic(fn func(StorageTransaction) error) error {
	var proxy *operationFaultTx
	err := s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error {
		proxy = &operationFaultTx{StorageTransaction: tx, owner: s}
		return fn(proxy)
	})
	if err == nil && ((s.phase == "ack-member" && proxy.sawTask) || (s.phase == "ack-member-and-read" && proxy.sawTask) || (s.phase == "ack-reserve" && !proxy.sawTask)) && !s.fired.Swap(true) {
		if s.phase == "ack-member-and-read" {
			s.denyReads.Store(true)
		}
		return errors.New("synthetic commit ack lost")
	}
	return err
}
func TestBatchOperationAtomicFailureAndUnknownCommit(t *testing.T) {
	for _, phase := range []string{"rollback-task", "rollback-link", "ack-reserve", "ack-member"} {
		t.Run(phase, func(t *testing.T) {
			url, _ := localOperationSource(t)
			dir := t.TempDir()
			s := &operationFaultStorage{AtomicStorage: NewBoltStorage(dir), phase: phase}
			d := operationDownloader(t, s, dir)
			defer d.Close()
			r := operationRequest(url, dir)
			v, e := d.CreateBatchOperation(context.Background(), r)
			if !errors.Is(e, ErrOperationUnavailable) {
				t.Fatalf("fault not observed: %+v %v", v, e)
			}
			query, e := d.QueryBatchOperation(r.OperationID)
			if e != nil {
				t.Fatal(e)
			}
			id := query.Members[0].TaskID
			var stored Task
			present, e := s.Get(bucketTask, id, &stored)
			if e != nil {
				t.Fatal(e)
			}
			if phase == "ack-member" {
				if !present || query.CreatedCount != 1 || d.GetTask(id) == nil {
					t.Fatal("committed but unknown ACK became invisible")
				}
			} else {
				if present || query.PendingCount != 1 || d.GetTask(id) != nil {
					t.Fatal("rollback/reserve failure left orphan")
				}
				var link operationTaskLink
				if ok, e := s.Get(bucketOperationTasks, id, &link); e != nil || ok {
					t.Fatal("rollback left a reverse link", e)
				}
			}
			again := requireOperation(t, d, r)
			if again.TaskIDs[0] != id || len(d.GetTasks()) != 1 {
				t.Fatal("retry changed reserved ID")
			}
			waitStartupDone(t, d, id)
		})
	}
}
func TestBatchOperationQueryDoesNotAttachAndCanceledPOSTRepairsQ(t *testing.T) {
	url, hits := localOperationSource(t)
	dir := t.TempDir()
	store := NewBoltStorage(dir)
	d := operationDownloader(t, store, dir)
	defer d.Close()
	r := operationRequest(url, dir)
	items, hash, e := canonicalBatch(r)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.reserveOperation(store, r, items, hash); e != nil {
		t.Fatal(e)
	}
	if _, e = d.createOperationMember(store, r.OperationID, hash, 0, items[0]); e != nil {
		t.Fatal(e)
	}
	q, e := d.QueryBatchOperation(r.OperationID)
	if e != nil || q.CreatedCount != 1 || d.GetTask(q.TaskIDs[0]) != nil || hits.Load() != 0 {
		t.Fatal("GET attached or executed M->Q gap")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, e := d.CreateBatchOperation(ctx, r)
	if e != nil || v.TaskIDs[0] != q.TaskIDs[0] {
		t.Fatal("cancelled POST lost committed task", e)
	}
	waitStartupDone(t, d, q.TaskIDs[0])
	before := hits.Load()
	requireOperation(t, d, r)
	if hits.Load() != before || len(d.GetTasks()) != 1 {
		t.Fatal("replay restarted published task")
	}
}

func TestBatchOperationDeleteAndLostReferenceRemainHistorical(t *testing.T) {
	for _, mode := range []string{"deleted", "api-delete", "lost-ref", "unknown-ref"} {
		t.Run(mode, func(t *testing.T) {
			url, _ := localOperationSource(t)
			dir := t.TempDir()
			d := operationDownloader(t, NewBoltStorage(dir), dir)
			r := operationRequest(url, dir)
			v := requireOperation(t, d, r)
			id := v.TaskIDs[0]
			waitStartupDone(t, d, id)
			if mode == "api-delete" {
				if e := d.Delete(&TaskFilter{IDs: []string{id}}, false); e != nil {
					t.Fatal(e)
				}
			}
			if e := d.Close(); e != nil {
				t.Fatal(e)
			}
			s := NewBoltStorage(dir)
			var task Task
			_, e := s.Get(bucketTask, id, &task)
			if e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "deleted":
				e = s.Delete(bucketTask, id)
			case "lost-ref":
				task.CreationOperation = nil
				task.Status = base.DownloadStatusPause
				e = s.Put(bucketTask, id, &task)
			case "unknown-ref":
				task.CreationOperation.Version = 99
				task.Status = base.DownloadStatusPause
				e = s.Put(bucketTask, id, &task)
			}
			if e != nil {
				t.Fatal(e)
			}
			_ = s.Close()
			d = operationDownloader(t, NewBoltStorage(dir), dir)
			defer d.Close()
			q, e := d.QueryBatchOperation(r.OperationID)
			if e != nil || q.TaskIDs[0] != id {
				t.Fatal("lost historical association", e)
			}
			if mode == "deleted" || mode == "api-delete" {
				if *q.Members[0].TaskPresent || d.GetTask(id) != nil {
					t.Fatal("deleted task appears present")
				}
				requireOperation(t, d, r)
				if len(d.GetTasks()) != 0 {
					t.Fatal("deleted task recreated")
				}
			} else {
				if q.Members[0].Error != "association_conflict" || !d.getTask(id).creationHeld() {
					t.Fatal("old reader lost-ref treated as legacy")
				}
				if e = d.Continue(&TaskFilter{IDs: []string{id}}); !errors.Is(e, ErrOperationConflict) {
					t.Fatalf("explicit execution bypass: %v", e)
				}
				_ = d.ContinueBatch(nil)
				_ = d.Continue(nil)
				if d.GetTask(id).Status != base.DownloadStatusPause {
					t.Fatal("incompatible association auto-started")
				}
			}
		})
	}
}

func TestBatchOperationMemberCrashWorker(t *testing.T) {
	stage := os.Getenv("K15_OPERATION_CRASH_STAGE")
	if stage == "" {
		return
	}
	dir := os.Getenv("K15_OPERATION_CRASH_DIR")
	url := os.Getenv("K15_OPERATION_CRASH_URL")
	store := NewBoltStorage(dir)
	d := operationDownloader(t, store, dir)
	r := operationRequest(url, dir)
	items, hash, e := canonicalBatch(r)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.reserveOperation(store, r, items, hash); e != nil {
		t.Fatal(e)
	}
	if stage == "R" {
		os.Exit(91)
	}
	if stage == "Minside" {
		crash := &operationCrashStorage{AtomicStorage: store}
		_, _ = d.createOperationMember(crash, r.OperationID, hash, 0, items[0])
		t.Fatal("crash hook not reached")
	}
	if _, e = d.createOperationMember(store, r.OperationID, hash, 0, items[0]); e != nil {
		t.Fatal(e)
	}
	os.Exit(91)
}

type operationCrashStorage struct{ AtomicStorage }
type operationCrashTx struct{ StorageTransaction }

func (tx operationCrashTx) Put(bucket, key string, v any) error {
	if e := tx.StorageTransaction.Put(bucket, key, v); e != nil {
		return e
	}
	if bucket == bucketTask {
		os.Exit(91)
	}
	return nil
}
func (s *operationCrashStorage) UpdateAtomic(fn func(StorageTransaction) error) error {
	return s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error { return fn(operationCrashTx{tx}) })
}
func TestBatchOperationActualProcessCrashRecovery(t *testing.T) {
	for _, stage := range []string{"R", "Minside", "MbeforeQ"} {
		t.Run(stage, func(t *testing.T) {
			url, hits := localOperationSource(t)
			dir := t.TempDir()
			binary, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			command := exec.Command(binary, "-test.run=^TestBatchOperationMemberCrashWorker$")
			command.Dir = dir
			command.Env = append(os.Environ(), "K15_OPERATION_CRASH_STAGE="+stage, "K15_OPERATION_CRASH_DIR="+dir, "K15_OPERATION_CRASH_URL="+url)
			raw, e := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 91 {
				t.Fatalf("child crash: %v %s", e, raw)
			}
			d := operationDownloader(t, NewBoltStorage(dir), dir)
			defer d.Close()
			r := operationRequest(url, dir)
			q, e := d.QueryBatchOperation(r.OperationID)
			if e != nil {
				t.Fatal(e)
			}
			id := q.Members[0].TaskID
			if hits.Load() != 0 {
				t.Fatal("reservation/transaction did network IO")
			}
			if stage == "MbeforeQ" {
				if q.CreatedCount != 1 || d.GetTask(id) == nil {
					t.Fatal("committed association lost after crash")
				}
				requireOperation(t, d, r)
				if d.GetTask(id).Status != base.DownloadStatusPause || hits.Load() != 0 {
					t.Fatal("replay restarted already loaded task")
				}
				if e = d.Continue(&TaskFilter{IDs: []string{id}}); e != nil {
					t.Fatal(e)
				}
			} else {
				if q.PendingCount != 1 || len(d.GetTasks()) != 0 {
					t.Fatal("uncommitted child left orphan")
				}
				v := requireOperation(t, d, r)
				if v.TaskIDs[0] != id {
					t.Fatal("crash changed stable member ID")
				}
			}
			waitStartupDone(t, d, id)
			if len(d.GetTasks()) != 1 {
				t.Fatal("crash recovery duplicated task")
			}
		})
	}
}

func TestBatchOperationUnsupportedStorageAndLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	s := &handoffFailStorage{Storage: NewMemStorage()}
	d := operationDownloader(t, s, dir)
	defer d.Close()
	if _, e := d.CreateBatchOperation(context.Background(), operationRequest("http://127.0.0.1:1", dir)); !errors.Is(e, ErrOperationUnsupported) {
		t.Fatal("non-atomic fallback permitted", e)
	}
	task := startupStoredTask("legacy", dir, base.DownloadStatusPause)
	raw, e := json.Marshal(task)
	if e != nil || strings.Contains(string(raw), "creationOperation") {
		t.Fatal("legacy JSON gained required operation state")
	}
}

func TestBatchOperationPartialStorageFailureHasAllThreeStates(t *testing.T) {
	url, _ := localOperationSource(t)
	dir := t.TempDir()
	s := &operationFaultStorage{AtomicStorage: NewBoltStorage(dir), phase: "rollback-second"}
	d := operationDownloader(t, s, dir)
	defer d.Close()
	r := operationRequest(url, dir)
	r.Reqs = append(r.Reqs, &base.CreateTaskBatchItem{Req: &base.Request{URL: url}, Opts: &base.Options{Path: dir, Name: "two.bin"}}, &base.CreateTaskBatchItem{Req: &base.Request{URL: "unsupported://invalid"}})
	v, e := d.CreateBatchOperation(context.Background(), r)
	if !errors.Is(e, ErrOperationUnavailable) || v.CreatedCount != 1 || v.PendingCount != 1 || v.FailedCount != 1 {
		t.Fatalf("partial outcome erased: %+v %v", v, e)
	}
	first := v.TaskIDs[0]
	pending := v.Members[1].TaskID
	replay := requireOperation(t, d, r)
	if replay.CreatedCount != 2 || replay.PendingCount != 0 || replay.FailedCount != 1 || replay.TaskIDs[0] != first || replay.TaskIDs[1] != pending || len(d.GetTasks()) != 2 {
		t.Fatalf("partial retry %+v", replay)
	}
	waitStartupDone(t, d, first)
	waitStartupDone(t, d, pending)
}
func TestBatchOperationCommitAndReadUnknownRecoversOnPOSTOnly(t *testing.T) {
	url, hits := localOperationSource(t)
	dir := t.TempDir()
	s := &operationFaultStorage{AtomicStorage: NewBoltStorage(dir), phase: "ack-member-and-read"}
	d := operationDownloader(t, s, dir)
	defer d.Close()
	r := operationRequest(url, dir)
	v, e := d.CreateBatchOperation(context.Background(), r)
	if !errors.Is(e, ErrOperationUnavailable) || v.OperationID != r.OperationID || v.State != "" || len(d.GetTasks()) != 0 || hits.Load() != 0 {
		t.Fatalf("unknown commit fabricated state: %+v %v", v, e)
	}
	s.denyReads.Store(false)
	query, e := d.QueryBatchOperation(r.OperationID)
	if e != nil || query.CreatedCount != 1 || len(d.GetTasks()) != 0 || hits.Load() != 0 {
		t.Fatal("query repaired or lost authority", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	replay, e := d.CreateBatchOperation(ctx, r)
	if e != nil || replay.TaskIDs[0] != query.TaskIDs[0] {
		t.Fatal("post failed to attach original Task", e)
	}
	waitStartupDone(t, d, replay.TaskIDs[0])
	if len(d.GetTasks()) != 1 {
		t.Fatal("unknown commit caused duplicate")
	}
}
func TestBatchOperationCancelRacesReplayWithoutResurrection(t *testing.T) {
	url, _ := localOperationSource(t)
	dir := t.TempDir()
	d := operationDownloader(t, NewBoltStorage(dir), dir)
	defer d.Close()
	r := operationRequest(url, dir)
	for i := 1; i < 8; i++ {
		r.Reqs = append(r.Reqs, &base.CreateTaskBatchItem{Req: &base.Request{URL: url}, Opts: &base.Options{Path: dir, Name: fmt.Sprintf("item-%d.bin", i)}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := d.CreateBatchOperation(ctx, r); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, e := d.CreateBatchOperation(context.Background(), r); errs <- e }()
	go func() { defer wg.Done(); _, e := d.CancelBatchOperation(r.OperationID); errs <- e }()
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	q, e := d.QueryBatchOperation(r.OperationID)
	if e != nil || q.PendingCount != 0 || q.CreatedCount+q.FailedCount != len(r.Reqs) {
		t.Fatalf("race state %+v %v", q, e)
	}
	again := requireOperation(t, d, r)
	if fmt.Sprint(again.TaskIDs) != fmt.Sprint(q.TaskIDs) || again.FailedCount != q.FailedCount || len(d.GetTasks()) != q.CreatedCount {
		t.Fatal("cancel replay resurrected members")
	}
	for _, id := range q.TaskIDs {
		waitStartupDone(t, d, id)
	}
}
func TestBatchOperationLargeNumberSurvivesTaskStorageAndClone(t *testing.T) {
	dir := t.TempDir()
	d := operationDownloader(t, NewBoltStorage(dir), dir)
	defer d.Close()
	r := operationRequest("http://127.0.0.1:1/never", dir)
	var extra any
	_ = decodeStoredJSON([]byte(`{"opaque":9007199254740993}`), &extra)
	r.Reqs[0].Req.Extra = extra
	items, hash, e := canonicalBatch(r)
	if e != nil {
		t.Fatal(e)
	}
	store := d.storage.(AtomicStorage)
	if e = d.reserveOperation(store, r, items, hash); e != nil {
		t.Fatal(e)
	}
	if _, e = d.createOperationMember(store, r.OperationID, hash, 0, items[0]); e != nil {
		t.Fatal(e)
	}
	q, e := d.QueryBatchOperation(r.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	var task Task
	if _, e = d.storage.Get(bucketTask, q.TaskIDs[0], &task); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(task.clone())
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("Task/clone rounded Extra: %s", raw)
	}
	r.Reqs[0].Req.Extra = map[string]any{"opaque": json.Number("9007199254740992")}
	if _, e = d.CreateBatchOperation(context.Background(), r); !errors.Is(e, ErrOperationConflict) {
		t.Fatal("different large numeric request replayed", e)
	}
}

func TestBatchOperationRejectsEachChangedIntent(t *testing.T) {
	dir := t.TempDir()
	d := operationDownloader(t, NewMemStorage(), dir)
	defer d.Close()
	r := operationRequest("http://127.0.0.1:1/source?sig=one", dir)
	r.Reqs = append(r.Reqs, &base.CreateTaskBatchItem{Req: &base.Request{URL: "http://127.0.0.1:1/second"}, Opts: &base.Options{Path: dir, Name: "two.bin"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := d.CreateBatchOperation(ctx, r); e != nil {
		t.Fatal(e)
	}
	changes := map[string]func(*BatchOperationRequest){
		"url": func(c *BatchOperationRequest) { c.Reqs[0].Req.URL += "&changed=true" },
		"body": func(c *BatchOperationRequest) {
			c.Reqs[0].Req.Extra = map[string]any{"method": "POST", "body": "synthetic-A"}
		},
		"headers": func(c *BatchOperationRequest) {
			c.Reqs[0].Req.Extra = map[string]any{"header": map[string]string{"Cookie": "synthetic-B"}}
		},
		"proxy": func(c *BatchOperationRequest) {
			c.Reqs[0].Req.Proxy = &base.RequestProxy{Mode: base.RequestProxyModeNone}
		},
		"tls":      func(c *BatchOperationRequest) { c.Reqs[0].Req.SkipVerifyCert = true },
		"protocol": func(c *BatchOperationRequest) { c.Reqs[0].Req.Protocol = "http" },
		"label":    func(c *BatchOperationRequest) { c.Reqs[0].Req.Labels = map[string]string{"scope": "changed"} },
		"name":     func(c *BatchOperationRequest) { c.Reqs[0].Opts.Name = "changed.bin" },
		"path":     func(c *BatchOperationRequest) { c.Reqs[0].Opts.Path = filepath.Join(dir, "other") },
		"files":    func(c *BatchOperationRequest) { c.Reqs[0].Opts.SelectFiles = []int{1} },
		"extra":    func(c *BatchOperationRequest) { c.Reqs[0].Opts.Extra = map[string]any{"connections": 2} },
		"order":    func(c *BatchOperationRequest) { c.Reqs[0], c.Reqs[1] = c.Reqs[1], c.Reqs[0] },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			copy, e := cloneOperationJSON(r)
			if e != nil {
				t.Fatal(e)
			}
			change(&copy)
			if _, e = d.CreateBatchOperation(context.Background(), copy); !errors.Is(e, ErrOperationConflict) {
				t.Fatalf("different %s accepted: %v", name, e)
			}
		})
	}
	if len(d.GetTasks()) != 0 {
		t.Fatal("conflict created tasks")
	}
}
func TestBatchOperationUnknownPersistedVersionsAreQuarantined(t *testing.T) {
	for _, mode := range []string{"manifest-version", "manifest-namespace", "index-version", "missing-index"} {
		t.Run(mode, func(t *testing.T) {
			url, _ := localOperationSource(t)
			dir := t.TempDir()
			store := NewBoltStorage(dir)
			d := operationDownloader(t, store, dir)
			r := operationRequest(url, dir)
			items, hash, e := canonicalBatch(r)
			if e != nil {
				t.Fatal(e)
			}
			if e = d.reserveOperation(store, r, items, hash); e != nil {
				t.Fatal(e)
			}
			if _, e = d.createOperationMember(store, r.OperationID, hash, 0, items[0]); e != nil {
				t.Fatal(e)
			}
			record, e := d.loadOperation(r.OperationID)
			if e != nil {
				t.Fatal(e)
			}
			id := record.Members[0].TaskID
			if e = d.Close(); e != nil {
				t.Fatal(e)
			}
			store = NewBoltStorage(dir)
			switch mode {
			case "manifest-version":
				record.Version = 99
				e = store.Put(bucketTaskOperations, operationKey(r.OperationID), record)
			case "manifest-namespace":
				record.Namespace = "unsupported/future"
				e = store.Put(bucketTaskOperations, operationKey(r.OperationID), record)
			case "index-version":
				var link operationTaskLink
				_, _ = store.Get(bucketOperationTasks, id, &link)
				link.Ref.Version = 99
				e = store.Put(bucketOperationTasks, id, &link)
			case "missing-index":
				e = store.Delete(bucketOperationTasks, id)
			}
			if e != nil {
				t.Fatal(e)
			}
			_ = store.Close()
			d = operationDownloader(t, NewBoltStorage(dir), dir)
			defer d.Close()
			if !d.getTask(id).creationHeld() {
				t.Fatal("unknown persisted state treated as executable")
			}
			if e = d.Continue(&TaskFilter{IDs: []string{id}}); !errors.Is(e, ErrOperationConflict) {
				t.Fatal("explicit continue bypassed version", e)
			}
			_ = d.Continue(nil)
			_ = d.ContinueBatch(nil)
			if e = d.Patch(id, r.Reqs[0].Req, r.Reqs[0].Opts); !errors.Is(e, ErrOperationConflict) {
				t.Fatal("patch bypassed quarantine", e)
			}
			if _, e = d.Stats(id); !errors.Is(e, ErrOperationConflict) {
				t.Fatal("stats restored unsupported state", e)
			}
			if d.GetTask(id).Status != base.DownloadStatusPause {
				t.Fatal("unknown state started")
			}
			if _, e = d.CreateBatchOperation(context.Background(), r); !errors.Is(e, ErrOperationConflict) {
				t.Fatal("replay accepted unknown association", e)
			}
			if len(d.GetTasks()) != 1 {
				t.Fatal("unknown state lost or recreated task")
			}
		})
	}
}

type operationTaskReadFailure struct {
	AtomicStorage
	enabled atomic.Bool
}

func (s *operationTaskReadFailure) Get(bucket, key string, v any) (bool, error) {
	if bucket == bucketTask && s.enabled.Load() {
		return false, errors.New("synthetic task lookup unavailable")
	}
	return s.AtomicStorage.Get(bucket, key, v)
}
func TestBatchOperationTaskReadFailureKeepsCompleteManifestReceipt(t *testing.T) {
	dir := t.TempDir()
	store := &operationTaskReadFailure{AtomicStorage: NewBoltStorage(dir)}
	d := operationDownloader(t, store, dir)
	defer d.Close()
	r := operationRequest("http://127.0.0.1:1/never", dir)
	r.Reqs = append(r.Reqs, &base.CreateTaskBatchItem{Req: &base.Request{URL: "unsupported://invalid"}}, &base.CreateTaskBatchItem{Req: &base.Request{URL: "http://127.0.0.1:1/later"}, Opts: &base.Options{Path: dir, Name: "later.bin"}})
	items, hash, e := canonicalBatch(r)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.reserveOperation(store, r, items, hash); e != nil {
		t.Fatal(e)
	}
	if _, e = d.createOperationMember(store, r.OperationID, hash, 0, items[0]); e != nil {
		t.Fatal(e)
	}
	store.enabled.Store(true)
	q, e := d.QueryBatchOperation(r.OperationID)
	if !errors.Is(e, ErrOperationUnavailable) || len(q.Members) != 3 || q.CreatedCount != 1 || q.FailedCount != 1 || q.PendingCount != 1 || q.State != "pending" || len(q.TaskIDs) != 1 || q.Members[0].TaskPresent != nil {
		t.Fatalf("task read failure erased durable outcome: %+v %v", q, e)
	}
	if len(d.GetTasks()) != 0 {
		t.Fatal("query attached a task while reading failure")
	}
	store.enabled.Store(false)
}
