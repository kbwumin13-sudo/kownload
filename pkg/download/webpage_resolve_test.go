package download

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func TestWebpageResolveUsesExistingGET(t *testing.T) {
	for _, tt := range []struct {
		name, method, mime, disposition string
		page                            bool
	}{
		{"html", "GET", "text/html; charset=UTF-8", "", true},
		{"xhtml", "GET", "application/xhtml+xml", "inline", true},
		{"attachment", "GET", "text/html", "Attachment; filename=report.html", false},
		{"video", "GET", "video/mp4", "", false},
		{"post", "POST", "text/html", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", tt.mime)
				w.Header().Set("Content-Disposition", tt.disposition)
				w.Header().Set("Accept-Ranges", "bytes")
				_, _ = w.Write([]byte("example content"))
			}))
			defer s.Close()
			d := NewDownloader(&DownloaderConfig{StorageDir: t.TempDir()})
			if err := d.Setup(); err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			rr, err := d.Resolve(&base.Request{Protocol: "http", URL: s.URL, Extra: &fhttp.ReqExtra{Method: tt.method}}, &base.Options{Path: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if rr.HTTPInlineHTML != tt.page {
				t.Errorf("httpInlineHtml = %v, want %v", rr.HTTPInlineHTML, tt.page)
			}
			if err := d.DiscardHTTPResolve(rr.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.DiscardHTTPResolve(rr.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Create(rr.ID); err == nil {
				t.Fatal("discarded resolution was reused")
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("served %d requests, want 1", got)
			}
		})
	}
}
