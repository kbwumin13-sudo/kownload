package download

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func rateLimitPayload(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i/97)
	}
	return data
}

// limitedOrigin refuses its first `refuse` requests with 503 and Retry-After.
type limitedOrigin struct {
	data    []byte
	refuse  atomic.Int32
	retry   string
	mu      sync.Mutex
	arrived []time.Time
}

func (o *limitedOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.arrived = append(o.arrived, time.Now())
	o.mu.Unlock()
	if o.refuse.Add(-1) >= 0 {
		w.Header().Set("Retry-After", o.retry)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(o.data))
}

func newRateLimitDownloader(t *testing.T, maxRunning int) *Downloader {
	t.Helper()
	d := NewDownloader(&DownloaderConfig{StorageDir: t.TempDir(), RefreshInterval: 50,
		FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := d.GetConfig()
	cfg.MaxRunning = maxRunning
	if err := d.PutConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return d
}

func waitStatus(t *testing.T, d *Downloader, id string, want base.Status, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if task := d.GetTask(id); task != nil && task.getStatus() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := base.Status("missing")
	if task := d.GetTask(id); task != nil {
		got = task.getStatus()
	}
	t.Fatalf("task %s status %s, want %s", id, got, want)
}

func sameContent(t *testing.T, path string, data []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("content of %s differs: %v", filepath.Base(path), err)
	}
}

func TestRateLimitedTaskYieldsItsSlotAndResumesAfterCooldown(t *testing.T) {
	data := rateLimitPayload(256 * 1024)
	limited := &limitedOrigin{data: data, retry: "2"}
	limited.refuse.Store(1)
	a := httptest.NewServer(limited)
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer b.Close()
	d := newRateLimitDownloader(t, 1)
	defer d.Clear()
	dir := t.TempDir()
	first, err := d.CreateDirect(&base.Request{URL: a.URL + "/a.bin"}, &base.Options{Path: dir, Name: "a.bin"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.CreateDirect(&base.Request{URL: b.URL + "/b.bin"}, &base.Options{Path: dir, Name: "b.bin"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	// With one slot, the second source finishes while the first waits.
	waitStatus(t, d, second, base.DownloadStatusDone, 3*time.Second)
	if task := d.GetTask(first); task.getStatus() != base.DownloadStatusWait {
		t.Fatalf("rate-limited task should wait in the queue, status %s", task.getStatus())
	}
	waitStatus(t, d, first, base.DownloadStatusDone, 10*time.Second)
	sameContent(t, filepath.Join(dir, "a.bin"), data)
	sameContent(t, filepath.Join(dir, "b.bin"), data)
	limited.mu.Lock()
	defer limited.mu.Unlock()
	if len(limited.arrived) < 2 || limited.arrived[1].Sub(limited.arrived[0]) < 1900*time.Millisecond {
		t.Fatal("the source was contacted again before its Retry-After")
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("deferred task was not restarted promptly after its cooldown")
	}
}

func TestPausedOrDeletedDeferredTasksStayStopped(t *testing.T) {
	data := rateLimitPayload(64 * 1024)
	for _, action := range []string{"pause", "delete"} {
		t.Run(action, func(t *testing.T) {
			limited := &limitedOrigin{data: data, retry: "1"}
			limited.refuse.Store(1)
			srv := httptest.NewServer(limited)
			defer srv.Close()
			d := newRateLimitDownloader(t, 2)
			defer d.Clear()
			dir := t.TempDir()
			id, err := d.CreateDirect(&base.Request{URL: srv.URL + "/f.bin"}, &base.Options{Path: dir, Name: "f.bin"})
			if err != nil {
				t.Fatal(err)
			}
			waitStatus(t, d, id, base.DownloadStatusWait, 3*time.Second)
			switch action {
			case "pause":
				if err := d.Pause(&TaskFilter{IDs: []string{id}}); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := d.Delete(&TaskFilter{IDs: []string{id}}, true); err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(1500 * time.Millisecond)
			limited.mu.Lock()
			requests := len(limited.arrived)
			limited.mu.Unlock()
			if requests != 1 {
				t.Fatalf("a %sd deferred task contacted the source again (%d requests)", action, requests)
			}
			if action == "pause" {
				waitStatus(t, d, id, base.DownloadStatusPause, time.Second)
			} else if d.GetTask(id) != nil {
				t.Fatal("deleted task came back")
			}
		})
	}
}

// trickleOrigin sends its first, unranged response slowly and refuses ranged
// requests with 429 while refusing is set.
type trickleOrigin struct {
	data     []byte
	refusing atomic.Bool
	refused  atomic.Int32
	retry    string
}

func (o *trickleOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Range") != "" {
		if o.refusing.Load() {
			o.refused.Add(1)
			w.Header().Set("Retry-After", o.retry)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.ServeContent(w, r, "a.bin", time.Unix(1700000000, 0), bytes.NewReader(o.data))
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
	w.WriteHeader(http.StatusOK)
	for off := 0; off < len(o.data); off += 16 << 10 {
		if _, err := w.Write(o.data[off:min(off+16<<10, len(o.data))]); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Re-acceptance finding R02 in the transfer phase: a download
// whose ranged requests were refused with a 4 s Retry-After waits with nothing
// queued; when another source's task joins the queue it gives its only slot
// up, the other task completes, and the first resumes after the cooldown and
// completes intact.
func TestRateLimitedTransferYieldsToLaterQueuedTask(t *testing.T) {
	data := rateLimitPayload(4 << 20)
	slow := &trickleOrigin{data: data, retry: "4"}
	slow.refusing.Store(true)
	a := httptest.NewServer(slow)
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer b.Close()
	d := newRateLimitDownloader(t, 1)
	defer d.Clear()
	dir := t.TempDir()
	first, err := d.CreateDirect(&base.Request{URL: a.URL + "/a.bin"}, &base.Options{Path: dir, Name: "a.bin"})
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "a ranged request is refused", 5*time.Second, func() bool { return slow.refused.Load() > 0 })
	refusedAt := time.Now()
	if st := d.GetTask(first).getStatus(); st != base.DownloadStatusRunning {
		t.Fatalf("with nothing queued the refused download should keep its slot, status %s", st)
	}
	second, err := d.CreateDirect(&base.Request{URL: b.URL + "/b.bin"}, &base.Options{Path: dir, Name: "b.bin"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, second, base.DownloadStatusDone, 3*time.Second)
	if st := d.GetTask(first).getStatus(); st != base.DownloadStatusWait {
		t.Fatalf("the cooling download should have given its slot up, status %s", st)
	}
	slow.refusing.Store(false)
	waitStatus(t, d, first, base.DownloadStatusDone, time.Until(refusedAt.Add(4*time.Second))+10*time.Second)
	sameContent(t, filepath.Join(dir, "a.bin"), data)
	sameContent(t, filepath.Join(dir, "b.bin"), data)
}

func waitForCondition(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Fetchers check the wait queue while the downloader may hold its lock and
// wait for them to stop (Continue pauses netdisk tasks synchronously), so the
// checks never take that lock.
func TestQueueChecksDoNotTakeTheDownloaderLock(t *testing.T) {
	d := newRateLimitDownloader(t, 1)
	defer d.Clear()
	d.lock.Lock()
	defer d.lock.Unlock()
	done := make(chan struct{})
	go func() {
		d.queueChanged()
		d.queueWaiting()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a queue check waited for the downloader lock")
	}
}
