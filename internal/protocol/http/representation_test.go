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
	"sync/atomic"
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

// A renewed signed link of the same file only changes the query. With a saved
// validator, scheme, host and path decide whether the link may continue the
// task; build 25 accepted these renewals.
func TestRepresentationPatchRenewedSignature(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 32)
	for _, tc := range []struct {
		old, renewed string
		accepted     bool
	}{
		{"https://cdn.example/file.bin?token=old-sign&e=1700000000", "https://cdn.example/file.bin?token=new-sign&e=1700003600", true},
		{"https://acct.blob.core.windows.net/c/file.bin?sv=2022&st=2026-11-07&se=2026-11-08&sp=r&sig=a", "https://acct.blob.core.windows.net/c/file.bin?sv=2022&st=2026-11-08&se=2026-11-09&sp=r&sig=b", true},
		{"https://cdn.example/file.bin?q-sign-time=1&q-key-time=1", "https://CDN.example/file.bin?q-sign-time=2&q-key-time=2", true},
		{"https://cdn.example/file.bin?token=a", "https://other.example/file.bin?token=a", false},
		{"https://cdn.example/file.bin?token=a", "http://cdn.example/file.bin?token=a", false},
		{"https://cdn.example/a/file.bin?token=a", "https://cdn.example/b/file.bin?token=a", false},
	} {
		f := representationPrefix(t, tc.old, &representation{ETag: `"v1"`, Size: int64(len(payload))}, payload)
		f.identity.ResourceKey = requestResourceKey(f.meta.Req) // as Resolve saved it
		err := f.Patch(&base.Request{URL: tc.renewed}, nil)
		if tc.accepted && err != nil || !tc.accepted && !errors.Is(err, ErrUnsafeResume) {
			t.Errorf("patch %s -> %s: %v", tc.old, tc.renewed, err)
		}
	}
}

// After such a renewal the resume sends the saved validator as If-Range and
// checks it on the response: the same version continues, another one is
// refused before a byte is written.
func TestRepresentationRenewedSignatureResumes(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 32)
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint("changed=", changed), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("token") != "new" || r.Header.Get("If-Range") != `"v1"` {
					t.Errorf("resume request: %s If-Range=%q", r.URL, r.Header.Get("If-Range"))
				}
				etag := `"v1"`
				if changed {
					etag = `"v2"`
				}
				w.Header().Set("ETag", etag)
				w.Header().Set("Content-Range", "bytes 8-31/32")
				w.Header().Set("Content-Length", "24")
				w.WriteHeader(http.StatusPartialContent)
				w.Write(payload[8:])
			}))
			defer srv.Close()
			f := representationPrefix(t, srv.URL+"/file.bin?e=1700000000&token=old", &representation{ETag: `"v1"`, Size: 32}, payload)
			f.identity.ResourceKey = requestResourceKey(f.meta.Req)
			if err := f.Patch(&base.Request{URL: srv.URL + "/file.bin?e=1700003600&token=new"}, nil); err != nil {
				t.Fatal(err)
			}
			err := f.Start()
			if err == nil {
				err = waitDone(t, f, 3*time.Second)
			}
			got, _ := os.ReadFile(f.Meta().SingleFilepath())
			if changed {
				// A stream reports the refusal through the completion error.
				if err == nil || !strings.Contains(err.Error(), "ETag changed") || !bytes.Equal(got, payload[:8]) {
					t.Fatalf("changed version after renewal: %v bytes=%d", err, len(got))
				}
			} else if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("renewed link: %v bytes=%d", err, len(got))
			}
		})
	}
}

// storeAndRestore saves f as the Downloader does and restores it, as after a
// restart.
func storeAndRestore(t *testing.T, f *Fetcher) *Fetcher {
	t.Helper()
	fm := &FetcherManager{}
	data, err := fm.Store(f)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(data)
	metaJSON, _ := json.Marshal(f.Meta())
	var meta fetcher.FetcherMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		t.Fatal(err)
	}
	value, restore := fm.Restore()
	if err := json.Unmarshal(encoded, value); err != nil {
		t.Fatal(err)
	}
	ctl := f.ctl
	f.Close()
	restored := restore(&meta, value).(*Fetcher)
	restored.Setup(ctl)
	t.Cleanup(func() { restored.Close() })
	return restored
}

// One download path serves two files chosen by a query parameter, with the
// same ETag and length (split archives a script made in the same second, for
// one). Patching the task of id=1 to id=2 keeps scheme, host and path, so the
// link is accepted, but a parameter other than a signature changed: the
// resume compares saved bytes too and refuses the other file, keeping the
// saved bytes. That includes a short signature name used without its
// provider's markers (?e=2 for episode 2). Renewing only signatures, as
// Qiniu, Azure SAS, Tencent Cloud and CloudFront links do, still resumes on
// the validator alone; a parameter the list does not know costs one
// comparison, not a refusal.
func TestPatchedQueryComparesSavedBytes(t *testing.T) {
	const size = 4 << 20
	files := map[string][]byte{"1": bytes.Repeat([]byte("A"), size), "2": bytes.Repeat([]byte("B"), size)}
	for _, tc := range []struct {
		name, selector, from, to string
		checks                   int32
		refused                  bool
	}{
		{"signature renewed", "id", "?id=1&token=a&e=1&Signature=x", "?id=1&token=b&e=2&Signature=y", 0, false},
		{"Qiniu", "id", "?id=1&e=1700000000&token=AK:sign1", "?id=1&e=1700003600&token=AK:sign2", 0, false},
		{"Azure SAS", "id", "?id=1&sv=2022-11-02&st=2026-11-07T00%3A00Z&se=2026-11-08T00%3A00Z&sr=b&sp=r&sig=a1",
			"?id=1&sv=2022-11-02&st=2026-11-08T00%3A00Z&se=2026-11-09T00%3A00Z&sr=b&sp=r&sig=b2", 0, false},
		{"Tencent Cloud", "id", "?id=1&q-sign-algorithm=sha1&q-ak=AK&q-sign-time=1700000000%3B1700003600&q-key-time=1700000000%3B1700003600&q-header-list=&q-url-param-list=&q-signature=a1",
			"?id=1&q-sign-algorithm=sha1&q-ak=AK&q-sign-time=1700003600%3B1700007200&q-key-time=1700003600%3B1700007200&q-header-list=&q-url-param-list=&q-signature=b2", 0, false},
		{"CloudFront", "id", "?id=1&Policy=p1&Signature=s1&Key-Pair-Id=K1", "?id=1&Policy=p2&Signature=s2&Key-Pair-Id=K1", 0, false},
		{"unknown parameter", "id", "?id=1&mirror=a", "?id=1&mirror=b", 1, false},
		{"other file", "id", "?id=1", "?id=2", 1, true},
		{"short name chooses the file", "e", "?e=1", "?e=2", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var checks atomic.Int32
			var withoutValidator atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data := files[r.URL.Query().Get(tc.selector)]
				if data == nil {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if isOverlapCheck(r) {
					checks.Add(1)
				} else if r.Header.Get("Range") != "" && r.Header.Get("If-Range") != `"same"` {
					withoutValidator.Store(true)
				}
				w.Header().Set("ETag", `"same"`)
				serveUnvalidatedRange(t, w, r, data, 0, true)
			}))
			defer srv.Close()
			f := buildFetcher()
			defer f.Close()
			if err := f.Resolve(&base.Request{URL: srv.URL + "/download" + tc.from}, &base.Options{Path: t.TempDir(), Name: "part.bin", Extra: &fhttp.OptsExtra{Connections: 1}}); err != nil {
				t.Fatal(err)
			}
			if !f.hasResumeValidator() {
				t.Fatal("fixture has no validator")
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for f.Progress().TotalDownloaded() < 256<<10 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if err := f.Pause(); err != nil {
				t.Fatal(err)
			}
			if paused := f.Progress().TotalDownloaded(); paused <= 0 || paused >= size {
				t.Fatalf("fixture did not pause mid-transfer: %d", paused)
			}
			if err := f.Patch(&base.Request{URL: srv.URL + "/download" + tc.to}, nil); err != nil {
				t.Fatal(err)
			}
			f = storeAndRestore(t, f)
			before, _ := os.ReadFile(f.Meta().SingleFilepath())
			err := f.Start()
			if err == nil {
				err = waitDone(t, f, 20*time.Second)
			}
			after, _ := os.ReadFile(f.Meta().SingleFilepath())
			if tc.refused {
				if !errors.Is(err, ErrUnsafeResume) || !bytes.Equal(before, after) {
					t.Fatalf("other file behind the same path: %v unchanged=%t", err, bytes.Equal(before, after))
				}
			} else if err != nil || !bytes.Equal(after, files["1"]) {
				t.Fatalf("same file: %v equal=%t", err, bytes.Equal(after, files["1"]))
			}
			if n := checks.Load(); n != tc.checks {
				t.Fatalf("saved-overlap requests=%d, want %d", n, tc.checks)
			}
			if withoutValidator.Load() {
				t.Fatal("a resumed range was sent without the saved validator")
			}
		})
	}
}

// Renewing only signature or expiry parameters keeps the query key; changing
// anything else, or adding a parameter, gives another one.
func TestQueryKeyIgnoresSignatureParameters(t *testing.T) {
	key := func(raw string) string { return requestQueryKey(&base.Request{URL: raw}) }
	for _, tc := range []struct{ name, old, renewed string }{
		{"Qiniu", "https://x.example/f.bin?e=1&token=ak:a", "https://x.example/f.bin?e=2&token=ak:b"},
		{"Azure SAS", "https://a.blob.core.windows.net/c/f.bin?sv=1&st=1&se=1&sp=r&sr=b&sig=a",
			"https://a.blob.core.windows.net/c/f.bin?sv=2&st=2&se=2&sp=rw&sr=c&sig=b"},
		{"Tencent Cloud", "https://b.cos.example/f.bin?q-sign-algorithm=sha1&q-ak=a&q-sign-time=1&q-key-time=1&q-signature=a",
			"https://b.cos.example/f.bin?q-sign-algorithm=sha1&q-ak=a&q-sign-time=2&q-key-time=2&q-signature=b"},
		{"CloudFront", "https://d.cloudfront.net/f.bin?Expires=1&Signature=a&Key-Pair-Id=K1",
			"https://d.cloudfront.net/f.bin?Policy=p&Signature=b&Key-Pair-Id=K2"},
		{"Alibaba Cloud OSS", "https://b.oss.example/f.bin?OSSAccessKeyId=a&Expires=1&Signature=a",
			"https://b.oss.example/f.bin?OSSAccessKeyId=b&Expires=2&Signature=b&security-token=t"},
		{"Alibaba Cloud CDN", "https://cdn.example/f.bin?auth_key=1-0-0-a", "https://cdn.example/f.bin?auth_key=2-0-0-b"},
		{"Akamai", "https://ak.example/f.bin?hdnts=exp=1~hmac=a", "https://ak.example/f.bin?__token__=exp=2~hmac=b"},
		{"Amazon S3", "https://s3.example/f.bin?X-Amz-Expires=1&X-Amz-Signature=a", "https://s3.example/f.bin?X-Amz-Expires=2&X-Amz-Signature=b"},
		{"parameter order", "https://x.example/f.bin?id=7&Signature=a", "https://x.example/f.bin?Signature=b&id=7"},
	} {
		if key(tc.old) != key(tc.renewed) {
			t.Errorf("%s: signature renewal changed the query key", tc.name)
		}
	}
	// Short names choose the file unless their provider's markers are there too.
	for _, pair := range [][2]string{
		{"https://x.example/download?id=1", "https://x.example/download?id=2"},
		{"https://x.example/download?file=a.bin&token=a", "https://x.example/download?file=b.bin&token=a"},
		{"https://x.example/f.bin?token=a", "https://x.example/f.bin?token=a&part=2"},
		{"https://x.example/show?e=1", "https://x.example/show?e=2"},
		{"https://x.example/show?e=1&sig=a", "https://x.example/show?e=2&sig=a"},
		{"https://x.example/f.bin?token=a", "https://x.example/f.bin?token=b"},
		{"https://x.example/f.bin?st=1&sp=a", "https://x.example/f.bin?st=2&sp=b"},
		{"https://x.example/f.bin?q-ak=a&q-sign-time=1", "https://x.example/f.bin?q-ak=b&q-sign-time=2"},
		{"https://x.example/f.bin?Policy=a&Signature=x", "https://x.example/f.bin?Policy=b&Signature=y"},
	} {
		if key(pair[0]) == key(pair[1]) {
			t.Errorf("query key ignored a resource parameter: %s -> %s", pair[0], pair[1])
		}
	}
}
