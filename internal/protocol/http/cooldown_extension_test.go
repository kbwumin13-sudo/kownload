package http

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// Existing queued work must be reconsidered when another same-source response
// extends a short cooldown past the deferral threshold.
func TestCoolingTransferRechecksExtendedDeadline(t *testing.T) {
	data := rangeProbePayload(2 * 1024 * 1024)
	var refused atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rg := r.Header.Get("Range"); rg != "" && !strings.HasPrefix(rg, "bytes=0-") && refused.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.ServeContent(w, r, "payload.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	b := transfer.NewBudget()
	defer b.Close()
	ctl := pooledController(pool, b, nil)
	var queued atomic.Bool
	queued.Store(true)
	var signalMu sync.Mutex
	signal := make(chan struct{})
	ctl.QueueWaiting = queued.Load
	ctl.QueueChanged = func() <-chan struct{} {
		signalMu.Lock()
		defer signalMu.Unlock()
		return signal
	}
	f := newPooledFetcher(ctl)
	defer f.Close()
	opts := &base.Options{Path: t.TempDir(), Name: "q.bin", Extra: &fhttp.OptsExtra{Connections: 4}}
	if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, opts); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for refused.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no ranged request was refused")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	// Another in-flight same-source request extends the short cooldown,
	// while another origin was already queued before the first refusal.
	until := b.RateLimited(f.sourceKey(), 429, 4*time.Second, true)

	select {
	case err := <-f.doneCh:
		if deferred, ok := transfer.AsDeferred(err); !ok || !deferred.Until.Equal(until) {
			t.Fatalf("the run ended with %v, want a deferral until %v", err, until)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cooldown extended while queue already waits, but running slot was not released")
	}
}
