// Command fixture serves deterministic downloads for kownload speed
// comparisons. One server covers every scenario; behaviour is chosen per
// request with query parameters, so baseline and candidate cores see exactly
// the same source. It never logs request URLs or headers.
//
//	/f/<name>?size=N            content of N bytes, identical for the same name and size
//	  mode=declared|hidden|ignore  Accept-Ranges declared, omitted, or Range ignored
//	  perconn=B                 pace every response stream at B bytes/s
//	  total=B&group=G           pace all streams of group G together at B bytes/s
//	  limit=K&status=429|503&retry=2|date:2|none
//	                            the first K range requests of the group are refused
//	  rps=R&burst=B&status=..&retry=auto|date|none
//	                            token bucket per group: a request without a token
//	                            is refused, Retry-After telling when one is free
//	  tailfrom=F&tailrate=B     bytes after fraction F of the file go at B bytes/s per stream
//	  dropafter=S&droprate=B    the group's total pace drops to B after S seconds
//	  probedelay=MS             delay only a single-byte capability probe
//	  slowonce=B                one range stream transfers at B bytes/s
//	  abortafter=B              truncate one active response after B bytes
//	  reject=404                fail every request with a terminal HTTP status
//	/sha?name=&size=            SHA-256 of that content
//	/stats                      counters and completed requests since the last /reset
//	/stats?summary=1            counters only, for drainage polling
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"math/big"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const chunkSize = 32 * 1024

var (
	acceptDelay = flag.Duration("accept-delay", 0, "delay before a new connection's first read (handshake latency)")
	rtt         = flag.Duration("rtt", 0, "delay before every response header")
	readyFile   = flag.String("ready-file", "", "write listening ports here as JSON")
	dmgFixture  = flag.String("dmg", "", "fixture file served as valid.dmg")
	fixedPorts  = flag.String("ports", "", "plain,tls,tls1 ports to listen on (default: any free ports)")
)

func contentByte(seed uint32, i int64) byte {
	return byte(uint32(i)*31 + uint32(i/251) + seed)
}

func seedOf(name string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(name))
	return h.Sum32()
}

func fill(buf []byte, seed uint32, offset int64) {
	for i := range buf {
		buf[i] = contentByte(seed, offset+int64(i))
	}
}

// pacer hands out FIFO send slots so streams sharing it get rate in total.
type pacer struct {
	mu       sync.Mutex
	rate     float64
	next     time.Time
	dropAt   time.Time
	dropRate float64
}

func (p *pacer) wait(ctx context.Context, n int) error {
	p.mu.Lock()
	now := time.Now()
	if !p.dropAt.IsZero() && now.After(p.dropAt) {
		p.rate, p.dropAt = p.dropRate, time.Time{}
	}
	if p.next.Before(now) {
		p.next = now
	}
	at := p.next
	p.next = p.next.Add(time.Duration(float64(n) / p.rate * float64(time.Second)))
	p.mu.Unlock()
	if d := time.Until(at); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

type group struct {
	pacer   *pacer
	limited atomic.Int64
	refused atomic.Int64
	bucket  *bucket
	slow    atomic.Bool
	aborted atomic.Bool
	abortAt atomic.Int64
	resumed atomic.Int64
}

// bucket is a token-bucket request limiter, the usual model behind 429.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// take spends a token, or returns how long until one is available.
func (b *bucket) take() (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}

type counters struct {
	connections   atomic.Int64
	requests      atomic.Int64
	rangeRequests atomic.Int64
	refused       atomic.Int64
	bytesSent     atomic.Int64
	inflight      atomic.Int64
	maxInflight   atomic.Int64
	requestNanos  atomic.Int64
	aborts        atomic.Int64
	slowStreams   atomic.Int64
}

type requestEvent struct {
	ID                int64  `json:"id"`
	Task              string `json:"task"`
	Group             string `json:"group"`
	StartNs           int64  `json:"startNs"`
	ResponseNs        int64  `json:"responseNs"`
	EndNs             int64  `json:"endNs"`
	Status            int    `json:"status"`
	RefusalSource     string `json:"refusalSource,omitempty"`
	IsRange           bool   `json:"isRange"`
	RangeStart        *int64 `json:"rangeStart,omitempty"`
	RangeEndExclusive *int64 `json:"rangeEndExclusive,omitempty"`
	RetryAfterType    string `json:"retryAfterType,omitempty"`
	RetryAfterWaitNs  *int64 `json:"retryAfterWaitNs,omitempty"`
	BodyStart         *int64 `json:"bodyStart,omitempty"`
	BytesSent         int64  `json:"bytesSent"`
	file              contentKey
}

// Separate the original synthetic content key from labels suitable for output.
type contentKey struct {
	name string
	size int64
}

type server struct {
	// Start, finish and reset a request epoch under the same lock. Consequently,
	// a snapshot with inflight == 0 contains every completed request's totals.
	mu       sync.Mutex
	groups   map[string]*group
	shas     map[string]string
	c        *counters
	epoch    time.Time
	timeline []requestEvent
	dmg      []byte
}

// Requests come from synthetic scenarios. Reject escaped, long and unexpected
// labels in the exported trace. No URL, query string or request header is emitted.
func fixtureLabel(label string) string {
	if len(label) > 64 || label == "" {
		return "_invalid_label"
	}
	for _, r := range label {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-') {
			return "_invalid_label"
		}
	}
	return label
}

func retryAfter(value string, now time.Time) (kind string, wait *int64) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "missing", nil
	}
	if seconds, err := strconv.ParseUint(value, 10, 63); err == nil && seconds <= uint64((1<<63-1)/time.Second) {
		nanos := int64(seconds) * int64(time.Second)
		return "seconds", &nanos
	}
	if at, err := http.ParseTime(value); err == nil {
		nanos := max(int64(0), at.Sub(now).Nanoseconds())
		return "date", &nanos
	}
	return "invalid", nil
}

type byteSpan struct {
	start, end int64
}

func duplicateBytes(timeline []requestEvent) int64 {
	byFile := map[contentKey][]byteSpan{}
	for _, event := range timeline {
		if event.BodyStart != nil && event.BytesSent > 0 {
			from := *event.BodyStart
			byFile[event.file] = append(byFile[event.file], byteSpan{from, from + event.BytesSent})
		}
	}
	var duplicates int64
	for _, spans := range byFile {
		sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
		var coveredEnd int64
		for _, span := range spans {
			duplicates += max(0, min(coveredEnd, span.end)-span.start)
			coveredEnd = max(coveredEnd, span.end)
		}
	}
	return duplicates
}

func (s *server) group(name string, q map[string][]string) *group {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[name]
	if g == nil {
		g = &group{}
		if total := num(q, "total"); total > 0 {
			g.pacer = &pacer{rate: total}
			if after := num(q, "dropafter"); after > 0 {
				g.pacer.dropAt = time.Now().Add(time.Duration(after * float64(time.Second)))
				g.pacer.dropRate = num(q, "droprate")
			}
		}
		g.limited.Store(int64(num(q, "limit")))
		if rps := num(q, "rps"); rps > 0 {
			burst := max(1, num(q, "burst"))
			g.bucket = &bucket{rate: rps, burst: burst, tokens: burst, last: time.Now()}
		}
		s.groups[name] = g
	}
	return g
}

func num(q map[string][]string, key string) float64 {
	if v := q[key]; len(v) > 0 {
		n, _ := strconv.ParseFloat(v[0], 64)
		return n
	}
	return 0
}

func str(q map[string][]string, key, def string) string {
	if v := q[key]; len(v) > 0 && v[0] != "" {
		return v[0]
	}
	return def
}

func parseRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if b != "" {
		if end, err = strconv.ParseInt(b, 10, 64); err != nil || end < start {
			return 0, 0, false
		}
		end = min(end, size-1)
	}
	return start, end, true
}

type downloadConnKey struct{}

func (s *server) serveFile(w http.ResponseWriter, r *http.Request) {
	entered := time.Now()
	s.mu.Lock()
	c, epoch := s.c, s.epoch
	if used, ok := r.Context().Value(downloadConnKey{}).(*atomic.Pointer[counters]); ok && used.Swap(c) != c {
		c.connections.Add(1)
	}
	id := c.requests.Add(1)
	n := c.inflight.Add(1)
	if n > c.maxInflight.Load() {
		c.maxInflight.Store(n)
	}
	s.mu.Unlock()

	q := r.URL.Query()
	name := strings.TrimPrefix(r.URL.Path, "/f/")
	size := int64(num(q, "size"))
	groupName := str(q, "group", name)
	rangeHeader := r.Header.Get("Range")
	event := requestEvent{
		ID: id, Task: fixtureLabel(name), Group: fixtureLabel(groupName),
		StartNs: max(0, entered.Sub(epoch).Nanoseconds()), IsRange: rangeHeader != "",
		file: contentKey{name, size},
	}
	defer func() {
		now := time.Now()
		event.EndNs = now.Sub(epoch).Nanoseconds()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.timeline = append(s.timeline, event)
		c.requestNanos.Add(now.Sub(entered).Nanoseconds())
		c.inflight.Add(-1)
	}()
	writeHeader := func(status int) {
		now := time.Now()
		event.ResponseNs, event.Status = now.Sub(epoch).Nanoseconds(), status
		if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
			event.RetryAfterType, event.RetryAfterWaitNs = retryAfter(w.Header().Get("Retry-After"), now)
		}
		w.WriteHeader(status)
	}
	if event.IsRange {
		c.rangeRequests.Add(1)
		if start, end, ok := parseRange(rangeHeader, size); ok {
			endExclusive := end + 1
			event.RangeStart, event.RangeEndExclusive = &start, &endExclusive
		}
	}
	if size <= 0 {
		event.ResponseNs, event.Status = time.Since(epoch).Nanoseconds(), http.StatusBadRequest
		http.Error(w, "size required", http.StatusBadRequest)
		return
	}
	mode := str(q, "mode", "declared")
	g := s.group(groupName, q)
	if *rtt > 0 {
		time.Sleep(*rtt)
	}
	if rangeHeader == "bytes=0-0" {
		if delay := num(q, "probedelay"); delay > 0 {
			timer := time.NewTimer(time.Duration(delay * float64(time.Millisecond)))
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
			}
		}
	}
	if g.bucket != nil {
		if ok, wait := g.bucket.take(); !ok {
			event.RefusalSource = "tokenBucket"
			g.refused.Add(1)
			c.refused.Add(1)
			switch str(q, "retry", "auto") {
			case "none":
			case "date":
				// HTTP dates have one-second resolution; round up.
				w.Header().Set("Retry-After", time.Now().Add(wait+time.Second).UTC().Format(http.TimeFormat))
			default:
				w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
			}
			status, _ := strconv.Atoi(str(q, "status", "429"))
			writeHeader(status)
			return
		}
	}
	if rangeHeader != "" && g.limited.Load() > 0 && g.limited.Add(-1) >= 0 {
		event.RefusalSource = "finiteRange"
		g.refused.Add(1)
		c.refused.Add(1)
		switch retry := str(q, "retry", "2"); {
		case retry == "none":
		case strings.HasPrefix(retry, "date:"):
			secs, _ := strconv.Atoi(strings.TrimPrefix(retry, "date:"))
			w.Header().Set("Retry-After", time.Now().Add(time.Duration(secs)*time.Second).UTC().Format(http.TimeFormat))
		default:
			w.Header().Set("Retry-After", retry)
		}
		status, _ := strconv.Atoi(str(q, "status", "429"))
		writeHeader(status)
		return
	}
	seed := seedOf(name + "/" + strconv.FormatInt(size, 10))
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Last-Modified", "Wed, 21 Oct 2026 12:00:55 GMT")
	h.Set("ETag", fmt.Sprintf(`"%s-%d"`, name, size))
	if mode == "declared" {
		h.Set("Accept-Ranges", "bytes")
	}
	start, end := int64(0), size-1
	status := http.StatusOK
	if rangeHeader != "" && mode != "ignore" {
		if event.RangeStart == nil {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			writeHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start, end = *event.RangeStart, *event.RangeEndExclusive-1
		status = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	if at := g.abortAt.Load(); at > 0 && start == at {
		g.resumed.Add(1)
	}
	if forced := int(num(q, "reject")); forced >= 400 {
		event.RefusalSource = "forced"
		writeHeader(forced)
		return
	}
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	event.BodyStart = &start
	writeHeader(status)
	var stream *pacer
	rate := num(q, "perconn")
	if slow := num(q, "slowonce"); slow > 0 && start >= size/3 && g.slow.CompareAndSwap(false, true) {
		c.slowStreams.Add(1)
		rate = slow
	}
	abort := int64(num(q, "abortafter"))
	if rate > 0 {
		stream = &pacer{rate: rate}
	}
	tailFrom := int64(num(q, "tailfrom") * float64(size))
	var tail *pacer
	if rate := num(q, "tailrate"); rate > 0 && tailFrom > 0 {
		tail = &pacer{rate: rate}
	}
	buf := make([]byte, chunkSize)
	ctx := r.Context()
	for off := start; off <= end; {
		n := int(min(int64(chunkSize), end-off+1))
		if stream != nil && stream.wait(ctx, n) != nil {
			return
		}
		if g.pacer != nil && g.pacer.wait(ctx, n) != nil {
			return
		}
		if tail != nil && off+int64(n) > tailFrom && tail.wait(ctx, n) != nil {
			return
		}
		block := buf[:n]
		if name == "valid.dmg" && int64(len(s.dmg)) == size {
			block = s.dmg[off : off+int64(n)]
		} else {
			fill(block, seed, off)
		}
		written, err := w.Write(block)
		event.BytesSent += int64(written)
		c.bytesSent.Add(int64(written))
		if err != nil {
			return
		}
		off += int64(n)
		if abort > 0 && off-start >= abort && off <= end && g.aborted.CompareAndSwap(false, true) {
			g.abortAt.Store(off)
			_ = http.NewResponseController(w).Flush()
			c.aborts.Add(1)
			panic(http.ErrAbortHandler)
		}
	}
}

func (s *server) sha(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := str(q, "name", "")
	size := int64(num(q, "size"))
	if name == "valid.dmg" && size > 0 && int64(len(s.dmg)) == size {
		sum := sha256.Sum256(s.dmg)
		w.Write([]byte(hex.EncodeToString(sum[:])))
		return
	}
	key := name + "/" + strconv.FormatInt(size, 10)
	s.mu.Lock()
	sum, ok := s.shas[key]
	s.mu.Unlock()
	if !ok {
		h := sha256.New()
		buf := make([]byte, chunkSize)
		seed := seedOf(key)
		for off := int64(0); off < size; off += chunkSize {
			n := int(min(int64(chunkSize), size-off))
			fill(buf[:n], seed, off)
			h.Write(buf[:n])
		}
		sum = hex.EncodeToString(h.Sum(nil))
		s.mu.Lock()
		s.shas[key] = sum
		s.mu.Unlock()
	}
	w.Write([]byte(sum))
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	c := s.c
	groups := map[string]int64{}
	resumes := map[string]map[string]int64{}
	for name, g := range s.groups {
		label := fixtureLabel(name)
		groups[label] += g.refused.Load()
		if at := g.abortAt.Load(); at > 0 {
			resumes[label] = map[string]int64{"missingFrom": at, "exactResumeRequests": g.resumed.Load()}
		}
	}
	result := map[string]any{
		"epochUnixNs": s.epoch.UnixNano(),
		"connections": c.connections.Load(), "requests": c.requests.Load(),
		"rangeRequests": c.rangeRequests.Load(), "refused": c.refused.Load(),
		"bytesSent": c.bytesSent.Load(), "inflight": c.inflight.Load(), "maxInflight": c.maxInflight.Load(),
		"refusedByGroup": groups, "aborts": c.aborts.Load(), "slowStreams": c.slowStreams.Load(),
		"requestSeconds": float64(c.requestNanos.Load()) / float64(time.Second),
		"disconnections": resumes,
	}
	if r.URL.Query().Get("summary") == "1" {
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	timeline := append([]requestEvent{}, s.timeline...)
	s.mu.Unlock()

	// Arrival order is independent of when handler completions were published.
	sort.Slice(timeline, func(i, j int) bool {
		a, b := timeline[i], timeline[j]
		return a.StartNs < b.StartNs || a.StartNs == b.StartNs && a.ID < b.ID
	})
	statuses := map[int]int64{}
	retryKinds := map[string]int64{}
	for _, event := range timeline {
		statuses[event.Status]++
		if event.RetryAfterType != "" {
			retryKinds[event.RetryAfterType]++
		}
	}
	result["duplicateBytesSent"] = duplicateBytes(timeline)
	result["statusCounts"] = statuses
	result["retryAfterCounts"] = retryKinds
	result["requestTimeline"] = timeline
	_ = json.NewEncoder(w).Encode(result)
}

func (s *server) reset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c.inflight.Load() != 0 {
		http.Error(w, "download requests still in flight", http.StatusConflict)
		return
	}
	s.groups = map[string]*group{}
	s.timeline = nil
	s.c = &counters{}
	s.epoch = time.Now()
	w.WriteHeader(http.StatusNoContent)
}

// delayedConn holds a new connection's first read, like a handshake round trip.
type delayedConn struct {
	net.Conn
	once sync.Once
}

func (c *delayedConn) Read(p []byte) (int, error) {
	c.once.Do(func() { time.Sleep(*acceptDelay) })
	return c.Conn.Read(p)
}

type delayedListener struct{ net.Listener }

func (l delayedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil || *acceptDelay <= 0 {
		return conn, err
	}
	return &delayedConn{Conn: conn}, nil
}

func selfSigned() tls.Certificate {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kownload-fixture"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func main() {
	flag.Parse()
	s := &server{groups: map[string]*group{}, shas: map[string]string{}, c: &counters{}, epoch: time.Now()}
	if *dmgFixture != "" {
		var err error
		s.dmg, err = os.ReadFile(*dmgFixture)
		if err != nil {
			panic(err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/f/", s.serveFile)
	mux.HandleFunc("/sha", s.sha)
	mux.HandleFunc("/stats", s.stats)
	mux.HandleFunc("/reset", s.reset)
	connContext := func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, downloadConnKey{}, new(atomic.Pointer[counters]))
	}
	cert := selfSigned()
	ports := map[string]int{}
	wanted := map[string]string{"plain": "0", "tls": "0", "tls1": "0"}
	if *fixedPorts != "" {
		parts := strings.Split(*fixedPorts, ",")
		for i, name := range []string{"plain", "tls", "tls1"} {
			if i < len(parts) {
				wanted[name] = parts[i]
			}
		}
	}
	serve := func(name string, protos []string) {
		ln, err := net.Listen("tcp", "127.0.0.1:"+wanted[name])
		if err != nil {
			panic(err)
		}
		ports[name] = ln.Addr().(*net.TCPAddr).Port
		srv := &http.Server{Handler: mux, ConnContext: connContext}
		var l net.Listener = delayedListener{ln}
		if protos != nil {
			srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: protos}
			if len(protos) == 1 {
				srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
			}
			l = tls.NewListener(l, srv.TLSConfig)
		}
		go srv.Serve(l)
	}
	serve("plain", nil)
	serve("tls", []string{"h2", "http/1.1"})
	serve("tls1", []string{"http/1.1"})
	ready, _ := json.Marshal(ports)
	if *readyFile != "" {
		os.WriteFile(*readyFile+".tmp", ready, 0o600)
		os.Rename(*readyFile+".tmp", *readyFile)
	}
	fmt.Println(string(ready))
	select {}
}
