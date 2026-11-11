package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	fetchModel "github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func TestAuthPostRedirectCachesFinalRequestForRangeDownload(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusSeeOther} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			payload := rangeProbePayload(4 << 20)
			var ranged atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != "GET" || len(body) != 0 || r.Header.Get("Authorization") != "" {
					t.Error("original POST context reached target")
				}
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("ETag", `"stable"`)
				if r.Header.Get("Range") == "" {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					_, _ = w.Write(payload[:4096])
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				ranged.Add(1)
				http.ServeContent(&hiddenRangeWriter{ResponseWriter: w, delay: time.Millisecond}, r, "payload.bin", time.Time{}, bytes.NewReader(payload))
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL+"/payload", status) }))
			defer source.Close()
			f := buildFetcher()
			defer f.Close()
			original := &base.Request{URL: source.URL, Extra: &model.ReqExtra{Method: "POST", Body: "source-form=A", Header: map[string]string{"Authorization": "Bearer A"}}}
			if e := f.Resolve(original, &base.Options{Path: t.TempDir(), Name: "payload.bin", Extra: &model.OptsExtra{Connections: 2}}); e != nil {
				t.Fatal(e)
			}
			if !f.Meta().Res.Range {
				t.Fatal("range response not recognized")
			}
			if e := f.Start(); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() { done <- f.Wait() }()
			select {
			case e := <-done:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cached redirect did not complete range download")
			}
			content, e := os.ReadFile(f.Meta().SingleFilepath())
			if e != nil || !bytes.Equal(content, payload) || ranged.Load() == 0 {
				t.Fatal("download missing real ranged content", e)
			}
			fallback, e := f.buildRequestWithOriginalURL(context.Background(), f.Meta().Req)
			if e != nil {
				t.Fatal(e)
			}
			body, _ := io.ReadAll(fallback.Body)
			if fallback.URL.String() != source.URL || fallback.Method != "POST" || string(body) != "source-form=A" {
				t.Fatal("original fallback request was rewritten")
			}
			saved, e := (&FetcherManager{}).Store(f)
			if e != nil {
				t.Fatal(e)
			}
			if saved.(*fetcherData).RedirectURL != "" {
				t.Fatal("method-converted URL persisted without method proof")
			}
		})
	}
}

func TestAuthPolicyRefusalStopsRangeAndFallbackLoops(t *testing.T) {
	for _, ranges := range []bool{false, true} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("ranges=%t/invalid=%t", ranges, invalid), func(t *testing.T) {
				payload := rangeProbePayload(2 << 20)
				var targetHits, sourceHits atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
				defer target.Close()
				targetURL := "http://user:synthetic@" + target.Listener.Addr().String() + "/file"
				var redirect atomic.Bool
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sourceHits.Add(1)
					if redirect.Load() {
						http.Redirect(w, r, targetURL, 302)
						return
					}
					if ranges {
						w.Header().Set("Accept-Ranges", "bytes")
					}
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					_, _ = w.Write(payload[:4096])
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				defer source.Close()
				// Use the target's authority, including explicit userinfo: no scoped proof
				// exists, so the final transport guard must refuse before sending to it.
				f := buildFetcher()
				defer f.Close()
				if e := f.Resolve(&base.Request{URL: source.URL}, &base.Options{Path: t.TempDir(), Name: "refused.bin", Extra: &model.OptsExtra{Connections: 1}}); e != nil {
					t.Fatal(e)
				}
				if f.Meta().Res.Range != ranges {
					t.Fatal("wrong transfer mode")
				}
				before := sourceHits.Load()
				f.stopPrefetch()
				f.resolveRespLock.Lock()
				if f.resolveResp != nil {
					_ = f.resolveResp.Body.Close()
					f.resolveResp = nil
				}
				f.resolveRespLock.Unlock()
				if invalid {
					f.SetRequestPolicy(func(r *http.Request) { r.URL.Scheme = "file" })
				} else {
					redirect.Store(true)
				}
				if e := f.Start(); e != nil {
					t.Fatal(e)
				}
				done := make(chan error, 1)
				go func() { done <- f.Wait() }()
				want := transfer.ErrScopedContext
				if invalid {
					want = transfer.ErrCredentialContext
				}
				select {
				case e := <-done:
					if !errors.Is(e, want) {
						t.Fatalf("wrong terminal classification: %v", e)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("permanent policy refusal retained running slot")
				}
				if targetHits.Load() != 0 || sourceHits.Load()-before > 1 {
					t.Fatal("refused context sent or retried network requests")
				}
			})
		}
	}
}

func TestAuthPostRedirectStoreRestoreRequiresFreshContext(t *testing.T) {
	payload := rangeProbePayload(2 << 20)
	var sourceHits, targetHits, badMethod atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != "GET" || len(body) != 0 {
			badMethod.Add(1)
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"stable"`)
		if r.Header.Get("Range") != "" {
			http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(payload))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload[:4096])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || string(body) != "original-form" {
			badMethod.Add(1)
		}
		http.Redirect(w, r, target.URL, http.StatusSeeOther)
	}))
	defer source.Close()
	f := buildFetcher()
	manager := &FetcherManager{}
	if err := f.Resolve(&base.Request{URL: source.URL, Extra: &model.ReqExtra{Method: "POST", Body: "original-form"}}, &base.Options{Path: t.TempDir(), Name: "file.bin"}); err != nil {
		t.Fatal(err)
	}
	saved, err := manager.Store(f)
	if err != nil {
		t.Fatal(err)
	}
	dataJSON, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	metaJSON, err := json.Marshal(f.Meta())
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	beforeSource, beforeTarget := sourceHits.Load(), targetHits.Load()
	var meta fetchModel.FetcherMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		t.Fatal(err)
	}
	data, restore := manager.Restore()
	if err := json.Unmarshal(dataJSON, data); err != nil {
		t.Fatal(err)
	}
	restored := restore(&meta, data).(*Fetcher)
	restored.Setup(f.ctl)
	defer restored.Close()
	err = restored.Start()
	if err == nil {
		done := make(chan error, 1)
		go func() { done <- restored.Wait() }()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
	if !errors.Is(err, transfer.ErrScopedContext) {
		_ = restored.Close()
		t.Fatalf("unsafe restored request was not stopped: err=%v source sends=%d target sends=%d", err, sourceHits.Load()-beforeSource, targetHits.Load()-beforeTarget)
	}
	if sourceHits.Load() != beforeSource || targetHits.Load() != beforeTarget || badMethod.Load() != 0 {
		t.Fatal("restore sent an unproved method/body")
	}
}

func TestAuthExpiredPostRedirectDoesNotReplayOriginal(t *testing.T) {
	var sends atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sends.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer source.Close()
	f := buildFetcher()
	defer f.Close()
	f.meta.Req = &base.Request{URL: source.URL, Extra: &model.ReqExtra{Method: "POST", Body: "original-form"}}
	f.meta.Res = &base.Resource{}
	_, err := f.tryFallbackToOriginalURL(context.Background(), f.buildClient(), 0, 0)
	if !errors.Is(err, transfer.ErrScopedContext) || sends.Load() != 0 {
		t.Fatalf("fallback replayed unproved POST: sends=%d err=%v", sends.Load(), err)
	}
}

func TestAuthRestoredInitialResolvePermitIsOneShot(t *testing.T) {
	var sends atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sends.Add(1); _, _ = w.Write([]byte("initial")) }))
	defer source.Close()
	manager := &FetcherManager{}
	data, restore := manager.Restore()
	req := &base.Request{URL: source.URL, Extra: &model.ReqExtra{Method: "POST", Body: "first"}}
	opts := &base.Options{Path: t.TempDir(), Name: "first.bin"}
	f := restore(&fetchModel.FetcherMeta{Req: req, Opts: opts}, data).(*Fetcher)
	f.Setup(buildFetcher().ctl)
	defer f.Close()
	f.AuthorizeInitialRequest()
	if err := f.Resolve(req, opts); err != nil {
		t.Fatal(err)
	}
	if err := f.Resolve(req, opts); !errors.Is(err, transfer.ErrScopedContext) {
		t.Fatalf("first-I/O permit was replayable: %v", err)
	}
	if sends.Load() != 1 {
		t.Fatalf("source sent %d times", sends.Load())
	}
}
