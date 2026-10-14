package download

import (
	"testing"

	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestMediaExplicitRoutingPreservesHTTP(t *testing.T) {
	d := NewDownloader(&DownloaderConfig{})
	for _, tc := range []struct{ url, protocol, want string }{
		{"https://example.test/play?sign=abc", "media", "media"},
		{"https://example.test/video.m3u8?sign=abc", "", "http"},
		{"https://example.test/image.jpg", "", "http"},
	} {
		fm, err := d.parseRequestFm(&base.Request{URL: tc.url, Protocol: tc.protocol})
		if err != nil || fm.Name() != tc.want {
			t.Fatalf("route %q/%q: %v, %v", tc.url, tc.protocol, fm, err)
		}
	}
	if _, err := d.parseRequestFm(&base.Request{URL: "https://example.test", Protocol: "unknown"}); err == nil {
		t.Fatal("unknown explicit protocol must fail")
	}
	if _, err := d.parseRequestFm(nil); err == nil {
		t.Fatal("missing request must fail")
	}
}
