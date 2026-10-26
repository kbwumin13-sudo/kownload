package http

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func TestTerminalRateLimitOverridesSupervisorDeferral(t *testing.T) {
	for _, retry := range []string{"3", "172800"} {
		t.Run(retry, func(t *testing.T) {
			data := rangeProbePayload(2 * 1024 * 1024)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "" {
					w.Header().Set("Retry-After", retry)
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				http.ServeContent(w, r, "payload.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
			}))
			defer srv.Close()
			pool := transfer.NewTransportPool()
			defer pool.Close()
			budget := transfer.NewBudget()
			defer budget.Close()
			ctl := pooledController(pool, budget, nil)
			f := newPooledFetcher(ctl)
			defer f.Close()
			var checks atomic.Int32
			ctl.QueueWaiting = func() bool {
				if retry == "3" && checks.Add(1) == 1 {
					// Hold the refusal's final classification until the supervisor
					// has consumed its budget notification and cancelled the run.
					select {
					case <-f.ctx.Done():
					case <-time.After(2 * time.Second):
					}
				}
				return true
			}
			if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, &base.Options{Path: t.TempDir(), Name: "q.bin", Extra: &fhttp.OptsExtra{Connections: 4}}); err != nil {
				t.Fatal(err)
			}
			if retry == "3" {
				f.deferrals.Store(maxDeferrals)
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-f.doneCh:
				if _, deferred := transfer.AsDeferred(err); err == nil || deferred {
					t.Fatalf("terminal refusal became %v, want a final error", err)
				}
				if phase := f.phase(0, false); phase != "error" {
					t.Fatalf("terminal refusal phase = %s, want error", phase)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("terminal refusal left task waiting")
			}
		})
	}
}
