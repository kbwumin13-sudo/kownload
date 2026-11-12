package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

type coverageOwner struct{ generation atomic.Uint64 }

func (o *coverageOwner) CheckRecovery(t transfer.RecoveryToken) error {
	if t.Generation != o.generation.Load() {
		return transfer.ErrRecoveryStale
	}
	return nil
}
func (o *coverageOwner) RetryRecovery(context.Context, transfer.RecoveryToken, uint64, string, []string) (*transfer.RecoveryPermit, error) {
	return nil, transfer.ErrRecoveryExhausted
}
func (o *coverageOwner) FinishRecovery(_ context.Context, t transfer.RecoveryToken, _ uint64, _ string) error {
	return o.CheckRecovery(t)
}
func coverageLease(o *coverageOwner, g uint64) *transfer.RecoveryLease {
	return &transfer.RecoveryLease{Owner: o, SourceHash: strings.Repeat("b", 64), Token: transfer.RecoveryToken{TaskID: "coverage-task", Episode: "episode", Generation: g, SourceRevision: 1}}
}
func coverageResponse(t *testing.T, l *transfer.RecoveryLease, size int64) *http.Response {
	t.Helper()
	r, e := http.NewRequestWithContext(transfer.WithRecoveryLease(context.Background(), l), "GET", "https://fixture.invalid/file?signature=exact%2Fbytes", nil)
	if e != nil {
		t.Fatal(e)
	}
	return &http.Response{Request: r, StatusCode: 200, ContentLength: size, Header: http.Header{"Etag": {`"fixed"`}}}
}
func coverageFixture(t *testing.T, size int64, prior *transfer.HTTPCoverageState, limit int) (*Fetcher, *coverageOwner, *transfer.RecoveryLease, *coverageResponseProof, *os.File) {
	t.Helper()
	o := &coverageOwner{}
	o.generation.Store(1)
	l := coverageLease(o, 1)
	f := &Fetcher{}
	f.SetRecoveryLease(l)
	if err := f.ObserveRecoveryCoverage(l, prior, limit); err != nil {
		t.Fatal(err)
	}
	p := f.captureCoverageResponse(coverageResponse(t, l, size), size)
	if p == nil {
		t.Fatal("response proof rejected")
	}
	fd, e := os.CreateTemp(t.TempDir(), "output")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { fd.Close() })
	f.file = fd
	return f, o, l, p, fd
}
func coverageCurrentRanges(f *Fetcher) []transfer.HTTPCoverageRange {
	o := f.coverage.Load()
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]transfer.HTTPCoverageRange{}, o.current...)
}

type coveragePrefixWriter struct {
	fd  *os.File
	n   int
	err error
}

func (w coveragePrefixWriter) WriteAt(p []byte, off int64) (int, error) {
	n, e := w.fd.WriteAt(p[:w.n], off)
	if e != nil {
		return n, e
	}
	return n, w.err
}
func TestCoverageActualWritePrefixesPreserveResults(t *testing.T) {
	sentinel := errors.New("target write failure")
	for _, c := range []struct {
		name string
		n    int
		err  error
	}{{"full", 8, nil}, {"partial-error", 3, sentinel}, {"partial-nil", 4, nil}} {
		t.Run(c.name, func(t *testing.T) {
			f, _, _, p, fd := coverageFixture(t, 8, nil, 8)
			n, e := f.writeCoverageAt(coveragePrefixWriter{fd, c.n, c.err}, []byte("abcdefgh"), 0, p)
			if n != c.n || e != c.err {
				t.Fatalf("changed writer result: %d %v", n, e)
			}
			if got := coverageCurrentRanges(f); !reflect.DeepEqual(got, []transfer.HTTPCoverageRange{{Begin: 0, End: int64(c.n)}}) {
				t.Fatal(got)
			}
			b, e := os.ReadFile(fd.Name())
			if e != nil || string(b) != "abcdefgh"[:c.n] {
				t.Fatal("actual prefix differs", e)
			}
		})
	}
}
func TestCoveragePrefetchTargetAndTruncatePreserveSettled(t *testing.T) {
	identity, e := transfer.HTTPCoverageIdentityHash("https://fixture.invalid/file?signature=exact%2Fbytes", `"fixed"`, 8)
	if e != nil {
		t.Fatal(e)
	}
	prior, _ := transfer.NewHTTPCoverageState(identity, 8)
	prior.SettledRanges = []transfer.HTTPCoverageRange{{Begin: 0, End: 8}}
	prior.DurableRanges = []transfer.HTTPCoverageRange{{Begin: 0, End: 8}}
	prior.CheckpointHash = strings.Repeat("d", 64)
	prior.SettlementSequence = 1
	f, _, _, p, fd := coverageFixture(t, 8, &prior, 8)
	if len(coverageCurrentRanges(f)) != 0 {
		t.Fatal("restored unproved durable bytes")
	}
	source, e := os.CreateTemp(t.TempDir(), "prefetch")
	if e != nil {
		t.Fatal(e)
	}
	defer source.Close()
	source.Write([]byte("abcdef-unrecorded-tail"))
	f.prefetchFile = source
	f.prefetchFilePath = source.Name()
	f.prefetchSize.Store(6)
	p.observer.mu.Lock()
	p.observer.resolve = p
	p.observer.mu.Unlock()
	n, e := f.stopPrefetchAndCopyData()
	if n != 6 || e != nil {
		t.Fatal(n, e)
	}
	if got := coverageCurrentRanges(f); !reflect.DeepEqual(got, []transfer.HTTPCoverageRange{{Begin: 0, End: 6}}) {
		t.Fatal(got)
	}
	if e = f.resetSingleStreamAttempt(&connection{Chunk: newChunk(0, 7)}, p); e != nil {
		t.Fatal(e)
	}
	info, _ := fd.Stat()
	if info.Size() != 0 || len(coverageCurrentRanges(f)) != 0 {
		t.Fatal("truncate did not clear current")
	}
	if !reflect.DeepEqual(p.observer.state.SettledRanges, prior.SettledRanges) || p.observer.state.SettlementSequence != 1 {
		t.Fatal("truncate lost settled history")
	}
}
func TestCoverageLimitOnlyDisablesEvidence(t *testing.T) {
	f, _, _, p, fd := coverageFixture(t, 8, nil, 1)
	for _, off := range []int64{0, 2, 4} {
		n, e := f.writeCoverageAt(fd, []byte("x"), off, p)
		if n != 1 || e != nil {
			t.Fatal("limit changed IO", n, e)
		}
	}
	p.observer.mu.Lock()
	failed := errors.Is(p.observer.failure, transfer.ErrHTTPCoverageLimit)
	p.observer.mu.Unlock()
	if !failed {
		t.Fatal("range limit ignored")
	}
	f.sealCoverageBeforeClose(false)
	if s, e := (&FetcherManager{}).StoreRecoveryCoverage(context.Background(), f, p.observer.lease.Token); s != nil || e == nil {
		t.Fatal("disabled observer produced seal")
	}
	b, _ := os.ReadFile(fd.Name())
	if !bytes.Equal(b, []byte{'x', 0, 'x', 0, 'x'}) {
		t.Fatal("download bytes changed", b)
	}
}

type coverageGateWriter struct {
	target           *os.File
	entered, release chan struct{}
}

func (w coverageGateWriter) WriteAt(p []byte, off int64) (int, error) {
	close(w.entered)
	<-w.release
	return w.target.WriteAt(p, off)
}
func TestCoverageOldWriteAndResponseCannotAffectNewObserver(t *testing.T) {
	f, o, l, p, fd := coverageFixture(t, 8, nil, 8)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		n, e := f.writeCoverageAt(coverageGateWriter{fd, entered, release}, []byte("abcd"), 0, p)
		if n != 4 && e == nil {
			e = io.ErrShortWrite
		}
		done <- e
	}()
	<-entered
	// Installation is permitted only before a new target writer is attached.
	f.file = nil
	o.generation.Store(2)
	newLease := coverageLease(o, 2)
	f.SetRecoveryLease(newLease)
	if e := f.ObserveRecoveryCoverage(newLease, nil, 8); e != nil {
		close(release)
		<-done
		t.Fatal(e)
	}
	newProof := f.captureCoverageResponse(coverageResponse(t, newLease, 8), 8)
	if newProof == nil {
		t.Fatal("new proof missing")
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if proof := f.captureCoverageResponse(coverageResponse(t, l, 8), 8); proof != nil {
		t.Fatal("old response authorized evidence")
	}
	if len(coverageCurrentRanges(f)) != 0 || newProof.observer.failure != nil {
		t.Fatal("old write poisoned/rewarded new run")
	}
	if p.observer.sealed != nil || len(p.observer.current) != 0 {
		t.Fatal("late old write recorded")
	}
}
func TestCoverageActualIdentityFailuresDoNotChangeWrites(t *testing.T) {
	for _, name := range []string{"weak-etag", "encoding", "size", "exact-query"} {
		t.Run(name, func(t *testing.T) {
			f, _, l, p, fd := coverageFixture(t, 8, nil, 8)
			r := coverageResponse(t, l, 8)
			switch name {
			case "weak-etag":
				r.Header.Set("ETag", `W/"fixed"`)
			case "encoding":
				r.Header.Set("Content-Encoding", "gzip")
			case "size":
				r.ContentLength = 7
			case "exact-query":
				r.Request.URL.RawQuery = "signature=different"
			}
			if proof := f.captureCoverageResponse(r, 8); proof != nil {
				t.Fatal("unproved identity accepted")
			}
			n, e := f.writeCoverageAt(fd, []byte("abcdefgh"), 0, p)
			if n != 8 || e != nil {
				t.Fatal("identity check changed IO", n, e)
			}
			f.sealCoverageBeforeClose(true)
			if s, e := (&FetcherManager{}).StoreRecoveryCoverage(context.Background(), f, l.Token); s != nil || e == nil {
				t.Fatal("unprovable response sealed")
			}
		})
	}
}

type coverageSyncFailure struct{ *os.File }

func (coverageSyncFailure) Sync() error { return errors.New("sync failed") }
func TestCoverageSameFDSealAndSyncFailure(t *testing.T) {
	for _, name := range []string{"success", "sync-failure", "closed-fd"} {
		t.Run(name, func(t *testing.T) {
			f, _, l, p, fd := coverageFixture(t, 8, nil, 8)
			if e := f.writeDownloadAt([]byte("abcdefgh"), 0, p); e != nil {
				t.Fatal(e)
			}
			var file coverageSyncFile = fd
			if name == "sync-failure" {
				file = coverageSyncFailure{fd}
			}
			if name == "closed-fd" {
				fd.Close()
			}
			s, e := f.syncCoverageCheckpoint(p.observer, file, true)
			if name != "success" {
				if s != nil || e == nil {
					t.Fatal("failed Sync/stat produced a seal")
				}
				return
			}
			if e != nil || s.Token() != l.Token || s.SourceHash() != l.SourceHash || !s.Complete() {
				t.Fatal("invalid seal", e)
			}
			a, e := s.Checkpoint()
			if e != nil {
				t.Fatal(e)
			}
			b, e := s.Checkpoint()
			if e != nil || a == b {
				t.Fatal("checkpoint not owned", e)
			}
			c := s.Coverage()
			c.DurableRanges[0].End = 1
			if s.Coverage().DurableRanges[0].End != 8 {
				t.Fatal("coverage aliases seal")
			}
			f.sealCoverageBeforeClose(true)
			fd.Close()
			f.file = nil
			got, e := (&FetcherManager{}).StoreRecoveryCoverage(context.Background(), f, l.Token)
			if e != nil || !got.Complete() {
				t.Fatal("same-fd preclose seal lost", e)
			}
		})
	}
}
func TestCoverageStoreWaitsCapturedWritersAndCancellation(t *testing.T) {
	f, _, l, p, _ := coverageFixture(t, 8, nil, 8)
	if e := f.writeDownloadAt([]byte("abcdefgh"), 0, p); e != nil {
		t.Fatal(e)
	}
	f.sealCoverageBeforeClose(true)
	loop, prefetch := make(chan struct{}), make(chan struct{})
	f.downloadLoopDone = loop
	f.prefetchDoneCh = prefetch
	base, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waits := make(chan struct{}, 2)
	ctx := observedQuiescenceContext{base, waits}
	done := make(chan error, 1)
	go func() { _, e := (&FetcherManager{}).StoreRecoveryCoverage(ctx, f, l.Token); done <- e }()
	<-waits
	close(loop)
	<-waits
	select {
	case e := <-done:
		t.Fatal("ignored prefetch join", e)
	default:
	}
	close(prefetch)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	canceled, c := context.WithCancel(context.Background())
	c()
	if _, e := (&FetcherManager{}).StoreRecoveryCoverage(canceled, f, l.Token); e != context.Canceled {
		t.Fatal(e)
	}
}
func TestCoverageRealHTTPWritesSealBeforeClose(t *testing.T) {
	for _, mode := range []string{"single", "ranges", "prefetch"} {
		t.Run(mode, func(t *testing.T) {
			payload := bytes.Repeat([]byte("real-coverage-payload"), 4096)
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("ETag", `"real-fixed"`)
				if mode == "single" {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					w.Write(payload)
					return
				}
				http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(payload))
			}))
			defer srv.Close()
			f := buildFetcher()
			defer f.Pause()
			f.ctl.GetRecoveryProxySnapshot = func(p *base.RequestProxy) (func(*http.Request) (*url.URL, error), string, bool) {
				return base.CaptureProxySnapshot(p, nil)
			}
			req := &base.Request{URL: srv.URL + "/file?exact=a%2Fb"}
			snapshot, supported, e := f.CaptureRecoveryContext(req)
			if e != nil || !supported {
				t.Fatal(e)
			}
			owner := &coverageOwner{}
			owner.generation.Store(1)
			lease := coverageLease(owner, 1)
			lease.SourceHash = snapshot.SourceHash()
			f.SetRecoveryContext(snapshot)
			f.SetRecoveryLease(lease)
			wait := f.BindRunWait(context.Background())
			if e = f.ObserveRecoveryCoverage(lease, nil, 64); e != nil {
				t.Fatal(e)
			}
			if mode == "ranges" {
				f.RequireVerifiedOutput()
			}
			opts := &base.Options{Path: t.TempDir(), Name: "coverage.bin", Extra: &fhttp.OptsExtra{Connections: 1}}
			if e = f.ResolveContext(transfer.WithRecoveryLease(context.Background(), lease), req, opts); e != nil {
				t.Fatal(e)
			}
			if mode == "ranges" {
				fd, e := os.Create(f.meta.SingleFilepath())
				if e != nil {
					t.Fatal(e)
				}
				if e = f.SetVerifiedOutput(fd); e != nil {
					fd.Close()
					t.Fatal(e)
				}
			}
			if mode == "prefetch" {
				select {
				case <-f.prefetchDoneCh:
				case <-time.After(3 * time.Second):
					t.Fatal("prefetch did not finish")
				}
			}
			if e = f.Start(); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { done <- wait() }()
			select {
			case e = <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("download failed to terminate")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			seal, e := (&FetcherManager{}).StoreRecoveryCoverage(ctx, f, lease.Token)
			if e != nil || !seal.Complete() {
				t.Fatal("real joined IO did not seal", e)
			}
			want, e := transfer.HTTPCoverageIdentityHash(req.URL, `"real-fixed"`, int64(len(payload)))
			if e != nil || seal.IdentityHash() != want {
				t.Fatal("wrong final URL identity", e)
			}
			ranges := seal.CurrentRanges()
			if !reflect.DeepEqual(ranges, []transfer.HTTPCoverageRange{{Begin: 0, End: int64(len(payload))}}) {
				t.Fatal(ranges)
			}
			f.fileMu.RLock()
			closed := f.file == nil
			f.fileMu.RUnlock()
			if !closed {
				t.Fatal("Wait returned before close")
			}
			b, e := os.ReadFile(f.meta.SingleFilepath())
			if e != nil || !bytes.Equal(b, payload) {
				t.Fatal("body differs", e)
			}
			if mode == "ranges" && requests.Load() < 2 {
				t.Fatal("did not exercise actual range body")
			}
			if mode == "prefetch" && requests.Load() != 1 {
				t.Fatal("full prefetch unexpectedly refetched", requests.Load())
			}
		})
	}
}

func TestCoveragePrefetchShortWriteObservesActualPrefix(t *testing.T) {
	sentinel := errors.New("prefetch target failed")
	for _, returned := range []error{nil, sentinel} {
		name := "short-nil"
		if returned != nil {
			name = "short-error"
		}
		t.Run(name, func(t *testing.T) {
			f, _, _, p, fd := coverageFixture(t, 8, nil, 8)
			target := coverageWriterAt{f, coveragePrefixWriter{fd, 3, returned}, p}
			n, err := copyPrefetchData(target, strings.NewReader("abcdefgh"), 8)
			want := returned
			if want == nil {
				want = io.ErrShortWrite
			}
			if n != 3 || err != want {
				t.Fatal("copy changed actual n/error", n, err)
			}
			if got := coverageCurrentRanges(f); !reflect.DeepEqual(got, []transfer.HTTPCoverageRange{{Begin: 0, End: 3}}) {
				t.Fatal(got)
			}
		})
	}
}

func TestCoveragePauseJoinsLoopSealWithoutCleanBoolean(t *testing.T) {
	for _, invalidated := range []bool{false, true} {
		t.Run(fmt.Sprint(invalidated), func(t *testing.T) {
			payload := bytes.Repeat([]byte("p"), 16384)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
				w.Header().Set("ETag", `"pause-fixed"`)
				w.Write(payload[:4096])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			f := buildFetcher()
			defer f.Pause()
			f.ctl.GetRecoveryProxySnapshot = func(p *base.RequestProxy) (func(*http.Request) (*url.URL, error), string, bool) {
				return base.CaptureProxySnapshot(p, nil)
			}
			req := &base.Request{URL: server.URL + "/partial"}
			snapshot, supported, err := f.CaptureRecoveryContext(req)
			if err != nil || !supported {
				t.Fatal(err)
			}
			owner := &coverageOwner{}
			owner.generation.Store(1)
			lease := coverageLease(owner, 1)
			lease.SourceHash = snapshot.SourceHash()
			f.SetRecoveryContext(snapshot)
			f.SetRecoveryLease(lease)
			f.BindRunWait(context.Background())
			if err = f.ObserveRecoveryCoverage(lease, nil, 8); err != nil {
				t.Fatal(err)
			}
			if err = f.ResolveContext(transfer.WithRecoveryLease(context.Background(), lease), req, &base.Options{Path: t.TempDir(), Name: "paused.bin", Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
				t.Fatal(err)
			}
			if err = f.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for len(coverageCurrentRanges(f)) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("actual target prefix not observed")
				}
				time.Sleep(time.Millisecond)
			}
			if invalidated {
				owner.generation.Store(2)
			}
			if err = f.Pause(); err != nil {
				t.Fatal(err)
			}
			seal, err := (&FetcherManager{}).StoreRecoveryCoverage(context.Background(), f, lease.Token)
			if invalidated {
				if seal != nil || !errors.Is(err, transfer.ErrRecoveryStale) {
					t.Fatal("cancelled owner obtained seal", err)
				}
				return
			}
			if err != nil || seal.Complete() || !reflect.DeepEqual(seal.CurrentRanges(), []transfer.HTTPCoverageRange{{Begin: 0, End: 4096}}) {
				t.Fatal("joined partial prefix not sealed", err)
			}
		})
	}
}
