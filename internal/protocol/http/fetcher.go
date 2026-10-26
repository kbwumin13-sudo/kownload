package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
	"github.com/xiaoqidun/setft"
)

const (
	connectTimeout     = 15 * time.Second
	readTimeout        = 15 * time.Second
	minFastFailTimeout = int64(3 * time.Second) // Minimum timeout for fast-fail retry

	// Work stealing parameters
	// When a connection finishes its chunk, it can "steal" work from slow connections.
	stealThresholdSeconds = 3          // Only steal if victim needs > 3 seconds to finish
	stealMinChunkSize     = 512 * 1024 // Min steal size: 512KB (avoid tiny chunks)

	// How long a file too small to split may finish on its first response
	// before the rest is fetched with a range request.
	smallFileDrainTimeout = 10 * time.Second

	// A range refused with 403 (usually a server connection limit) is handed
	// to another stream this many times before it counts as a failure.
	maxHandoffs = 3
)

// ============================================================================
// State Machine
// ============================================================================

type fetcherState int32

const (
	stateIdle      fetcherState = iota // Initial state
	stateResolving                     // Resolving resource info
	stateResolved                      // Resolved, waiting for Start or downloading
	stateSlowStart                     // Slow-start phase: exponential connection growth
	stateSteady                        // Steady state: max connections reached
	statePaused                        // Paused
	stateDone                          // Completed
	stateError                         // Error occurred
)

// ============================================================================
// Connection
// ============================================================================

type connectionState int32

const (
	connNotStarted  connectionState = iota // Not yet started
	connConnecting                         // Sending HTTP request
	connDownloading                        // HTTP response OK, downloading
	connCompleted                          // Completed
	connFailed                             // Failed
)

type connectionRole int

const (
	roleResolve connectionRole = iota // Resolve connection: initial probe + temp download
	rolePrimary                       // Primary connection: first successful takeover from Resolve
	roleWorker                        // Worker connection: subsequent connections
)

type chunk struct {
	Begin      int64
	End        int64
	Downloaded int64
}

func (c *chunk) remain() int64 {
	return c.End - c.Begin + 1 - c.Downloaded
}

func newChunk(begin int64, end int64) *chunk {
	return &chunk{
		Begin: begin,
		End:   end,
	}
}

type connection struct {
	ID         int
	Role       connectionRole
	State      connectionState
	Chunk      *chunk
	Downloaded int64
	Completed  bool

	failed     bool
	retryTimes int
	lastErr    error

	// Budget state, never persisted: permit marks a granted stream; shedding
	// means the stream stops after its shortened chunk; parked means the chunk
	// waits for a stream and no goroutine owns it. Guarded by connMu except
	// permit, which only the owning goroutine (or the launcher before it) uses.
	permit   bool
	shedding bool
	parked   bool
	// handoffs counts consecutive 403s on the current range; such a range is
	// parked for a running stream to take over, up to maxHandoffs times.
	handoffs int

	// Speed tracking for work stealing decisions
	speed             int64 // bytes per second
	lastSpeedCheck    int64 // timestamp in nanoseconds
	lastSpeedDownload int64 // bytes downloaded at last check

	ctx    context.Context
	cancel context.CancelFunc
}

// ============================================================================
// Slow Start Controller
// ============================================================================

type slowStartController struct {
	mu             sync.Mutex
	maxConnections int
	totalLaunched  int
	batchPending   int           // Connections in current batch waiting for HTTP response
	batchReady     int           // Connections in current batch that succeeded
	nextBatchSize  int           // Next batch size: 1, 2, 4, 8...
	expansionCh    chan struct{} // Signal to trigger next expansion
	paused         bool          // Pause expansion (e.g., on 429)
}

func newSlowStartController(maxConnections int) *slowStartController {
	return &slowStartController{
		maxConnections: maxConnections,
		nextBatchSize:  1,
		expansionCh:    make(chan struct{}, 1),
	}
}

// onConnectSuccess is called when a connection successfully gets HTTP response
// Returns true if this completes the current batch
func (s *slowStartController) onConnectSuccess() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.batchReady++
	if s.batchReady >= s.batchPending {
		// Batch complete, signal expansion
		select {
		case s.expansionCh <- struct{}{}:
		default:
		}
		return true
	}
	return false
}

// onConnectFailed is called when a connection fails
func (s *slowStartController) onConnectFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Reduce pending count
	if s.batchPending > 0 {
		s.batchPending--
	}
	// If all pending resolved (success or fail), trigger expansion
	// This handles both successful completion and all-failures case
	if s.batchPending == 0 {
		select {
		case s.expansionCh <- struct{}{}:
		default:
		}
	}
}

// getNextBatchSize returns how many connections to start in next batch
// Returns 0 if max reached. With live >= 0 the limit counts running streams
// instead of every stream launched, so streams handed back by the adaptive
// budget can be replaced later.
func (s *slowStartController) getNextBatchSize(live int) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.paused {
		return 0
	}

	remaining := s.maxConnections - s.totalLaunched
	if live >= 0 {
		remaining = s.maxConnections - live
	}
	if remaining <= 0 {
		return 0
	}

	batchSize := s.nextBatchSize
	if batchSize > remaining {
		batchSize = remaining
	}

	return batchSize
}

// commitBatch confirms that a batch of connections is being launched
func (s *slowStartController) commitBatch(count int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalLaunched += count
	s.nextBatchSize = s.nextBatchSize * 2 // Exponential growth: 1, 2, 4, 8...
	s.batchPending = count
	s.batchReady = 0
}

// ============================================================================
// Fetcher
// ============================================================================

type Fetcher struct {
	requestPolicy  func(*http.Request)
	responsePolicy func(*http.Request, *http.Response) error
	ctl            *controller.Controller
	config         *config
	cookieJar      http.CookieJar // Shared only within this fetcher; domain-scoped by net/http.
	doneCh         chan error

	meta *fetcher.FetcherMeta

	// State machine
	state atomic.Int32 // fetcherState

	// Connections
	connMu               sync.Mutex
	connections          []*connection
	resolveConn          *connection // The special resolve connection
	rangeResumeValidator string      // Persisted If-Range validator for upgraded legacy prefixes.

	// Slow start controller
	slowStart *slowStartController

	// Max connection time for adaptive timeout (stored as int64 nanoseconds for atomic ops)
	maxConnTime atomic.Int64

	// First primary connection success signal
	primaryReadyOnce sync.Once
	primaryReadyCh   chan struct{}

	// Start pending mechanism
	startPending   atomic.Bool
	resolvedCh     chan struct{} // Signal when resolve completes
	resolvedOnce   sync.Once
	resolveDataPos atomic.Int64 // How many bytes downloaded during resolve

	// Resolve response - kept open for one-time URLs
	resolveResp     *http.Response
	resolveRespLock sync.Mutex

	// Async prefetch during resolve phase
	drainPrefetch    bool          // Small file: let the prefetch finish in the download loop
	prefetchFile     *os.File      // Temporary file for prefetch data
	prefetchFilePath string        // Path to temporary file
	prefetchSize     atomic.Int64  // Bytes prefetched so far
	prefetchErr      error         // Error during prefetch (if any)
	prefetchStopCh   chan struct{} // Signal to stop prefetch
	prefetchDoneCh   chan struct{} // Closed after all prefetch file writes and cleanup

	// Target file. Streams write disjoint ranges with WriteAt (pwrite), which
	// is safe concurrently, so they share fileMu; replacing or closing the
	// file takes it exclusively.
	file         *os.File
	fileMu       sync.RWMutex
	redirectURL  string
	redirectLock sync.Mutex

	// Lifecycle control
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// downloadLoop lifecycle tracking
	downloadLoopDone chan struct{} // Closed when downloadLoop exits

	// Resolve connection control
	resolveCtx      context.Context
	resolveCancel   context.CancelFunc
	resolveCancelMu sync.Mutex

	// Source budget membership for the current run, nil without a budget.
	member atomic.Pointer[transfer.Member]
	// Set when a long server rate limit ends the run; reported through doneCh.
	deferred atomic.Pointer[transfer.DeferredError]
	// Consecutive deferrals without a successful response.
	deferrals atomic.Int32
	// Stream limit of the current run, readable while Resolve rewrites options.
	configuredStreams atomic.Int32
	// Running stream goroutines; streamExit wakes the supervisor when one ends.
	liveStreams atomic.Int32
	streamExit  chan struct{}

	diag diagnostics
}

func (f *Fetcher) tuning() tuning {
	return f.config.tuning()
}

// requestContext attaches connection timing hooks when tracing is enabled.
func (f *Fetcher) requestContext(ctx context.Context) context.Context {
	if f.tuning().trace {
		return f.diag.trace(ctx)
	}
	return ctx
}

func (f *Fetcher) Setup(ctl *controller.Controller) {
	f.ctl = ctl
	f.cookieJar, _ = cookiejar.New(nil)
	f.doneCh = make(chan error, 1)
	if f.meta == nil {
		f.meta = &fetcher.FetcherMeta{}
	}
	f.ctl.GetConfig(&f.config)
	f.resolvedCh = make(chan struct{})
	f.primaryReadyCh = make(chan struct{})

	// Check if this is a restore scenario (has existing connections or meta)
	if f.meta.Res != nil {
		// Already resolved, close the channel immediately
		close(f.resolvedCh)
		f.state.Store(int32(stateResolved))
	} else {
		f.state.Store(int32(stateIdle))
	}
}

func (f *Fetcher) getState() fetcherState {
	return fetcherState(f.state.Load())
}

func (f *Fetcher) setState(s fetcherState) {
	f.state.Store(int32(s))
}

// updateMaxConnTime updates maxConnTime if the new duration is larger
func (f *Fetcher) updateMaxConnTime(d time.Duration) {
	newVal := int64(d)
	if newVal > f.maxConnTime.Load() {
		f.maxConnTime.Store(newVal)
	}
}

func (f *Fetcher) Resolve(req *base.Request, opts *base.Options) error {
	return f.ResolveContext(context.Background(), req, opts)
}

func (f *Fetcher) ResolveContext(ctx context.Context, req *base.Request, opts *base.Options) error {
	parent := ctx
	ctx, cancel := context.WithCancel(context.Background())
	headersReady := make(chan struct{})
	var headersOnce sync.Once
	finishHeaders := func() { headersOnce.Do(func() { close(headersReady) }) }
	go func() {
		select {
		case <-headersReady:
		case <-parent.Done():
			select {
			case <-headersReady:
			default:
				cancel()
			}
		}
	}()
	keepResponse := false
	f.resolveCancelMu.Lock()
	f.resolveCancel = cancel
	f.resolveCancelMu.Unlock()
	defer func() {
		finishHeaders()
		if !keepResponse {
			cancel()
		}
	}()
	if err := base.ParseReqExtra[fhttp.ReqExtra](req); err != nil {
		return err
	}
	f.meta.Req = req
	f.meta.Opts = opts
	if f.meta.Opts == nil {
		f.meta.Opts = &base.Options{}
	}

	// Parse options
	if err := base.ParseOptExtra[fhttp.OptsExtra](opts); err != nil {
		return err
	}
	if opts.Extra == nil {
		opts.Extra = &fhttp.OptsExtra{}
	}
	extra := opts.Extra.(*fhttp.OptsExtra)
	if extra.Connections <= 0 {
		extra.Connections = f.config.Connections
		if extra.Connections <= 0 {
			extra.Connections = 1
		}
	}
	f.configuredStreams.Store(int32(extra.Connections))

	f.setState(stateResolving)

	// A source cooling down from a server rate limit gets no new request, and
	// after a cooldown this request waits its turn behind the recovery probe;
	// a wait too long to hold the task's running slot releases it instead.
	var admission *transfer.Member
	if f.ctl.Budget != nil {
		m, err := f.admitFirstRequest(ctx)
		if err != nil {
			f.setState(stateError)
			if _, ok := transfer.AsDeferred(err); ok {
				return err
			}
			// Pause cancels ctx through CancelResolve; a caller's parent too.
			if parent.Err() != nil {
				return parent.Err()
			}
			return context.Canceled
		}
		if m != nil {
			admission = m
			defer admission.Leave()
		}
	}

	// Build HTTP request WITHOUT Range header (normal request)
	// This allows the response to be reused for downloading (important for one-time URLs)
	httpReq, err := f.buildRequest(f.requestContext(ctx), req)
	if err != nil {
		f.setState(stateError)
		return err
	}

	client := f.buildClient()
	// Client.Do may add cookie-jar cookies to the request. Keep a clean copy
	// for the optional capability probe, including the original redirect scope.
	probeReq := httpReq.Clone(ctx)

	// Send normal HTTP request (no Range header)
	// Track connection time for adaptive timeout in download phase
	connStartTime := time.Now()
	resp, err := client.Do(httpReq)
	finishHeaders()
	if err != nil {
		f.diag.failure(err)
		f.setState(stateError)
		return err
	}
	// Record connection time as baseline for fast-fail timeout
	f.updateMaxConnTime(time.Since(connStartTime))
	f.diag.setResolve(time.Since(connStartTime))
	f.diag.response(resp)

	// Parse response to get resource info
	res := &base.Resource{
		Range: false,
		Files: []*base.FileInfo{},
	}

	if resp.StatusCode != base.HttpCodeOK && resp.StatusCode != base.HttpCodePartialContent {
		resp.Body.Close()
		f.setState(stateError)
		re := newResponseError(resp)
		f.diag.failure(re)
		// Requeue rate-limited first requests until the source cooldown ends,
		// a bounded number of times before reporting the server error. A wait
		// beyond a day is reported at once rather than shortened.
		if transfer.IsRateLimitStatus(re.Code) && admission != nil {
			f.diag.rateLimited()
			until, _ := admission.RateLimitedEpisode(re.Code, re.RetryAfter, re.HasRetryAfter)
			if time.Until(until) <= transfer.MaxRetryAfter && f.deferrals.Add(1) <= maxDeferrals {
				return &transfer.DeferredError{Until: until, Status: re.Code}
			}
		}
		return re
	}
	f.deferrals.Store(0)
	if admission != nil {
		admission.Succeeded()
	}

	// Check if server supports range requests
	acceptRanges := resp.Header.Get(base.HttpHeaderAcceptRanges)
	contentRange := resp.Header.Get(base.HttpHeaderContentRange)
	if acceptRanges == base.HttpHeaderBytes || strings.HasPrefix(contentRange, base.HttpHeaderBytes) {
		res.Range = true
		f.diag.setRange(rangeDeclared, "")
	}

	// Get content length from Content-Length header
	contentLength := resp.Header.Get(base.HttpHeaderContentLength)
	if contentLength != "" {
		parse, err := strconv.ParseInt(contentLength, 10, 64)
		if err == nil {
			res.Size = parse
		}
	}
	if !res.Range {
		if f.ctl.Budget != nil && !f.ctl.Budget.Probe(f.sourceKey()) {
			// Never wait for a second permit while holding the first response.
			f.diag.setRange(rangeNotProbed, "sourceBusy")
		} else {
			var state, reason string
			res.Range, state, reason = probeRangeSupport(f.requestContext(parent), client, probeReq, resp, res.Size, extra.Connections)
			f.diag.setRange(state, reason)
		}
	}

	// Parse last modified time
	var lastModifiedTime *time.Time
	lastModified := resp.Header.Get(base.HttpHeaderLastModified)
	if lastModified != "" {
		t, _ := time.Parse(time.RFC1123, lastModified)
		lastModifiedTime = &t
	}

	file := &base.FileInfo{
		Size:  res.Size,
		Ctime: lastModifiedTime,
	}

	// Parse filename
	contentDisposition := resp.Header.Get(base.HttpHeaderContentDisposition)
	if contentDisposition != "" {
		file.Name = parseFilename(contentDisposition)
	}
	if file.Name == "" {
		file.Name = path.Base(httpReq.URL.Path)
		if file.Name != "" {
			// Use PathUnescape instead of QueryUnescape to correctly handle %2B (should decode to +, not space)
			file.Name, _ = url.PathUnescape(file.Name)
		}
	}
	if file.Name == "" || file.Name == "/" || file.Name == "." {
		file.Name = httpReq.URL.Hostname()
	}

	res.Files = append(res.Files, file)
	f.meta.Res = res

	// Save redirect URL for later connections
	f.redirectURL = resp.Request.URL.String()

	// IMPORTANT: Keep the response body open for downloading in Start phase
	// This is crucial for one-time URLs that can only be accessed once
	f.resolveRespLock.Lock()
	f.resolveResp = resp
	f.resolveRespLock.Unlock()

	// Publish prefetch lifecycle channels before resolvedCh lets Start proceed.
	// Non-range responses remain open for the one-time download connection.
	keepResponse = true
	if res.Range && res.Size > 0 {
		f.prefetchStopCh = make(chan struct{})
		f.prefetchDoneCh = make(chan struct{})
		go f.asyncPrefetch()
	}
	f.setState(stateResolved)
	f.resolvedOnce.Do(func() {
		close(f.resolvedCh)
	})

	// If start was called before resolve completed, auto-start
	if f.startPending.Load() {
		go func() {
			if err := f.doStart(); err != nil {
				f.setState(stateError)
				select {
				case f.doneCh <- err:
				default:
				}
			}
		}()
	}

	return nil
}

// asyncPrefetch downloads data in background during resolve phase
// This data can be reused when Start is called to save time
func (f *Fetcher) asyncPrefetch() {
	defer func() {
		close(f.prefetchDoneCh)
	}()

	// Get the resolve response
	f.resolveRespLock.Lock()
	resp := f.resolveResp
	f.resolveRespLock.Unlock()

	if resp == nil {
		return
	}

	// Create temporary file for prefetch data
	tmpFile, err := os.CreateTemp("", "gopeed-prefetch-*")
	if err != nil {
		f.prefetchErr = err
		return
	}
	f.prefetchFile = tmpFile
	f.prefetchFilePath = tmpFile.Name()

	defer func() {
		// Close response body when prefetch stops
		f.resolveRespLock.Lock()
		if f.resolveResp != nil {
			f.resolveResp.Body.Close()
			f.resolveResp = nil
		}
		f.resolveRespLock.Unlock()
	}()

	buf := make([]byte, 32*1024) // 32KB buffer
	reader := NewTimeoutReader(resp.Body, readTimeout)

	for {
		select {
		case <-f.prefetchStopCh:
			// Stop signal received (Start was called)
			return
		default:
		}

		n, err := reader.Read(buf)
		if n > 0 {
			_, writeErr := tmpFile.Write(buf[:n])
			if writeErr != nil {
				f.prefetchErr = writeErr
				return
			}
			f.prefetchSize.Add(int64(n))
		}
		if err != nil {
			if err == io.EOF {
				// Prefetch completed
				return
			}
			f.prefetchErr = err
			return
		}
	}
}

// prefetchRunning reports whether the resolve-phase prefetch is still reading.
func (f *Fetcher) prefetchRunning() bool {
	if f.prefetchDoneCh == nil {
		return false
	}
	select {
	case <-f.prefetchDoneCh:
		return false
	default:
		return true
	}
}

// awaitPrefetch lets a small file's first response finish, bounded so that a
// stalled server still falls back to a range request. False means paused.
func (f *Fetcher) awaitPrefetch() bool {
	timer := time.NewTimer(smallFileDrainTimeout)
	defer timer.Stop()
	select {
	case <-f.prefetchDoneCh:
	case <-timer.C:
	case <-f.ctx.Done():
		return false
	}
	return true
}

// adoptPrefetch stops the resolve-phase prefetch and copies its bytes into the
// target file. On failure nothing is counted, so a retry starts at byte zero.
func (f *Fetcher) adoptPrefetch() error {
	prefetchedBytes, err := f.stopPrefetchAndCopyData()
	if err != nil {
		// Do not publish a partial prefix as downloaded. A retry will fetch
		// from byte zero now that the prefetch file has been consumed.
		f.resolveDataPos.Store(0)
		f.fileMu.Lock()
		if f.file != nil {
			f.file.Close()
			f.file = nil
		}
		f.fileMu.Unlock()
		f.setState(stateError)
		return fmt.Errorf("copy prefetch data: %w", err)
	}
	f.resolveDataPos.Store(prefetchedBytes)

	// Also close resolve response if still open
	f.resolveRespLock.Lock()
	if f.resolveResp != nil {
		f.resolveResp.Body.Close()
		f.resolveResp = nil
	}
	f.resolveRespLock.Unlock()
	return nil
}

// stopPrefetch interrupts any blocked response read and joins its owner before
// the caller accesses the temporary file. A timeout is not proof of completion.
func (f *Fetcher) stopPrefetch() {
	if f.prefetchStopCh == nil {
		return
	}
	f.resolveRespLock.Lock()
	select {
	case <-f.prefetchStopCh:
	default:
		close(f.prefetchStopCh)
	}
	resp := f.resolveResp
	f.resolveResp = nil
	f.resolveRespLock.Unlock()
	if resp != nil {
		resp.Body.Close()
	}
	<-f.prefetchDoneCh
}

// stopPrefetchAndCopyData joins prefetch, then copies its bytes to the target.
func (f *Fetcher) stopPrefetchAndCopyData() (int64, error) {
	f.stopPrefetch()
	defer f.cleanupPrefetchFile()

	// Consume the prefetch once; retries and resumes must not reuse its count
	// after the temporary file has been removed.
	prefetched := f.prefetchSize.Swap(0)
	if prefetched == 0 {
		return 0, nil
	}
	if f.prefetchFile == nil || f.file == nil {
		return 0, fmt.Errorf("prefetch copy requires source and target files")
	}
	return copyPrefetchData(f.file, f.prefetchFile, prefetched)
}

func copyPrefetchData(target io.WriterAt, source io.ReadSeeker, size int64) (int64, error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	// Bound reads to the recorded prefix and count only bytes actually written.
	// Preserve copy errors even when the writer reports the full byte count.
	copied, err := io.Copy(io.NewOffsetWriter(target, 0), io.LimitReader(source, size))
	if err == nil && copied != size {
		err = io.ErrUnexpectedEOF
	}
	return copied, err
}

// cleanupPrefetchFile closes and removes the prefetch temporary file
func (f *Fetcher) cleanupPrefetchFile() {
	f.prefetchSize.Store(0)
	if f.prefetchFile != nil {
		f.prefetchFile.Close()
		f.prefetchFile = nil
	}
	if f.prefetchFilePath != "" {
		os.Remove(f.prefetchFilePath)
		f.prefetchFilePath = ""
	}
}

func (f *Fetcher) Start() error {
	state := f.getState()

	switch state {
	case stateResolved, statePaused:
		// Normal case: resolved or resuming from pause
		return f.doStart()

	case stateResolving:
		// Early start: mark pending and return immediately
		f.startPending.Store(true)
		return nil

	case stateSlowStart, stateSteady:
		// Already downloading, this is a resume from pause
		return f.doStart()

	case stateError:
		// Retry after error: reset and restart
		return f.doStart()

	default:
		return fmt.Errorf("cannot start in current state: %v", state)
	}
}

func (f *Fetcher) doStart() error {
	// Wait for resolve to complete
	<-f.resolvedCh

	state := f.getState()
	if state == stateDone {
		return nil
	}

	// If retrying after error, reset connection states for retry
	if state == stateError {
		// Completion signals precede the loop's deferred cleanup. Join the old
		// loop before replacing its context/channel for a retry.
		if f.downloadLoopDone != nil {
			<-f.downloadLoopDone
		}
		// Drain any pending error from doneCh before retry
		select {
		case <-f.doneCh:
		default:
		}
	}

	// OnStart extensions may replace Extra with a JSON object after Restore.
	// Normalize it before any resume probe builds a request.
	if err := base.ParseReqExtra[fhttp.ReqExtra](f.meta.Req); err != nil {
		return err
	}
	if err := f.upgradeRangeResume(); err != nil {
		return err
	}

	if state == stateError {
		f.connMu.Lock()
		for _, conn := range f.connections {
			// Reset connections that can be retried
			if !conn.Completed && conn.State != connCompleted {
				f.resetConnectionForRestart(conn)
				conn.State = connNotStarted
				conn.failed = false
				conn.retryTimes = 0
				conn.lastErr = nil
			}
		}
		f.connMu.Unlock()
	}

	// Open or create target file first (needed for prefetch copy)
	name := f.meta.SingleFilepath()
	var err error
	var file *os.File
	_, err = os.Stat(name)
	if err != nil {
		if os.IsNotExist(err) {
			file, err = f.ctl.Touch(name, f.meta.Res.Size)
		} else {
			return err
		}
	} else {
		file, err = os.OpenFile(name, os.O_RDWR, os.ModeAppend)
	}
	if err != nil {
		return err
	}
	f.fileMu.Lock()
	f.file = file
	f.fileMu.Unlock()

	// For range-supported resources, stop prefetch and copy data
	// For non-range resources, the response will be used directly
	f.drainPrefetch = false
	if f.meta.Res.Range {
		if len(f.connections) == 0 && f.meta.Res.Size > 0 && f.meta.Res.Size < 2*stealMinChunkSize && f.prefetchRunning() {
			// A file too small to split finishes on its first response; the
			// download loop waits for it so that response's connection stays
			// reusable instead of being cut off for one more range request.
			f.drainPrefetch = true
			f.resolveDataPos.Store(0)
		} else if err = f.adoptPrefetch(); err != nil {
			return err
		}
	}

	// Avoid request extra modified by extension
	if err = base.ParseReqExtra[fhttp.ReqExtra](f.meta.Req); err != nil {
		return err
	}

	// Initialize slow start controller
	maxConns := f.meta.Opts.Extra.(*fhttp.OptsExtra).Connections
	f.configuredStreams.Store(int32(maxConns))
	f.slowStart = newSlowStartController(maxConns)

	// Create main context
	f.ctx, f.cancel = context.WithCancel(context.Background())

	// Create downloadLoop lifecycle channel
	f.downloadLoopDone = make(chan struct{})
	f.streamExit = make(chan struct{}, 1)
	f.deferred.Store(nil)
	f.diag.startRun(time.Now())
	f.joinBudget()

	// Start download
	f.setState(stateSlowStart)
	go f.downloadLoop()

	return nil
}

func (f *Fetcher) downloadLoop() {
	defer func() {
		f.leaveBudget()
		if deferred := f.deferred.Load(); deferred != nil {
			// Report a deferral only after every stream stopped, so the saved
			// ranges and progress are final when the scheduler requeues it.
			f.wg.Wait()
			// Budget changes can request deferral before a stream finishes
			// classifying its refusal. An exhausted retry budget or excessive
			// Retry-After must remain terminal after all streams have stopped.
			var result error = deferred
			f.connMu.Lock()
			for _, conn := range f.connections {
				if re := extractRequestError(conn.lastErr); conn.failed && re != nil && transfer.IsRateLimitStatus(re.Code) {
					result = fmt.Errorf("source rate limit exhausted: %w", re)
					f.deferred.Store(nil)
					break
				}
			}
			f.connMu.Unlock()
			f.fileMu.Lock()
			if f.file != nil {
				f.file.Close()
				f.file = nil
			}
			f.fileMu.Unlock()
			f.setState(stateError)
			select {
			case f.doneCh <- result:
			default:
			}
		}

		// Update file last modified time before closing
		if f.config.UseServerCtime && f.meta.Res.Files[0].Ctime != nil {
			setft.SetFileTime(f.meta.SingleFilepath(), time.Now(), *f.meta.Res.Files[0].Ctime, *f.meta.Res.Files[0].Ctime)
		}

		// Signal that downloadLoop has exited
		if f.downloadLoopDone != nil {
			close(f.downloadLoopDone)
		}
	}()

	// A run that starts inside a long source cooldown releases its slot now.
	if f.deferIfCooling() {
		return
	}

	// Check if this is a resume or fresh start
	isResume := len(f.connections) > 0
	expand := true

	if !isResume {
		if f.drainPrefetch {
			if !f.awaitPrefetch() {
				return
			}
			if err := f.adoptPrefetch(); err != nil {
				select {
				case f.doneCh <- err:
				default:
				}
				return
			}
		}
		// Fresh start: begin with resolve connection
		f.startResolveDownload()
		if f.getState() == stateDone {
			return
		}
		if !f.meta.Res.Range || f.meta.Res.Size == 0 {
			return
		}
	} else {
		// Resume: restart existing connections
		f.resumeConnections()
		if !f.meta.Res.Range {
			// Resumed single streams can wait in a source cooldown too. Keep
			// listening for queued work while they wait, without adding ranges.
			f.superviseStreams(false)
			return
		}
		// Upgraded legacy prefixes expand; other resumes keep their streams.
		expand = f.rangeResumeValidator != ""
	}
	f.superviseStreams(expand)
}

// superviseStreams runs until every range is downloaded or the run stops. It
// expands streams in slow-start batches, follows source budget changes (cooldown
// ends, freed or regained capacity) and hands work back when an adaptive
// target drops. Every byte range always belongs to a connection: running,
// parked, completed or failed.
func (f *Fetcher) superviseStreams(expand bool) {
	adaptive := f.tuning().adaptive
	var governor <-chan time.Time
	// The budget and queue signals are kept until they fire and taken again
	// before acting on them, so a change while the loop handles another event
	// is not lost.
	var changed, queued <-chan struct{}
	m := f.member.Load()
	if m != nil {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		governor = ticker.C
		changed, queued = m.Changed(), f.queueChanged()
		// A stream refused before the signals were taken saw the queue as it
		// was then.
		f.deferIfCooling()
	}
	for {
		if f.liveStreams.Load() == 0 && f.ctx.Err() == nil &&
			f.launchParked() == 0 && !f.hasParkedWork() {
			f.waitForCompletion()
			return
		}
		select {
		case <-f.ctx.Done():
			// Paused, cancelled or deferred
			return
		case <-f.slowStart.expansionCh:
			// Batch completed, try to expand
			if expand && !f.checkCompletion() {
				f.expandConnections()
			}
		case <-changed:
			changed = m.Changed()
			if f.deferIfCooling() {
				return
			}
			f.launchParked()
			if expand && (adaptive || f.getState() != stateSteady) && !f.checkCompletion() {
				f.expandConnections()
			}
		case <-queued:
			// Another task joined the queue: a run idling in a source cooldown
			// gives its slot up if that is now due.
			queued = f.queueChanged()
			f.deferIfCooling()
		case <-governor:
			if f.meta.Res.Range {
				f.govern()
			}
		case <-f.streamExit:
		}
	}
}

func (f *Fetcher) startResolveDownload() {
	// If no range support or size unknown, just use single connection with resolve response
	if !f.meta.Res.Range || f.meta.Res.Size == 0 {
		// Create a single connection for the entire file
		conn := &connection{
			ID:    0,
			Role:  rolePrimary,
			State: connNotStarted,
			Chunk: newChunk(0, 0), // For non-range, end doesn't matter
		}
		conn.ctx, conn.cancel = context.WithCancel(f.ctx)
		f.connMu.Lock()
		f.connections = append(f.connections, conn)
		f.connMu.Unlock()

		f.wg.Add(1)
		// Use the resolve response directly
		go f.runConnectionWithResolveResp(conn)

		// For non-range downloads, wait for completion directly in this goroutine
		// Don't create another goroutine to avoid WaitGroup reuse issues
		f.waitForCompletion()
		return
	}

	// Range supported: use slow start to launch connections
	// Start first batch of connections
	f.expandConnections()
}

func (f *Fetcher) expandConnections() {
	if f.ctx != nil && f.ctx.Err() != nil {
		return
	}
	live := -1
	if f.tuning().adaptive && f.member.Load() != nil {
		live = int(f.liveStreams.Load())
	}
	batchSize := f.slowStart.getNextBatchSize(live)
	if batchSize <= 0 {
		// Max reached, transition to steady state
		f.setState(stateSteady)
		// Don't start a new goroutine - let the downloadLoop handle completion
		// This avoids multiple goroutines calling wg.Wait() simultaneously
		return
	}

	totalSize := f.meta.Res.Size

	f.connMu.Lock()

	// For first batch (no existing connections), allocate the remaining file to first connection
	if len(f.connections) == 0 {
		// Check if we have prefetched data
		prefetched := f.resolveDataPos.Load()

		// If prefetched all data, mark as done
		if prefetched >= totalSize {
			f.resolveConn = &connection{ID: 0, Role: roleResolve, State: connCompleted, Downloaded: totalSize, Completed: true}
			f.connMu.Unlock()

			// Close the file before signaling completion
			f.fileMu.Lock()
			if f.file != nil {
				f.file.Close()
				f.file = nil
			}
			f.fileMu.Unlock()

			f.setState(stateDone)
			f.doneCh <- nil
			return
		}

		// First connection starts from prefetched position
		conn := &connection{
			ID:    0,
			Role:  rolePrimary,
			State: connNotStarted,
			Chunk: newChunk(prefetched, totalSize-1),
		}
		// Mark prefetched bytes as already downloaded
		conn.Chunk.Downloaded = 0    // Start fresh from prefetched position
		conn.Downloaded = prefetched // Track total downloaded including prefetch

		conn.ctx, conn.cancel = context.WithCancel(f.ctx)
		f.connections = append(f.connections, conn)
		// A cooling-down source parks the whole range until it may be fetched.
		permitted := f.tryPermit()
		conn.parked = !permitted
		f.connMu.Unlock()

		if permitted {
			conn.permit = true
			f.slowStart.commitBatch(1)
			f.startStream(conn)
		}
		return
	}
	f.connMu.Unlock()

	// Parked ranges get streams before any running range is split again.
	launched := f.launchParked()

	// For subsequent batches, use "help other connection" strategy
	// Find connections with enough remaining work to split
	// During slow start, use fixed minimum size since speed is not yet stable
	minSplitSize := int64(stealMinChunkSize)

	f.connMu.Lock()
	denied := false
	newConns := make([]*connection, 0, batchSize)
	for i := launched; i < batchSize; i++ {
		// Find the connection with most remaining work
		var maxRemainConn *connection
		var maxRemain int64

		for _, conn := range f.connections {
			if conn.Completed || conn.State == connFailed || conn.parked || conn.shedding {
				continue
			}
			remain := conn.Chunk.remain()
			// Only split if remaining work is at least 2x the minimum split size
			if remain > maxRemain && remain > minSplitSize*2 {
				maxRemainConn = conn
				maxRemain = remain
			}
		}

		if maxRemainConn == nil {
			// No connection has enough work to split
			break
		}
		if !f.tryPermit() {
			denied = true
			break
		}

		// Split the work: new connection takes the latter half
		splitPoint := maxRemainConn.Chunk.End - maxRemainConn.Chunk.remain()/2
		newChunk := newChunk(splitPoint+1, maxRemainConn.Chunk.End)
		maxRemainConn.Chunk.End = splitPoint

		connID := len(f.connections)
		conn := &connection{
			ID:     connID,
			Role:   roleWorker,
			State:  connNotStarted,
			Chunk:  newChunk,
			permit: true,
		}
		conn.ctx, conn.cancel = context.WithCancel(f.ctx)

		newConns = append(newConns, conn)
		f.connections = append(f.connections, conn)
	}

	f.connMu.Unlock()

	if !denied && launched+len(newConns) < batchSize {
		// Out of work to split: stop counting as a task that wants streams.
		if m := f.member.Load(); m != nil {
			m.NoDemand()
		}
	}
	if launched+len(newConns) == 0 {
		// Nothing could start. Without a budget refusal there is no work left
		// to split, so expansion is over; a refusal is retried on a budget change.
		if !denied {
			f.setState(stateSteady)
		}
		return
	}

	// Commit batch to slow start controller
	f.slowStart.commitBatch(launched + len(newConns))

	// Launch connections
	for _, conn := range newConns {
		f.startStream(conn)
	}
}

func (f *Fetcher) runConnection(conn *connection) {
	defer f.wg.Done()
	defer f.streamExited()
	defer f.releasePermit(conn)

	f.connMu.Lock()
	conn.State = connConnecting
	f.connMu.Unlock()

	// Use fast-fail client for quick retry during download phase
	client := f.buildFastFailClient()
	buf := transfer.GetBuffer(f.tuning().readBuffer)
	defer transfer.PutBuffer(buf)

	retries := 0
	rateLimits := 0
	f.connMu.Lock()
	conn.retryTimes = 0
	f.connMu.Unlock()

	for {
		// No new request reaches a source that asked clients to wait: a stream
		// moving to its next chunk gives its permit back during a cooldown.
		if m := f.member.Load(); m != nil && conn.permit && m.CoolingDown() {
			f.releasePermit(conn)
		}
		// A stream that gave its permit back waits for the budget, which also
		// keeps it out of a source cooldown.
		if !conn.permit {
			if err := f.waitPermit(conn); err != nil {
				return
			}
		}
		// Rebuild client with updated fast-fail timeout on retries
		if retries > 0 {
			client = f.buildFastFailClient()
		}

		err := f.downloadChunkOnce(conn, client, buf)
		if err == nil {
			rateLimits = 0
			f.connMu.Lock()
			shedding := conn.shedding
			f.connMu.Unlock()
			if shedding || !f.meta.Res.Range || f.yieldStream() || !f.helpOtherConnection(conn) {
				f.connMu.Lock()
				conn.Completed = true
				conn.State = connCompleted
				f.connMu.Unlock()
				return
			}

			// Reset counters after a successful help switch
			retries = 0
			f.connMu.Lock()
			conn.retryTimes = 0
			f.connMu.Unlock()
			continue
		}

		if errors.Is(err, context.Canceled) {
			return
		}

		if re := extractRequestError(err); re != nil {
			conn.lastErr = re
		} else {
			conn.lastErr = err
		}
		f.diag.failure(err)

		// 429/503 pause the whole source: no stream retries early, and a long
		// server-requested wait releases the task's running slot.
		if re := extractRequestError(err); re != nil && transfer.IsRateLimitStatus(re.Code) {
			rateLimits++
			wait, deferred, giveUp := f.rateLimit(re, rateLimits)
			f.connMu.Lock()
			conn.State = connFailed
			conn.failed = giveUp
			f.connMu.Unlock()
			if giveUp {
				if f.slowStart != nil {
					f.slowStart.onConnectFailed()
				}
				return
			}
			if deferred != nil {
				f.deferRun(deferred)
				return
			}
			f.releasePermit(conn)
			if f.member.Load() == nil {
				start := time.Now()
				if !sleepContext(conn.ctx, wait) {
					return
				}
				f.diag.waited(time.Since(start))
			}
			retries++
			continue
		}

		if shouldCountHTTPFailure(err) || errors.Is(err, errInvalidRangeResponse) {
			if re := extractRequestError(err); re != nil && re.Code == 403 {
				f.connMu.Lock()
				conn.State = connFailed
				conn.failed = true
				// 403 usually means the server limits connections. Leave the rest
				// of this range for a stream that already holds a connection,
				// instead of orphaning it; a range refused again and again is a
				// real failure.
				if f.meta.Res.Range && conn.Chunk.remain() > 0 && conn.handoffs < maxHandoffs {
					conn.handoffs++
					conn.parked = true
				}
				f.connMu.Unlock()
				if f.slowStart != nil {
					f.slowStart.onConnectFailed()
				}
				return
			}
			f.connMu.Lock()
			conn.retryTimes++
			conn.failed = true
			f.connMu.Unlock()
			if f.slowStart != nil {
				f.slowStart.onConnectFailed()
			}
			if conn.retryTimes >= 3 {
				f.connMu.Lock()
				conn.State = connFailed
				f.connMu.Unlock()
				return
			}
		}

		f.connMu.Lock()
		conn.State = connFailed
		f.connMu.Unlock()
		retryDelay := time.Second * time.Duration(retries+1)
		if retryDelay > 5*time.Second {
			retryDelay = 5 * time.Second
		}
		retries++
		if !sleepContext(conn.ctx, retryDelay) {
			return
		}
	}
}

// downloadChunkOnce performs a single HTTP request for the current chunk without retrying.
// If the redirect URL fails with an expiration-related error (401, 403, 410),
// it will automatically retry with the original URL and update the redirect URL on success.
func (f *Fetcher) downloadChunkOnce(conn *connection, client *http.Client, buf []byte) error {
	if conn.ctx.Err() != nil {
		return conn.ctx.Err()
	}

	// Read chunk boundaries under lock to get a consistent snapshot
	// This protects against concurrent modification by helpOtherConnection
	f.connMu.Lock()
	if f.meta.Res.Range && conn.Chunk.remain() <= 0 {
		f.connMu.Unlock()
		return nil
	}
	rangeStart := conn.Chunk.Begin + conn.Chunk.Downloaded
	rangeEnd := conn.Chunk.End
	f.connMu.Unlock()

	httpReq, err := f.buildRequest(f.requestContext(conn.ctx), f.meta.Req)
	if err != nil {
		return err
	}

	if f.meta.Res.Range {
		httpReq.Header.Set(base.HttpHeaderRange,
			fmt.Sprintf(base.HttpHeaderRangeFormat, rangeStart, rangeEnd))
		if f.rangeResumeValidator != "" {
			httpReq.Header.Set("If-Range", f.rangeResumeValidator)
			httpReq.Header.Set("Accept-Encoding", "identity")
		}
	}

	// Record connection start time for adaptive timeout tracking
	connStartTime := time.Now()

	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}

	if resp.StatusCode != base.HttpCodeOK && resp.StatusCode != base.HttpCodePartialContent {
		resp.Body.Close()
		originalErr := newResponseError(resp)

		// Check if this might be a redirect URL expiration error
		// If so, try falling back to the original URL
		if f.hasRedirectURL() && isRedirectExpiredError(originalErr) {
			fallbackResp, fallbackErr := f.tryFallbackToOriginalURL(conn.ctx, client, rangeStart, rangeEnd)
			if fallbackErr == nil && fallbackResp != nil {
				// Fallback succeeded, use this response instead
				resp = fallbackResp
				// Update the redirect URL from the response
				if resp.Request != nil && resp.Request.URL != nil {
					f.updateRedirectURL(resp.Request.URL.String())
				}
			} else {
				// Fallback also failed, return the original error
				if fallbackResp != nil {
					fallbackResp.Body.Close()
				}
				return originalErr
			}
		} else {
			return originalErr
		}
	}
	defer resp.Body.Close()
	if f.meta.Res.Range {
		begin, end, total, valid := parseContentRange(resp.Header.Get(base.HttpHeaderContentRange))
		if resp.StatusCode != http.StatusPartialContent || !valid ||
			begin != rangeStart || end < rangeEnd || total != f.meta.Res.Size ||
			(resp.ContentLength >= 0 && resp.ContentLength < rangeEnd-rangeStart+1) {
			return fmt.Errorf("%w: status=%d, Content-Range=%q", errInvalidRangeResponse,
				resp.StatusCode, resp.Header.Get(base.HttpHeaderContentRange))
		}
		if f.rangeResumeValidator != "" {
			modified, err := http.ParseTime(resp.Header.Get("Last-Modified"))
			encoding := resp.Header.Get("Content-Encoding")
			if err != nil || modified.UTC().Format(http.TimeFormat) != f.rangeResumeValidator ||
				resp.Uncompressed || (encoding != "" && !strings.EqualFold(encoding, "identity")) {
				return fmt.Errorf("%w: saved representation changed", errInvalidRangeResponse)
			}
		}
	}

	// Record successful connection time for adaptive timeout
	f.updateMaxConnTime(time.Since(connStartTime))
	f.diag.response(resp)
	f.deferrals.Store(0)
	member := f.member.Load()
	if member != nil {
		member.Succeeded()
	}

	f.connMu.Lock()
	conn.State = connDownloading
	conn.failed = false
	f.connMu.Unlock()

	if conn.Role == rolePrimary || conn.ID == 0 {
		f.primaryReadyOnce.Do(func() {
			close(f.primaryReadyCh)
		})
	}
	if f.slowStart != nil {
		f.slowStart.onConnectSuccess()
	}

	reader := NewTimeoutReader(resp.Body, readTimeout)
	for {
		if conn.ctx.Err() != nil {
			return conn.ctx.Err()
		}

		n, err := reader.Read(buf)
		if n > 0 {
			finished := false
			var writeOffset int64

			// Lock to safely read chunk state and calculate write parameters
			// This protects against concurrent chunk splitting by helpOtherConnection
			f.connMu.Lock()
			if f.meta.Res.Range {
				// Check current chunk boundaries - this respects any concurrent chunk splitting
				remain := conn.Chunk.remain()
				if remain <= 0 {
					// Chunk has been fully downloaded (possibly split and reduced)
					f.connMu.Unlock()
					return nil
				}
				if remain < int64(n) {
					n = int(remain)
					finished = true
				}
			}
			writeOffset = conn.Chunk.Begin + conn.Chunk.Downloaded
			f.connMu.Unlock()

			f.fileMu.RLock()
			if f.file != nil {
				_, writeErr := f.file.WriteAt(buf[:n], writeOffset)
				if writeErr != nil {
					f.fileMu.RUnlock()
					return writeErr
				}
			}
			f.fileMu.RUnlock()

			// Lock again to update Downloaded atomically with the read above
			f.connMu.Lock()
			conn.Chunk.Downloaded += int64(n)
			conn.Downloaded += int64(n)
			if member != nil {
				member.Transferred(int64(n))
			}
			wrote := time.Now()
			f.diag.data(wrote)
			// Update connection speed periodically
			now := wrote.UnixNano()
			if conn.lastSpeedCheck == 0 {
				conn.lastSpeedCheck = now
				conn.lastSpeedDownload = conn.Downloaded
			} else if now-conn.lastSpeedCheck >= int64(500*time.Millisecond) {
				elapsed := float64(now-conn.lastSpeedCheck) / float64(time.Second)
				if elapsed > 0 {
					conn.speed = int64(float64(conn.Downloaded-conn.lastSpeedDownload) / elapsed)
				}
				conn.lastSpeedCheck = now
				conn.lastSpeedDownload = conn.Downloaded
			}
			f.connMu.Unlock()

			if finished {
				return nil
			}
		}

		if err != nil {
			if err == io.EOF {
				f.connMu.Lock()
				incomplete := f.meta.Res.Range && conn.Chunk.remain() > 0
				f.connMu.Unlock()
				if incomplete {
					return fmt.Errorf("%w: short range body", errInvalidRangeResponse)
				}
				return nil
			}
			if f.meta.Res.Range && errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("%w: truncated range body", errInvalidRangeResponse)
			}
			return err
		}
	}
}

// runConnectionWithResolveResp uses the response body from Resolve phase
// This is crucial for one-time URLs that can only be accessed once
func (f *Fetcher) runConnectionWithResolveResp(conn *connection) {
	defer f.wg.Done()

	f.connMu.Lock()
	conn.State = connConnecting
	f.connMu.Unlock()

	// The single stream of a non-range download counts against its source
	// budget for as long as it runs.
	member := f.member.Load()
	if member != nil {
		member.Hold()
		defer member.Release()
	}

	buf := transfer.GetBuffer(f.tuning().readBuffer)
	defer transfer.PutBuffer(buf)

	// Get the resolve response
	f.resolveRespLock.Lock()
	resp := f.resolveResp
	f.resolveResp = nil // Take ownership
	f.resolveRespLock.Unlock()

	if resp == nil {
		// No resolve response available, fall back to normal connection
		f.runConnectionFallback(conn)
		return
	}

	defer resp.Body.Close()

	f.connMu.Lock()
	conn.State = connDownloading
	conn.failed = false
	f.connMu.Unlock()

	// Signal primary ready
	f.primaryReadyOnce.Do(func() {
		close(f.primaryReadyCh)
	})
	if f.slowStart != nil {
		f.slowStart.onConnectSuccess()
	}

	// Download data from resolve response
	reader := NewTimeoutReader(resp.Body, readTimeout)
	for {
		if conn.ctx.Err() != nil {
			return
		}

		n, err := reader.Read(buf)
		if n > 0 {
			f.fileMu.RLock()
			if f.file != nil {
				_, writeErr := f.file.WriteAt(buf[:n], conn.Chunk.Downloaded)
				if writeErr != nil {
					f.fileMu.RUnlock()
					f.connMu.Lock()
					conn.State = connFailed
					conn.failed = true
					f.connMu.Unlock()
					if f.slowStart != nil {
						f.slowStart.onConnectFailed()
					}
					return
				}
			}
			f.fileMu.RUnlock()

			f.connMu.Lock()
			conn.Chunk.Downloaded += int64(n)
			conn.Downloaded += int64(n)
			f.connMu.Unlock()
			if member != nil {
				member.Transferred(int64(n))
			}
			f.diag.data(time.Now())
		}
		if err != nil {
			if err == io.EOF {
				f.connMu.Lock()
				conn.Completed = true
				conn.State = connCompleted
				f.connMu.Unlock()
				return
			}
			f.diag.failure(err)
			// Reading from resolve response failed: treat as transient (do not count as fail)
			f.connMu.Lock()
			conn.State = connFailed
			f.connMu.Unlock()
			return
		}
	}
}

// runConnectionFallback is used when resolve response is not available
func (f *Fetcher) runConnectionFallback(conn *connection) {
	// Use fast-fail client for quick retry during download phase
	client := f.buildFastFailClient()
	buf := transfer.GetBuffer(f.tuning().readBuffer)
	defer transfer.PutBuffer(buf)
	member := f.member.Load()

	retries := 0
	countedRetries := 0
	rateLimits := 0

	for {
		if conn.ctx.Err() != nil {
			return
		}

		// Rebuild client with updated fast-fail timeout on retries
		if retries > 0 {
			client = f.buildFastFailClient()
		}

		f.connMu.Lock()
		conn.State = connConnecting
		f.connMu.Unlock()

		err := func() error {
			httpReq, err := f.buildRequest(f.requestContext(conn.ctx), f.meta.Req)
			if err != nil {
				return err
			}

			// Record connection start time for adaptive timeout tracking
			connStartTime := time.Now()

			resp, err := client.Do(httpReq)
			if err != nil {
				return err
			}
			defer resp.Body.Close()

			if resp.StatusCode != base.HttpCodeOK && resp.StatusCode != base.HttpCodePartialContent {
				return newResponseError(resp)
			}

			// Record successful connection time for adaptive timeout
			f.updateMaxConnTime(time.Since(connStartTime))
			f.diag.response(resp)
			f.deferrals.Store(0)
			if member != nil {
				member.Succeeded()
			}

			f.connMu.Lock()
			conn.State = connDownloading
			conn.failed = false
			f.connMu.Unlock()

			f.primaryReadyOnce.Do(func() {
				close(f.primaryReadyCh)
			})
			if f.slowStart != nil {
				f.slowStart.onConnectSuccess()
			}

			reader := NewTimeoutReader(resp.Body, readTimeout)
			for {
				if conn.ctx.Err() != nil {
					return conn.ctx.Err()
				}

				n, err := reader.Read(buf)
				if n > 0 {
					f.fileMu.RLock()
					if f.file != nil {
						_, writeErr := f.file.WriteAt(buf[:n], conn.Chunk.Downloaded)
						if writeErr != nil {
							f.fileMu.RUnlock()
							return writeErr
						}
					}
					f.fileMu.RUnlock()

					f.connMu.Lock()
					conn.Chunk.Downloaded += int64(n)
					conn.Downloaded += int64(n)
					f.connMu.Unlock()
					if member != nil {
						member.Transferred(int64(n))
					}
					f.diag.data(time.Now())
				}
				if err != nil {
					if err == io.EOF {
						return nil
					}
					return err
				}
			}
		}()

		if err == nil {
			f.connMu.Lock()
			conn.Completed = true
			conn.State = connCompleted
			f.connMu.Unlock()
			return
		}

		if errors.Is(err, context.Canceled) {
			return
		}

		if re := extractRequestError(err); re != nil {
			conn.lastErr = re
		} else {
			conn.lastErr = err
		}
		f.diag.failure(err)

		if re := extractRequestError(err); re != nil && transfer.IsRateLimitStatus(re.Code) {
			rateLimits++
			wait, deferred, giveUp := f.rateLimit(re, rateLimits)
			f.connMu.Lock()
			conn.State = connFailed
			conn.failed = giveUp
			f.connMu.Unlock()
			if giveUp {
				return
			}
			if deferred != nil {
				f.deferRun(deferred)
				return
			}
			start := time.Now()
			if !sleepContext(conn.ctx, wait) {
				return
			}
			f.diag.waited(time.Since(start))
			retries++
			continue
		}

		if shouldCountHTTPFailure(err) {
			// Immediate fail for server connection limit (403)
			if re := extractRequestError(err); re != nil && re.Code == 403 {
				f.connMu.Lock()
				conn.State = connFailed
				conn.failed = true
				f.connMu.Unlock()
				if f.slowStart != nil {
					f.slowStart.onConnectFailed()
				}
				return
			}
			f.connMu.Lock()
			conn.retryTimes++
			f.connMu.Unlock()
			countedRetries++
			if countedRetries >= 3 {
				f.connMu.Lock()
				conn.State = connFailed
				conn.failed = true
				f.connMu.Unlock()
				if f.slowStart != nil {
					f.slowStart.onConnectFailed()
				}
				return
			}
			// Retry again for counted failures below the cap
			f.connMu.Lock()
			conn.State = connFailed
			f.connMu.Unlock()
			retryDelay := time.Second * time.Duration(retries+1)
			if retryDelay > 5*time.Second {
				retryDelay = 5 * time.Second
			}
			retries++
			if !sleepContext(conn.ctx, retryDelay) {
				return
			}
			continue
		}

		// Retry indefinitely for non-counted errors
		f.connMu.Lock()
		conn.State = connFailed
		f.connMu.Unlock()
		retryDelay := time.Second * time.Duration(retries+1)
		if retryDelay > 5*time.Second {
			retryDelay = 5 * time.Second
		}
		retries++
		if !sleepContext(conn.ctx, retryDelay) {
			return
		}
	}
}

// helpOtherConnection implements work stealing: when a connection finishes its chunk,
// it looks for connections that need more than stealThresholdSeconds to finish and steals half of its work.
func (f *Fetcher) helpOtherConnection(helper *connection) bool {
	f.connMu.Lock()
	defer f.connMu.Unlock()

	// A range handed back by a shed stream has no owner: take all of it.
	if f.takeParked(helper) {
		return true
	}

	// Find the connection with longest remaining time
	var slowestConn *connection
	var maxRemainSeconds int64
	for _, r := range f.connections {
		if r == helper || r.Completed || r.State == connFailed || r.parked {
			continue
		}

		remain := r.Chunk.remain()
		if remain < stealMinChunkSize {
			continue
		}

		// Calculate remaining time in seconds for this connection
		var remainSeconds int64
		if r.speed > 0 {
			remainSeconds = remain / r.speed
		} else {
			// Speed unknown, assume it needs help if chunk is large enough
			remainSeconds = stealThresholdSeconds + 1
		}

		// Only consider if it needs more than threshold seconds to finish
		if remainSeconds > stealThresholdSeconds && remainSeconds > maxRemainSeconds {
			slowestConn = r
			maxRemainSeconds = remainSeconds
		}
	}

	if slowestConn == nil {
		return false
	}

	// Re-calculate the chunk range: steal half of the remaining work
	helper.Chunk.Begin = slowestConn.Chunk.End - slowestConn.Chunk.remain()/2
	helper.Chunk.End = slowestConn.Chunk.End
	helper.Chunk.Downloaded = 0
	helper.handoffs = 0
	slowestConn.Chunk.End = helper.Chunk.Begin - 1
	return true
}

func (f *Fetcher) resetConnectionForRestart(conn *connection) {
	if f.meta.Res.Range {
		return
	}

	// Without range support a new request always starts from byte 0,
	// so pause/retry must restart instead of continuing from the old offset.
	if conn.Chunk == nil {
		conn.Chunk = newChunk(0, 0)
	} else {
		conn.Chunk.Begin = 0
		conn.Chunk.End = 0
		conn.Chunk.Downloaded = 0
	}
	conn.Downloaded = 0
	conn.Completed = false
	conn.speed = 0
	conn.lastSpeedCheck = 0
	conn.lastSpeedDownload = 0
}

func (f *Fetcher) resumeConnections() {
	// Collect connections to resume while holding the lock
	var toResume []*connection

	f.connMu.Lock()
	for _, conn := range f.connections {
		// Only skip connections that have truly completed successfully
		if conn.Completed || conn.State == connCompleted {
			continue
		}
		// For failed connections, skip if:
		// 1. They have exhausted retries (retryTimes >= 3), OR
		// 2. They failed with a permanent error like 403
		if conn.State == connFailed && conn.failed {
			// Check if it's a permanent error (like 403)
			if re := extractRequestError(conn.lastErr); re != nil && re.Code == 403 {
				continue
			}
			// Check if retries exhausted
			if conn.retryTimes >= 3 {
				continue
			}
		}
		f.resetConnectionForRestart(conn)
		// Reset the connection state for resume. A range parked or being shed
		// in the previous run gets its own stream now, so it must not also be
		// launched as parked work.
		conn.ctx, conn.cancel = context.WithCancel(f.ctx)
		conn.State = connNotStarted
		conn.failed = false // Clear failed flag for resumed connection
		conn.parked = false
		conn.shedding = false
		conn.handoffs = 0
		toResume = append(toResume, conn)
	}
	f.connMu.Unlock()

	// Start connections outside the lock
	if f.meta.Res.Range && f.rangeResumeValidator != "" {
		f.slowStart.commitBatch(len(toResume))
		if len(toResume) == 0 {
			f.slowStart.onConnectFailed()
		}
	}
	// Each resumed stream asks the budget for its permit before its first
	// request, so a cooldown or a lowered limit is respected on resume too.
	for _, conn := range toResume {
		conn.permit = false
		f.startStream(conn)
	}
}

func (f *Fetcher) waitForCompletion() {
	f.wg.Wait()
	// Only trigger completion if not cancelled/paused
	if f.ctx != nil && f.ctx.Err() == nil {
		f.onDownloadComplete()
	}
}

func (f *Fetcher) onDownloadComplete() {
	f.connMu.Lock()

	// First, check if download actually completed successfully
	// Calculate total downloaded from all connections
	totalDownloaded := int64(0)
	if f.resolveConn != nil {
		totalDownloaded += f.resolveConn.Downloaded
	}
	for _, conn := range f.connections {
		totalDownloaded += conn.Downloaded
	}

	// Check if all chunks are complete (no remaining bytes)
	allChunksComplete := true
	for _, conn := range f.connections {
		needsMoreData := false
		if f.meta.Res.Range {
			needsMoreData = conn.Chunk != nil && conn.Chunk.remain() > 0
		} else if f.meta.Res.Size > 0 {
			needsMoreData = conn.Downloaded < f.meta.Res.Size
		} else {
			needsMoreData = !conn.Completed && conn.State != connCompleted
		}

		if needsMoreData && !conn.Completed && conn.State != connCompleted {
			// This connection has remaining work and isn't done
			// Check if it failed with 403 (server limit) - these can be ignored if other connections completed the work
			if conn.State == connFailed && conn.failed {
				if re := extractRequestError(conn.lastErr); re != nil && re.Code == 403 {
					// 403 is server connection limit, check if other connections will complete this chunk
					continue
				}
			}
			allChunksComplete = false
			break
		}
	}

	// If total downloaded matches file size, consider it a success regardless of connection failures
	downloadComplete := f.meta.Res.Size > 0 && totalDownloaded >= f.meta.Res.Size
	// With a known size every byte must be accounted for; ranges of failed
	// connections are never taken over, so they cannot count as finished.
	if f.meta.Res.Size > 0 && !downloadComplete {
		allChunksComplete = false
	}

	// Check for any errors, but ignore 403 (server connection limit) errors if download completed
	var finalErr error
	if !downloadComplete && !allChunksComplete {
		// A stream may also stop early without a counted failure (a truncated
		// single-stream response, a range nobody took): missing bytes are an
		// error, never a finished download.
		finalErr = fmt.Errorf("download incomplete: %d of %d bytes", totalDownloaded, f.meta.Res.Size)
		for _, conn := range f.connections {
			if conn.State == connFailed && conn.failed {
				// Skip 403 errors (server connection limit) - these are expected when exceeding server's limit
				if re := extractRequestError(conn.lastErr); re != nil && re.Code == 403 {
					continue
				}
				if re := extractRequestError(conn.lastErr); re != nil {
					finalErr = fmt.Errorf("connection %d failed: retries=%d, status=%d", conn.ID, conn.retryTimes, re.Code)
				} else if conn.lastErr != nil {
					finalErr = fmt.Errorf("connection %d failed: retries=%d, err=%v", conn.ID, conn.retryTimes, conn.lastErr)
				} else {
					finalErr = fmt.Errorf("connection %d failed: retries=%d", conn.ID, conn.retryTimes)
				}
				break
			}
		}
	}
	f.connMu.Unlock()

	// Close the file before signaling completion
	// This ensures the file handle is released before Wait() returns
	f.fileMu.Lock()
	if f.file != nil {
		f.file.Close()
		f.file = nil
	}
	f.fileMu.Unlock()

	if finalErr != nil {
		f.setState(stateError)
	} else {
		f.setState(stateDone)
	}

	select {
	case f.doneCh <- finalErr:
	default:
	}
}

func (f *Fetcher) checkCompletion() bool {
	// Check if all data has been downloaded
	f.connMu.Lock()
	defer f.connMu.Unlock()

	totalDownloaded := int64(0)
	if f.resolveConn != nil {
		totalDownloaded += f.resolveConn.Downloaded
	}
	for _, conn := range f.connections {
		totalDownloaded += conn.Downloaded
	}

	if f.meta.Res.Size > 0 && totalDownloaded >= f.meta.Res.Size {
		// Don't start a new goroutine - let the caller handle completion
		return true
	}

	// Check if all connections completed
	allCompleted := true
	if f.resolveConn != nil && !f.resolveConn.Completed && f.resolveConn.State != connCompleted {
		allCompleted = false
	}
	for _, conn := range f.connections {
		if !conn.Completed && conn.State != connCompleted && conn.State != connFailed {
			allCompleted = false
			break
		}
	}

	if allCompleted {
		// Don't start a new goroutine - let the caller handle completion
		return true
	}

	return false
}

// Patch modifies the HTTP request information.
func (f *Fetcher) Patch(req *base.Request, opts *base.Options) error {
	// Patch request info
	if req != nil {
		if req.URL != "" {
			f.meta.Req.URL = req.URL
			// Clear redirect URL when URL is changed, so new requests use the new URL
			f.updateRedirectURL("")
		}
		if req.Extra != nil {
			if err := base.ParseReqExtra[fhttp.ReqExtra](req); err != nil {
				return err
			}
			patchExtra := req.Extra.(*fhttp.ReqExtra)
			// Merge Extra fields instead of replacing entirely
			if f.meta.Req.Extra == nil {
				f.meta.Req.Extra = &fhttp.ReqExtra{}
			}
			existingExtra := f.meta.Req.Extra.(*fhttp.ReqExtra)
			// Update Method only if non-empty
			if patchExtra.Method != "" {
				existingExtra.Method = patchExtra.Method
			}
			// Update Body only if non-empty
			if patchExtra.Body != "" {
				existingExtra.Body = patchExtra.Body
			}
			// Merge Headers: existing keys are overwritten, new keys are added
			if patchExtra.Header != nil {
				if existingExtra.Header == nil {
					existingExtra.Header = make(map[string]string)
				}
				for k, v := range patchExtra.Header {
					existingExtra.Header[k] = v
				}
			}
		}
		// Merge Labels: existing keys are overwritten, new keys are added
		if req.Labels != nil {
			if f.meta.Req.Labels == nil {
				f.meta.Req.Labels = make(map[string]string)
			}
			for k, v := range req.Labels {
				f.meta.Req.Labels[k] = v
			}
		}
		if req.Proxy != nil {
			f.meta.Req.Proxy = req.Proxy
		}
	}

	return nil
}

func (f *Fetcher) Pause() error {
	if f.cancel != nil {
		f.cancel()
	}
	f.resolveCancelMu.Lock()
	if f.resolveCancel != nil {
		f.resolveCancel()
	}
	f.resolveCancelMu.Unlock()

	f.stopPrefetch()

	// Wait for downloadLoop to exit first (it will call wg.Wait internally)
	if f.downloadLoopDone != nil {
		<-f.downloadLoopDone
	}

	// Wait for all connection goroutines to stop
	f.wg.Wait()

	// Clean up prefetch file
	f.cleanupPrefetchFile()

	// Clean up resolve response if still held
	f.resolveRespLock.Lock()
	if f.resolveResp != nil {
		f.resolveResp.Body.Close()
		f.resolveResp = nil
	}
	f.resolveRespLock.Unlock()

	f.fileMu.Lock()
	if f.file != nil {
		f.file.Close()
		f.file = nil
	}
	f.fileMu.Unlock()

	f.setState(statePaused)
	return nil
}

func (f *Fetcher) Close() error {
	return f.Pause()
}

func (f *Fetcher) Meta() *fetcher.FetcherMeta {
	return f.meta
}

func (f *Fetcher) Stats() any {
	f.connMu.Lock()
	statsConnections := make([]*fhttp.StatsConnection, 0)
	active, transferring := 0, 0
	for _, connection := range f.connections {
		statsConnections = append(statsConnections, &fhttp.StatsConnection{
			Downloaded: connection.Downloaded,
			Completed:  connection.Completed,
			Failed:     connection.failed,
			RetryTimes: connection.retryTimes,
		})
		switch connection.State {
		case connConnecting:
			active++
		case connDownloading:
			active++
			transferring++
		}
	}
	f.connMu.Unlock()

	stats := &fhttp.Stats{
		Connections:       statsConnections,
		ActiveStreams:     active,
		ConfiguredStreams: int(f.configuredStreams.Load()),
	}
	f.diag.fill(stats)
	cooling := false
	if m := f.member.Load(); m != nil {
		snap := m.Snapshot()
		// Streams this task may run now: its own limit, or what the source
		// capacity leaves after the other tasks' streams.
		stats.TargetStreams = max(1, min(snap.Limit, snap.SourceCapacity-(snap.SourceActive-snap.Active)))
		if !snap.CooldownUntil.IsZero() {
			until := snap.CooldownUntil
			stats.CooldownUntil = &until
			cooling = true
		}
		if a := snap.Adaptive; a != nil {
			stats.Adaptive = &fhttp.StatsAdaptive{Target: a.Target, Level: a.Level, Phase: a.Phase,
				Rate: a.Rate, Experiments: a.Experiments}
		}
	}
	stats.Phase = f.phase(transferring, cooling)
	return stats
}

// phase summarises what the download is doing for Stats.
func (f *Fetcher) phase(transferring int, cooling bool) string {
	switch f.getState() {
	case stateIdle, stateResolved:
		return "waiting"
	case stateResolving:
		return "resolving"
	case statePaused:
		return "paused"
	case stateDone:
		return "done"
	case stateError:
		if f.deferred.Load() != nil {
			return "deferred"
		}
		return "error"
	}
	if transferring > 0 {
		return "transferring"
	}
	if cooling {
		return "cooldown"
	}
	return "connecting"
}

func (f *Fetcher) Progress() fetcher.Progress {
	f.connMu.Lock()
	defer f.connMu.Unlock()
	p := make(fetcher.Progress, 0)

	total := int64(0)
	if f.resolveConn != nil {
		total += f.resolveConn.Downloaded
	}

	for _, conn := range f.connections {
		total += conn.Downloaded
	}

	p = append(p, total)
	return p
}

func (f *Fetcher) Wait() error {
	return <-f.doneCh
}
