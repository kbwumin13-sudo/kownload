package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func TestRecoveryHTTPRangeRetryUsesDurableEpisode(t *testing.T) {
	for _, mode := range []string{"one-transient", "one-truncated", "exhausted"} {
		t.Run(mode, func(t *testing.T) {
			payload := bytes.Repeat([]byte("recovery-payload"), 1<<18)
			var ranges atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"recovery-fixture"`)
				w.Header().Set("Accept-Ranges", "bytes")
				if r.Header.Get("Range") == "" {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					_, _ = w.Write(payload[:4096])
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				n := ranges.Add(1)
				if mode == "one-truncated" && n == 1 {
					var start, end int64
					if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
					w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
					w.WriteHeader(206)
					_, _ = w.Write(payload[start : start+1024])
					return
				}
				if mode == "exhausted" || mode == "one-transient" && n == 1 {
					w.WriteHeader(500)
					return
				}
				http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(payload))
			}))
			defer server.Close()
			defer server.CloseClientConnections()
			dir := t.TempDir()
			dbDir := filepath.Join(dir, "db")
			d := operationDownloader(t, NewBoltStorage(dbDir), dir)
			defer d.Close()
			id, err := d.CreateDirect(&base.Request{URL: server.URL + "/payload?sig=synthetic"}, &base.Options{Path: dir, Name: "payload.bin", Extra: &httpModel.OptsExtra{Connections: 1}})
			if err != nil {
				t.Fatal(err)
			}
			want := base.DownloadStatusDone
			if mode == "exhausted" {
				want = base.DownloadStatusError
			}
			waitStatus(t, d, id, want, 15*time.Second)
			var record recoveryRecord
			if ok, e := d.storage.Get(bucketTaskRecovery, id, &record); e != nil || !ok {
				t.Fatalf("no durable recovery: %v %v", ok, e)
			}
			if mode != "exhausted" {
				body, e := os.ReadFile(filepath.Join(dir, "payload.bin"))
				if e != nil || !bytes.Equal(body, payload) {
					t.Fatalf("incomplete recovered payload: %v", e)
				}
				if record.TransportUsed != 1 || record.TotalUsed != 1 || ranges.Load() != 2 {
					t.Fatalf("debit/request mismatch: %+v requests=%d", record, ranges.Load())
				}
			} else {
				if record.TransportUsed != recoveryMaxTransport || record.TotalUsed != recoveryMaxTransport || ranges.Load() != int32(recoveryMaxTransport+1) {
					t.Fatalf("unbounded/replenished retries: %+v requests=%d", record, ranges.Load())
				}
				episode := record.Episode
				before := ranges.Load()
				if err = d.Close(); err != nil {
					t.Fatal(err)
				}
				reopened := operationDownloader(t, NewBoltStorage(dbDir), dir)
				defer reopened.Close()
				// Ordinary Continue cannot refill an exhausted episode, including a
				// real process's persisted state loaded into a new Downloader.
				if err = reopened.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
					t.Fatal(err)
				}
				waitStatus(t, reopened, id, base.DownloadStatusError, 3*time.Second)
				var after recoveryRecord
				if _, err = reopened.storage.Get(bucketTaskRecovery, id, &after); err != nil {
					t.Fatal(err)
				}
				if after.Episode != episode || after.TotalUsed != record.TotalUsed || ranges.Load() != before {
					t.Fatal("restart/Continue reset exhausted budget or sent IO")
				}
			}
		})
	}
}

func TestRecoveryRequestProjectionAndSourceCASPreserveBudget(t *testing.T) {
	store := NewMemStorage()
	task, owner := recoveryFixtureStore(t, store, t.TempDir())
	lease, err := owner.begin(1, "")
	if err != nil {
		t.Fatal(err)
	}
	permit, err := lease.Retry(context.Background(), 0, "a", []string{"a"})
	if err != nil || permit.Consume() != nil {
		t.Fatal(err)
	}
	if err = permit.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	var saved Task
	if _, err = store.Get(bucketTask, task.ID, &saved); err != nil {
		t.Fatal(err)
	}
	saved.Meta.Req.Extra = map[string]any{"method": "GET", "opaque": map[string]any{"large": "9007199254740993"}}
	before := owner.record
	if err = owner.persistSnapshot(&saved, nil, false); err != nil {
		t.Fatal(err)
	}
	if owner.record.SourceRevision != before.SourceRevision || owner.record.Episode != before.Episode || owner.record.TotalUsed != before.TotalUsed || lease.Check() != nil {
		t.Fatal("equivalent interpreted request changed/reset episode")
	}
	saved.Meta.Req.Extra = &httpModel.ReqExtra{Method: "GET", Header: map[string]string{"X-Source": "B-synthetic"}}
	if err = owner.persistSnapshot(&saved, nil, false); err != nil {
		t.Fatal(err)
	}
	if owner.record.SourceRevision != before.SourceRevision+1 || owner.record.Episode != before.Episode || owner.record.TotalUsed != before.TotalUsed || !errors.Is(lease.Check(), transfer.ErrRecoveryStale) {
		t.Fatal("source CAS did not invalidate old token/preserve episode")
	}
}

// The wrapper changes only the optional capability; the legacy proxy and the
// actual HTTP implementation remain intact. It represents a dynamic PAC/nil
// hook without consulting or changing the machine's proxy configuration.
type recoveryCapabilityFetcher struct {
	*httpProtocol.Fetcher
	mode *atomic.Int32
}

func (f *recoveryCapabilityFetcher) Setup(ctl *controller.Controller) {
	copied := *ctl
	if f.mode.Load() == 1 {
		copied.GetRecoveryProxySnapshot = func(*base.RequestProxy) (func(*http.Request) (*url.URL, error), string, bool) { return nil, "", false }
	} else if f.mode.Load() == 2 {
		copied.GetRecoveryProxySnapshot = nil
	}
	// Evaluate on each capture too, including a live task after a clean Pause.
	original := copied.GetRecoveryProxySnapshot
	if original != nil {
		copied.GetRecoveryProxySnapshot = func(req *base.RequestProxy) (func(*http.Request) (*url.URL, error), string, bool) {
			if mode := f.mode.Load(); mode == 1 || mode == 2 {
				return nil, "", false
			}
			return original(req)
		}
	}
	f.Fetcher.Setup(&copied)
}

func (f *recoveryCapabilityFetcher) BindRunWait(ctx context.Context) func() error {
	if f.mode.Load() == 3 {
		return nil
	}
	return f.Fetcher.BindRunWait(ctx)
}

type recoveryCapabilityManager struct {
	httpProtocol.FetcherManager
	mode *atomic.Int32
}

func (m *recoveryCapabilityManager) Build() fetcher.Fetcher {
	return &recoveryCapabilityFetcher{Fetcher: m.FetcherManager.Build().(*httpProtocol.Fetcher), mode: m.mode}
}
func (m *recoveryCapabilityManager) Store(f fetcher.Fetcher) (any, error) {
	return m.FetcherManager.Store(f.(*recoveryCapabilityFetcher).Fetcher)
}
func (m *recoveryCapabilityManager) Restore() (any, func(*fetcher.FetcherMeta, any) fetcher.Fetcher) {
	value, restore := m.FetcherManager.Restore()
	return value, func(meta *fetcher.FetcherMeta, value any) fetcher.Fetcher {
		return &recoveryCapabilityFetcher{Fetcher: restore(meta, value).(*httpProtocol.Fetcher), mode: m.mode}
	}
}
func TestRecoveryUnsupportedContextKeepsLegacyButCannotDowngradeEnrolled(t *testing.T) {
	for _, mode := range []int32{1, 2} {
		t.Run(fmt.Sprint("legacy-", mode), func(t *testing.T) {
			var capability atomic.Int32
			capability.Store(mode)
			var hits atomic.Int32
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = w.Write([]byte("legacy-context")) }))
			defer source.Close()
			dir := t.TempDir()
			store := NewBoltStorage(filepath.Join(dir, "db"))
			d := NewDownloader(&DownloaderConfig{Storage: store, StorageDir: dir, FetchManagers: []fetcher.FetcherManager{&recoveryCapabilityManager{mode: &capability}}})
			if err := d.Setup(); err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			id, err := d.CreateDirect(&base.Request{URL: source.URL}, &base.Options{Path: dir, Name: "legacy.bin"})
			if err != nil {
				t.Fatal(err)
			}
			waitStatus(t, d, id, base.DownloadStatusDone, 3*time.Second)
			for _, bucket := range []string{bucketTaskRecovery, bucketRecoveryRequired} {
				var raw json.RawMessage
				if found, e := store.Get(bucket, id, &raw); found || e != nil {
					t.Fatal("legacy path enrolled unsupported context", bucket, e)
				}
			}
			if d.getTask(id).Recovery != nil || hits.Load() == 0 {
				t.Fatal("legacy HTTP ability lost")
			}
		})
	}
	t.Run("enrolled-to-unsupported", func(t *testing.T) {
		var capability atomic.Int32
		var hits atomic.Int32
		ranged := make(chan struct{}, 1)
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("ETag", `"fixed-fixture"`)
			w.Header().Set("Accept-Ranges", "bytes")
			if r.Header.Get("Range") != "" {
				var start, end int64
				if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/4194304", start, end))
				w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
				w.WriteHeader(206)
				select {
				case ranged <- struct{}{}:
				default:
				}
			} else {
				w.Header().Set("Content-Length", "4194304")
			}
			_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer source.Close()
		defer source.CloseClientConnections()
		dir := t.TempDir()
		store := NewBoltStorage(filepath.Join(dir, "db"))
		d := NewDownloader(&DownloaderConfig{Storage: store, StorageDir: dir, FetchManagers: []fetcher.FetcherManager{&recoveryCapabilityManager{mode: &capability}}})
		if err := d.Setup(); err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		id, err := d.CreateDirect(&base.Request{URL: source.URL}, &base.Options{Path: dir, Name: "held.bin", Extra: &httpModel.OptsExtra{Connections: 1}})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ranged:
		case <-time.After(3 * time.Second):
			t.Fatal("range run not reached")
		}
		if err = d.Pause(&TaskFilter{IDs: []string{id}}); err != nil {
			t.Fatal(err)
		}
		task := d.getTask(id)
		task.statusLock.Lock()
		paused := task.pauseDone
		task.statusLock.Unlock()
		select {
		case <-paused:
		case <-time.After(3 * time.Second):
			t.Fatal("pause did not join")
		}
		var before recoveryRecord
		if found, e := store.Get(bucketTaskRecovery, id, &before); !found || e != nil {
			t.Fatal("not enrolled", e)
		}
		requests := hits.Load()
		capability.Store(1)
		if err = d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
			t.Fatal(err)
		}
		waitStatus(t, d, id, base.DownloadStatusError, 3*time.Second)
		var after recoveryRecord
		if _, err = store.Get(bucketTaskRecovery, id, &after); err != nil {
			t.Fatal(err)
		}
		if hits.Load() != requests || after.Episode != before.Episode || after.TotalUsed != before.TotalUsed || after.InitialAttempt != before.InitialAttempt || after.SourceRevision != before.SourceRevision {
			t.Fatal("unsupported enrolled context sent IO or changed durable recovery")
		}
	})
}

func TestRecoveryMissingRunBindingIsHeldBeforeResolve(t *testing.T) {
	var mode atomic.Int32
	mode.Store(3)
	var hits atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = w.Write([]byte("must-not-send")) }))
	defer source.Close()
	dir := t.TempDir()
	d := NewDownloader(&DownloaderConfig{Storage: NewMemStorage(), StorageDir: dir, FetchManagers: []fetcher.FetcherManager{&recoveryCapabilityManager{mode: &mode}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	result, err := d.CreateBatchOperation(context.Background(), operationRequest(source.URL, dir))
	if err != nil || len(result.TaskIDs) != 1 {
		t.Fatal(result, err)
	}
	waitStatus(t, d, result.TaskIDs[0], base.DownloadStatusError, 3*time.Second)
	if hits.Load() != 0 {
		t.Fatal("unbound managed task sent initial IO")
	}
}

type recoveryRetryFaultStore struct {
	AtomicStorage
	afterCommit bool
}
type recoveryRetryFaultTx struct {
	StorageTransaction
	store *recoveryRetryFaultStore
	debit *bool
}

func (t recoveryRetryFaultTx) Put(bucket, key string, value any) error {
	if bucket == bucketTaskRecovery {
		raw, _ := json.Marshal(value)
		var record recoveryRecord
		if json.Unmarshal(raw, &record) == nil && record.TotalUsed > 0 {
			*t.debit = true
			if !t.store.afterCommit {
				return errors.New("synthetic debit write failure")
			}
		}
	}
	return t.StorageTransaction.Put(bucket, key, value)
}
func (s *recoveryRetryFaultStore) UpdateAtomic(fn func(StorageTransaction) error) error {
	debit := false
	err := s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error { return fn(recoveryRetryFaultTx{tx, s, &debit}) })
	if err == nil && debit && s.afterCommit {
		return errors.New("synthetic debit ACK loss")
	}
	return err
}
func TestRecoveryHTTPDebitFailureNeverSendsNextRange(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "write-failed", true: "ack-unknown"}[after], func(t *testing.T) {
			var ranges atomic.Int32
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"fault-fixture"`)
				w.Header().Set("Accept-Ranges", "bytes")
				if r.Header.Get("Range") != "" {
					ranges.Add(1)
					w.WriteHeader(500)
					return
				}
				w.Header().Set("Content-Length", "4194304")
				_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer source.Close()
			defer source.CloseClientConnections()
			dir := t.TempDir()
			store := &recoveryRetryFaultStore{AtomicStorage: NewBoltStorage(filepath.Join(dir, "db")), afterCommit: after}
			d := operationDownloader(t, store, dir)
			defer d.Close()
			id, err := d.CreateDirect(&base.Request{URL: source.URL}, &base.Options{Path: dir, Name: "fault.bin", Extra: &httpModel.OptsExtra{Connections: 1}})
			if err != nil {
				t.Fatal(err)
			}
			waitStatus(t, d, id, base.DownloadStatusError, 3*time.Second)
			var record recoveryRecord
			if found, e := store.Get(bucketTaskRecovery, id, &record); !found || e != nil {
				t.Fatal("lost ledger", e)
			}
			expected := 0
			if after {
				expected = 1
			}
			if ranges.Load() != 1 || record.TotalUsed != expected {
				t.Fatalf("failure granted another IO or lost committed debit: ranges=%d used=%d", ranges.Load(), record.TotalUsed)
			}
		})
	}
}

type recoveryCrashDebitStore struct{ AtomicStorage }
type recoveryCrashDebitTx struct {
	StorageTransaction
	debit *bool
}

func (t recoveryCrashDebitTx) Put(bucket, key string, value any) error {
	if bucket == bucketTaskRecovery {
		raw, _ := json.Marshal(value)
		var record recoveryRecord
		if json.Unmarshal(raw, &record) == nil && record.TotalUsed > 0 {
			*t.debit = true
		}
	}
	return t.StorageTransaction.Put(bucket, key, value)
}
func (s recoveryCrashDebitStore) UpdateAtomic(fn func(StorageTransaction) error) error {
	debit := false
	err := s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error { return fn(recoveryCrashDebitTx{tx, &debit}) })
	if err == nil && debit {
		os.Exit(74)
	}
	return err
}
func TestRecoveryHTTPDebitCrashWorker(t *testing.T) {
	dir := os.Getenv("K15_HTTP_DEBIT_CRASH_DIR")
	if dir == "" {
		return
	}
	store := recoveryCrashDebitStore{NewBoltStorage(filepath.Join(dir, "db"))}
	d := operationDownloader(t, store, dir)
	id, err := d.CreateDirect(&base.Request{URL: os.Getenv("K15_HTTP_DEBIT_CRASH_URL")}, &base.Options{Path: dir, Name: "crash.bin", Extra: &httpModel.OptsExtra{Connections: 1}})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, id, base.DownloadStatusDone, 10*time.Second)
	t.Fatal("crash checkpoint was not reached")
}
func TestRecoveryHTTPDurableDebitSurvivesProcessExitBeforeNextIO(t *testing.T) {
	var ranges atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"crash-fixture"`)
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") != "" {
			ranges.Add(1)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Length", "4194304")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer source.Close()
	defer source.CloseClientConnections()
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, binary, "-test.run=^TestRecoveryHTTPDebitCrashWorker$")
	child.Dir = dir
	child.Env = append(os.Environ(), "K15_HTTP_DEBIT_CRASH_DIR="+dir, "K15_HTTP_DEBIT_CRASH_URL="+source.URL)
	output, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 74 {
		t.Fatalf("worker did not crash after durable debit: %v %s", err, output)
	}
	if ranges.Load() != 1 {
		t.Fatal("crash worker sent IO after the debit checkpoint", ranges.Load())
	}
	store := NewBoltStorage(filepath.Join(dir, "db"))
	var tasks []*Task
	if err = store.List(bucketTask, &tasks); err != nil || len(tasks) != 1 {
		t.Fatal("lost/recreated task", err)
	}
	id := tasks[0].ID
	var before recoveryRecord
	if found, e := store.Get(bucketTaskRecovery, id, &before); !found || e != nil || before.TotalUsed != 1 {
		t.Fatal("durable debit lost", e)
	}
	d := operationDownloader(t, store, dir)
	defer d.Close()
	if err = d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, id, base.DownloadStatusError, 15*time.Second)
	var after recoveryRecord
	if _, err = store.Get(bucketTaskRecovery, id, &after); err != nil {
		t.Fatal(err)
	}
	if after.Episode != before.Episode || after.TotalUsed < 2 || after.TotalUsed > recoveryMaxTransport || len(d.GetTasks()) != 1 || d.GetTask(id) == nil {
		t.Fatal("restart replaced task/episode or recreated a free initial attempt")
	}
}

func TestRecoveryManagedResolveUsesClosedClassificationWithoutSignedContext(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer source.Close()
	dir := t.TempDir()
	d := operationDownloader(t, NewMemStorage(), dir)
	defer d.Close()
	reported := make(chan error, 1)
	d.Listener(func(event *Event) {
		if event.Key == EventKeyError {
			select {
			case reported <- event.Err:
			default:
			}
		}
	})
	request := operationRequest(strings.Replace(source.URL, "http://", "http://synthetic-user:synthetic-password@", 1)+"/file?token=synthetic-secret", dir)
	result, err := d.CreateBatchOperation(context.Background(), request)
	if err != nil || len(result.TaskIDs) != 1 {
		t.Fatal(result, err)
	}
	select {
	case failure := <-reported:
		var classified *httpProtocol.RecoveryFailure
		if !errors.As(failure, &classified) || strings.Contains(failure.Error(), "synthetic-") || strings.Contains(failure.Error(), source.URL) {
			t.Fatal("managed failure did not retain closed classification and sanitized context")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resolve failure did not terminate task")
	}
}

func TestRecoveryCloseCancelsActiveHTTPWithoutLateSuccess(t *testing.T) {
	ranged := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"close-fixture"`)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", "4194304")
		isRange := r.Header.Get("Range") != ""
		if isRange {
			var start, end int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/4194304", start, end))
			w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
			w.WriteHeader(206)
			select {
			case ranged <- struct{}{}:
			default:
			}
		}
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		if isRange {
			select {
			case canceled <- struct{}{}:
			default:
			}
		}
	}))
	defer source.Close()
	defer source.CloseClientConnections()
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "db")
	d := operationDownloader(t, NewBoltStorage(dbDir), dir)
	defer d.Close()
	var done atomic.Int32
	d.Listener(func(event *Event) {
		if event.Key == EventKeyDone {
			done.Add(1)
		}
	})
	id, err := d.CreateDirect(&base.Request{URL: source.URL}, &base.Options{Path: dir, Name: "close.bin", Extra: &httpModel.OptsExtra{Connections: 1}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ranged:
	case <-time.After(3 * time.Second):
		t.Fatal("active range not reached")
	}
	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	select {
	case err = <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not join active HTTP")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel actual wire IO")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.watchedTasks.Load(id); !ok {
			break
		}
		runtime.Gosched()
	}
	if _, ok := d.watchedTasks.Load(id); ok || done.Load() != 0 {
		t.Fatal("closed run retained waiter or emitted success")
	}
	store := NewBoltStorage(dbDir)
	defer store.Close()
	var task Task
	var record recoveryRecord
	if found, e := store.Get(bucketTask, id, &task); !found || e != nil || task.Status != base.DownloadStatusPause {
		t.Fatal("Close lost paused task", e)
	}
	if found, e := store.Get(bucketTaskRecovery, id, &record); !found || e != nil || record.TaskID != id || record.InitialAttempt.State == "ungranted" {
		t.Fatal("Close lost issued evidence", e)
	}
}
