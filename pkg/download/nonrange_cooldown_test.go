package download

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestNonRangeResumeLaterQueue(t *testing.T) {
	data := rateLimitPayload(64 << 10)
	var calls atomic.Int32
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 2 {
			w.Header().Set("Retry-After", "8")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			_, _ = w.Write(data[:1024])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = w.Write(data)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "other.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer b.Close()
	d := newRateLimitDownloader(t, 1)
	defer d.Clear()
	targetDir := t.TempDir()
	first, err := d.CreateDirect(&base.Request{URL: a.URL + "/a.bin"}, &base.Options{Path: targetDir, Name: "a.bin", Extra: map[string]any{"connections": 1}})
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "non-range transfer started", 3*time.Second, func() bool {
		x := d.GetTask(first)
		return x != nil && x.Meta.Res != nil && !x.Meta.Res.Range && x.Progress.Downloaded > 0
	})
	if err = d.Pause(&TaskFilter{IDs: []string{first}}); err != nil {
		t.Fatal(err)
	}
	task := d.getTask(first)
	task.statusLock.Lock()
	paused := task.pauseDone
	task.statusLock.Unlock()
	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatal("pause did not finish")
	}
	if err = d.Continue(&TaskFilter{IDs: []string{first}}); err != nil {
		t.Fatal(err)
	}
	key := transfer.SourceKey("origin|" + a.URL + "|direct")
	waitForCondition(t, "resumed GET refused and cooldown entered", 3*time.Second, func() bool {
		return calls.Load() >= 2 && !d.budget.Cooldown(key).IsZero()
	})
	second, err := d.CreateDirect(&base.Request{URL: b.URL + "/other.bin"}, &base.Options{Path: t.TempDir(), Name: "other.bin"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	one, two := d.GetTask(first), d.GetTask(second)
	if one.Status != base.DownloadStatusWait || two.Status != base.DownloadStatusDone {
		t.Fatalf("non-range resumed cooldown holds slot: first=%s other=%s calls=%d cooldown left=%v", one.Status, two.Status, calls.Load(), time.Until(d.budget.Cooldown(key)))
	}

	waitForCondition(t, "deferred non-range task completed", 12*time.Second, func() bool { return d.GetTask(first).Status == base.DownloadStatusDone })
	got, err := os.ReadFile(filepath.Join(targetDir, "a.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("resumed file differs from source")
	}
}
