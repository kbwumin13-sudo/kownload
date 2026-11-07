package netdisk

import (
	"crypto/tls"
	"crypto/x509"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func digestResponse(t *testing.T) (*http.Request, *http.Response) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://d.pcs.baidu.com/file/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req, &http.Response{StatusCode: 200, ContentLength: 3, Header: http.Header{"Content-Md5": {"900150983cd24fb0d6963f7d28e17f72"}}, Request: req,
		TLS: &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}}
}

func TestBaiduResponseDigestPolicy(t *testing.T) {
	for _, raw := range []string{"900150983cd24fb0d6963f7d28e17f72", "900150983CD24FB0D6963F7D28E17F72", "kAFQmDzST7DWlj99KOF/cg=="} {
		t.Run(raw, func(t *testing.T) {
			req, res := digestResponse(t)
			res.Header.Set("Content-MD5", raw)
			d := &baiduResponseDigest{enabled: true, total: 3}
			if err := d.policy(req, res); err != nil || d.value() != "900150983cd24fb0d6963f7d28e17f72" {
				t.Fatalf("digest not captured: %q %v", d.value(), err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*http.Request, *http.Response)
	}{
		{"http", func(q *http.Request, r *http.Response) { q.URL.Scheme = "http" }},
		{"untrusted-host", func(q *http.Request, r *http.Response) { q.URL.Host = "baidu.com.example.invalid" }},
		{"redirect-untrusted", func(q *http.Request, r *http.Response) {
			other, _ := http.NewRequest("GET", "https://example.invalid/file", nil)
			r.Request = other
		}},
		{"unverified-tls", func(q *http.Request, r *http.Response) { r.TLS.VerifiedChains = nil }},
		{"missing-tls", func(q *http.Request, r *http.Response) { r.TLS = nil }},
		{"head", func(q *http.Request, r *http.Response) { q.Method = "HEAD" }},
		{"partial", func(q *http.Request, r *http.Response) {
			r.StatusCode = 206
			r.Header.Set("Content-Range", "bytes 0-2/3")
		}},
		{"range", func(q *http.Request, r *http.Response) {
			q.Header.Set("Range", "bytes=0-2")
			r.StatusCode = 206
			r.Header.Set("Content-Range", "bytes 0-2/3")
		}},
		{"wrong-length", func(q *http.Request, r *http.Response) { r.ContentLength = 4 }},
		{"unknown-length", func(q *http.Request, r *http.Response) { r.ContentLength = -1 }},
		{"encoded", func(q *http.Request, r *http.Response) { r.Header.Set("Content-Encoding", "gzip") }},
		{"decompressed", func(q *http.Request, r *http.Response) { r.Uncompressed = true }},
		{"etag-only", func(q *http.Request, r *http.Response) {
			r.Header.Del("Content-MD5")
			r.Header.Set("ETag", "900150983cd24fb0d6963f7d28e17f72")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, res := digestResponse(t)
			tc.change(req, res)
			d := &baiduResponseDigest{enabled: true, total: 3}
			if err := d.policy(req, res); err != nil {
				t.Fatal(err)
			}
			if d.value() != "" {
				t.Fatal("untrusted/partial digest captured")
			}
		})
	}
	t.Run("other-provider-or-skip-verify", func(t *testing.T) {
		req, res := digestResponse(t)
		d := &baiduResponseDigest{total: 3}
		if err := d.policy(req, res); err != nil || d.value() != "" {
			t.Fatal("disabled policy accepted digest", err)
		}
	})
	for _, raw := range []string{"not-a-digest", "8e23f7635tb64136eddb0719602bc477", "YWJj"} {
		t.Run("malformed-"+raw, func(t *testing.T) {
			req, res := digestResponse(t)
			res.Header.Set("Content-MD5", raw)
			d := &baiduResponseDigest{enabled: true, total: 3}
			if err := d.policy(req, res); err == nil {
				t.Fatal("malformed digest accepted")
			}
		})
	}
	t.Run("conflicting-responses", func(t *testing.T) {
		req, res := digestResponse(t)
		d := &baiduResponseDigest{enabled: true, total: 3}
		if err := d.policy(req, res); err != nil {
			t.Fatal(err)
		}
		res.Header.Set("Content-MD5", "d41d8cd98f00b204e9800998ecf8427e")
		if err := d.policy(req, res); err == nil {
			t.Fatal("changed digest accepted")
		}
	})
	t.Run("duplicate-header", func(t *testing.T) {
		req, res := digestResponse(t)
		res.Header.Add("Content-MD5", "d41d8cd98f00b204e9800998ecf8427e")
		d := &baiduResponseDigest{enabled: true, total: 3}
		if err := d.policy(req, res); err == nil {
			t.Fatal("ambiguous digest accepted")
		}
	})
}

func TestBaiduResponseMD5VerifiesDespiteStaleListing(t *testing.T) {
	dir := t.TempDir()
	f := &Fetcher{meta: &fetcher.FetcherMeta{Opts: &base.Options{Path: dir}, Res: &base.Resource{Hash: "md5:900150983cd24fb0d6963f7d28e17f72", Files: []*base.FileInfo{{Name: "photo.jpg"}}}}}
	if err := os.WriteFile(filepath.Join(dir, "photo.jpg"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	e := &model.ReqExtra{Provider: "baidu", File: model.File{Size: 3, Hash: "8e23f7635tb64136eddb0719602bc477"}}
	if err := f.verifyFile(e); err != nil {
		t.Fatalf("valid response digest rejected due to stale listing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "photo.jpg"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.verifyFile(e); err == nil {
		t.Fatal("corruption passed response digest verification")
	}
}
