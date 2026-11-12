package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
)

// RecoveryCoverageCheckpoint is an owned observation, never an IO grant or a
// settlement receipt. Only the joined, same-fd Sync path can construct one.
type RecoveryCoverageCheckpoint struct {
	token      transfer.RecoveryToken
	source     string
	coverage   transfer.HTTPCoverageState
	checkpoint []byte
	complete   bool
}

func (s *RecoveryCoverageCheckpoint) Token() transfer.RecoveryToken { return s.token }
func (s *RecoveryCoverageCheckpoint) SourceHash() string            { return s.source }
func (s *RecoveryCoverageCheckpoint) IdentityHash() string          { return s.coverage.IdentityHash }
func (s *RecoveryCoverageCheckpoint) Complete() bool                { return s.complete }
func (s *RecoveryCoverageCheckpoint) Coverage() transfer.HTTPCoverageState {
	return cloneCoverage(s.coverage)
}
func (s *RecoveryCoverageCheckpoint) CurrentRanges() []transfer.HTTPCoverageRange {
	return append(make([]transfer.HTTPCoverageRange, 0, len(s.coverage.DurableRanges)), s.coverage.DurableRanges...)
}
func (s *RecoveryCoverageCheckpoint) Checkpoint() (any, error) {
	var value fetcherData
	decoder := json.NewDecoder(bytes.NewReader(s.checkpoint))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, transfer.ErrHTTPCoverageUnprovable
	}
	return &value, nil
}
func cloneCoverage(s transfer.HTTPCoverageState) transfer.HTTPCoverageState {
	s.DurableRanges = append(make([]transfer.HTTPCoverageRange, 0, len(s.DurableRanges)), s.DurableRanges...)
	s.SettledRanges = append(make([]transfer.HTTPCoverageRange, 0, len(s.SettledRanges)), s.SettledRanges...)
	return s
}

type httpCoverageObserver struct {
	lease   transfer.RecoveryLease // immutable copy; it cannot issue new permits
	limit   int
	mu      sync.Mutex
	state   *transfer.HTTPCoverageState
	current []transfer.HTTPCoverageRange
	failure error
	resolve *coverageResponseProof
	sealed  *RecoveryCoverageCheckpoint
}
type coverageResponseProof struct {
	observer *httpCoverageObserver
	identity string
}

// ObserveRecoveryCoverage must be installed at a quiescent boundary before any
// target byte. Prior DurableRanges are deliberately not trusted as current IO:
// output/checkpoint continuity is a separate Core obligation. No caller is wired
// yet; nil retains all existing HTTP/S4 behavior.
func (f *Fetcher) ObserveRecoveryCoverage(lease *transfer.RecoveryLease, prior *transfer.HTTPCoverageState, limit int) error {
	if lease == nil || lease.Owner == nil || !transfer.ValidExecutionHash(lease.SourceHash) || lease.Token.TaskID == "" || lease.Token.Episode == "" || lease.Token.Generation == 0 || lease.Token.SourceRevision == 0 || limit <= 0 {
		return transfer.ErrHTTPCoverageInvalid
	}
	if err := lease.Check(); err != nil {
		return err
	}
	run := f.recovery.Load()
	if run == nil || run.lease == nil || run.lease.Token != lease.Token || run.lease.SourceHash != lease.SourceHash {
		return transfer.ErrRecoveryStale
	}
	f.fileMu.RLock()
	active := f.file != nil
	f.fileMu.RUnlock()
	if active {
		return transfer.ErrHTTPCoverageUnprovable
	}
	observer := &httpCoverageObserver{lease: *lease, limit: limit, current: make([]transfer.HTTPCoverageRange, 0)}
	if prior != nil {
		if err := transfer.ValidateHTTPCoverageState(*prior, limit); err != nil {
			return err
		}
		owned := cloneCoverage(*prior)
		observer.state = &owned
		if prior.Status != "tracking" {
			observer.failure = transfer.ErrHTTPCoverageUnprovable
		}
	}
	f.coverage.Store(observer)
	return nil
}
func (f *Fetcher) coverageCurrent(o *httpCoverageObserver) bool {
	if o == nil || f.coverage.Load() != o {
		return false
	}
	run := f.recovery.Load()
	return run != nil && run.lease != nil && run.lease.Token == o.lease.Token && run.lease.SourceHash == o.lease.SourceHash && o.lease.Check() == nil
}
func (o *httpCoverageObserver) disable(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failure == nil {
		o.failure = err
	}
	o.sealed = nil
}

// Proofs belong to the actual response, not merely a resource name or a request
// URL stripped of signed parameters. Old responses cannot poison a newer run.
func (f *Fetcher) captureCoverageResponse(resp *http.Response, size int64) *coverageResponseProof {
	o := f.coverage.Load()
	if !f.coverageCurrent(o) {
		return nil
	}
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		o.disable(transfer.ErrHTTPCoverageUnprovable)
		return nil
	}
	lease := transfer.RecoveryLeaseFromContext(resp.Request.Context())
	if lease == nil || lease.Token != o.lease.Token || lease.SourceHash != o.lease.SourceHash {
		return nil
	}
	encoding := resp.Header.Get("Content-Encoding")
	validSize := resp.ContentLength == size
	if resp.StatusCode == http.StatusPartialContent {
		_, _, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		validSize = ok && total == size
	}
	identity, err := transfer.HTTPCoverageIdentityHash(resp.Request.URL.String(), resp.Header.Get("ETag"), size)
	if err != nil || resp.Uncompressed || encoding != "" && !strings.EqualFold(encoding, "identity") || !validSize || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent) {
		o.disable(transfer.ErrHTTPCoverageUnprovable)
		return nil
	}
	if !f.coverageCurrent(o) {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failure != nil {
		return nil
	}
	if o.state == nil {
		s, e := transfer.NewHTTPCoverageState(identity, size)
		if e != nil {
			o.failure = e
			return nil
		}
		o.state = &s
	}
	if o.state.IdentityHash != identity || o.state.Size != size {
		o.failure = transfer.ErrHTTPCoverageUnprovable
		o.sealed = nil
		return nil
	}
	return &coverageResponseProof{o, identity}
}
func (f *Fetcher) rememberCoverageResolve(resp *http.Response, size int64) {
	proof := f.captureCoverageResponse(resp, size)
	if proof == nil {
		return
	}
	proof.observer.mu.Lock()
	proof.observer.resolve = proof
	proof.observer.mu.Unlock()
}
func (f *Fetcher) resolveCoverageProof() *coverageResponseProof {
	o := f.coverage.Load()
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.resolve
}
func (f *Fetcher) coverageProofCurrent(p *coverageResponseProof) bool {
	return p != nil && f.coverageCurrent(p.observer)
}
func (f *Fetcher) recordCoverageWrite(p *coverageResponseProof, offset int64, n int) {
	if !f.coverageProofCurrent(p) {
		return
	}
	o := p.observer
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failure != nil || o.state == nil || o.state.IdentityHash != p.identity {
		return
	}
	if o.sealed != nil {
		o.failure = transfer.ErrHTTPCoverageUnprovable
		o.sealed = nil
		return
	}
	next, err := transfer.MergeHTTPCoverageWrite(o.current, o.state.Size, offset, int64(n), o.limit)
	if err != nil {
		o.failure = err
		return
	}
	o.current = next
}

// Observer errors never replace the target writer's actual n/err, including
// n>0 with an error. This wrapper also covers writes made by prefetch copying.
func (f *Fetcher) writeCoverageAt(target io.WriterAt, data []byte, offset int64, p *coverageResponseProof) (int, error) {
	valid := f.coverageProofCurrent(p)
	n, err := target.WriteAt(data, offset)
	if valid && n > 0 {
		f.recordCoverageWrite(p, offset, n)
	}
	return n, err
}

type coverageWriterAt struct {
	f      *Fetcher
	target io.WriterAt
	proof  *coverageResponseProof
}

func (w coverageWriterAt) WriteAt(data []byte, offset int64) (int, error) {
	return w.f.writeCoverageAt(w.target, data, offset, w.proof)
}
func (f *Fetcher) resetCoverage(p *coverageResponseProof) {
	if !f.coverageProofCurrent(p) {
		return
	}
	o := p.observer
	o.mu.Lock()
	defer o.mu.Unlock()
	o.current = make([]transfer.HTTPCoverageRange, 0)
	o.sealed = nil
}

// Called after writer join and before Close, never from an HTTP worker. No
// Task/main/status/owner/persistence lock is acquired here or held by callers.
// A failure only disables evidence; legacy completion/IO results are unchanged.
func (f *Fetcher) sealCoverageBeforeClose(complete bool) {
	o := f.coverage.Load()
	if o == nil {
		return
	}
	f.stopPrefetch()
	f.wg.Wait()
	if !f.coverageCurrent(o) {
		return
	}
	f.fileMu.RLock()
	if f.file == nil {
		f.fileMu.RUnlock()
		return
	}
	seal, err := f.syncCoverageCheckpoint(o, f.file, complete)
	f.fileMu.RUnlock()
	if err != nil {
		o.disable(err)
		return
	}
	if !f.coverageCurrent(o) {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failure == nil {
		o.sealed = seal
	}
}

type coverageSyncFile interface {
	Sync() error
	Stat() (os.FileInfo, error)
}

// The interface is private so focused tests can inject an actual fd Sync error
// without a production test flag or a path-based reopen.
func (f *Fetcher) syncCoverageCheckpoint(o *httpCoverageObserver, file coverageSyncFile, complete bool) (*RecoveryCoverageCheckpoint, error) {
	o.mu.Lock()
	if o.failure != nil || o.state == nil {
		o.mu.Unlock()
		return nil, transfer.ErrHTTPCoverageUnprovable
	}
	state := cloneCoverage(*o.state)
	state.DurableRanges = append(make([]transfer.HTTPCoverageRange, 0, len(o.current)), o.current...)
	o.mu.Unlock()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > state.Size {
		return nil, transfer.ErrHTTPCoverageUnprovable
	}
	if len(state.DurableRanges) > 0 && info.Size() < state.DurableRanges[len(state.DurableRanges)-1].End {
		return nil, transfer.ErrHTTPCoverageUnprovable
	}
	if err = file.Sync(); err != nil {
		return nil, transfer.ErrHTTPCoverageUnprovable
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(info, after) || after.Size() != info.Size() {
		return nil, transfer.ErrHTTPCoverageUnprovable
	}
	checkpoint, err := (&FetcherManager{}).Store(f)
	if err != nil {
		return nil, transfer.ErrHTTPCoverageUnprovable
	}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, transfer.ErrHTTPCoverageUnprovable
	}
	digest := sha256.Sum256(raw)
	state.CheckpointHash = hex.EncodeToString(digest[:])
	if err = transfer.ValidateHTTPCoverageState(state, o.limit); err != nil {
		return nil, err
	}
	whole := len(state.DurableRanges) == 1 && state.DurableRanges[0].Begin == 0 && state.DurableRanges[0].End == state.Size && after.Size() == state.Size
	return &RecoveryCoverageCheckpoint{token: o.lease.Token, source: o.lease.SourceHash, coverage: state, checkpoint: raw, complete: complete && whole}, nil
}

// StoreRecoveryCoverage joins the captured run, then returns its pre-Close
// same-fd seal. It neither starts/stops a run nor turns cancellation into a clean
// pause. Calling it with an active run waits (bounded by ctx), never snapshots
// concurrent writers. Core must invoke it without any Task/owner/main lock.
func (fm *FetcherManager) StoreRecoveryCoverage(ctx context.Context, ff fetcher.Fetcher, token transfer.RecoveryToken) (*RecoveryCoverageCheckpoint, error) {
	f, ok := ff.(*Fetcher)
	if !ok || f == nil {
		return nil, transfer.ErrHTTPCoverageUnprovable
	}
	o := f.coverage.Load()
	if o == nil || o.lease.Token != token || !f.coverageCurrent(o) {
		return nil, transfer.ErrRecoveryStale
	}
	join := f.CaptureRunQuiescence()
	if err := join(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !f.coverageCurrent(o) {
		return nil, transfer.ErrRecoveryStale
	}
	o.mu.Lock()
	if o.failure != nil {
		err := o.failure
		o.mu.Unlock()
		return nil, err
	}
	if o.sealed == nil {
		o.mu.Unlock()
		return nil, transfer.ErrHTTPCoverageUnprovable
	}
	// All nested values are copied again; retaining an earlier result cannot
	// mutate either the cached seal or another caller's snapshot.
	s := *o.sealed
	s.coverage = cloneCoverage(s.coverage)
	s.checkpoint = append([]byte(nil), s.checkpoint...)
	o.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !f.coverageCurrent(o) {
		return nil, transfer.ErrRecoveryStale
	}
	return &s, nil
}
