package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/netdisk/mailru"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// All named HTTPS hosts terminate at a private TLS server through this local
// CONNECT proxy. No DNS lookup or public-site request is made by these tests.
func publicLinkProxy(t *testing.T, handler http.Handler) *url.URL {
	t.Helper()
	target := httptest.NewTLSServer(handler)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(400)
			return
		}
		conn, err := net.Dial("tcp", target.Listener.Addr().String())
		if err != nil {
			w.WriteHeader(502)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			conn.Close()
			return
		}
		fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { defer client.Close(); defer conn.Close(); io.Copy(conn, client) }()
		go func() { defer client.Close(); defer conn.Close(); io.Copy(client, conn) }()
	}))
	t.Cleanup(func() { proxy.Close(); target.Close() })
	u, _ := url.Parse(proxy.URL)
	return u
}

func TestPublicLinkRefreshPreservesRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name                                      string
		changed, refuseAll, pauseRefresh, restore bool
	}{
		{name: "stable resource"},
		{name: "pause store restore", restore: true},
		{name: "changed resource", changed: true},
		{name: "refresh exhausted", refuseAll: true},
		{name: "pause during refresh", pauseRefresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("A"), 2<<20)
			var dispatches, oldRanges, newRanges atomic.Int32
			refreshEntered := make(chan struct{})
			proxy := publicLinkProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v2/dispatcher" {
					n := dispatches.Add(1)
					if n == 2 && tc.pauseRefresh {
						close(refreshEntered)
						<-r.Context().Done()
						return
					}
					fmt.Fprint(w, `{"body":{"weblink_get":[{"url":"https://cloclo1.cloud.mail.ru/public/synthetic/g/no"}]}}`)
					return
				}
				if strings.HasPrefix(r.URL.Path, "/public/synthetic/g/no/") {
					w.Header().Set("Location", fmt.Sprintf("https://cloclo2.datacloudmail.ru/public/get/synthetic/%d/file", dispatches.Load()))
					w.WriteHeader(301)
					return
				}
				if !strings.HasPrefix(r.URL.Path, "/public/get/synthetic/") {
					w.WriteHeader(404)
					return
				}
				old := strings.Contains(r.URL.Path, "/synthetic/1/")
				begin, end := 0, len(payload)-1
				ranged := r.Header.Get("Range") != ""
				limit := len(payload)
				if ranged {
					if old {
						if oldRanges.Add(1) > 1 || tc.refuseAll {
							w.WriteHeader(403)
							return
						}
						limit = 128 << 10
					} else {
						newRanges.Add(1)
						if tc.refuseAll {
							w.WriteHeader(403)
							return
						}
					}
					if r.Header.Get("If-Range") != `"stable"` {
						t.Error("range lost persisted validator")
					}
					fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end)
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
				}
				etag := `"stable"`
				body := payload
				if !old && tc.changed {
					etag = `"changed"`
					body = bytes.Repeat([]byte("B"), len(payload))
				}
				w.Header().Set("ETag", etag)
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
				if ranged {
					w.WriteHeader(206)
				}
				for n := begin; n <= end; {
					if n-begin >= limit {
						panic(http.ErrAbortHandler)
					}
					last := min(n+4096, end+1, begin+limit)
					if _, err := w.Write(body[n:last]); err != nil {
						return
					}
					w.(http.Flusher).Flush()
					n = last
					time.Sleep(time.Millisecond)
				}
			}))
			f := buildFetcher()
			f.ctl.GetProxy = func(*base.RequestProxy) func(*http.Request) (*url.URL, error) { return http.ProxyURL(proxy) }
			defer func() { f.Close() }()
			if err := f.Resolve(&base.Request{URL: "https://cloud.mail.ru/public/aaaa/bbbb", SkipVerifyCert: true}, &base.Options{Path: t.TempDir(), Name: "synthetic.bin", Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
				t.Fatal(err)
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			if tc.restore {
				deadline := time.Now().Add(3 * time.Second)
				for f.Progress().TotalDownloaded() < 32<<10 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if err := f.Pause(); err != nil {
					t.Fatal(err)
				}
				data, err := (&FetcherManager{}).Store(f)
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(data)
				value, restore := (&FetcherManager{}).Restore()
				if err := json.Unmarshal(encoded, value); err != nil {
					t.Fatal(err)
				}
				ctl := f.ctl
				meta := f.Meta()
				f.Close()
				f = restore(meta, value).(*Fetcher)
				f.Setup(ctl)
				if err := f.Start(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.pauseRefresh {
				select {
				case <-refreshEntered:
				case <-time.After(5 * time.Second):
					t.Fatal("refresh did not start")
				}
				started := time.Now()
				stored := make(chan error, 1)
				go func() { _, err := (&FetcherManager{}).Store(f); stored <- err }()
				select {
				case err := <-stored:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("Store waited for an in-flight network refresh")
				}
				if err := f.Pause(); err != nil {
					t.Fatal(err)
				}
				if time.Since(started) > time.Second {
					t.Fatal("Pause did not cancel public resolution")
				}
				return
			}
			err := waitDone(t, f, 8*time.Second)
			got, readErr := os.ReadFile(f.Meta().SingleFilepath())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.changed {
				if err == nil || bytes.Contains(got, []byte("B")) {
					t.Fatalf("changed resource wrote old file: %v", err)
				}
			} else if tc.refuseAll {
				if err == nil || !strings.Contains(err.Error(), "status=403") {
					t.Fatalf("refresh exhaustion lost refusal: %v", err)
				}
			} else if err != nil || !bytes.Equal(got, payload) || f.Progress().TotalDownloaded() != int64(len(payload)) {
				t.Fatalf("refreshed bytes=%d progress=%d err=%v", len(got), f.Progress().TotalDownloaded(), err)
			}
			if dispatches.Load() != 2 {
				t.Fatalf("source resolved %d times, want initial and one refresh", dispatches.Load())
			}
			if !tc.refuseAll && newRanges.Load() == 0 {
				t.Fatal("no resumed range reached fresh link")
			}
			stored, storeErr := (&FetcherManager{}).Store(f)
			if storeErr != nil || stored.(*fetcherData).PublicSource == nil || stored.(*fetcherData).PublicSource.ShareID != "aaaa/bbbb" {
				t.Fatal("public source identity was not persisted")
			}
		})
	}
}

// A public link refreshed during one run expires while the task is paused.
// Without a validator, continuing compares saved bytes with the server first;
// that request belongs to the new run, which may refresh the link once more.
func TestPublicLinkContinueRefreshesWithoutValidator(t *testing.T) {
	payload := rangeProbePayload(4 << 20)
	var dispatches, validGen atomic.Int32
	validGen.Store(1)
	proxy := publicLinkProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/dispatcher" {
			dispatches.Add(1)
			fmt.Fprint(w, `{"body":{"weblink_get":[{"url":"https://cloclo1.cloud.mail.ru/public/synthetic/g/no"}]}}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/public/synthetic/g/no/") {
			w.Header().Set("Location", fmt.Sprintf("https://cloclo2.datacloudmail.ru/public/get/synthetic/%d/file", dispatches.Load()))
			w.WriteHeader(301)
			return
		}
		var gen int32
		if _, err := fmt.Sscanf(r.URL.Path, "/public/get/synthetic/%d/file", &gen); err != nil {
			w.WriteHeader(404)
			return
		}
		// An open transfer keeps going; a new request to an old link is refused.
		if gen < validGen.Load() {
			w.WriteHeader(403)
			return
		}
		begin, end := 0, len(payload)-1
		if v := r.Header.Get("Range"); v != "" {
			fmt.Sscanf(v, "bytes=%d-%d", &begin, &end)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
		if r.Header.Get("Range") != "" {
			w.WriteHeader(206)
		}
		for off := begin; off <= end; {
			last := min(off+4096, end+1)
			if _, err := w.Write(payload[off:last]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			off = last
			time.Sleep(time.Millisecond)
		}
	}))
	f := buildFetcher()
	f.ctl.GetProxy = func(*base.RequestProxy) func(*http.Request) (*url.URL, error) { return http.ProxyURL(proxy) }
	defer func() { f.Close() }()
	if err := f.Resolve(&base.Request{URL: "https://cloud.mail.ru/public/aaaa/bbbb", SkipVerifyCert: true}, &base.Options{Path: t.TempDir(), Name: "synthetic.bin", Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
		t.Fatal(err)
	}
	if f.hasResumeValidator() {
		t.Fatal("fixture unexpectedly has a validator")
	}
	validGen.Store(2) // the resolved link refuses new requests: run 1 refreshes it
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.Progress().TotalDownloaded() < 512<<10 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := f.Pause(); err != nil {
		t.Fatal(err)
	}
	if paused := f.Progress().TotalDownloaded(); paused <= 0 || paused >= int64(len(payload)) || dispatches.Load() != 2 {
		t.Fatalf("fixture did not pause after one refresh: progress=%d dispatches=%d", paused, dispatches.Load())
	}
	validGen.Store(3) // the refreshed link expired while paused
	err := f.Start()
	if err == nil {
		err = waitDone(t, f, 20*time.Second)
	}
	if err != nil {
		t.Fatalf("same-process continue could not refresh the expired link: %v (dispatches=%d)", err, dispatches.Load())
	}
	got, _ := os.ReadFile(f.Meta().SingleFilepath())
	if !bytes.Equal(got, payload) || dispatches.Load() != 3 {
		t.Fatalf("continued download: equal=%t dispatches=%d", bytes.Equal(got, payload), dispatches.Load())
	}
}

// mailruLinks is a Mail.ru fixture whose dispatcher hands out numbered direct
// links: link n is the one issued by the n-th dispatcher call (plus first).
// refuse decides which new requests a link turns away; an open transfer is
// never cut by it.
type mailruLinks struct {
	payload    []byte
	first      int32         // number of the link issued by the first call
	delay      time.Duration // dispatcher latency
	refuse     func(link int32, r *http.Request) bool
	dispatches atomic.Int32
	mu         sync.Mutex
	at         []time.Time // dispatcher calls
}

func (m *mailruLinks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v2/dispatcher" {
		time.Sleep(m.delay)
		m.mu.Lock()
		m.at = append(m.at, time.Now())
		m.mu.Unlock()
		m.dispatches.Add(1)
		fmt.Fprint(w, `{"body":{"weblink_get":[{"url":"https://cloclo1.cloud.mail.ru/public/synthetic/g/no"}]}}`)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/public/synthetic/g/no/") {
		w.Header().Set("Location", fmt.Sprintf("https://cloclo2.datacloudmail.ru/public/get/synthetic/%d/file", m.first+m.dispatches.Load()-1))
		w.WriteHeader(301)
		return
	}
	var link int32
	if _, err := fmt.Sscanf(r.URL.Path, "/public/get/synthetic/%d/file", &link); err != nil {
		w.WriteHeader(404)
		return
	}
	if m.refuse != nil && m.refuse(link, r) {
		w.WriteHeader(403)
		return
	}
	begin, end := 0, len(m.payload)-1
	if v := r.Header.Get("Range"); v != "" {
		fmt.Sscanf(v, "bytes=%d-%d", &begin, &end)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(m.payload)))
	}
	w.Header().Set("ETag", `"stable"`)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
	if r.Header.Get("Range") != "" {
		w.WriteHeader(206)
	}
	for off := begin; off <= end; {
		last := min(off+16<<10, end+1)
		if _, err := w.Write(m.payload[off:last]); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		off = last
	}
}

func (m *mailruLinks) calls() []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Time(nil), m.at...)
}

// restoredPublicLink restores a Mail.ru share task with a strong ETag, as
// after a restart: saved is the direct link it last used, and each
// connection's saved bytes are on disk.
func restoredPublicLink(t *testing.T, proxy *url.URL, payload []byte, conns []*connection, saved string) *Fetcher {
	t.Helper()
	req := &base.Request{URL: "https://cloud.mail.ru/public/aaaa/bbbb", Extra: &fhttp.ReqExtra{}, SkipVerifyCert: true}
	opts := &base.Options{Path: t.TempDir(), Name: "synthetic.bin", Extra: &fhttp.OptsExtra{Connections: len(conns)}}
	meta := &fetcher.FetcherMeta{Req: req, Opts: opts, Res: &base.Resource{Range: true, Size: int64(len(payload)),
		Files: []*base.FileInfo{{Name: "synthetic.bin", Size: int64(len(payload))}}}}
	local := make([]byte, len(payload))
	for _, c := range conns {
		copy(local[c.Chunk.Begin:], payload[c.Chunk.Begin:c.Chunk.Begin+c.Chunk.Downloaded])
	}
	if err := os.WriteFile(meta.SingleFilepath(), local, 0600); err != nil {
		t.Fatal(err)
	}
	identity := representation{ETag: `"stable"`, Size: int64(len(payload)), ResourceKey: requestResourceKey(req), QueryKey: requestQueryKey(req)}
	encoded, _ := json.Marshal(&fetcherData{PublicSource: &mailru.Source{ShareID: "aaaa/bbbb"}, Representation: &identity,
		Connections: conns, RedirectURL: saved})
	value, restore := (&FetcherManager{}).Restore()
	if err := json.Unmarshal(encoded, value); err != nil {
		t.Fatal(err)
	}
	f := restore(meta, value).(*Fetcher)
	ctl := buildFetcher().ctl
	ctl.GetProxy = func(*base.RequestProxy) func(*http.Request) (*url.URL, error) { return http.ProxyURL(proxy) }
	f.Setup(ctl)
	t.Cleanup(func() { f.Close() })
	return f
}

// Within one run the link expires twice, each time some minutes (here: the
// shortened interval) after it was issued. The run renews it both times on its
// own and finishes with the right bytes, without a manual continue.
func TestPublicLinkRefreshesAgainInOneRun(t *testing.T) {
	// Large enough not to wait for a small file's first response to finish.
	payload := rangeProbePayload(1152 << 10)
	var mu sync.Mutex
	expired := map[int32]bool{}
	links := &mailruLinks{payload: payload, first: 1}
	links.refuse = func(link int32, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		return expired[link]
	}
	cutting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/public/get/") {
			links.ServeHTTP(w, r)
			return
		}
		var link int32
		fmt.Sscanf(r.URL.Path, "/public/get/synthetic/%d/file", &link)
		mu.Lock()
		refused := expired[link]
		mu.Unlock()
		if refused {
			w.WriteHeader(403)
			return
		}
		if r.Header.Get("Range") == "" {
			// The first response gives a little, then waits for Start to stop it.
			w.Header().Set("ETag", `"stable"`)
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.Write(payload[:4<<10])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		// A range response breaks off after 448 KiB; its link takes no new
		// request after that, as an expired Mail.ru link.
		var begin, end int
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end)
		w.Header().Set("ETag", `"stable"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
		w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
		w.WriteHeader(206)
		for off := begin; off <= end; {
			if off-begin >= 448<<10 {
				mu.Lock()
				expired[link] = true
				mu.Unlock()
				panic(http.ErrAbortHandler)
			}
			last := min(off+16<<10, end+1)
			if _, err := w.Write(payload[off:last]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			off = last
		}
	})
	proxy := publicLinkProxy(t, cutting)
	f := buildFetcher()
	f.ctl.GetProxy = func(*base.RequestProxy) func(*http.Request) (*url.URL, error) { return http.ProxyURL(proxy) }
	f.publicRefreshGap = 500 * time.Millisecond
	defer func() { f.Close() }()
	if err := f.Resolve(&base.Request{URL: "https://cloud.mail.ru/public/aaaa/bbbb", SkipVerifyCert: true}, &base.Options{Path: t.TempDir(), Name: "synthetic.bin", Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, f, 30*time.Second); err != nil {
		t.Fatalf("run could not renew its link again: %v (dispatches=%d)", err, links.dispatches.Load())
	}
	if got, _ := os.ReadFile(f.Meta().SingleFilepath()); !bytes.Equal(got, payload) {
		t.Fatal("renewed download bytes differ")
	}
	calls := links.calls()
	if len(calls) != 3 {
		t.Fatalf("dispatcher calls=%d, want the resolve and two renewals in one run", len(calls))
	}
	if gap := calls[2].Sub(calls[1]); gap < f.publicRefreshGap {
		t.Fatalf("renewals %v apart, closer than the interval", gap)
	}
}

// Four restored streams find the saved link expired at the same time. They
// share one renewal instead of calling the dispatcher each.
func TestPublicLinkConcurrentRefusalsShareOneRefresh(t *testing.T) {
	const KiB = 1 << 10
	payload := rangeProbePayload(4 << 20)
	links := &mailruLinks{payload: payload, first: 2, delay: 200 * time.Millisecond,
		refuse: func(link int32, r *http.Request) bool { return link < 2 }}
	proxy := publicLinkProxy(t, links)
	var conns []*connection
	for i := 0; i < 4; i++ {
		begin := int64(i) << 20
		conns = append(conns, &connection{ID: i, Role: roleWorker, Chunk: &chunk{Begin: begin, End: begin + 1<<20 - 1, Downloaded: 128 * KiB}, Downloaded: 128 * KiB})
	}
	conns[0].Role = rolePrimary
	f := restoredPublicLink(t, proxy, payload, conns, "https://cloclo2.datacloudmail.ru/public/get/synthetic/1/file")
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, f, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	checkFile(t, f.Meta().SingleFilepath(), payload)
	if n := links.dispatches.Load(); n != 1 {
		t.Fatalf("dispatcher calls=%d, want one shared renewal", n)
	}
}

// The admission rules themselves: a stream refused on a link already renewed
// uses the new one, a fresh link refused within the interval is not renewed
// again, the interval allows the next renewal, and the run's limit ends them.
func TestPublicLinkRefreshAdmission(t *testing.T) {
	links := &mailruLinks{payload: rangeProbePayload(1 << 10), first: 2, delay: 100 * time.Millisecond}
	proxy := publicLinkProxy(t, links)
	f := restoredPublicLink(t, proxy, links.payload, []*connection{{ID: 0, Role: rolePrimary, Chunk: &chunk{Begin: 0, End: 1<<10 - 1}}},
		"https://cloclo2.datacloudmail.ru/public/get/synthetic/1/file")
	f.publicRefreshLimit = 3
	link := func(n int) string {
		return fmt.Sprintf("https://cloclo2.datacloudmail.ru/public/get/synthetic/%d/file", n)
	}
	current := func() string {
		f.redirectLock.Lock()
		defer f.redirectLock.Unlock()
		return f.redirectURL
	}
	ctx := context.Background()
	// Eight streams refused on link 1 together: one renewal.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.refreshPublicLink(ctx, link(1)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if links.dispatches.Load() != 1 || current() != link(2) {
		t.Fatalf("concurrent refusals: dispatches=%d link=%s", links.dispatches.Load(), current())
	}
	// Link 2 is refused again right away: not renewed within the interval.
	if err := f.refreshPublicLink(ctx, link(2)); err == nil || links.dispatches.Load() != 1 {
		t.Fatalf("renewed again within the interval: %v dispatches=%d", err, links.dispatches.Load())
	}
	// Once the interval has passed, the next refusal renews it.
	f.publicMu.Lock()
	f.publicRefreshedAt = time.Now().Add(-publicRefreshInterval)
	f.publicMu.Unlock()
	if err := f.refreshPublicLink(ctx, link(2)); err != nil || links.dispatches.Load() != 2 || current() != link(3) {
		t.Fatalf("after the interval: %v dispatches=%d link=%s", err, links.dispatches.Load(), current())
	}
	f.publicMu.Lock()
	f.publicRefreshedAt = time.Now().Add(-publicRefreshInterval)
	f.publicMu.Unlock()
	if err := f.refreshPublicLink(ctx, link(3)); err != nil || links.dispatches.Load() != 3 {
		t.Fatalf("third renewal: %v dispatches=%d", err, links.dispatches.Load())
	}
	// The limit of three is reached, whatever the interval.
	f.publicMu.Lock()
	f.publicRefreshedAt = time.Now().Add(-publicRefreshInterval)
	f.publicMu.Unlock()
	if err := f.refreshPublicLink(ctx, link(4)); err == nil || !strings.Contains(err.Error(), "exhausted") || links.dispatches.Load() != 3 {
		t.Fatalf("beyond the limit: %v dispatches=%d", err, links.dispatches.Load())
	}
}

// Every renewed link is refused too. Renewals stop at the run's limit and the
// run ends as before, with status=403 for the wrapper's refresh contract.
func TestPublicLinkRefreshLimitEndsWithRefusal(t *testing.T) {
	payload := rangeProbePayload(1 << 20)
	links := &mailruLinks{payload: payload, first: 2, refuse: func(int32, *http.Request) bool { return true }}
	proxy := publicLinkProxy(t, links)
	conns := []*connection{{ID: 0, Role: rolePrimary, Chunk: &chunk{Begin: 0, End: 1<<20 - 1, Downloaded: 64 << 10}, Downloaded: 64 << 10}}
	f := restoredPublicLink(t, proxy, payload, conns, "https://cloclo2.datacloudmail.ru/public/get/synthetic/1/file")
	f.publicRefreshGap, f.publicRefreshLimit = time.Nanosecond, 2
	before, _ := os.ReadFile(f.Meta().SingleFilepath())
	err := f.Start()
	if err == nil {
		err = waitDone(t, f, 30*time.Second)
	}
	if err == nil || !strings.Contains(err.Error(), "status=403") || !IsExpiredLinkError(err) {
		t.Fatalf("exhausted renewals: %v", err)
	}
	if n := links.dispatches.Load(); n != 2 {
		t.Fatalf("dispatcher calls=%d, want the limit of 2", n)
	}
	if after, _ := os.ReadFile(f.Meta().SingleFilepath()); !bytes.Equal(before, after) {
		t.Fatal("refused run changed the file")
	}
}
