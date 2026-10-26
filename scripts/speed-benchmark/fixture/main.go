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
//	/sha?name=&size=            SHA-256 of that content
//	/stats                      counters since the last /reset
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
}

type server struct {
	mu     sync.Mutex
	groups map[string]*group
	shas   map[string]string
	c      *counters
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

func (s *server) serveFile(w http.ResponseWriter, r *http.Request) {
	c := s.c
	c.requests.Add(1)
	n := c.inflight.Add(1)
	defer c.inflight.Add(-1)
	for {
		m := c.maxInflight.Load()
		if n <= m || c.maxInflight.CompareAndSwap(m, n) {
			break
		}
	}
	q := r.URL.Query()
	name := strings.TrimPrefix(r.URL.Path, "/f/")
	size := int64(num(q, "size"))
	if size <= 0 {
		http.Error(w, "size required", http.StatusBadRequest)
		return
	}
	mode := str(q, "mode", "declared")
	g := s.group(str(q, "group", name), q)
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" {
		c.rangeRequests.Add(1)
	}
	if *rtt > 0 {
		time.Sleep(*rtt)
	}
	if g.bucket != nil {
		if ok, wait := g.bucket.take(); !ok {
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
			w.WriteHeader(status)
			return
		}
	}
	if rangeHeader != "" && g.limited.Load() > 0 && g.limited.Add(-1) >= 0 {
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
		w.WriteHeader(status)
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
		var ok bool
		if start, end, ok = parseRange(rangeHeader, size); !ok {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		status = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(status)
	var stream *pacer
	if rate := num(q, "perconn"); rate > 0 {
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
		fill(buf[:n], seed, off)
		written, err := w.Write(buf[:n])
		c.bytesSent.Add(int64(written))
		if err != nil {
			return
		}
		off += int64(n)
	}
}

func (s *server) sha(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := str(q, "name", "")
	size := int64(num(q, "size"))
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

func (s *server) stats(w http.ResponseWriter, _ *http.Request) {
	c := s.c
	groups := map[string]int64{}
	s.mu.Lock()
	for name, g := range s.groups {
		groups[name] = g.refused.Load()
	}
	s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{
		"connections": c.connections.Load(), "requests": c.requests.Load(),
		"rangeRequests": c.rangeRequests.Load(), "refused": c.refused.Load(),
		"bytesSent": c.bytesSent.Load(), "maxInflight": c.maxInflight.Load(),
		"refusedByGroup": groups,
	})
}

func (s *server) reset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.groups = map[string]*group{}
	s.mu.Unlock()
	s.c.connections.Store(0)
	s.c.requests.Store(0)
	s.c.rangeRequests.Store(0)
	s.c.refused.Store(0)
	s.c.bytesSent.Store(0)
	s.c.maxInflight.Store(0)
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
	s := &server{groups: map[string]*group{}, shas: map[string]string{}, c: &counters{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/f/", s.serveFile)
	mux.HandleFunc("/sha", s.sha)
	mux.HandleFunc("/stats", s.stats)
	mux.HandleFunc("/reset", s.reset)
	state := func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			s.c.connections.Add(1)
		}
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
		srv := &http.Server{Handler: mux, ConnState: state}
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
