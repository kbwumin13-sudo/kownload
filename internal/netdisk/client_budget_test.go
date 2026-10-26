package netdisk

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func budgetedClient(t *testing.T, budget *transfer.Budget, fn adapterTransport) *APIClient {
	t.Helper()
	p := adapterProvider(t, "quark")
	ctl := controller.NewController()
	ctl.Budget = budget
	c := NewAPIClient(ctl, &base.Request{}, p.Info(), adapterSession("quark"))
	c.Client.Transport = fn
	return c
}

func TestAPIRateLimitIsHonouredAndNotReplayed(t *testing.T) {
	budget := transfer.NewBudget()
	defer budget.Close()
	var calls []time.Time
	var mu sync.Mutex
	c := budgetedClient(t, budget, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls = append(calls, time.Now())
		first := len(calls) == 1
		mu.Unlock()
		if first {
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"1"}},
				Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	if _, err := c.Do(context.Background(), "POST", "https://drive-pc.quark.cn/1/clouddrive/file/save", nil, nil); err == nil {
		t.Fatal("a rate-limited call must fail instead of being replayed")
	}
	res, err := c.Do(context.Background(), "GET", "https://drive-pc.quark.cn/1/clouddrive/file/download", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("expected the failed call once and the next call once, got %d calls", len(calls))
	}
	if gap := calls[1].Sub(calls[0]); gap < 900*time.Millisecond {
		t.Fatalf("next API call came %v after a Retry-After of 1s", gap)
	}
}

func TestAPIConcurrencyIsBoundedPerAccount(t *testing.T) {
	budget := transfer.NewBudget()
	defer budget.Close()
	var inflight, peak atomic.Int64
	c := budgetedClient(t, budget, func(r *http.Request) (*http.Response, error) {
		n := inflight.Add(1)
		for {
			m := peak.Load()
			if n <= m || peak.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		inflight.Add(-1)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.Do(context.Background(), "GET", "https://drive-pc.quark.cn/1/clouddrive/file/download", nil, nil)
			if err != nil {
				t.Error(err)
				return
			}
			res.Body.Close()
		}()
	}
	wg.Wait()
	if got := peak.Load(); got > 4 || got < 2 {
		t.Fatalf("peak concurrent API calls %d, want between 2 and 4", got)
	}
}

func TestAPICooldownWaitEndsWithContext(t *testing.T) {
	budget := transfer.NewBudget()
	defer budget.Close()
	var calls atomic.Int64
	c := budgetedClient(t, budget, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": {"60"}},
			Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	_, _ = c.Do(context.Background(), "GET", "https://drive-pc.quark.cn/a", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Do(ctx, "GET", "https://drive-pc.quark.cn/b", nil, nil)
	adapterError(t, err, "rate_limited")
	if time.Since(start) > time.Second || calls.Load() != 1 {
		t.Fatalf("cancelled wait must end promptly without a request: calls=%d", calls.Load())
	}
}
