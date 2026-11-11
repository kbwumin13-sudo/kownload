package netdisk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/controller"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

type targetContextProvider struct {
	fixtureProvider
	info    model.ProviderInfo
	headers map[string]string
	cookies []model.Cookie
}

func (p *targetContextProvider) Info() model.ProviderInfo { return p.info }
func (p *targetContextProvider) Resolve(_ context.Context, _ *cloud.APIClient, _ *cloud.Session, _ model.Source, file model.File) (*cloud.Download, error) {
	file.Size = 3
	return &cloud.Download{URL: p.url, File: file, Header: p.headers, Cookies: p.cookies}, nil
}
func TestFreshProviderTargetContextPreservesNineAdapters(t *testing.T) {
	for _, builtin := range cloud.BuiltinProviders() {
		info := builtin.Info()
		t.Run(info.ID, func(t *testing.T) {
			seen := make(chan http.Header, 32)
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Clone()
				w.Header().Set("Content-Length", "3")
				_, _ = w.Write([]byte("abc"))
			}))
			defer target.Close()
			old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "old-origin", Value: "A", Path: "/"})
				w.Header().Set("Content-Length", "3")
				_, _ = w.Write([]byte("abc"))
			}))
			defer old.Close()
			p := &targetContextProvider{fixtureProvider: fixtureProvider{url: old.URL, size: 3}, info: info, headers: map[string]string{"Authorization": "Bearer A", "X-Device": "A", "Cookie": "header-auth=A"}}
			service := cloud.NewService(p)
			cap, e := service.Capture("", "", model.Capture{Provider: info.ID, SourcePage: "https://" + info.Hosts[0] + "/s/abc", InstanceID: "test-browser", ContextID: "test-tab", AccountID: "test-account"})
			if e != nil {
				t.Fatal(e)
			}
			f := (&FetcherManager{Service: service}).Build().(*Fetcher)
			f.Setup(controller.NewController())
			defer f.Close()
			req := &base.Request{URL: cap.Source.URL, Protocol: "netdisk", Extra: &model.ReqExtra{Provider: info.ID, Source: cap.Source, SessionID: cap.SessionID, AccountID: "test-account", File: model.File{ID: "file", Name: "cloud.bin", Size: 3}}}
			opts := &base.Options{Path: t.TempDir()}
			if e = f.Resolve(req, opts); e != nil {
				t.Fatal(e)
			}
			p.url = target.URL + "/signed?token=synthetic-B"
			p.headers = map[string]string{"Authorization": "Bearer B", "X-Device": "B", "Cookie": "header-auth=B", "User-Agent": "provider-UA", "Referer": "https://public.test/share?proof=B"}
			p.cookies = []model.Cookie{{Name: "jar-auth", Value: "B", Domain: "127.0.0.1", Path: "/"}}
			if e = f.Resolve(req, opts); e != nil {
				t.Fatal(e)
			}
			if e = f.Start(); e != nil {
				t.Fatal(e)
			}
			if e = f.Wait(); e != nil {
				t.Fatal(e)
			}
			body, e := os.ReadFile(f.Meta().SingleFilepath())
			if e != nil || string(body) != "abc" {
				t.Fatal("provider transfer changed", e)
			}
			count := 0
			for len(seen) > 0 {
				h := <-seen
				count++
				if h.Get("Authorization") != "Bearer B" || h.Get("X-Device") != "B" || h.Get("User-Agent") != "provider-UA" || h.Get("Referer") != "https://public.test/share?proof=B" || !strings.Contains(h.Get("Cookie"), "header-auth=B") || !strings.Contains(h.Get("Cookie"), "jar-auth=B") || strings.Contains(h.Get("Cookie"), "=A") {
					t.Fatal("fresh provider target context lost or inherited A")
				}
			}
			if count == 0 {
				t.Fatal("new target never received a request")
			}
			saved, e := (&FetcherManager{}).Store(f)
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal([]any{f.Meta(), saved})
			for _, secret := range []string{"Bearer A", "Bearer B", "synthetic-B", "header-auth", "jar-auth"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("provider context persisted")
				}
			}
		})
	}
}
