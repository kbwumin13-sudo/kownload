package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestMCPPublishedCooldownClearsSnapshots(t *testing.T) {
	data := rateLimitPayload(4 << 20)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"same-cooling-representation"`)
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = w.Write(data[:1024])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		n := calls.Add(1)
		if n == 2 {
			w.Header().Set("Retry-After", "8")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if n == 1 {
			start, end := 0, len(data)-1
			if n, _ := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); n < 1 || start < 0 || end < start || end >= len(data) {
				t.Error("unexpected range", r.Header.Get("Range"))
				w.WriteHeader(400)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
			w.WriteHeader(http.StatusPartialContent)
			partial := start + 1024
			if partial > end+1 {
				partial = end + 1
			}
			_, _ = w.Write(data[start:partial])
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		http.ServeContent(w, r, "first.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer source.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "other.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer other.Close()
	d := newRateLimitDownloader(t, 1)
	defer d.Clear()
	dir := t.TempDir()
	first, err := d.CreateDirect(&base.Request{URL: source.URL + "/file"}, &base.Options{Path: dir, Name: "first.bin", Extra: map[string]any{"connections": 1}})
	if err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "real positive HTTP published streams", 4*time.Second, func() bool {
		v, err := d.MCPReadGetStats(context.Background(), first)
		return err == nil && v.Available && v.ActiveStreams != nil && *v.ActiveStreams > 0
	})
	task := d.getTask(first)
	task.statusLock.Lock()
	generation := task.runGeneration
	f := task.fetcher.(*httpProtocol.Fetcher)
	task.statusLock.Unlock()
	second, err := d.CreateDirect(&base.Request{URL: other.URL + "/file"}, &base.Options{Path: t.TempDir(), Name: "second.bin"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, second, base.DownloadStatusWait, time.Second)
	unblock() // Incomplete validated Range forces a retry; that request receives 429.
	_, proxyID, supported := d.protocolController("http").GetRecoveryProxySnapshot(nil)
	if !supported {
		t.Fatal("proxy snapshot unavailable")
	}
	key := transfer.SourceKey("origin|" + source.URL + "|" + proxyID)
	waitForCondition(t, "real 429 accepted and network slot released", 4*time.Second, func() bool {
		return calls.Load() >= 2 && !d.budget.Cooldown(key).IsZero() && d.GetTask(first).Status == base.DownloadStatusWait
	})
	waitStatus(t, d, second, base.DownloadStatusDone, 3*time.Second)
	if time.Until(d.budget.Cooldown(key)) <= 0 {
		t.Fatal("did not observe actual cooling interval")
	}
	assertUnavailable := func() {
		t.Helper()
		stats, err := d.MCPReadGetStats(context.Background(), first)
		if err != nil || stats.Available {
			t.Fatal("cooling task retained streams", stats, err)
		}
		failure, err := d.MCPReadGetErrors(context.Background(), first)
		if err != nil || failure.Available {
			t.Fatal("cooling task retained error snapshot", failure, err)
		}
	}
	assertUnavailable()
	// Protocol sampling and obsolete accepted-result callbacks cannot republish
	// observations for the generation invalidated by the actual deferral.
	_ = f.Progress()
	d.doOnError(task, errors.New("synthetic late old result"), generation)
	d.deferTask(task, &transfer.DeferredError{Until: time.Now().Add(time.Hour)}, generation)
	assertUnavailable()
	if d.GetTask(first).Status != base.DownloadStatusWait {
		t.Fatal("late result changed waiting task")
	}
	waitStatus(t, d, first, base.DownloadStatusDone, 12*time.Second)
	sameContent(t, filepath.Join(dir, "first.bin"), data)
}
