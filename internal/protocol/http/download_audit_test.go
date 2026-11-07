package http

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// Audit-only regression: deliberately red on the unmodified build 25 baseline.
func TestDownloadAuditUnknownSizePartialMustFail(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 2 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // No Content-Length: use chunked encoding.
		w.Write([]byte("ABCD"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // Missing final chunk: transport reports unexpected EOF.
	}))
	defer srv.Close()
	f := buildFetcher()
	defer f.Close()
	if err := f.Resolve(&base.Request{URL: srv.URL + "/synthetic.bin"}, &base.Options{Path: t.TempDir(), Name: "synthetic.bin", Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
		t.Fatal(err)
	}
	if !f.Meta().Res.Range || f.Meta().Res.Size != 0 {
		t.Fatal("fixture did not enter declared-range, unknown-size path")
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	err := waitDone(t, f, 10*time.Second)
	got, readErr := os.ReadFile(f.Meta().SingleFilepath())
	if readErr != nil {
		t.Fatal(readErr)
	}
	t.Logf("requests=%d file_bytes=%d progress=%d wait_error=%v state=%d", requests.Load(), len(got), f.Progress().TotalDownloaded(), err, f.getState())
	if err == nil {
		t.Error("interrupted unknown-size stream reported success with a truncated file")
	}
}

// A valid persisted partial checkpoint from v1 is patched to v2 of equal size.
// Resume must reject the changed representation or restart instead of mixing it.
func TestDownloadAuditPatchedVersionMustNotSplice(t *testing.T) {
	old := bytes.Repeat([]byte("A"), 32)
	fresh := bytes.Repeat([]byte("B"), 32)
	var missingIfRange atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var begin, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end); err != nil {
			t.Error("resume should request its saved range")
			w.WriteHeader(400)
			return
		}
		missingIfRange.Store(r.Header.Get("If-Range") == "")
		w.Header().Set("ETag", `"version-2"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(fresh)))
		w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(fresh[begin : end+1])
	}))
	defer srv.Close()
	opts := &base.Options{Path: t.TempDir(), Name: "synthetic.bin", Extra: &fhttp.OptsExtra{Connections: 1}}
	meta := &fetcher.FetcherMeta{Req: &base.Request{URL: srv.URL + "/version-1", Extra: &fhttp.ReqExtra{}}, Opts: opts, Res: &base.Resource{Range: true, Size: int64(len(old)), Files: []*base.FileInfo{{Name: "synthetic.bin", Size: int64(len(old))}}}}
	if err := os.WriteFile(meta.SingleFilepath(), old[:8], 0600); err != nil {
		t.Fatal(err)
	}
	data := &fetcherData{Connections: []*connection{{ID: 0, Role: rolePrimary, State: connNotStarted, Chunk: &chunk{Begin: 0, End: 31, Downloaded: 8}, Downloaded: 8}}}
	_, restore := (&FetcherManager{}).Restore()
	f := restore(meta, data).(*Fetcher)
	// Reuse the isolated controller configuration used by existing tests.
	controller := buildFetcher().ctl
	f.Setup(controller)
	defer f.Close()
	if err := f.Patch(&base.Request{URL: srv.URL + "/version-2"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	err := waitDone(t, f, 10*time.Second)
	got, readErr := os.ReadFile(meta.SingleFilepath())
	if readErr != nil {
		t.Fatal(readErr)
	}
	mixed := append(append([]byte{}, old[:8]...), fresh[8:]...)
	t.Logf("wait_error=%v file_bytes=%d missing_if_range=%t mixed_versions=%t", err, len(got), missingIfRange.Load(), bytes.Equal(got, mixed))
	if err == nil && !bytes.Equal(got, fresh) {
		t.Error("patched URL resumed successfully with bytes spliced from two versions")
	}
}

// Public Resolve/Start/Wait path, including one manual restart then an automatic
// full-GET retry. A full GET must never append behind the previous partial GET.
func TestDownloadAuditNonRangeRetryMustMatchResource(t *testing.T) {
	payload := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUV")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		if n <= 2 {
			w.Write(payload[:4])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		w.Write(payload)
	}))
	defer srv.Close()
	f := buildFetcher()
	defer f.Close()
	if err := f.Resolve(&base.Request{URL: srv.URL + "/synthetic.bin"}, &base.Options{Path: t.TempDir(), Name: "synthetic.bin", Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
		t.Fatal(err)
	}
	if f.Meta().Res.Range {
		t.Fatal("fixture unexpectedly supports range")
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, f, 10*time.Second); err == nil {
		t.Fatal("initial known-size interruption should report incomplete")
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	err := waitDone(t, f, 10*time.Second)
	got, readErr := os.ReadFile(f.Meta().SingleFilepath())
	if readErr != nil {
		t.Fatal(readErr)
	}
	t.Logf("requests=%d expected_bytes=%d file_bytes=%d progress=%d wait_error=%v prefixed_duplicate=%t", requests.Load(), len(payload), len(got), f.Progress().TotalDownloaded(), err, bytes.Equal(got, append(append([]byte{}, payload[:4]...), payload...)))
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("retried full response must exactly match the resource: error=%v bytes_equal=%t", err, bytes.Equal(got, payload))
	}
}
