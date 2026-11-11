package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// upgradeRangeResume preserves a legacy sequential prefix when the server
// omitted Accept-Ranges during the original resolve. A saved modification date
// and a matching overlap are required before converting the persisted chunk.
func (f *Fetcher) upgradeRangeResume() error {
	if f.meta.Res.Range || f.meta.Res.Size <= 0 || len(f.meta.Res.Files) != 1 ||
		f.meta.Res.Files[0].Ctime == nil || f.getState() == stateSlowStart || f.getState() == stateSteady {
		return nil
	}
	modified := *f.meta.Res.Files[0].Ctime
	if modified.IsZero() || time.Since(modified) < time.Minute {
		return nil
	}
	f.connMu.Lock()
	if len(f.connections) != 1 {
		f.connMu.Unlock()
		return nil
	}
	conn := f.connections[0]
	prefix := conn.Downloaded
	eligible := !conn.Completed && prefix > 0 && prefix < f.meta.Res.Size &&
		conn.Chunk != nil && conn.Chunk.Begin == 0 && conn.Chunk.Downloaded == prefix
	f.connMu.Unlock()
	if !eligible {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), rangeProbeTimeout)
	defer cancel()
	request, err := f.buildRequestWithOriginalURL(ctx, f.meta.Req)
	if err != nil {
		return err
	}
	if request.Method != http.MethodGet || (request.Body != nil && request.Body != http.NoBody) ||
		request.Header.Get("Range") != "" {
		return nil
	}
	// A failed verification leaves the counted prefix and file untouched. It
	// must not silently turn an upgrade/restart into a byte-zero redownload.
	failure := func() error {
		return fmt.Errorf("cannot verify saved HTTP prefix for range resume; partial file preserved")
	}
	file, err := os.Open(f.meta.SingleFilepath())
	if err != nil {
		return failure()
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || stat.Size() < prefix {
		return failure()
	}
	begin := max(int64(0), prefix-64*1024)
	expected := make([]byte, prefix-begin)
	if _, err := file.ReadAt(expected, begin); err != nil {
		return failure()
	}
	validator := modified.UTC().Format(http.TimeFormat)
	request.Header.Set("If-Range", validator)
	if err := f.compareRangeOverlap(request, begin, expected, func(response *http.Response) bool {
		remoteModified, modifiedErr := http.ParseTime(response.Header.Get("Last-Modified"))
		responseDate, dateErr := http.ParseTime(response.Header.Get("Date"))
		return modifiedErr == nil && remoteModified.Equal(modified) && dateErr == nil && responseDate.Sub(remoteModified) >= time.Minute
	}); err != nil {
		return failure()
	}
	f.connMu.Lock()
	defer f.connMu.Unlock()
	if len(f.connections) != 1 || f.connections[0] != conn || conn.Downloaded != prefix {
		return failure()
	}
	conn.Chunk = newChunk(0, f.meta.Res.Size-1)
	conn.Chunk.Downloaded = prefix
	f.meta.Res.Range = true
	f.rangeResumeValidator = validator
	f.diag.setRange(rangeResumed, "")
	return nil
}

// compareRangeOverlap compares saved bytes with the server's bytes at begin,
// for the legacy single-stream upgrade and validator-free checkpoints. It
// never changes file bytes or counters. ErrUnsafeResume means the source
// changed: a legal 206 with other bytes or another total length, a 416 naming
// another length, or a response that accept rejects. Refusals return their
// RequestError and unusable answers errInvalidRangeResponse, which includes a
// 2xx that ignored the Range header: a full response says nothing about the
// saved offsets. Network errors and timeouts are returned as they are.
func (f *Fetcher) compareRangeOverlap(request *http.Request, begin int64, expected []byte, accept func(*http.Response) bool) error {
	end := begin + int64(len(expected)) - 1
	if request.Method != http.MethodGet || request.Body != nil && request.Body != http.NoBody || begin < 0 || end < begin {
		return fmt.Errorf("%w: saved overlap could not be verified", ErrUnsafeResume)
	}
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", begin, end))
	request.Header.Set("Accept-Encoding", "identity")
	client := f.buildClient()
	response, err := doCredentialRequest(client, request)
	if err == nil {
		response, err = f.refreshResponse(request.Context(), client, request, response)
	}
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusPartialContent && f.publicSourceSnapshot() == nil && f.hasRedirectURL() && isRedirectExpiredError(newResponseError(response)) {
		response.Body.Close()
		if response, err = f.tryFallbackToOriginalURL(request.Context(), client, begin, end); err != nil {
			return err
		}
	}
	defer response.Body.Close()
	contentRange := response.Header.Get("Content-Range")
	switch {
	case response.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		if total, err := strconv.ParseInt(strings.TrimPrefix(contentRange, "bytes */"), 10, 64); err == nil && total != f.meta.Res.Size {
			return fmt.Errorf("%w: saved total length changed", ErrUnsafeResume)
		}
		return newResponseError(response)
	case response.StatusCode >= 200 && response.StatusCode < 300 && response.StatusCode != http.StatusPartialContent:
		return fmt.Errorf("%w: status=%d, the server ignored the range request", errInvalidRangeResponse, response.StatusCode)
	case response.StatusCode != http.StatusPartialContent:
		return newResponseError(response)
	}
	start, last, total, valid := parseContentRange(contentRange)
	if valid && total != f.meta.Res.Size {
		return fmt.Errorf("%w: saved total length changed", ErrUnsafeResume)
	}
	if !valid || start != begin || last < end || response.Uncompressed || normalizedEncoding(response.Header.Get("Content-Encoding")) != "" {
		return fmt.Errorf("%w: status=%d, Content-Range=%q", errInvalidRangeResponse, response.StatusCode, contentRange)
	}
	if accept != nil && !accept(response) {
		return fmt.Errorf("%w: saved validator changed", ErrUnsafeResume)
	}
	actual := make([]byte, len(expected))
	if _, err := io.ReadFull(response.Body, actual); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: truncated range body", errInvalidRangeResponse)
		}
		return err
	}
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("%w: saved bytes differ from the server", ErrUnsafeResume)
	}
	return nil
}

const (
	overlapSpanSize = 64 << 10
	// A resume without validators compares this many saved spans: the two
	// farthest apart and the one ending the longest run of saved bytes.
	maxOverlapSamples = 3
	// Each comparison request has its own limit for its headers and 64 KiB.
	overlapCheckTimeout = 30 * time.Second
)

// savedSpan is the byte range [begin, end) of saved data compared with the
// server; run is the length of the saved bytes that end at end.
type savedSpan struct{ begin, end, run int64 }

// overlapSpans takes up to 64 KiB just before each saved download position:
// where every connection stands (chunk start plus bytes downloaded) and the
// end of the resolve-phase prefix as recorded for this run. A span stops at
// the end of any range that a connection has not downloaded yet, so it only
// covers bytes already written. A stolen chunk's start, for one, is preceded
// by the unfinished tail of the stream it was taken from.
func (f *Fetcher) overlapSpans() []savedSpan {
	f.connMu.Lock()
	defer f.connMu.Unlock()
	type gap struct{ begin, end int64 } // not downloaded yet, end exclusive
	var gaps []gap
	ends := []int64{f.resolveDataPos.Load()}
	for _, conn := range f.connections {
		if conn.Chunk == nil {
			continue
		}
		pos := conn.Chunk.Begin + conn.Chunk.Downloaded
		ends = append(ends, pos)
		if pos <= conn.Chunk.End {
			gaps = append(gaps, gap{pos, conn.Chunk.End + 1})
		}
	}
	var spans []savedSpan
	seen := map[int64]bool{}
	for _, end := range ends {
		end = min(end, f.meta.Res.Size)
		if end <= 0 || seen[end] {
			continue
		}
		seen[end] = true
		floor := int64(0)
		for _, g := range gaps {
			if g.begin < end && end < g.end {
				floor = end // inside an unfinished range: nothing before it is known
				break
			}
			if g.end <= end {
				floor = max(floor, g.end)
			}
		}
		if floor < end {
			spans = append(spans, savedSpan{begin: max(floor, end-overlapSpanSize), end: end, run: end - floor})
		}
	}
	return spans
}

// sampleOverlapSpans keeps the first and the last span and, between them,
// the one that ends the longest run of saved bytes.
func sampleOverlapSpans(spans []savedSpan) []savedSpan {
	sort.Slice(spans, func(i, j int) bool { return spans[i].end < spans[j].end })
	if len(spans) <= maxOverlapSamples {
		return spans
	}
	longest := 1
	for i := 2; i < len(spans)-1; i++ {
		if spans[i].run > spans[longest].run {
			longest = i
		}
	}
	return []savedSpan{spans[0], spans[longest], spans[len(spans)-1]}
}

type overlapSample struct {
	begin    int64
	expected []byte
}

// readOverlapSamples reads the sampled saved bytes once for every attempt.
func (f *Fetcher) readOverlapSamples() ([]overlapSample, error) {
	spans := sampleOverlapSpans(f.overlapSpans())
	if len(spans) == 0 {
		return nil, fmt.Errorf("%w: no saved overlap available", ErrUnsafeResume)
	}
	file, err := os.Open(f.meta.SingleFilepath())
	if err != nil {
		return nil, fmt.Errorf("%w: saved overlap unavailable", ErrUnsafeResume)
	}
	defer file.Close()
	samples := make([]overlapSample, len(spans))
	for i, span := range spans {
		samples[i] = overlapSample{begin: span.begin, expected: make([]byte, span.end-span.begin)}
		if _, err := file.ReadAt(samples[i].expected, span.begin); err != nil {
			return nil, fmt.Errorf("%w: saved overlap unavailable", ErrUnsafeResume)
		}
	}
	return samples, nil
}

// verifySavedOverlaps confirms a checkpoint without a validator or trusted
// digest, or one whose link changed in more than its signatures, before any
// stream writes next to its saved bytes. It runs in the
// download loop, so no task lock waits for the network. Only a changed source
// (see compareRangeOverlap) fails the task as an unsafe resume, keeping the
// file. A rate limit follows the source cooldown, Retry-After and deferral
// like a stream; refusals and unusable responses are retried like a stream's
// counted failures, timeouts and network errors without limit. False means
// the run ended.
func (f *Fetcher) verifySavedOverlaps() bool {
	samples, err := f.readOverlapSamples()
	if err != nil {
		f.diag.failure(err)
		f.failRun(err)
		return false
	}
	failures, retries, rateLimits := 0, 0, 0
	for {
		err := f.compareOverlapSamples(samples)
		if err == nil {
			// The saved bytes match this link; resumes from it need no check.
			f.identityMu.Lock()
			f.identity.QueryKey = requestQueryKey(f.meta.Req)
			f.identityMu.Unlock()
			f.deferrals.Store(0)
			f.diag.setRange(rangeResumed, "仅按重叠比对确认")
			return true
		}
		if f.ctx.Err() != nil {
			return false // paused or deferred
		}
		f.diag.failure(err)
		if errors.Is(err, ErrUnsafeResume) {
			f.failRun(err)
			return false
		}
		if re := extractRequestError(err); re != nil && transfer.IsRateLimitStatus(re.Code) {
			rateLimits++
			wait, deferred, giveUp := f.rateLimit(re, rateLimits)
			if giveUp {
				f.failRun(resumeCheckError(err))
				return false
			}
			if deferred != nil {
				f.deferRun(deferred)
				return false
			}
			// With a source budget, the next permit waits for the cooldown.
			if f.member.Load() == nil && !sleepContext(f.ctx, wait) {
				return false
			}
			continue
		}
		if shouldCountHTTPFailure(err) || errors.Is(err, errInvalidRangeResponse) {
			if failures++; failures >= 3 {
				f.failRun(resumeCheckError(err))
				return false
			}
		}
		retries++
		if !sleepContext(f.ctx, min(time.Duration(retries)*time.Second, 5*time.Second)) {
			return false
		}
	}
}

// compareOverlapSamples holds one stream permit of the source budget and
// sends one request per sample, each with its own timeout. Saved validators
// must match too.
func (f *Fetcher) compareOverlapSamples(samples []overlapSample) error {
	release, err := f.checkPermit()
	if err != nil {
		return err
	}
	defer release()
	sameValidator := func(response *http.Response) bool { return f.validateRepresentation(response) == nil }
	for _, sample := range samples {
		ctx, cancel := context.WithTimeout(f.ctx, overlapCheckTimeout)
		request, err := f.buildRequest(ctx, f.meta.Req)
		if err == nil {
			err = f.compareRangeOverlap(request, sample.begin, sample.expected, sameValidator)
		} else {
			err = fmt.Errorf("%w: invalid resume request", ErrUnsafeResume)
		}
		cancel()
		if err != nil {
			return err
		}
	}
	if m := f.member.Load(); m != nil {
		m.Succeeded()
	}
	return nil
}

// checkPermit waits like a stream until the source budget lets this run send
// a request: no cooldown, and while the source recovers this request is the
// probe or the probe has succeeded. A cooldown too long to hold the task's
// running slot, also once other tasks queue, defers the run instead.
func (f *Fetcher) checkPermit() (func(), error) {
	m := f.member.Load()
	if m == nil {
		return func() {}, f.ctx.Err()
	}
	for {
		granted, wake, until, closed := m.TryAcquireOrWait()
		if granted {
			return m.Release, nil
		}
		if closed {
			return func() {}, f.ctx.Err()
		}
		queued := f.queueChanged()
		if !until.IsZero() && f.shouldDefer(time.Until(until)) {
			f.deferRun(&transfer.DeferredError{Until: until})
			return nil, context.Canceled
		}
		select {
		case <-f.ctx.Done():
			return nil, f.ctx.Err()
		case <-wake:
		case <-queued:
		}
	}
}

// resumeCheckError reports a resume check that kept failing for a reason
// other than a changed source, naming the status as a failed stream does so
// that wrappers can still refresh an expired link.
func resumeCheckError(err error) error {
	if re := extractRequestError(err); re != nil {
		if transfer.IsExpiredLinkStatus(re.Code) {
			return fmt.Errorf("resume check failed: status=%d, request refused; the download link may have expired: %w", re.Code, re)
		}
		return fmt.Errorf("resume check failed: status=%d: %w", re.Code, re)
	}
	return fmt.Errorf("resume check failed: %w", err)
}

// failRun ends a run that stopped before any stream started. A paused run
// reports nothing; its next Start checks again.
func (f *Fetcher) failRun(err error) {
	if f.ctx.Err() != nil {
		return
	}
	f.fileMu.Lock()
	if f.file != nil {
		f.file.Close()
		f.file = nil
	}
	f.fileMu.Unlock()
	f.setState(stateError)
	select {
	case f.doneCh <- err:
	default:
	}
}
