package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/GopeedLab/gopeed/internal/netdisk/mailru"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// RecoveryPublicRefreshRequired captures a source-selection request, not an IO
// permit. In particular Resolve returns this to Core before the Task-locked
// preparation stack may call a durable owner. Stream callers must first leave
// their request/body/write scope before reporting it to a coordinator.
//
// The fields are private so callers cannot turn an arbitrary URL into an
// authorized public resolver plan. URL-bearing fields never enter Error().
type RecoveryPublicRefreshRequired struct {
	token       transfer.RecoveryToken
	sourceHash  string
	seen        uint64
	source      mailru.Source
	redirectURL string
	refusedURL  string
	public bool
	begin,end int64
}

func (*RecoveryPublicRefreshRequired) Error() string {
	return "recovery_public_refresh_required"
}

// Until the owner has reserved and admitted the bounded resolver work, an
// unhandled candidate retains the enrolled task's fail-closed behavior.
func (*RecoveryPublicRefreshRequired) Unwrap() error { return transfer.ErrRecoveryWork }

func (request *RecoveryPublicRefreshRequired) Token() transfer.RecoveryToken {
	return request.token
}

func (request *RecoveryPublicRefreshRequired) SourceHash() string { return request.sourceHash }
func (request *RecoveryPublicRefreshRequired) Seen() uint64       { return request.seen }

func (f *Fetcher) capturePublicRefresh(ctx context.Context, refusedURL string) error {
	if ctx == nil || ctx.Err() != nil {
		return transfer.ErrRecoveryStale
	}
	lease := transfer.RecoveryLeaseFromContext(f.recoveryContext(ctx))
	run := f.recovery.Load()
	if lease == nil || lease.ActionVersion != 4 || run == nil || run.lease.Token != lease.Token || run.shared == nil {
		return transfer.ErrRecoveryStale
	}
	source := f.publicSourceSnapshot()
	if source == nil {
		return transfer.ErrRecoveryWork
	}
	checked, ok := mailru.Match(source.Page(), "")
	if !ok || checked != *source {
		return transfer.ErrRecoveryWork
	}
	run.mu.Lock()
	seen := run.round
	current := f.recovery.Load() == run && lease.Token == run.lease.Token && lease.Context != nil && lease.Context.Err() == nil
	run.mu.Unlock()
	if !current {
		return transfer.ErrRecoveryStale
	}
	f.redirectLock.Lock()
	redirectURL := f.redirectURL
	f.redirectLock.Unlock()
	return &RecoveryPublicRefreshRequired{token: lease.Token, sourceHash: lease.SourceHash,
		seen: seen, source: *source, public:true, redirectURL: redirectURL, refusedURL: refusedURL}
}

// A pending response belongs to one paid original-URL refresh. It has not been
// read by the application and can be consumed only by its exact new-wave work.
type sharedRefreshResponse struct {
	response *http.Response
	begin, end int64
}
type sharedHTTPRefresh struct {
	request *RecoveryPublicRefreshRequired
	members []string
	work string
	done chan struct{}
	err error
}

type httpRefreshSourceSeal struct { seal *RecoveryWaveSeal }
func (s *httpRefreshSourceSeal) Check(ctx context.Context) error { return s.seal.Check(ctx) }
func (s *httpRefreshSourceSeal) Snapshot(ctx context.Context) (transfer.RecoverySourceSnapshot,error) {
	v,err := s.seal.Snapshot(ctx)
	return transfer.RecoverySourceSnapshot{Token:v.Token,SourceHash:v.SourceHash,Epoch:v.Epoch,Members:v.Members},err
}

// Resolve calls this only after its Task-locked preparation returned the typed
// candidate. Stream workers call reportSharedRefresh after leaving their IO
// scope. Neither path joins itself or calls SetRecoveryLease on its run.
func (f *Fetcher) RefreshSharedSource(ctx context.Context, request *RecoveryPublicRefreshRequired) (*transfer.RecoveryLease,error) {
	if request == nil { return nil,transfer.ErrRecoveryWork }
	_,err := f.reportSharedRefresh(ctx,"start",request)
	if err != nil { return nil,err }
	run := f.recovery.Load()
	if run == nil { return nil,transfer.ErrRecoveryStale }
	return run.lease,nil
}

func (f *Fetcher) reportSharedRefresh(ctx context.Context, work string, cause error) (bool,error) {
	var request *RecoveryPublicRefreshRequired
	if !errors.As(cause,&request) { return false,nil }
	s := f.sharedRun()
	if s == nil || s.failure != nil { return true,transfer.ErrRecoveryHeld }
	if ctx.Err() != nil { return true,ctx.Err() }
	s.run.mu.Lock()
	if request.token != s.lease.Token {
		current := s.lease.Token
		s.run.mu.Unlock()
		if request.token.TaskID == current.TaskID && request.token.Episode == current.Episode && request.token.Generation == current.Generation && request.token.SourceRevision < current.SourceRevision {
			return true,nil // This old wave's refresh was already committed.
		}
		return true,transfer.ErrRecoveryStale
	}
	var report *sharedWaveFailure
	if errors.As(cause,&report) && report.barrier != s.barrier { s.run.mu.Unlock();return true,transfer.ErrRecoveryStale }
	s.mu.Lock()
	if s.pending != nil { s.mu.Unlock();s.run.mu.Unlock();return true,transfer.ErrRecoveryWork }
	p := s.refresh
	if p == nil {
		members := []string{}
		f.connMu.Lock()
		for _,conn := range f.connections { if !conn.Completed { members = append(members,f.recoveryWork(conn)) } }
		f.connMu.Unlock()
		if len(members) == 0 { members = []string{work} }
		members,err := waveMembers(members)
		if err != nil { s.mu.Unlock();s.run.mu.Unlock();return true,err }
		p = &sharedHTTPRefresh{request:request,members:members,work:work,done:make(chan struct{})}
		s.refresh = p
		go s.coordinateRefresh(p)
	}
	s.mu.Unlock()
	s.run.mu.Unlock()
	select { case <-ctx.Done():return true,ctx.Err();case <-p.done:return true,p.err }
}

// This client retains actual proxy/TLS/MCP transport and the original every-hop
// credential policy. Its request lifetime is the parent run, not the drained
// old IO wave. The returned body is left unread until source ACK/new scope.
func (f *Fetcher) refreshPlanClient(anonymous bool) *http.Client {
	client := f.buildClient()
	outer := client.Transport.(*policyTransport)
	guard := outer.transport.(*recoveryGuardTransport)
	if anonymous {
		// Existing Mail.ru resolver is public-only: no inherited auth/cookie
		// headers. Source.Refresh independently validates both resolver hosts.
		client.Transport = &recoveryGuardTransport{transport:guard.transport}
		client.Jar = nil
	} else {
		copy := *outer
		copy.transport = &recoveryGuardTransport{transport:guard.transport}
		client.Transport = &copy
	}
	return client
}

func (s *httpSharedRecovery) coordinateRefresh(p *sharedHTTPRefresh) {
	ctx := s.lease.Context
	var pending *http.Response
	var err error
	defer func(){
		if err != nil {
			if pending != nil { _ = pending.Body.Close() }
			s.barrier.close(err)
		}
		s.mu.Lock();p.err = err;close(p.done);s.mu.Unlock()
	}()
	owner,ok := s.lease.Owner.(transfer.RecoveryParentController)
	if !ok { err = transfer.ErrRecoveryHeld;return }
	seal,sealErr := s.barrier.drain(ctx,p.members)
	if sealErr != nil { err = sealErr;return }
	seen,seenErr := owner.CurrentRecoveryRound(ctx,s.lease.Token)
	if seenErr != nil { err = seenErr;return }
	permit,reserveErr := owner.ReserveRecoveryAction(ctx,s.lease.Token,seen,transfer.RecoveryActionRefresh,p.work,p.members,time.Time{})
	if reserveErr != nil { err = reserveErr;return }
	if permit == nil { err = transfer.ErrRecoveryWork;return }
	if err = permit.Consume(ctx);err != nil { return }
	request := p.request
	requestContext := transfer.WithRecoveryLease(ctx,s.lease)
	var link string
	if request.public {
		client := s.f.refreshPlanClient(true)
		client.Timeout = 25*time.Second
		resolveContext,cancel := context.WithTimeout(requestContext,25*time.Second)
		download,buildErr := s.f.buildRequest(resolveContext,s.f.meta.Req)
		if buildErr != nil { cancel();err = buildErr;return }
		link,err = request.source.Refresh(resolveContext,client,download.UserAgent())
		cancel()
		if err != nil { return }
	} else {
		if !originalRequestReplaySafe(s.f.meta.Req) { err = transfer.ErrScopedContext;return }
		original,buildErr := s.f.buildRequestWithOriginalURL(requestContext,s.f.meta.Req)
		if buildErr != nil { err = buildErr;return }
		if s.f.meta.Res.Range && request.end > 0 {
			original.Header.Set(base.HttpHeaderRange,fmt.Sprintf(base.HttpHeaderRangeFormat,request.begin,request.end))
			s.f.applyIfRange(original)
		}
		pending,err = doCredentialRequest(s.f.refreshPlanClient(false),original)
		if err != nil { return }
		if pending.StatusCode != http.StatusOK && pending.StatusCode != http.StatusPartialContent { err = newResponseError(pending);return }
		link = pending.Request.URL.String()
	}
	if err = seal.Check(ctx);err != nil { return }
	checkpoint,storeErr := (&FetcherManager{}).Store(s.f)
	if storeErr != nil { err = storeErr;return }
	stored := checkpoint.(*fetcherData)
	stored.RedirectURL = link
	payload,marshalErr := json.Marshal(stored)
	if marshalErr != nil { err = marshalErr;return }
	lease,commitErr := owner.CommitRecoverySource(ctx,s.lease.Token,permit.ActionID,&httpRefreshSourceSeal{seal},payload)
	if commitErr != nil { err = commitErr;return }
	if lease == nil || lease.Token.TaskID != s.lease.Token.TaskID || lease.Token.Episode != s.lease.Token.Episode || lease.Token.Generation != s.lease.Token.Generation || lease.Token.SourceRevision != s.lease.Token.SourceRevision+1 {
		err = transfer.ErrRecoveryStale;return
	}
	if err = lease.Check();err != nil { return }
	if err = seal.Check(ctx);err != nil { return }
	// Publish a new immutable lease/run pair. The old barrier stays sealed;
	// it wakes parked old workers only to let them enter the new wave.
	next := &httpRecoveryRun{lease:lease,round:lease.Round,base:lease.Round,members:append([]string{},p.members...),initialLimit:s.run.initialLimit,initialClaims:map[int]bool{}}
	for id := 0;id < next.initialLimit;id++ { next.initialClaims[id] = true }
	next.shared = newHTTPSharedRecovery(s.f,next,lease)
	if next.shared.failure != nil { err = next.shared.failure;return }
	if pending != nil {
		next.shared.responses = map[string]sharedRefreshResponse{p.work:{response:pending,begin:request.begin,end:request.end}}
		context.AfterFunc(lease.Context,func(){
			next.shared.mu.Lock()
			defer next.shared.mu.Unlock()
			for work,body := range next.shared.responses { _ = body.response.Body.Close();delete(next.shared.responses,work) }
		})
	}
	s.run.mu.Lock()
	if s.f.recovery.Load() != s.run || ctx.Err() != nil { s.run.mu.Unlock();err = transfer.ErrRecoveryStale;return }
	s.f.updateRedirectURL(link)
	s.f.recovery.Store(next)
	s.run.mu.Unlock()
	// A raw network body changes no context or authority. Its sole new-wave
	// consumer will attach a fresh IO scope before any application Read/Write.
	pending = nil
	s.barrier.close(errSharedWaveDraining)
}

func (f *Fetcher) takeSharedRefreshResponse(work string, begin,end int64) (*http.Response,error) {
	s := f.sharedRun()
	if s == nil { return nil,nil }
	s.mu.Lock()
	pending,ok := s.responses[work]
	if ok { delete(s.responses,work) }
	s.mu.Unlock()
	if !ok { return nil,nil }
	if pending.begin != begin || pending.end != end { _ = pending.response.Body.Close();return nil,transfer.ErrRecoveryWork }
	return pending.response,nil
}
