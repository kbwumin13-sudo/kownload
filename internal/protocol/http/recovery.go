package http

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// RecoveryFailure carries only a closed category, never an underlying URL,
// header or network error string. The same classification will feed Core's
// future ephemeral error snapshot; there is no second MCP error classifier.
type RecoveryFailure struct {
	Category  string
	Retryable bool
}

func (e *RecoveryFailure) Error() string { return "http_transport_" + e.Category }
func ClassifyRecoveryError(err error) *RecoveryFailure {
	var wrapped *url.Error
	if errors.As(err, &wrapped) {
		err = wrapped.Err
	}
	if errors.Is(err, context.Canceled) {
		return &RecoveryFailure{"cancelled", false}
	}
	if credentialPolicyError(err) != nil {
		return &RecoveryFailure{"authentication_context", false}
	}
	if !errors.Is(err, ErrUnsafeResume) && !isLocalFileError(err) && (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)) {
		return &RecoveryFailure{"truncated", true}
	}
	if errors.Is(err, ErrUnsafeResume) || errors.Is(err, errInvalidRangeResponse) {
		return &RecoveryFailure{"unsafe_resume", false}
	}
	if isLocalFileError(err) {
		return &RecoveryFailure{"local_file", false}
	}
	if re := extractRequestError(err); re != nil {
		switch re.Code {
		case 401, 403, 407:
			return &RecoveryFailure{"authentication", false}
		case 404, 410:
			return &RecoveryFailure{"source_missing", false}
		case 408:
			return &RecoveryFailure{"timeout", true}
		case 429, 503:
			return &RecoveryFailure{"source_cooldown", false} // existing Budget owns this path
		case 416:
			return &RecoveryFailure{"unsafe_resume", false}
		}
		if re.Code >= 500 && re.Code <= 599 {
			return &RecoveryFailure{"server", true}
		}
		return &RecoveryFailure{"http_status", false}
	}
	var cert x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &cert) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return &RecoveryFailure{"tls", false}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return &RecoveryFailure{"dns", !dns.IsNotFound && (dns.IsTimeout || dns.IsTemporary)}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &RecoveryFailure{"timeout", true}
	}
	var network net.Error
	if errors.As(err, &network) {
		return &RecoveryFailure{"network", true}
	}
	return &RecoveryFailure{"unknown", false}
}

type httpBoundRun struct{ ctx context.Context }

func (f *Fetcher) boundRunContext() context.Context {
	if binding := f.boundRun.Load(); binding != nil {
		return binding.ctx
	}
	return context.Background()
}

type httpRecoveryRun struct {
	mu            sync.Mutex
	lease         *transfer.RecoveryLease
	round         uint64
	base          uint64
	members       []string
	initialLimit  int
	initialClaims map[int]bool
	hadFailure    atomic.Bool
	shared        *httpSharedRecovery
}

// Core calls this only after durable authorization and after the previous
// pause joined. Join old cleanup before giving this run a new result channel.
func (f *Fetcher) SetRecoveryLease(lease *transfer.RecoveryLease) {
	copy := *lease
	copy.InitialMembers = append([]string(nil), lease.InitialMembers...)
	lease = &copy

	if f.downloadLoopDone != nil {
		<-f.downloadLoopDone
	}
	// No old worker can still consume/finish these connection-local values.
	// Persistent debits belong to the owner; only obsolete run permits go away.
	f.wg.Wait()
	f.connMu.Lock()
	clear := func(conn *connection) {
		if conn != nil {
			conn.recoveryPermit = nil
			conn.recoveryRound = 0
			conn.recoveryInitialized = false
		}
	}
	for _, conn := range f.connections {
		clear(conn)
	}
	clear(f.resolveConn)
	f.connMu.Unlock()
	f.doneCh = make(chan error, 1)
	next := &httpRecoveryRun{lease: lease, round: lease.Round, base: lease.Round}
	if lease.ActionVersion == 4 {
		next.shared = newHTTPSharedRecovery(f, next, lease)
	}
	old := f.recovery.Load()
	if old != nil {
		old.mu.Lock()
		defer old.mu.Unlock()
	}
	f.deferred.Store(nil)
	f.recovery.Store(next)
}
func (f *Fetcher) BindRunWait(ctx context.Context) func() error {
	if f.downloadLoopDone != nil {
		<-f.downloadLoopDone
	}
	f.doneCh = make(chan error, 1)
	result := f.doneCh
	f.boundRun.Store(&httpBoundRun{ctx: ctx})
	f.readPublished.Store(nil)
	return func() error {
		select {
		case err := <-result:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// CaptureRunQuiescence captures this run's writer completion, never a channel
// looked up later after replacement. Core calls while its lifecycle decision
// excludes a new Start, then waits without holding Task/Downloader locks.
// A run that failed before Start has no download writer to join.
func (f *Fetcher) CaptureRunQuiescence() func(context.Context) error {
	writers := []<-chan struct{}{f.downloadLoopDone, f.prefetchDoneCh}
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, done := range writers {
			if done == nil {
				continue
			}
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return ctx.Err()
	}
}
func (f *Fetcher) recoveryContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if previous := transfer.RecoveryLeaseFromContext(ctx); previous != nil {
		// Only future scopes on this still-live run may inherit a confirmed
		// source handoff. An old request/body scope is never relabeled.
		if ctx.Err() == nil && ctx.Value(sharedIOContextKey{}) == nil {
			if run := f.recovery.Load(); run != nil && previous.Owner == run.lease.Owner &&
				previous.Token.TaskID == run.lease.Token.TaskID && previous.Token.Episode == run.lease.Token.Episode &&
				previous.Token.Generation == run.lease.Token.Generation && previous.Token.SourceRevision < run.lease.Token.SourceRevision {
				return transfer.WithRecoveryLease(ctx,run.lease)
			}
		}
		return ctx
	}
	if run := f.recovery.Load(); run != nil {
		return transfer.WithRecoveryLease(ctx, run.lease)
	}
	return ctx
}
func recoveryContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lease := transfer.RecoveryLeaseFromContext(ctx); lease != nil {
		return lease.Check()
	}
	return nil
}

// Worker slot IDs belong to the bounded scheduler, not connection object
// addresses or changing chunk offsets. Slow start may claim an unused slot;
// replacing the object for a claimed slot must pay for a new attempt.
func (f *Fetcher) recoveryWork(conn *connection) string {
	return fmt.Sprintf("worker/%d", conn.ID)
}
func (f *Fetcher) configureInitialRecoveryWork(limit int) error {
	run := f.recovery.Load()
	if run == nil {
		return nil
	}
	if limit < 1 || limit > 1024 {
		return transfer.ErrRecoveryWork
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	run.initialLimit = limit
	run.initialClaims = make(map[int]bool)
	if run.lease.ActionVersion == 4 && len(run.lease.InitialMembers) > 0 {
		start := len(run.lease.InitialMembers) == 1 && run.lease.InitialMembers[0] == "start"
		if !start {
			for id := 0; id < limit; id++ {
				run.initialClaims[id] = true
			}
			for _, work := range run.lease.InitialMembers {
				for id := 0; id < limit; id++ {
					if work == fmt.Sprintf("worker/%d", id) {
						run.initialClaims[id] = false
					}
				}
			}
		}
	}
	return nil
}
func (f *Fetcher) authorizeInitialRecoveryWork(conn *connection) error {
	lease := transfer.RecoveryLeaseFromContext(f.recoveryContext(conn.ctx))
	if lease == nil {
		return nil
	}
	if err := lease.Check(); err != nil {
		return err
	}
	run := f.recovery.Load()
	if run == nil || run.lease.Token != lease.Token {
		return transfer.ErrRecoveryStale
	}
	run.mu.Lock()
	if run.shared != nil && conn.recoveryInitialized && conn.recoveryRound == run.round {
		run.shared.mu.Lock()
		admitted := run.shared.completed[f.recoveryWork(conn)]
		run.shared.mu.Unlock()
		if admitted {
			run.mu.Unlock()
			return lease.Check()
		}
	}
	if conn.ID < 0 || conn.ID >= run.initialLimit {
		run.mu.Unlock()
		return transfer.ErrRecoveryWork
	}
	if !run.initialClaims[conn.ID] {
		run.initialClaims[conn.ID] = true
		run.mu.Unlock()
		return lease.Check()
	}
	// A replacement cannot join a previously consumed wave as a new object.
	conn.recoveryInitialized = true
	conn.recoveryRound = run.round
	run.mu.Unlock()
	if f.retryManagedConnection(conn, io.ErrUnexpectedEOF) {
		return transfer.ErrRecoveryStale
	}
	return nil
}

func (f *Fetcher) retryManagedConnection(conn *connection, cause error) bool {
	lease := transfer.RecoveryLeaseFromContext(f.recoveryContext(conn.ctx))
	if lease == nil {
		return false
	}
	run := f.recovery.Load()
	if run == nil || run.lease.Token != lease.Token {
		f.stopForCredentialFailure(conn, transfer.ErrRecoveryStale)
		return true
	}
	// The parent provider owns signed-link refresh. Preserve only this typed
	// expiry cause when returning its child result; HTTP still performs no
	// authentication replay and direct HTTP retains its original classifier.
	if lease.ParentContext != nil && lease.ParentContext.Protocol() == "netdisk" && IsExpiredLinkError(cause) {
		f.stopForCredentialFailure(conn, cause)
		return true
	}
	classification := ClassifyRecoveryError(cause)
	if !classification.Retryable {
		f.stopForCredentialFailure(conn, classification)
		return true
	}
	run.mu.Lock()
	f.connMu.Lock()
	work := f.recoveryWork(conn)
	if !conn.recoveryInitialized {
		conn.recoveryInitialized = true
		conn.recoveryRound = run.base
	}
	seen := conn.recoveryRound
	members := run.members
	if seen < run.round {
		included := false
		for _, member := range members {
			if member == work {
				included = true
				break
			}
		}
		// A slot that joined after an earlier wave cannot take its free
		// entitlement. It may start a new, persistently charged wave.
		if !included {
			seen = run.round
		}
	}
	if seen == run.round {
		members = nil
		unique := map[string]bool{}
		for _, other := range f.connections {
			if other.Completed || other.Chunk == nil {
				continue
			}
			id := f.recoveryWork(other)
			if !unique[id] {
				members = append(members, id)
				unique[id] = true
			}
		}
		if !unique[work] {
			members = append(members, work)
		}
	}
	f.connMu.Unlock()
	permit, err := lease.Retry(conn.ctx, seen, work, members)
	if err == nil {
		run.round = permit.Round
		run.members = members
	}
	run.mu.Unlock()
	if err == nil {
		err = permit.Consume()
	}
	if err != nil {
		f.stopForCredentialFailure(conn, err)
		return true
	}
	conn.recoveryRound = permit.Round
	conn.recoveryPermit = permit
	// Stable per episode/work/round jitter; unrelated tasks do not synchronize.
	hash := fnv.New32a()
	_, _ = fmt.Fprintf(hash, "%s/%s/%d", lease.Token.Episode, work, permit.Round)
	delay := time.Duration(100+hash.Sum32()%101) * time.Millisecond * time.Duration(permit.Round-lease.Round)
	if delay > 2*time.Second {
		delay = 2 * time.Second
	}
	if !sleepContext(conn.ctx, delay) {
		return true
	}
	return false
}
func (f *Fetcher) observeManagedFailure(ctx context.Context, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	lease := transfer.RecoveryLeaseFromContext(ctx)
	if lease == nil {
		return
	}
	if run := f.recovery.Load(); run != nil && run.lease.Token == lease.Token {
		run.hadFailure.Store(true)
	}
}

func (f *Fetcher) finishManagedConnection(conn *connection) error {
	if conn.recoveryPermit == nil {
		return nil
	}
	permit := conn.recoveryPermit
	conn.recoveryPermit = nil
	err := permit.Finish(conn.ctx)
	if errors.Is(err,transfer.ErrRecoveryStale) {
		old := transfer.RecoveryLeaseFromContext(conn.ctx)
		if run := f.recovery.Load(); old != nil && run != nil && old.Token.TaskID == run.lease.Token.TaskID && old.Token.Episode == run.lease.Token.Episode && old.Token.Generation == run.lease.Token.Generation && old.Token.SourceRevision < run.lease.Token.SourceRevision {
			return nil // Late old-source bookkeeping must not cancel the new source.
		}
	}
	return err
}

func (f *Fetcher) CaptureRecoveryContext(req *base.Request) (*transfer.HTTPExecutionContext, bool, error) {
	if f.downloadLoopDone != nil {
		<-f.downloadLoopDone
	}
	if f.ctl.GetRecoveryProxySnapshot == nil {
		return nil, false, nil
	}
	proxy, id, supported := f.ctl.GetRecoveryProxySnapshot(req.Proxy)
	if !supported || id == "" {
		return nil, false, nil
	}
	current := f.config
	if f.ctl.GetConfig != nil {
		f.ctl.GetConfig(&current)
	}
	snapshot, err := transfer.NewHTTPExecutionContext(req, current.UserAgent, id, proxy)
	return snapshot, true, err
}
func (f *Fetcher) SetRecoveryContext(snapshot *transfer.HTTPExecutionContext) {
	f.executionContext.Store(snapshot)
}
func (f *Fetcher) validateRecoveryRequest(ctx context.Context) error {
	lease := transfer.RecoveryLeaseFromContext(ctx)
	if lease == nil {
		return nil
	}
	if err := lease.Check(); err != nil {
		return err
	}
	snapshot := f.executionContext.Load()
	if snapshot == nil {
		return transfer.ErrRecoveryHeld
	}
	return snapshot.Validate(f.meta.Req, lease.SourceHash)
}

func (f *Fetcher) clientProxySnapshot() func(*http.Request) (*url.URL, error) {
	if snapshot := f.executionContext.Load(); snapshot != nil {
		return snapshot.Proxy()
	}
	return f.ctl.GetProxy(f.meta.Req.Proxy)
}

func (f *Fetcher) RecoveryCleanPause() bool {
	if run := f.recovery.Load(); run != nil && run.hadFailure.Load() {
		return false
	}
	if f.getState() != statePaused || !f.usesRanges() || !f.hasResumeData() || f.policyFailure.Load() != nil || f.deferred.Load() != nil {
		return false
	}
	identity := f.representationSnapshot()
	if identity.ETag == "" || strings.HasPrefix(identity.ETag, "W/") {
		return false
	}
	f.connMu.Lock()
	defer f.connMu.Unlock()
	for _, connection := range f.connections {
		if connection.failed || connection.lastErr != nil {
			return false
		}
	}
	return true
}

type recoveryGuardTransport struct {
	transport http.RoundTripper
	fetcher   *Fetcher
}

func (t *recoveryGuardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var work *recoveryWaveWork
	if t.fetcher != nil {
		var err error
		work, err = t.fetcher.enterSharedRequest(req.Context())
		if err != nil {
			return nil, err
		}
		if work != nil {
			req = req.Clone(work.Context)
		}
	}
	keep := false
	defer func() {
		if !keep && work != nil {
			work.Leave()
		}
	}()
	if err := recoveryContextError(req.Context()); err != nil {
		return nil, err
	}
	response, err := t.transport.RoundTrip(req)
	if err == nil {
		if obsolete := recoveryContextError(req.Context()); obsolete != nil {
			response.Body.Close()
			return nil, obsolete
		}
	}
	if err == nil && work != nil {
		response.Body = wrapSharedWaveBody(response.Body, work)
		keep = true
	}
	return response, err
}
