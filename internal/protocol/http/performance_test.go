package http

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// localSource serves size bytes from memory without any pacing, so the
// download is bounded by the client's read, write and bookkeeping costs.
func localSource(b testing.TB, size int) (*httptest.Server, []byte) {
	data := rangeProbePayload(size)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := 0, size-1
		if raw := r.Header.Get("Range"); raw != "" {
			if b, e, ok := parseSimpleRange(raw, size); ok {
				start, end = b, e
				w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(end)+"/"+strconv.Itoa(size))
				w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(data[start : end+1])
				return
			}
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = w.Write(data)
	}))
	return srv, data
}

func parseSimpleRange(raw string, size int) (int, int, bool) {
	var begin, end int
	if n, err := fmt.Sscanf(raw, "bytes=%d-%d", &begin, &end); err != nil || n != 2 || begin > end || end >= size {
		return 0, 0, false
	}
	return begin, end, true
}

// BenchmarkLocalDownload measures a 256 MiB download from an unpaced local
// source. Run with -cpuprofile to see where client CPU goes, for example:
//
//	go test -run '^$' -bench LocalDownload -benchtime 3x -cpuprofile cpu.out ./internal/protocol/http
//
// KOWNLOAD_BENCH_BUFFER and KOWNLOAD_BENCH_POOL=0 select read buffer size and
// private transports for comparisons.
func BenchmarkLocalDownload(b *testing.B) {
	const size = 256 << 20
	srv, data := localSource(b, size)
	defer srv.Close()
	want := sha256.Sum256(data)
	tune := &tuningConfig{}
	if v, err := strconv.Atoi(os.Getenv("KOWNLOAD_BENCH_BUFFER")); err == nil {
		tune.ReadBuffer = v
	}
	if os.Getenv("KOWNLOAD_BENCH_POOL") == "0" {
		tune.SharedTransport = boolPtr(false)
	}
	pool := transfer.NewTransportPool()
	defer pool.Close()
	budget := transfer.NewBudget()
	defer budget.Close()
	ctl := pooledController(pool, budget, tune)
	dir := b.TempDir()
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := newPooledFetcher(ctl)
		opts := &base.Options{Path: dir, Name: "bench.bin", Extra: &fhttp.OptsExtra{Connections: 4}}
		if err := f.Resolve(&base.Request{URL: srv.URL + "/bench.bin"}, opts); err != nil {
			b.Fatal(err)
		}
		if err := f.Start(); err != nil {
			b.Fatal(err)
		}
		if err := <-f.doneCh; err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		got, err := os.ReadFile(f.Meta().SingleFilepath())
		if err != nil || sha256.Sum256(got) != want {
			b.Fatal("content mismatch")
		}
		os.Remove(f.Meta().SingleFilepath())
		b.StartTimer()
	}
}
