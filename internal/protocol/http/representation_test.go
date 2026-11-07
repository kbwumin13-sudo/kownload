package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func representationPrefix(t *testing.T, raw string, identity *representation, payload []byte) *Fetcher {
	t.Helper()
	opts := &base.Options{Path: t.TempDir(), Name: "synthetic.bin", Extra: &fhttp.OptsExtra{Connections: 1}}
	meta := &fetcher.FetcherMeta{Req: &base.Request{URL: raw, Extra: &fhttp.ReqExtra{}}, Opts: opts,
		Res: &base.Resource{Range: true, Size: int64(len(payload)), Files: []*base.FileInfo{{Name: "synthetic.bin", Size: int64(len(payload))}}}}
	if err := os.WriteFile(meta.SingleFilepath(), payload[:8], 0600); err != nil {
		t.Fatal(err)
	}
	data := &fetcherData{Representation: identity, Connections: []*connection{{ID: 0, Role: rolePrimary,
		Chunk: &chunk{Begin: 0, End: int64(len(payload)) - 1, Downloaded: 8}, Downloaded: 8}}}
	encoded, _ := json.Marshal(data)
	value, restore := (&FetcherManager{}).Restore()
	if err := json.Unmarshal(encoded, value); err != nil {
		t.Fatal(err)
	}
	f := restore(meta, value).(*Fetcher)
	f.Setup(buildFetcher().ctl)
	t.Cleanup(func() { f.Close() })
	return f
}

func TestRepresentationResumeAndPatch(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 32)
	modified := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC).Format(http.TimeFormat)
	for _, tc := range []struct {
		name, etag, modified, responseTag, responseModified string
		full, reject                                        bool
	}{
		{name: "strong ETag", etag: `"v1"`, responseTag: `"v1"`},
		{name: "Last-Modified", modified: modified, responseModified: modified},
		{name: "changed ETag legal full response", etag: `"v1"`, responseTag: `"v2"`, full: true, reject: true},
		{name: "changed ETag with lying 206", etag: `"v1"`, responseTag: `"v2"`, reject: true},
		{name: "missing ETag", etag: `"v1"`, reject: true},
		{name: "changed Last-Modified", modified: modified, responseModified: time.Now().UTC().Format(http.TimeFormat), reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := tc.etag
				if want == "" {
					want = tc.modified
				}
				if r.Header.Get("If-Range") != want {
					t.Error("saved If-Range missing")
				}
				w.Header().Set("ETag", tc.responseTag)
				w.Header().Set("Last-Modified", tc.responseModified)
				if tc.full {
					w.Write(payload)
					return
				}
				w.Header().Set("Content-Range", "bytes 8-31/32")
				w.Header().Set("Content-Length", "24")
				w.WriteHeader(http.StatusPartialContent)
				w.Write(payload[8:])
			}))
			defer srv.Close()
			f := representationPrefix(t, srv.URL+"/file?sig=old", &representation{ETag: tc.etag, LastModified: tc.modified, Size: 32}, payload)
			if err := f.Patch(&base.Request{URL: srv.URL + "/file?sig=new"}, nil); err != nil {
				t.Fatal(err)
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			err := waitDone(t, f, 3*time.Second)
			got, readErr := os.ReadFile(f.Meta().SingleFilepath())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.reject {
				if err == nil || !bytes.Equal(got, payload[:8]) {
					t.Fatalf("changed response wrote old prefix: %v bytes=%d", err, len(got))
				}
			} else if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("stable response: %v bytes=%d", err, len(got))
			}
		})
	}
}

// A checkpoint without a saved representation whose saved bytes differ from
// the server is refused before anything is written.
func TestRepresentationLegacyCheckpointPreserved(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveUnvalidatedRange(t, w, r, bytes.Repeat([]byte("B"), len(payload)), 0, false)
	}))
	defer srv.Close()
	f := representationPrefix(t, srv.URL+"/synthetic.bin", nil, payload)
	err := f.Start()
	if err == nil {
		err = waitDone(t, f, 5*time.Second)
	}
	if !errors.Is(err, ErrUnsafeResume) {
		t.Fatalf("legacy checkpoint: %v", err)
	}
	if !strings.Contains(f.Stats().(*fhttp.Stats).LastError, "preserved") {
		t.Fatal("identity refusal missing from task diagnostics")
	}

	got, _ := os.ReadFile(f.Meta().SingleFilepath())
	if !bytes.Equal(got, payload[:8]) {
		t.Fatal("unverifiable legacy checkpoint was changed")
	}
}

func TestRepresentationValidatorCannotCrossResource(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 32)
	f := representationPrefix(t, "https://same.test/file-one", &representation{ETag: `"v1"`, Size: 32}, payload)
	if err := f.Patch(&base.Request{URL: "https://same.test/file-two"}, nil); !errors.Is(err, ErrUnsafeResume) {
		t.Fatalf("opaque equal validators crossed a resource: %v", err)
	}
	got, _ := os.ReadFile(f.Meta().SingleFilepath())
	if !bytes.Equal(got, payload[:8]) {
		t.Fatal("resource-scope rejection changed the file")
	}
}

func TestRepresentationStoredAndWeakValidatorIgnored(t *testing.T) {
	for _, etag := range []string{`"v1"`, `W/"v1"`} {
		t.Run(etag, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", etag)
				w.Header().Set("Content-Length", "4")
				w.Write([]byte("data"))
			}))
			defer srv.Close()
			f := buildFetcher()
			defer f.Close()
			if err := f.Resolve(&base.Request{URL: srv.URL}, &base.Options{Path: t.TempDir(), Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
				t.Fatal(err)
			}
			data, err := (&FetcherManager{}).Store(f)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(data)
			value, restore := (&FetcherManager{}).Restore()
			if err := json.Unmarshal(encoded, value); err != nil {
				t.Fatal(err)
			}
			other := restore(f.Meta(), value).(*Fetcher)
			got := other.representationSnapshot()
			if got.Size != 4 || (got.ETag != "") != (etag == `"v1"`) {
				t.Fatalf("stored identity: %+v", got)
			}
		})
	}
}

func TestRepresentationPatchRequiresPausedRequest(t *testing.T) {
	f := buildFetcher()
	f.meta.Req = &base.Request{URL: "https://old.test/file"}
	f.setState(stateSteady)
	if err := f.Patch(&base.Request{URL: "https://new.test/file"}, nil); err == nil {
		t.Fatal("active request patch allowed")
	}
	if f.meta.Req.URL != "https://old.test/file" {
		t.Fatal("active URL was changed")
	}
}

func TestRepresentationHashRequiresCurrentWrapper(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 32)
	digest := "md5:" + fmt.Sprintf("%032x", 1)
	f := representationPrefix(t, "http://127.0.0.1:1/synthetic.bin", &representation{Hash: digest, Size: 32}, payload)
	// Without the wrapper's digest only a comparison with the server confirms it.
	if overlap, err := f.checkResumeIdentity(); err != nil || !overlap {
		t.Fatalf("persisted hash was trusted without a verifier: overlap=%t err=%v", overlap, err)
	}
	if err := f.SetResumeHash(digest); err != nil {
		t.Fatal(err)
	}
	if overlap, err := f.checkResumeIdentity(); err != nil || overlap {
		t.Fatalf("trusted digest: overlap=%t err=%v", overlap, err)
	}
	if err := f.SetResumeHash("md5:" + fmt.Sprintf("%032x", 2)); !errors.Is(err, ErrUnsafeResume) {
		t.Fatalf("changed expected hash: %v", err)
	}
}
