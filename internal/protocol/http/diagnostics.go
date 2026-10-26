package http

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// Range capability as reported in Stats.
const (
	rangeDeclared    = "declared"    // the first response declared byte ranges
	rangeProbed      = "probed"      // undeclared, confirmed by a one-byte probe
	rangeResumed     = "resumed"     // a saved single-stream prefix was verified and upgraded
	rangeUnsupported = "unsupported" // the server ignored or mishandled the probe
	rangeProbeFailed = "probeFailed" // the probe failed for network or time reasons
	rangeNotProbed   = "notProbed"   // the download was not eligible for a probe
)

// diagnostics records how a download spends its time. It never stores URLs,
// headers or cookies, so Stats can be shown and exported safely.
type diagnostics struct {
	mu          sync.Mutex
	rangeState  string
	rangeReason string
	protocol    string
	newConns    int64
	reusedConns int64
	requests    int64
	rateLimits  int64
	dns         durationTotal
	connect     durationTotal
	tlsShake    durationTotal
	resolve     time.Duration
	runStart    time.Time
	cooldown    time.Duration
	// Unix nanoseconds of the first and latest payload bytes of the run;
	// updated on every read, so kept off the mutex.
	firstData atomic.Int64
	lastData  atomic.Int64
	lastError string
}

type durationTotal struct {
	total time.Duration
	count int64
}

func (d *durationTotal) add(v time.Duration) {
	d.total += v
	d.count++
}

func (d *diagnostics) setRange(state, reason string) {
	d.mu.Lock()
	d.rangeState, d.rangeReason = state, reason
	d.mu.Unlock()
}

func (d *diagnostics) setResolve(elapsed time.Duration) {
	d.mu.Lock()
	d.resolve = elapsed
	d.mu.Unlock()
}

// startRun marks the beginning of a Start; data timings restart with it.
func (d *diagnostics) startRun(now time.Time) {
	d.mu.Lock()
	d.runStart = now
	d.firstData.Store(0)
	d.lastData.Store(0)
	d.mu.Unlock()
}

func (d *diagnostics) response(resp *http.Response) {
	d.mu.Lock()
	d.protocol = resp.Proto
	d.mu.Unlock()
}

func (d *diagnostics) data(now time.Time) {
	nano := now.UnixNano()
	d.firstData.CompareAndSwap(0, nano)
	d.lastData.Store(nano)
}

func (d *diagnostics) rateLimited() {
	d.mu.Lock()
	d.rateLimits++
	d.mu.Unlock()
}

func (d *diagnostics) waited(v time.Duration) {
	d.mu.Lock()
	d.cooldown += v
	d.mu.Unlock()
}

// failure keeps a short, credential-free description of the latest error.
func (d *diagnostics) failure(err error) {
	if err == nil {
		return
	}
	text := "network error"
	if re := extractRequestError(err); re != nil {
		text = http.StatusText(re.Code)
		if text == "" {
			text = "HTTP error"
		}
		text = "HTTP " + strconv.Itoa(re.Code) + " " + text
	} else if errors.Is(err, context.DeadlineExceeded) {
		text = "timeout"
	} else if errors.Is(err, errInvalidRangeResponse) {
		text = "invalid range response"
	}
	d.mu.Lock()
	d.lastError = text
	d.mu.Unlock()
}

// trace attaches connection timing hooks to a request context.
func (d *diagnostics) trace(ctx context.Context) context.Context {
	d.mu.Lock()
	d.requests++
	d.mu.Unlock()
	var mu sync.Mutex
	var dnsStart, tlsStart time.Time
	connectStarts := map[string]time.Time{}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			mu.Lock()
			dnsStart = time.Now()
			mu.Unlock()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			mu.Lock()
			elapsed := time.Since(dnsStart)
			mu.Unlock()
			d.mu.Lock()
			d.dns.add(elapsed)
			d.mu.Unlock()
		},
		ConnectStart: func(network, addr string) {
			mu.Lock()
			connectStarts[network+addr] = time.Now()
			mu.Unlock()
		},
		ConnectDone: func(network, addr string, err error) {
			mu.Lock()
			start := connectStarts[network+addr]
			mu.Unlock()
			if err == nil && !start.IsZero() {
				d.mu.Lock()
				d.connect.add(time.Since(start))
				d.mu.Unlock()
			}
		},
		TLSHandshakeStart: func() {
			mu.Lock()
			tlsStart = time.Now()
			mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			mu.Lock()
			elapsed := time.Since(tlsStart)
			mu.Unlock()
			if err == nil {
				d.mu.Lock()
				d.tlsShake.add(elapsed)
				d.mu.Unlock()
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			d.mu.Lock()
			if info.Reused {
				d.reusedConns++
			} else {
				d.newConns++
			}
			d.mu.Unlock()
		},
	})
}

func ms(v time.Duration) int64 { return v.Milliseconds() }

// fill copies the recorded facts into a Stats value.
func (d *diagnostics) fill(stats *fhttp.Stats) {
	d.mu.Lock()
	defer d.mu.Unlock()
	stats.RangeState, stats.RangeReason = d.rangeState, d.rangeReason
	stats.Protocol = d.protocol
	stats.NewConnections, stats.ReusedConnections = d.newConns, d.reusedConns
	stats.Requests, stats.RateLimits = d.requests, d.rateLimits
	stats.LastError = d.lastError
	timings := &fhttp.StatsTimings{
		ResolveMs:  ms(d.resolve),
		DNSMs:      ms(d.dns.total),
		ConnectMs:  ms(d.connect.total),
		TLSMs:      ms(d.tlsShake.total),
		CooldownMs: ms(d.cooldown),
	}
	if first, last := d.firstData.Load(), d.lastData.Load(); !d.runStart.IsZero() && first != 0 {
		timings.FirstByteMs = ms(time.Unix(0, first).Sub(d.runStart))
		timings.TransferMs = ms(time.Duration(last - first))
	}
	stats.Timings = timings
}
