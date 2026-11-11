package download

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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

func handoffDownloader(t *testing.T, storage Storage, dir string) *Downloader {
	t.Helper()
	d := NewDownloader(&DownloaderConfig{Storage: storage, StorageDir: dir, FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	return d
}
func handoffRequest(url, dir string) BrowserHandoffRequest {
	sum := sha256.Sum256([]byte(url))
	return BrowserHandoffRequest{Action: "prepare", OperationID: "capture-test-1", ClientID: "profile-A", Browser: "chrome",
		Download: BrowserDownloadBinding{ID: 19, StartTime: "2026-11-11T00:00:00.000Z", URLHash: hex.EncodeToString(sum[:])},
		Req:      &base.Request{URL: url}, Opts: &base.Options{Path: dir, Name: "handoff.bin"}}
}
func requirePrepared(t *testing.T, d *Downloader, r BrowserHandoffRequest) BrowserHandoffResult {
	t.Helper()
	v, err := d.BrowserHandoff(r)
	if err != nil || v.State != "prepared" || len(v.TaskIDs) != 1 {
		t.Fatalf("prepare: %+v %v", v, err)
	}
	return v
}
func commitRequest(r BrowserHandoffRequest, id string) BrowserHandoffRequest {
	r.Action = "commit"
	r.TaskID = id
	r.CancelConfirmed = true
	r.BrowserState = BrowserDownloadState{State: "interrupted", Error: "USER_CANCELED"}
	return r
}
func assertHeld(t *testing.T, d *Downloader, id, path string, hits *atomic.Int32) {
	t.Helper()
	task := d.GetTask(id)
	if task == nil || task.IsCreated || task.Status != base.DownloadStatusPause {
		t.Fatalf("held task: %+v", task)
	}
	if hits.Load() != 0 {
		t.Fatal("prepare made a network request")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("prepare touched output: %v", err)
	}
}
func TestBrowserHandoffPrepareIsDurableAndAllStartPathsHeld(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte("owned-payload")) }))
	defer server.Close()
	dir := t.TempDir()
	d := handoffDownloader(t, NewBoltStorage(dir), dir)
	r := handoffRequest(server.URL+"/file", filepath.Join(dir, "output"))
	v := requirePrepared(t, d, r)
	if err := d.Continue(&TaskFilter{IDs: []string{v.TaskID}}); err == nil {
		t.Fatal("explicit Continue accepted held task")
	}
	_ = d.ContinueBatch(nil)
	_ = d.Continue(nil)
	_ = d.ContinueBatch(&TaskFilter{IDs: []string{v.TaskID}})
	if err := d.doStart(d.getTask(v.TaskID)); !errors.Is(err, ErrHandoffHeld) {
		t.Fatalf("start guard %v", err)
	}
	if err := d.Patch(v.TaskID, r.Req, r.Opts); !errors.Is(err, ErrHandoffHeld) {
		t.Fatalf("patch guard %v", err)
	}
	d.lock.Lock()
	d.enqueueLocked(d.getTaskLocked(v.TaskID))
	d.lock.Unlock()
	d.notifyRunning()
	assertHeld(t, d, v.TaskID, filepath.Join(r.Opts.Path, r.Opts.Name), &hits)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d = handoffDownloader(t, NewBoltStorage(dir), dir)
	defer d.Close()
	again := requirePrepared(t, d, r)
	if again.TaskID != v.TaskID {
		t.Fatal("restart duplicated task")
	}
	_ = d.ContinueBatch(nil)
	assertHeld(t, d, v.TaskID, filepath.Join(r.Opts.Path, r.Opts.Name), &hits)
	commit := commitRequest(r, v.TaskID)
	got, err := d.BrowserHandoff(commit)
	if err != nil || got.State != "committed" {
		t.Fatalf("commit %+v %v", got, err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if d.GetTask(v.TaskID).Status == base.DownloadStatusDone {
			break
		}
	}
	if d.GetTask(v.TaskID).Status != base.DownloadStatusDone {
		t.Fatal("committed task did not finish")
	}
	data, err := os.ReadFile(filepath.Join(r.Opts.Path, r.Opts.Name))
	if err != nil || string(data) != "owned-payload" {
		t.Fatalf("output %q %v", data, err)
	}
	before := hits.Load()
	again, err = d.BrowserHandoff(commit)
	if err != nil || again.TaskID != v.TaskID || len(d.taskList()) != 1 || hits.Load() != before {
		t.Fatal("commit replay started second download")
	}
}

func TestBrowserHandoffConcurrentPrepareAbortAndIdentity(t *testing.T) {
	dir := t.TempDir()
	d := handoffDownloader(t, NewMemStorage(), dir)
	defer d.Close()
	r := handoffRequest("http://127.0.0.1:1/never", dir)
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); v, e := d.BrowserHandoff(r); ids <- v.TaskID; errs <- e }()
	}
	wg.Wait()
	close(ids)
	close(errs)
	var id string
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	for got := range ids {
		if id == "" {
			id = got
		}
		if got != id {
			t.Fatal("same op duplicated")
		}
	}
	if len(d.taskList()) != 1 {
		t.Fatal("more than one task")
	}
	for _, change := range []func(*BrowserHandoffRequest){func(x *BrowserHandoffRequest) { x.ClientID = "profile-B" }, func(x *BrowserHandoffRequest) { x.Browser = "edge" }, func(x *BrowserHandoffRequest) { x.Opts = &base.Options{Path: dir, Name: "changed"} }, func(x *BrowserHandoffRequest) { x.Download.ID++ }} {
		bad := r
		change(&bad)
		if _, e := d.BrowserHandoff(bad); !errors.Is(e, ErrHandoffConflict) {
			t.Fatalf("identity conflict %v", e)
		}
	}
	unsafe := commitRequest(r, id)
	unsafe.BrowserState = BrowserDownloadState{State: "complete"}
	if _, e := d.BrowserHandoff(unsafe); !errors.Is(e, ErrHandoffInvalid) {
		t.Fatalf("complete permitted %v", e)
	}
	abort := r
	abort.Action = "abort"
	abort.Reason = "browser_completed"
	got, e := d.BrowserHandoff(abort)
	if e != nil || got.State != "aborted" {
		t.Fatalf("abort %+v %v", got, e)
	}
	if _, e = d.BrowserHandoff(commitRequest(r, id)); !errors.Is(e, ErrHandoffConflict) {
		t.Fatal("late commit accepted")
	}
	_ = d.ContinueBatch(nil)
	if d.GetTask(id).Status != base.DownloadStatusPause {
		t.Fatal("aborted resumed")
	}
	if e = d.Delete(&TaskFilter{IDs: []string{id}}, false); e != nil {
		t.Fatal(e)
	}
	r.Action = "query"
	got, e = d.BrowserHandoff(r)
	if e != nil || got.State != "unknown" || len(d.taskList()) != 0 {
		t.Fatal("deleted task resurrected")
	}
}

type handoffFailStorage struct {
	Storage
	fail atomic.Bool
}

func (s *handoffFailStorage) Put(bucket, key string, v any) error {
	if bucket == bucketTask && s.fail.Load() {
		return errors.New("injected disk failure")
	}
	return s.Storage.Put(bucket, key, v)
}
func TestBrowserHandoffPersistenceFailureNeverReleasesGate(t *testing.T) {
	dir := t.TempDir()
	storage := &handoffFailStorage{Storage: NewMemStorage()}
	d := handoffDownloader(t, storage, dir)
	defer d.Close()
	r := handoffRequest("http://127.0.0.1:1/never", dir)
	storage.fail.Store(true)
	if v, e := d.BrowserHandoff(r); e == nil || v.State == "prepared" || len(d.taskList()) != 0 {
		t.Fatal("failed prepare published")
	}
	storage.fail.Store(false)
	v := requirePrepared(t, d, r)
	storage.fail.Store(true)
	if got, e := d.BrowserHandoff(commitRequest(r, v.TaskID)); e == nil || got.State != "prepared" {
		t.Fatal("failed commit released gate")
	}
	_ = d.ContinueBatch(nil)
	if !d.getTask(v.TaskID).handoffHeld() {
		t.Fatal("failed commit can start")
	}
	storage.fail.Store(false)
}

func TestBrowserHandoffHeldDoesNotChangeOrdinaryContinueOrJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ordinary")) }))
	defer server.Close()
	dir := t.TempDir()
	d := handoffDownloader(t, NewMemStorage(), dir)
	defer d.Close()
	r := handoffRequest(server.URL+"/held", dir)
	v := requirePrepared(t, d, r)
	// An ordinary persisted task is deliberately paused, as at App startup.
	ordinary := NewTask()
	ordinary.Protocol = "http"
	ordinary.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: server.URL + "/ordinary"}, Opts: &base.Options{Path: dir, Name: "ordinary.bin"}}
	ordinary.Progress = &Progress{}
	ordinary.Status = base.DownloadStatusPause
	d.assignFetcherManager(ordinary)
	initTask(ordinary)
	d.lock.Lock()
	d.tasks = append(d.tasks, ordinary)
	d.lock.Unlock()
	// Model the persisted startup task promised by this fixture. Recovery
	// enrollment must never infer durable membership from a UI-only object.
	if err := d.storage.Put(bucketTask, ordinary.ID, ordinary.clone()); err != nil {
		t.Fatal(err)
	}
	data, err := ordinary.MarshalJSON()
	if err != nil || strings.Contains(string(data), "handoff") {
		t.Fatal("ordinary task JSON changed")
	}
	if err = d.ContinueBatch(nil); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && d.GetTask(ordinary.ID).Status != base.DownloadStatusDone; time.Sleep(10 * time.Millisecond) {
	}
	if d.GetTask(ordinary.ID).Status != base.DownloadStatusDone || d.GetTask(v.TaskID).Status != base.DownloadStatusPause {
		t.Fatal("startup ContinueAll changed ordinary or held behavior")
	}
}
