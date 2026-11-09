package mailru

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (fn roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestMatchPublicShareOnly(t *testing.T) {
	page := "https://cloud.mail.ru/public/aaaa/bbbb"
	for _, tc := range []struct {
		raw, referer string
		want         bool
	}{
		{page, "", true},
		{"https://cloclo1.datacloudmail.ru/public/get/synthetic/file", page, true},
		{"https://cloclo1.datacloudmail.ru/public/get/synthetic/file", "https://cloud.mail.ru/", false},
		{"https://evil.test/public/get/synthetic/file", page, false},
		{"https://cloud.mail.ru.evil.test/public/aaaa/bbbb", "", false},
		{"https://user:password@cloud.mail.ru/public/aaaa/bbbb", "", false},
		{"http://cloud.mail.ru/public/aaaa/bbbb", "", false},
		{"https://cloud.mail.ru/public/aaaa/bbbb/child", "", false},
	} {
		if _, ok := Match(tc.raw, tc.referer); ok != tc.want {
			t.Errorf("match result %t, want %t", ok, tc.want)
		}
	}
	if !ClassifyError(403) || !ClassifyError(410) || ClassifyError(429) || ClassifyError(503) {
		t.Fatal("expiry and rate limits conflated")
	}
}

func TestRefreshStopsBeforeConsumingDirectResponse(t *testing.T) {
	source, _ := Match("https://cloud.mail.ru/public/aaaa/bbbb", "")
	for _, tc := range []struct {
		name, base, location string
		status               int
		reject               bool
	}{
		{"public redirect", "https://cloclo1.cloud.mail.ru/public/synthetic/g/no", "https://cloclo2.datacloudmail.ru/public/get/synthetic/file", 301, false},
		{"login required", "https://cloclo1.cloud.mail.ru/public/synthetic/g/no", "https://cloud.mail.ru/login", 302, true},
		{"foreign CDN", "https://cloclo1.cloud.mail.ru/public/synthetic/g/no", "https://evil.test/public/get/synthetic/file", 301, true},
		{"insecure URL", "https://cloclo1.cloud.mail.ru/public/synthetic/g/no", "http://cloclo2.datacloudmail.ru/public/get/synthetic/file", 301, true},
		{"foreign dispatcher result", "https://evil.test/public/synthetic", "", 301, true},
		{"authentication response", "https://cloclo1.cloud.mail.ru/public/synthetic/g/no", "", 403, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.UserAgent() != "DownloadBrowser/1.0" {
					t.Fatal("public resolver changed the download User-Agent")
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
					t.Fatal("public source received credentials")
				}
				if r.URL.Host == "cloud.mail.ru" {
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"body":{"weblink_get":[{"url":%q}]}}`, tc.base))), Request: r}, nil
				}
				if r.URL.Host != "cloclo1.cloud.mail.ru" || !strings.HasSuffix(r.URL.Path, "/aaaa/bbbb") {
					t.Fatal("unexpected public resolver request")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": []string{tc.location}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})}
			url, err := source.Refresh(context.Background(), client, "DownloadBrowser/1.0")
			if (err != nil) != tc.reject {
				t.Fatalf("refresh error=%v", err)
			}
			if !tc.reject && (!Direct(url) || requests != 2) {
				t.Fatal("direct link was not returned without fetching its body")
			}
		})
	}
}
