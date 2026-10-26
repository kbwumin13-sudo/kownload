package http

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// A server that hides Accept-Ranges but serves 206 gets one probe and then
// several range requests in flight at the same time.
func TestRangeProbeLeadsToConcurrentRangeRequests(t *testing.T) {
	data := rangeProbePayload(8 * 1024 * 1024)
	var probes, inflight, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			probes.Add(1)
		} else if r.Header.Get("Range") != "" {
			n := inflight.Add(1)
			defer inflight.Add(-1)
			for {
				m := peak.Load()
				if n <= m || peak.CompareAndSwap(m, n) {
					break
				}
			}
		}
		http.ServeContent(&hiddenRangeWriter{ResponseWriter: w, delay: 2 * time.Millisecond}, r,
			"payload.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer server.Close()
	f := buildFetcher()
	if err := f.Resolve(&base.Request{URL: server.URL + "/payload.bin"}, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	checkRangeProbeDownload(t, f, data)
	if probes.Load() != 1 || peak.Load() < 2 {
		t.Fatalf("probes=%d peak concurrent range requests=%d", probes.Load(), peak.Load())
	}
}

// countingBody records how much of a probe response the client consumed.
type countingBody struct {
	reader io.Reader
	read   atomic.Int64
	closed atomic.Bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error {
	b.closed.Store(true)
	return nil
}

type probeTransport struct {
	body   *countingBody
	header http.Header
	status int
}

func (t *probeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: t.status, Header: t.header, Body: t.body, Request: req,
		ContentLength: -1}, nil
}

func TestRangeProbeReadsAtMostTwoBytesOfAnOversizedBody(t *testing.T) {
	size := int64(4 * 1024 * 1024)
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/file", nil)
	first := &http.Response{StatusCode: http.StatusOK, Request: req,
		Header: http.Header{"Content-Length": {strconv.FormatInt(size, 10)}}}
	for _, status := range []int{http.StatusPartialContent, http.StatusOK} {
		body := &countingBody{reader: bytes.NewReader(make([]byte, 1<<20))}
		header := http.Header{"Content-Range": {"bytes 0-0/" + strconv.FormatInt(size, 10)}}
		client := &http.Client{Transport: &probeTransport{body: body, header: header, status: status}}
		ok, state, reason := probeRangeSupport(context.Background(), client, req, first, size, 4)
		if ok || state != rangeUnsupported {
			t.Fatalf("status %d: oversized probe accepted (%s/%s)", status, state, reason)
		}
		if read := body.read.Load(); read > 2 || !body.closed.Load() {
			t.Fatalf("status %d: probe consumed %d bytes, closed=%t", status, read, body.closed.Load())
		}
	}
}

func TestRangeProbeKeepsCustomHostHeader(t *testing.T) {
	data := rangeProbePayload(2 * 1024 * 1024)
	var hosts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.Host)
		http.ServeContent(&hiddenRangeWriter{ResponseWriter: w, delay: time.Millisecond}, r,
			"payload.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	defer server.Close()
	f := buildFetcher()
	req := &base.Request{URL: server.URL + "/payload.bin",
		Extra: &fhttp.ReqExtra{Header: map[string]string{"Host": "files.example.test"}}}
	if err := f.Resolve(req, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	if !f.Meta().Res.Range {
		t.Fatal("probe with a custom Host header failed")
	}
	checkRangeProbeDownload(t, f, data)
	for _, host := range hosts {
		if !strings.EqualFold(host, "files.example.test") {
			t.Fatalf("a request lost the custom Host header: %q", host)
		}
	}
}
