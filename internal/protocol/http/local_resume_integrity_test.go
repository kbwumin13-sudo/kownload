package http

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func integrityPayload(size int) []byte {
	data := make([]byte, size)
	_, _ = rand.New(rand.NewSource(20261101)).Read(data)
	return data
}

func awaitIntegrityDownload(t *testing.T, f *Fetcher) {
	t.Helper()
	select {
	case err := <-f.doneCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fixture download did not finish")
	}
	if err := f.Pause(); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPFixtureByteIntegrity(t *testing.T) {
	for _, size := range []int{48*1024 + 17, 4*1024*1024 + 123} {
		for _, streams := range []int{1, 4} {
			t.Run(fmt.Sprintf("size=%d/streams=%d", size, streams), func(t *testing.T) {
				data := integrityPayload(size)
				srv := httptest.NewServer(servePayload(data, true))
				defer srv.Close()
				pool, budget := transfer.NewTransportPool(), transfer.NewBudget()
				defer pool.Close()
				defer budget.Close()
				f := newPooledFetcher(pooledController(pool, budget, nil))
				defer f.Pause()
				if err := download(t, f, srv.URL+"/fixture.bin", t.TempDir(), "fixture.bin", streams, false); err != nil {
					t.Fatal(err)
				}
				if err := f.Pause(); err != nil {
					t.Fatal(err)
				}
				checkFile(t, f.Meta().SingleFilepath(), data)
				stats := f.Stats().(*fhttp.Stats)
				if streams == 4 && size > 1<<20 && len(stats.Connections) < 2 {
					t.Fatal("fixture did not exercise multiple streams")
				}
				t.Logf("bytes=%d streams=%d actual-connections=%d md5=%x", size, streams, len(stats.Connections), md5.Sum(data))
			})
		}
	}
}

func TestHTTPResumeUnavailableLocalFile(t *testing.T) {
	for _, complete := range []bool{false, true} {
		for _, restore := range []bool{false, true} {
			for _, mutation := range []string{"unchanged", "deleted", "truncated", "replaced-short"} {
				t.Run(fmt.Sprintf("complete=%t/restore=%t/%s", complete, restore, mutation), func(t *testing.T) {
					data := integrityPayload(4*1024*1024 + 123)
					srv := httptest.NewServer(servePayload(data, true))
					defer srv.Close()
					pool, budget := transfer.NewTransportPool(), transfer.NewBudget()
					defer pool.Close()
					defer budget.Close()
					ctl := pooledController(pool, budget, nil)
					f := newPooledFetcher(ctl)
					defer f.Pause()
					if err := f.Resolve(&base.Request{URL: srv.URL + "/fixture.bin"}, &base.Options{Path: t.TempDir(), Name: "fixture.bin", Extra: &fhttp.OptsExtra{Connections: 4}}); err != nil {
						t.Fatal(err)
					}
					if err := f.Start(); err != nil {
						t.Fatal(err)
					}
					if complete {
						awaitIntegrityDownload(t, f)
					} else {
						deadline := time.Now().Add(3 * time.Second)
						for f.Progress().TotalDownloaded() < 128*1024 && time.Now().Before(deadline) {
							time.Sleep(time.Millisecond)
						}
						if err := f.Pause(); err != nil {
							t.Fatal(err)
						}
						progress := f.Progress().TotalDownloaded()
						if progress == 0 || progress >= int64(len(data)) {
							t.Fatalf("fixture must pause with partial data, got %d", progress)
						}
					}
					if restore {
						fm := &FetcherManager{}
						checkpoint, err := fm.Store(f)
						if err != nil {
							t.Fatal(err)
						}
						template, restoreFetcher := fm.Restore()
						saved, _ := json.Marshal(checkpoint)
						if err := json.Unmarshal(saved, template); err != nil {
							t.Fatal(err)
						}
						var meta fetcher.FetcherMeta
						saved, _ = json.Marshal(f.Meta())
						if err := json.Unmarshal(saved, &meta); err != nil {
							t.Fatal(err)
						}
						f = restoreFetcher(&meta, template).(*Fetcher)
						f.Setup(ctl)
						defer f.Pause()
					}
					path := f.Meta().SingleFilepath()
					switch mutation {
					case "deleted":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
					case "truncated":
						if err := os.Truncate(path, 1); err != nil {
							t.Fatal(err)
						}
					case "replaced-short":
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					if err := f.Start(); err != nil {
						t.Fatal(err)
					}
					awaitIntegrityDownload(t, f)
					checkFile(t, path, data)
					if f.Progress().TotalDownloaded() != int64(len(data)) {
						t.Fatal("resume progress does not match the complete file")
					}
				})
			}
		}
	}
}

func TestHTTPNonRangeCompletedResumeUnavailableFile(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			data := integrityPayload(200*1024 + 31)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(len(data)))
				_, _ = w.Write(data)
			}))
			defer srv.Close()
			f := buildFetcher()
			defer f.Pause()
			if err := download(t, f, srv.URL+"/fixture.bin", t.TempDir(), "fixture.bin", 1, false); err != nil {
				t.Fatal(err)
			}
			if err := f.Pause(); err != nil {
				t.Fatal(err)
			}
			if f.Meta().Res.Range {
				t.Fatal("fixture must exercise a non-range stream")
			}
			path := f.Meta().SingleFilepath()
			var err error
			if missing {
				err = os.Remove(path)
			} else {
				err = os.Truncate(path, 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			awaitIntegrityDownload(t, f)
			checkFile(t, path, data)
		})
	}
}

func TestHTTPPrefetchOnlyResumeUnavailableFile(t *testing.T) {
	for _, mode := range []string{"unchanged", "deleted", "truncated", "deleted-refused"} {
		t.Run(mode, func(t *testing.T) {
			data := integrityPayload(48*1024 + 17)
			var refuse atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if refuse.Load() {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				http.ServeContent(w, r, "fixture.bin", time.Time{}, bytes.NewReader(data))
			}))
			defer srv.Close()
			f := buildFetcher()
			defer f.Pause()
			if err := f.Resolve(&base.Request{URL: srv.URL + "/fixture.bin"}, &base.Options{Path: t.TempDir(), Name: "fixture.bin"}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-f.prefetchDoneCh:
			case <-time.After(time.Second):
				t.Fatal("small fixture did not finish prefetch")
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			awaitIntegrityDownload(t, f)
			if len(f.connections) != 0 || f.resolveConn == nil || !f.resolveConn.Completed {
				t.Fatal("fixture did not use the prefetch-only completion path")
			}
			path := f.Meta().SingleFilepath()
			var err error
			if mode == "truncated" {
				err = os.Truncate(path, 1)
			} else if mode != "unchanged" {
				err = os.Remove(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			refuse.Store(mode == "deleted-refused")
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			if mode == "deleted-refused" {
				select {
				case err := <-f.doneCh:
					if err == nil {
						t.Fatal("stale prefetch completion hid the failed redownload")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("refused redownload did not finish")
				}
				return
			}
			awaitIntegrityDownload(t, f)
			checkFile(t, path, data)
			if got := f.Progress().TotalDownloaded(); got != int64(len(data)) {
				t.Fatalf("stale prefetch counted twice: got %d, want %d", got, len(data))
			}
		})
	}
}

func TestHTTPResumeTruncatedPrefetchPrefixBeforeRangeData(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprintf("restore=%t", restore), func(t *testing.T) {
			data := integrityPayload(2*1024*1024 + 123)
			const prefix = 128 * 1024
			var blockRange atomic.Bool
			blockRange.Store(true)
			entered := make(chan struct{})
			var once sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "" {
					w.Header().Set("Accept-Ranges", "bytes")
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					_, _ = w.Write(data[:prefix])
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				if blockRange.Load() {
					once.Do(func() { close(entered) })
					<-r.Context().Done()
					return
				}
				http.ServeContent(w, r, "fixture.bin", time.Time{}, bytes.NewReader(data))
			}))
			defer srv.Close()
			f := buildFetcher()
			defer f.Pause()
			if err := f.Resolve(&base.Request{URL: srv.URL + "/fixture.bin"}, &base.Options{Path: t.TempDir(), Name: "fixture.bin", Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for f.prefetchSize.Load() < prefix && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if f.prefetchSize.Load() != prefix {
				t.Fatal("fixture did not prefetch the intended prefix")
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("range request did not start")
			}
			if err := f.Pause(); err != nil {
				t.Fatal(err)
			}
			if len(f.connections) != 1 || f.connections[0].Chunk.Begin != prefix || f.connections[0].Chunk.Downloaded != 0 {
				t.Fatal("fixture wrote range bytes before pausing")
			}
			if restore {
				fm := &FetcherManager{}
				checkpoint, err := fm.Store(f)
				if err != nil {
					t.Fatal(err)
				}
				value, restoreFetcher := fm.Restore()
				encoded, _ := json.Marshal(checkpoint)
				if err := json.Unmarshal(encoded, value); err != nil {
					t.Fatal(err)
				}
				f = restoreFetcher(f.Meta(), value).(*Fetcher)
				f.Setup(buildFetcher().ctl)
				defer f.Pause()
			}
			path := f.Meta().SingleFilepath()
			if err := os.Truncate(path, 1); err != nil {
				t.Fatal(err)
			}
			blockRange.Store(false)
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			awaitIntegrityDownload(t, f)
			checkFile(t, path, data)
		})
	}
}
