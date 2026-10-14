package http

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func prefetchCopyFile(t *testing.T, data []byte) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "prefetch-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestStopPrefetchCopyCountsWrittenBytes(t *testing.T) {
	data := bytes.Repeat([]byte("p"), 32*1024+17)
	for _, tc := range []struct {
		name string
		want int64
	}{
		{"closed_source", 0},
		{"write_only_source", 0},
		{"read_only_target", 0},
		{"closed_target", 0},
		{"truncated_source", int64(len(data) - 1)},
		{"missing_source", 0},
		{"missing_target", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := prefetchCopyFile(t, data)
			target := prefetchCopyFile(t, nil)
			f := &Fetcher{prefetchFile: source, prefetchFilePath: source.Name(), file: target}
			f.prefetchSize.Store(int64(len(data)))
			switch tc.name {
			case "closed_source":
				_ = source.Close()
			case "write_only_source":
				writeOnly, err := os.OpenFile(source.Name(), os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				f.prefetchFile = writeOnly
			case "read_only_target":
				readOnly, err := os.Open(target.Name())
				if err != nil {
					t.Fatal(err)
				}
				defer readOnly.Close()
				f.file = readOnly
			case "closed_target":
				_ = target.Close()
			case "truncated_source":
				if err := source.Truncate(tc.want); err != nil {
					t.Fatal(err)
				}
			case "missing_source":
				f.prefetchFile = nil
			case "missing_target":
				f.file = nil
			}
			copied, err := f.stopPrefetchAndCopyData()
			if err == nil {
				t.Error("failed copy did not report an error")
			}
			if copied != tc.want {
				t.Errorf("reported copied bytes = %d, want actual writes %d", copied, tc.want)
			}
			if f.prefetchFile != nil || f.prefetchFilePath != "" {
				t.Error("prefetch ownership was not released after copy failure")
			}
			if _, err := os.Stat(source.Name()); !os.IsNotExist(err) {
				t.Errorf("prefetch file was not removed: %v", err)
			}
			if copied, err := f.stopPrefetchAndCopyData(); copied != 0 || err != nil {
				t.Errorf("consumed prefetch was reused: copied=%d, err=%v", copied, err)
			}
		})
	}
}

type shortPrefetchWriter struct {
	data []byte
	err  error
	full bool
}

func (w *shortPrefetchWriter) WriteAt(p []byte, off int64) (int, error) {
	n := len(p) / 2
	if w.full {
		n = len(p)
	}
	w.data = append(w.data, p[:n]...)
	return n, w.err
}

func TestCopyPrefetchDataShortWrite(t *testing.T) {
	writeErr := errors.New("disk full")
	for _, tc := range []struct {
		name string
		err  error
		want error
		full bool
		n    int64
	}{
		{"without_error", nil, io.ErrShortWrite, false, 4},
		{"with_error", writeErr, writeErr, false, 4},
		{"full_write_with_error", writeErr, writeErr, true, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := &shortPrefetchWriter{err: tc.err, full: tc.full}
			copied, err := copyPrefetchData(target, strings.NewReader("abcdefgh"), 8)
			if copied != tc.n || string(target.data) != "abcdefgh"[:tc.n] || !errors.Is(err, tc.want) {
				t.Fatalf("short write: copied=%d, data=%q, err=%v", copied, target.data, err)
			}
		})
	}
}

func TestStopPrefetchCopyBoundsAndConsumesPrefix(t *testing.T) {
	source := prefetchCopyFile(t, []byte("prefix-trailing-data"))
	target := prefetchCopyFile(t, nil)
	f := &Fetcher{prefetchFile: source, prefetchFilePath: source.Name(), file: target}
	f.prefetchSize.Store(6)
	if copied, err := f.stopPrefetchAndCopyData(); copied != 6 || err != nil {
		t.Fatalf("prefix copy: copied=%d, err=%v", copied, err)
	}
	got, err := os.ReadFile(target.Name())
	if err != nil || string(got) != "prefix" {
		t.Fatalf("prefix copy wrote unrecorded bytes: data=%q, err=%v", got, err)
	}
	if copied, err := f.stopPrefetchAndCopyData(); copied != 0 || err != nil {
		t.Fatalf("consumed prefix was reused: copied=%d, err=%v", copied, err)
	}
}

type readOnlyPrefetchTarget struct{ fetcher *Fetcher }

func (c readOnlyPrefetchTarget) Touch(name string, size int64) (*os.File, error) {
	if c.fetcher != nil {
		<-c.fetcher.prefetchDoneCh
	}
	if err := os.WriteFile(name, make([]byte, size), 0600); err != nil {
		return nil, err
	}
	return os.Open(name)
}

func TestFetcherPrefetchCopyFailureRetry(t *testing.T) {
	data := strings.Repeat("prefetch payload\n", 4096)
	for _, failure := range []string{"write_failure", "incomplete_read", "seek_failure", "pause_before_start"} {
		t.Run(failure, func(t *testing.T) {
			requests := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.Header.Get("Range")
				http.ServeContent(w, r, "payload.bin", time.Time{}, strings.NewReader(data))
			}))
			defer server.Close()
			f := buildFetcher()
			t.Cleanup(func() { _ = f.Pause() })
			f.meta.Req = &base.Request{URL: server.URL + "/payload.bin"}
			f.meta.Opts = &base.Options{Path: t.TempDir(), Name: "payload.bin", Extra: &fhttp.OptsExtra{Connections: 1}}
			f.meta.Res = &base.Resource{Range: true, Size: int64(len(data)), Files: []*base.FileInfo{{Name: "payload.bin", Size: int64(len(data))}}}
			f.setState(stateResolved)
			close(f.resolvedCh)
			source := prefetchCopyFile(t, []byte(data))
			f.prefetchFile, f.prefetchFilePath = source, source.Name()
			f.prefetchSize.Store(int64(len(data)))
			switch failure {
			case "write_failure":
				f.ctl.FileController = readOnlyPrefetchTarget{}
			case "incomplete_read":
				if err := source.Truncate(int64(len(data) - 1)); err != nil {
					t.Fatal(err)
				}
			case "seek_failure":
				_ = source.Close()
			}
			wantState := stateError
			if failure == "pause_before_start" {
				if err := f.Pause(); err != nil {
					t.Fatal(err)
				}
				wantState = statePaused
			} else if err := f.Start(); err == nil {
				t.Fatal("Start accepted a failed prefetch copy as downloaded data")
			}
			if f.getState() != wantState || f.file != nil || f.downloadLoopDone != nil {
				t.Fatalf("failed copy did not stop startup and release target: state=%v, file=%v, loop=%v", f.getState(), f.file, f.downloadLoopDone)
			}
			if f.resolveDataPos.Load() != 0 || f.Progress().TotalDownloaded() != 0 || len(f.doneCh) != 0 {
				t.Fatal("failed copy was counted as progress or completion")
			}
			if _, err := os.Stat(source.Name()); !os.IsNotExist(err) {
				t.Fatalf("failed copy leaked temporary file: %v", err)
			}
			f.ctl.FileController = &controller.DefaultFileController{}
			if err := f.Start(); err != nil {
				t.Fatalf("retry after prefetch failure: %v", err)
			}
			select {
			case err := <-f.doneCh:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("retry did not complete")
			}
			if got := <-requests; !strings.HasPrefix(got, "bytes=0-") {
				t.Fatalf("retry skipped missing prefix: Range=%q", got)
			}
			got, err := os.ReadFile(filepath.Join(f.meta.Opts.Path, "payload.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != data || f.Progress().TotalDownloaded() != int64(len(data)) {
				t.Fatal("retry did not restore the complete payload and progress")
			}
		})
	}
}

func TestFetcherPrefetchPendingStartReportsCopyError(t *testing.T) {
	data := strings.Repeat("payload", 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "payload.bin", time.Time{}, strings.NewReader(data))
	}))
	defer server.Close()
	f := buildFetcher()
	t.Cleanup(func() { _ = f.Pause() })
	// Hold target creation until the real prefetch has completed, then return
	// a read-only target to force the asynchronous startup's copy to fail.
	f.ctl.FileController = readOnlyPrefetchTarget{fetcher: f}
	f.setState(stateResolving)
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := f.Resolve(&base.Request{URL: server.URL + "/payload.bin"}, &base.Options{
		Path: t.TempDir(), Name: "payload.bin", Extra: &fhttp.OptsExtra{Connections: 1},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-f.doneCh:
		if err == nil || f.getState() != stateError || f.file != nil || f.resolveDataPos.Load() != 0 {
			t.Fatalf("pending startup did not propagate copy failure: err=%v, state=%v", err, f.getState())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending startup swallowed the copy error")
	}
}
