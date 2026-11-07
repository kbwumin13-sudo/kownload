package http

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// expiringLinkServer behaves like a signed download link that expires while
// a download runs: it answers the first allowed range requests and refuses
// every later one (403 unless refuse says otherwise), while responses already
// streaming continue. Ranges that start at slowFrom or later are sent slowly.
type expiringLinkServer struct {
	data       []byte
	allowed    int32
	slowFrom   int64
	refuse     int
	retryAfter string
	ranges     atomic.Int32
	refused    atomic.Int32
}

func (s *expiringLinkServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	delay := 20 * time.Millisecond
	if raw := r.Header.Get("Range"); raw != "" {
		if s.ranges.Add(1) > s.allowed {
			s.refused.Add(1)
			if s.retryAfter != "" {
				w.Header().Set("Retry-After", s.retryAfter)
			}
			w.WriteHeader(cmp.Or(s.refuse, http.StatusForbidden))
			return
		}
		var begin int64
		_, _ = fmt.Sscanf(raw, "bytes=%d-", &begin)
		delay = 5 * time.Millisecond
		if begin >= s.slowFrom {
			delay = 200 * time.Millisecond
		}
	}
	http.ServeContent(&slowWriter{ResponseWriter: w, delay: delay}, r, "payload.bin",
		time.Unix(1700000000, 0), bytes.NewReader(s.data))
}

// cutWriter stops a response after left bytes, so the server closes the
// connection with the body short of its Content-Length.
type cutWriter struct {
	http.ResponseWriter
	left int
}

func (w *cutWriter) Write(p []byte) (int, error) {
	if w.left <= 0 {
		return 0, errors.New("connection cut")
	}
	if len(p) > w.left {
		p = p[:w.left]
	}
	n, err := w.ResponseWriter.Write(p)
	w.left -= n
	return n, err
}

func waitDone(t *testing.T, f *Fetcher, limit time.Duration) error {
	t.Helper()
	select {
	case err := <-f.doneCh:
		return err
	case <-time.After(limit):
		t.Fatal("download did not finish")
		return nil
	}
}

// A stream that finishes early plans to take the tail of a slower one. When
// the link has expired by then, its request is refused; the tail must stay
// with the slower stream, whose open response still delivers it, instead of
// being cut off and handed between streams that are all refused.
func TestRefusedStealLeavesTailWithOpenStream(t *testing.T) {
	data := rangeProbePayload(3 << 19)
	server := &expiringLinkServer{data: data, allowed: 2, slowFrom: int64(len(data) / 2)}
	srv := httptest.NewServer(server)
	defer srv.Close()

	f := buildFetcher()
	opts := &base.Options{Path: t.TempDir(), Name: "payload.bin", Extra: &fhttp.OptsExtra{Connections: 2}}
	if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, opts); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, f, 30*time.Second); err != nil {
		t.Fatalf("download failed after a refused steal: %v", err)
	}
	checkFile(t, f.Meta().SingleFilepath(), data)
	if got := f.Progress().TotalDownloaded(); got != int64(len(data)) {
		t.Fatalf("progress %d, want exactly %d", got, len(data))
	}
	// One refused steal, and no handoffs that would ask the server again.
	if n := server.refused.Load(); n != 1 {
		t.Fatalf("server refused %d requests, want exactly the one steal", n)
	}
	for _, c := range f.Stats().(*fhttp.Stats).Connections {
		if c.Failed {
			t.Fatalf("a connection was left failed: %+v", c)
		}
	}
}

// A planned steal moves nothing until the helper's response is valid: the
// victim keeps its range, no second helper targets it, a stored snapshot has
// disjoint ranges, and the commit refuses a tail the victim has reached.
func TestPlannedStealMovesTheTailOnlyOnCommit(t *testing.T) {
	const size = 8 << 20
	setup := func() (*Fetcher, *connection, *connection) {
		f := buildFetcher()
		f.meta.Res = &base.Resource{Range: true, Size: size}
		victim := &connection{ID: 0, State: connDownloading, Chunk: newChunk(0, size-1)}
		victim.Chunk.Downloaded = 1 << 20
		victim.Downloaded = 1 << 20
		helper := &connection{ID: 1, State: connDownloading, Chunk: newChunk(size, size-1)}
		f.connections = []*connection{victim, helper}
		if !f.helpOtherConnection(helper) || helper.steal == nil {
			t.Fatal("no steal was planned")
		}
		return f, victim, helper
	}

	f, victim, helper := setup()
	begin := helper.steal.begin
	if victim.Chunk.End != size-1 || !victim.stealTarget || helper.Chunk.remain() != 0 {
		t.Fatalf("planning moved the range: victim=%+v helper=%+v", *victim.Chunk, *helper.Chunk)
	}
	other := &connection{ID: 2, State: connDownloading, Chunk: newChunk(size, size-1)}
	f.connections = append(f.connections, other)
	if f.helpOtherConnection(other) {
		t.Fatal("a second helper targeted a stream with a planned steal")
	}
	stored, err := new(FetcherManager).Store(f)
	if err != nil {
		t.Fatal(err)
	}
	covered := int64(0)
	for _, c := range stored.(*fetcherData).Connections {
		covered += c.Chunk.End - c.Chunk.Begin + 1
	}
	if covered != size {
		t.Fatalf("stored ranges cover %d bytes, want %d with no overlap", covered, size)
	}
	if !f.commitSteal(helper) {
		t.Fatal("commit refused a tail far ahead of the victim")
	}
	if victim.Chunk.End != begin-1 || helper.Chunk.Begin != begin || helper.Chunk.End != size-1 ||
		helper.Chunk.Downloaded != 0 || victim.stealTarget || helper.steal != nil {
		t.Fatalf("commit left inconsistent ranges: victim=%+v helper=%+v", *victim.Chunk, *helper.Chunk)
	}

	// While the helper waited for its response the victim came within one
	// read of the tail; exactly one read short of it is still safe.
	inFlight := max(int64(f.tuning().readBuffer), transfer.SmallBuffer)
	for _, gap := range []int64{inFlight - 1, inFlight} {
		f, victim, helper = setup()
		begin = helper.steal.begin
		victim.Chunk.Downloaded = begin - gap
		committed := f.commitSteal(helper)
		if committed != (gap == inFlight) || victim.stealTarget || helper.steal != nil {
			t.Fatalf("gap %d: commit=%t, victim=%+v", gap, committed, *victim.Chunk)
		}
		if !committed && (victim.Chunk.End != size-1 || helper.Chunk.remain() != 0) {
			t.Fatalf("gap %d: a refused commit moved the range: victim=%+v helper=%+v", gap, *victim.Chunk, *helper.Chunk)
		}
	}

	// The victim's range changed meanwhile, for example it was shed.
	f, victim, helper = setup()
	shedEnd := helper.steal.begin + 1024
	victim.Chunk.End = shedEnd
	if f.commitSteal(helper) || victim.Chunk.End != shedEnd || helper.Chunk.remain() != 0 {
		t.Fatalf("commit took a tail from a changed range: victim=%+v helper=%+v", *victim.Chunk, *helper.Chunk)
	}

	// Dropping a plan releases the victim.
	f, victim, helper = setup()
	if !f.dropSteal(helper) || victim.stealTarget || helper.steal != nil || f.dropSteal(helper) {
		t.Fatal("dropping the plan did not release the victim")
	}
}

// A 429 on a steal request still cools the whole source down, the same as on
// any other request, while the slower stream's open response finishes the tail.
func TestRateLimitedStealCoolsTheSourceDown(t *testing.T) {
	data := rangeProbePayload(3 << 19)
	server := &expiringLinkServer{data: data, allowed: 2, slowFrom: int64(len(data) / 2),
		refuse: http.StatusTooManyRequests, retryAfter: "20"}
	srv := httptest.NewServer(server)
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()

	f := newPooledFetcher(pooledController(pool, budget, nil))
	opts := &base.Options{Path: t.TempDir(), Name: "payload.bin", Extra: &fhttp.OptsExtra{Connections: 2}}
	if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, opts); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, f, 30*time.Second); err != nil {
		t.Fatalf("download failed after a rate-limited steal: %v", err)
	}
	checkFile(t, f.Meta().SingleFilepath(), data)
	if n := server.refused.Load(); n != 1 {
		t.Fatalf("server refused %d requests, want exactly the one steal", n)
	}
	if stats := f.Stats().(*fhttp.Stats); stats.RateLimits != 1 {
		t.Fatalf("rate limit not recorded: %d", stats.RateLimits)
	}
	m := budget.Join(f.sourceKey(), 1, false)
	until := m.Snapshot().CooldownUntil
	m.Leave()
	if !until.After(time.Now()) {
		t.Fatalf("the 429 did not cool the source down: until=%v", until)
	}
}

// Shedding hands back unfetched work; a helper still waiting to commit its
// steal holds none, so it is no candidate.
func TestShedSkipsAPlannedSteal(t *testing.T) {
	const size = 8 << 20
	f := buildFetcher()
	f.ctx = context.Background()
	f.meta.Res = &base.Resource{Range: true, Size: size}
	victim := &connection{ID: 0, State: connConnecting, Chunk: newChunk(0, size-1)}
	helper := &connection{ID: 1, State: connDownloading, Chunk: newChunk(size, size-1)}
	f.connections = []*connection{victim, helper}
	if !f.helpOtherConnection(helper) {
		t.Fatal("no steal was planned")
	}
	if f.parkOne() || helper.shedding {
		t.Fatal("shed a stream whose steal is still planned")
	}
}

// A single-stream download of unknown size, with or without declared ranges,
// has no other stream to finish what a 403 refused; reporting it done would
// keep a truncated file.
func TestRefusedSingleStreamOfUnknownSizeIsAnError(t *testing.T) {
	for _, declared := range []bool{false, true} {
		f := buildFetcher()
		f.meta.Res = &base.Resource{Range: declared, Size: 0}
		f.connections = []*connection{{ID: 0, State: connFailed, failed: true, lastErr: NewRequestError(403),
			Chunk: newChunk(0, 0)}}
		f.onDownloadComplete()
		select {
		case err := <-f.doneCh:
			if err == nil || !strings.Contains(err.Error(), "status=403") {
				t.Fatalf("range=%t: refused single stream reported as %v", declared, err)
			}
		default:
			t.Fatalf("range=%t: no completion was reported", declared)
		}
		if f.getState() != stateError {
			t.Fatalf("range=%t: state %d, want error", declared, f.getState())
		}
	}
}

// A steal refused for longer than any wait the downloader honours ends the
// task with the rate limit, as on any other stream, instead of deferring it
// for days.
func TestStealRefusedForDaysFails(t *testing.T) {
	data := rangeProbePayload(3 << 19)
	server := &expiringLinkServer{data: data, allowed: 2, slowFrom: int64(len(data) / 2),
		refuse: http.StatusTooManyRequests, retryAfter: "172800"}
	srv := httptest.NewServer(server)
	defer srv.Close()
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()

	f := newPooledFetcher(pooledController(pool, budget, nil))
	opts := &base.Options{Path: t.TempDir(), Name: "payload.bin", Extra: &fhttp.OptsExtra{Connections: 2}}
	if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, opts); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	err := waitDone(t, f, 30*time.Second)
	if _, deferred := transfer.AsDeferred(err); deferred || err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("steal refused for two days ended as %v, want a rate limit error", err)
	}
}

// When the missing bytes can only come from requests the server refuses, the
// error names the 403. A netdisk task looks for "status=403" to refresh its
// link; "download incomplete" alone hid the cause.
func TestRefusedRemainingRangeReportsStatus(t *testing.T) {
	data := rangeProbePayload(1 << 20)
	var ranges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			http.ServeContent(&slowWriter{ResponseWriter: w, delay: 20 * time.Millisecond}, r, "payload.bin",
				time.Unix(1700000000, 0), bytes.NewReader(data))
			return
		}
		if ranges.Add(1) > 1 {
			// The link expired after the only stream started...
			w.WriteHeader(http.StatusForbidden)
			return
		}
		// ...and that stream breaks off half-way.
		http.ServeContent(&cutWriter{ResponseWriter: w, left: len(data) / 2}, r, "payload.bin",
			time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer srv.Close()

	f := buildFetcher()
	opts := &base.Options{Path: t.TempDir(), Name: "payload.bin", Extra: &fhttp.OptsExtra{Connections: 1}}
	if err := f.Resolve(&base.Request{URL: srv.URL + "/payload.bin"}, opts); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	err := waitDone(t, f, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "status=403") {
		t.Fatalf("refused remaining range reported as %v, want the 403 named", err)
	}
	if got := f.Progress().TotalDownloaded(); got >= int64(len(data)) {
		t.Fatalf("progress %d claims the refused range", got)
	}
}
