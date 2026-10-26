package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const rangeProbeTimeout = 3 * time.Second

var errInvalidRangeResponse = errors.New("invalid HTTP range response")

// parseContentRange accepts a single, satisfied byte range with a known size.
func parseContentRange(value string) (begin, end, size int64, ok bool) {
	value, found := strings.CutPrefix(value, "bytes ")
	if !found {
		return
	}
	interval, total, found := strings.Cut(value, "/")
	if !found {
		return
	}
	first, last, found := strings.Cut(interval, "-")
	if !found {
		return
	}
	values := []*int64{&begin, &end, &size}
	for i, part := range []string{first, last, total} {
		if part == "" || strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return
		}
		*values[i] = n
	}
	ok = begin <= end && end < size
	return
}

// probeEligibility explains why a response is not probed, or "" when it is.
func probeEligibility(req *http.Request, resp *http.Response, size int64, connections int) string {
	switch {
	case connections <= 1:
		return "singleStream"
	case size <= 0:
		return "unknownSize"
	case size < 2*stealMinChunkSize:
		return "small"
	case req.Method != http.MethodGet:
		return "method"
	case req.Body != nil && req.Body != http.NoBody:
		return "requestBody"
	case req.Header.Get("Range") != "":
		return "presetRange"
	case resp.StatusCode != http.StatusOK:
		return "status"
	case resp.Header.Get("Accept-Ranges") != "":
		return "declaredNone"
	case resp.Header.Get("Content-Range") != "":
		return "contentRange"
	case resp.Uncompressed:
		return "compressed"
	}
	return ""
}

// probeRangeSupport handles servers that honor Range but omit Accept-Ranges.
// Failure is only a capability miss: the original response remains untouched
// so single-use URLs can still be downloaded through their first response.
// state and reason describe the outcome for diagnostics.
func probeRangeSupport(parent context.Context, client *http.Client, req *http.Request, resp *http.Response, size int64, connections int) (ok bool, state, reason string) {
	if reason := probeEligibility(req, resp, size, connections); reason != "" {
		return false, rangeNotProbed, reason
	}

	// Resolve's caller owns cancellation until headers arrive. After that the
	// original stream has its own lifetime, but this optional work must still
	// stop promptly on caller cancellation or Pause's request cancellation.
	ctx, cancel := context.WithTimeout(parent, rangeProbeTimeout)
	defer cancel()
	stop := context.AfterFunc(req.Context(), cancel)
	defer stop()

	probe := req.Clone(ctx)
	probe.Header.Set("Range", "bytes=0-0")
	if etag := resp.Header.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		probe.Header.Set("If-Range", etag)
	} else if modified := resp.Header.Get("Last-Modified"); modified != "" {
		probe.Header.Set("If-Range", modified)
	}
	// Reuse the same client for its proxy, policies, redirect filtering and
	// cookies. Rebuilding from metadata at the final URL would reattach headers
	// that the original redirect chain may have stripped across domains.
	partial, err := client.Do(probe)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return false, rangeProbeFailed, "timeout"
		}
		return false, rangeProbeFailed, "network"
	}
	defer partial.Body.Close()
	if partial.StatusCode != http.StatusPartialContent {
		if partial.StatusCode == http.StatusOK {
			return false, rangeUnsupported, "ignored"
		}
		return false, rangeUnsupported, "status"
	}
	begin, end, total, valid := parseContentRange(partial.Header.Get("Content-Range"))
	if partial.Uncompressed || !valid || begin != 0 || end != 0 || total != size ||
		(partial.ContentLength >= 0 && partial.ContentLength != 1) {
		return false, rangeUnsupported, "contentRange"
	}
	if resp.Request == nil || partial.Request == nil ||
		resp.Request.URL.String() != partial.Request.URL.String() {
		return false, rangeUnsupported, "redirect"
	}
	// Range offsets must refer to the same representation as the saved stream.
	// Treat missing/changed validators or representation headers conservatively.
	for _, header := range []string{"ETag", "Last-Modified", "Content-Encoding", "Content-Type"} {
		if resp.Header.Get(header) != partial.Header.Get(header) {
			return false, rangeUnsupported, "representation"
		}
	}
	// Read at most two bytes: require the promised byte and EOF, rejecting
	// short, oversized or stalled bodies without draining an ignored full GET.
	var body [2]byte
	n, err := io.ReadFull(partial.Body, body[:])
	if n == 1 && err == io.ErrUnexpectedEOF {
		return true, rangeProbed, ""
	}
	if ctx.Err() == context.DeadlineExceeded {
		return false, rangeProbeFailed, "timeout"
	}
	return false, rangeUnsupported, "body"
}
