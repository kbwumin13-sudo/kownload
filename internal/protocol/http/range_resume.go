package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
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
	validator := modified.UTC().Format(http.TimeFormat)
	request.Header.Set("If-Range", validator)
	if err := f.compareRangeOverlap(file, request, begin, prefix-1, func(response *http.Response) bool {
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

// compareRangeOverlap is shared by the legacy single-stream upgrade and
// validator-free stopped checkpoints. It never changes file bytes or counters.
func (f *Fetcher) compareRangeOverlap(file *os.File, request *http.Request, begin, end int64, accept func(*http.Response) bool) error {
	failure := func() error { return fmt.Errorf("%w: saved overlap could not be verified", ErrUnsafeResume) }
	if request.Method != http.MethodGet || request.Body != nil && request.Body != http.NoBody || begin < 0 || end < begin {
		return failure()
	}
	expected := make([]byte, end-begin+1)
	if _, err := file.ReadAt(expected, begin); err != nil {
		return failure()
	}
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", begin, end))
	request.Header.Set("Accept-Encoding", "identity")
	client := f.buildClient()
	response, err := client.Do(request)
	if err == nil {
		response, err = f.refreshResponse(request.Context(), client, request, response)
	}
	if err != nil {
		return errors.Join(failure(), err)
	}
	if response.StatusCode != http.StatusPartialContent && f.publicSourceSnapshot() == nil && f.hasRedirectURL() && isRedirectExpiredError(newResponseError(response)) {
		response.Body.Close()
		response, err = f.tryFallbackToOriginalURL(request.Context(), client, begin, end)
		if err != nil {
			return errors.Join(failure(), err)
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		return errors.Join(failure(), newResponseError(response))
	}
	start, last, size, valid := parseContentRange(response.Header.Get("Content-Range"))
	if !valid || start != begin || last != end || size != f.meta.Res.Size || response.Uncompressed ||
		normalizedEncoding(response.Header.Get("Content-Encoding")) != "" || accept != nil && !accept(response) {
		return failure()
	}
	actual, err := io.ReadAll(io.LimitReader(response.Body, int64(len(expected))+1))
	if err != nil || !bytes.Equal(actual, expected) {
		return failure()
	}
	return nil
}

const overlapSpanSize = 64 << 10

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
	// A prefix not yet adopted is read from the prefetch file, which only
	// exists before the target does.
	ends := []int64{f.resolveDataPos.Load(), f.prefetchSize.Load()}
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

func (f *Fetcher) verifySavedOverlaps() error {
	spans := f.overlapSpans()
	if len(spans) == 0 {
		return fmt.Errorf("%w: no saved overlap available", ErrUnsafeResume)
	}
	file, err := os.Open(f.meta.SingleFilepath())
	if err != nil && f.prefetchFilePath != "" {
		file, err = os.Open(f.prefetchFilePath)
	}
	if err != nil {
		return fmt.Errorf("%w: saved overlap unavailable", ErrUnsafeResume)
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), rangeProbeTimeout)
	defer cancel()
	for _, saved := range spans {
		request, err := f.buildRequest(ctx, f.meta.Req)
		if err != nil {
			return fmt.Errorf("%w: invalid resume request", ErrUnsafeResume)
		}
		if err := f.compareRangeOverlap(file, request, saved.begin, saved.end-1, nil); err != nil {
			return err
		}
	}
	f.diag.setRange(rangeResumed, "仅按重叠比对确认")
	return nil
}
