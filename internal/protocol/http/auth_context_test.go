package http

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func TestAuthCrossOriginPatchNeverInheritsOldCredentials(t *testing.T) {
	f := buildFetcher()
	defer f.Close()
	f.meta.Req = &base.Request{URL: "https://a.example/file", Extra: &model.ReqExtra{Header: map[string]string{"Cookie": "A-cookie", "Authorization": "Bearer A", "X-Private": "A-private", "User-Agent": "browser-UA"}}}
	if err := f.Patch(&base.Request{URL: "https://b.example/file"}, nil); err != nil {
		t.Fatal(err)
	}
	request, err := f.buildRequest(context.Background(), f.meta.Req)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Cookie", "Authorization", "X-Private"} {
		if request.Header.Get(name) != "" {
			t.Fatalf("old %s crossed origin", name)
		}
	}
	if request.UserAgent() != "browser-UA" {
		t.Fatal("UA lost")
	}
}
func TestAuthPatchInvalidCandidateKeepsOldURLAndHeaders(t *testing.T) {
	f := buildFetcher()
	defer f.Close()
	f.meta.Req = &base.Request{URL: "https://a.example/file", Extra: &model.ReqExtra{Header: map[string]string{"Cookie": "A-cookie"}}}
	err := f.Patch(&base.Request{URL: "https://b.example/file", Extra: "not-an-object"}, nil)
	if err == nil || f.meta.Req.URL != "https://a.example/file" {
		t.Fatal("invalid candidate changed old URL", err)
	}
	if f.meta.Req.Extra.(*model.ReqExtra).Header["Cookie"] != "A-cookie" {
		t.Fatal("invalid candidate changed old headers")
	}
}
func TestAuthRedirectSeparatesACookieAndBOwnCookie(t *testing.T) {
	var arrivals []http.Header
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrivals = append(arrivals, r.Header.Clone())
		http.SetCookie(w, &http.Cookie{Name: "B", Value: "own-B", Path: "/"})
		_, _ = w.Write([]byte("B"))
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "A", Value: "old-A", Path: "/"})
		http.Redirect(w, r, b.URL+"/file", 302)
	}))
	defer a.Close()
	f := buildFetcher()
	defer f.Close()
	f.meta.Req = &base.Request{URL: a.URL + "/file", Extra: &model.ReqExtra{Header: map[string]string{"Authorization": "Bearer A", "X-Unknown-Secret": "A-private"}}}
	client := f.buildClient()
	req, err := f.buildRequest(context.Background(), f.meta.Req)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	req, _ = http.NewRequest("GET", b.URL+"/again", nil)
	response, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if len(arrivals) != 2 {
		t.Fatal("missing target requests")
	}
	for _, h := range arrivals {
		if strings.Contains(h.Get("Cookie"), "old-A") || h.Get("Authorization") != "" || h.Get("X-Unknown-Secret") != "" {
			t.Fatalf("A context reached B: %v", h)
		}
	}
	if !strings.Contains(arrivals[1].Get("Cookie"), "own-B") {
		t.Fatal("B own response cookie was incorrectly blocked")
	}
}

func TestAuthRetainReplaceClearAndHeaderCase(t *testing.T) {
	for _, mode := range []transfer.HeaderUpdateMode{transfer.HeadersRetain, transfer.HeadersReplace, transfer.HeadersClear} {
		t.Run(string(mode), func(t *testing.T) {
			f := buildFetcher()
			defer f.Close()
			f.meta.Req = &base.Request{URL: "https://user:old-basic@a.example/file", Extra: &model.ReqExtra{Header: map[string]string{"Cookie": "old", "X-Private": "old-private", "User-Agent": "kept-UA"}}}
			patch := &base.Request{Extra: &model.ReqExtra{Header: map[string]string{"cookie": "new"}}}
			if mode == transfer.HeadersClear {
				patch.Extra = nil
			}
			if err := f.PatchWithContext(patch, nil, mode, nil); err != nil {
				t.Fatal(err)
			}
			request, err := f.buildRequest(context.Background(), f.meta.Req)
			if err != nil {
				t.Fatal(err)
			}
			if mode == transfer.HeadersClear {
				if request.Header.Get("Cookie") != "" || request.Header.Get("X-Private") != "" || request.URL.User != nil {
					t.Fatal("clear retained credentials")
				}
			} else {
				if request.Header.Get("Cookie") != "new" || len(request.Header.Values("Cookie")) != 1 {
					t.Fatal("case-insensitive update kept old cookie")
				}
			}
			if mode == transfer.HeadersReplace && (request.Header.Get("X-Private") != "" || request.URL.User != nil) {
				t.Fatal("replace retained old fields")
			}
		})
	}
	f := buildFetcher()
	defer f.Close()
	f.meta.Req = &base.Request{URL: "https://a.example/old", Extra: &model.ReqExtra{Header: map[string]string{"Cookie": "old"}}}
	if err := f.Patch(&base.Request{Extra: &model.ReqExtra{Header: map[string]string{"X-A": "one", "x-a": "two"}}}, nil); err == nil {
		t.Fatal("ambiguous duplicate header accepted")
	}
	if f.meta.Req.URL != "https://a.example/old" {
		t.Fatal("bad candidate applied")
	}
}
func TestAuthFreshTargetBasicAndUnknownHeaderAreNotStripped(t *testing.T) {
	var seen http.Header
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Clone(); _, _ = w.Write([]byte("B")) }))
	defer b.Close()
	target, _ := url.Parse(b.URL + "/file")
	target.User = url.UserPassword("user-B", "password-B")
	contextB, err := transfer.NewTargetCredentials(target.String(), map[string]string{"X-Opaque-Credential": "fresh-B", "Cookie": "session=B"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := buildFetcher()
	defer f.Close()
	f.meta.Req = &base.Request{URL: "http://a.example/file", Extra: &model.ReqExtra{Header: map[string]string{"Cookie": "A", "Authorization": "Bearer A", "X-Opaque-Credential": "old-A"}}}
	if err = f.PatchWithContext(&base.Request{URL: target.String()}, nil, transfer.HeadersReplace, contextB); err != nil {
		t.Fatal(err)
	}
	req, err := f.buildRequest(context.Background(), f.meta.Req)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.buildClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("user-B:password-B"))
	if seen.Get("Authorization") != expected || seen.Get("X-Opaque-Credential") != "fresh-B" || seen.Get("Cookie") != "session=B" {
		t.Fatalf("explicit B context lost: %v", seen)
	}
}
func TestAuthUnprovedNewTargetCredentialsRejectWithoutMutation(t *testing.T) {
	for _, patch := range []*base.Request{
		{URL: "https://user-B:password-B@b.example/file"},
		{URL: "https://b.example/file", Extra: &model.ReqExtra{Header: map[string]string{"Authorization": "Bearer B"}}},
	} {
		f := buildFetcher()
		f.meta.Req = &base.Request{URL: "https://a.example/file", Extra: &model.ReqExtra{Header: map[string]string{"Cookie": "A"}}}
		if err := f.Patch(patch, nil); !errors.Is(err, transfer.ErrScopedContext) {
			t.Fatalf("unproved credentials accepted: %v", err)
		}
		if f.meta.Req.URL != "https://a.example/file" {
			t.Fatal("rejected target changed old request")
		}
		_ = f.Close()
	}
}
func TestAuthRedirectStatusAndBodyBoundaries(t *testing.T) {
	for _, status := range []int{302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls int
			b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "" || r.Header.Get("X-Secret") != "" {
					t.Error("old credentials reached redirect")
				}
				_, _ = w.Write([]byte("B"))
			}))
			defer b.Close()
			a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, b.URL, status) }))
			defer a.Close()
			f := buildFetcher()
			defer f.Close()
			f.meta.Req = &base.Request{URL: a.URL, Extra: &model.ReqExtra{Method: "POST", Body: "synthetic-A-body", Header: map[string]string{"Authorization": "Bearer A", "X-Secret": "A"}}}
			req, _ := f.buildRequest(context.Background(), f.meta.Req)
			response, err := f.buildClient().Do(req)
			if status == 307 || status == 308 {
				if err == nil || calls != 0 {
					t.Fatal("foreign POST body replayed")
				}
			} else {
				if err != nil || calls != 1 {
					t.Fatal("safe POST-to-GET redirect blocked", err)
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
		})
	}
}
func TestAuthCachedRedirectUsesFinalGuardAndTargetCookie(t *testing.T) {
	var seen http.Header
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = r.Header.Clone(); _, _ = w.Write([]byte("B")) }))
	defer b.Close()
	f := buildFetcher()
	defer f.Close()
	f.meta.Req = &base.Request{URL: "http://source.example/old", Extra: &model.ReqExtra{Header: map[string]string{"Authorization": "Bearer A", "X-Unnamed": "A", "Cookie": "A=one"}}}
	f.updateRedirectURL(b.URL)
	req, _ := f.buildRequest(context.Background(), f.meta.Req)
	res, err := f.buildClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if seen.Get("Authorization") != "" || seen.Get("X-Unnamed") != "" || strings.Contains(seen.Get("Cookie"), "A=") {
		t.Fatal("cached redirect bypassed final guard")
	}
}

func TestAuthSameOriginURLRetainsBasicUntilClear(t *testing.T) {
	f := buildFetcher()
	defer f.Close()
	f.meta.Req = &base.Request{URL: "https://user:old-password@example.test/file?sig=old", Extra: &model.ReqExtra{Header: map[string]string{"X-Private": "same-origin"}}}
	if err := f.Patch(&base.Request{URL: "https://example.test:443/file?sig=new"}, nil); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(f.meta.Req.URL)
	if u.User == nil || u.User.Username() != "user" {
		t.Fatal("same-origin retain lost Basic context")
	}
	if err := f.PatchWithContext(&base.Request{}, nil, transfer.HeadersClear, nil); err != nil {
		t.Fatal(err)
	}
	u, _ = url.Parse(f.meta.Req.URL)
	if u.User != nil {
		t.Fatal("explicit clear kept Basic context")
	}
}

func TestAuthFinalGuardCoversOriginBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, root, target string
		foreign, downgrade bool
	}{
		{"same origin default port", "https://a.example.test/old", "https://a.example.test:443/new", false, false},
		{"subdomain", "https://a.example.test/old", "https://cdn.a.example.test/new", true, false},
		{"port", "https://a.example.test/old", "https://a.example.test:444/new", true, false},
		{"downgrade", "https://a.example.test/old", "http://a.example.test/new", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", tc.target, nil)
			r.Host = "old-host.example.test"
			r.Header = http.Header{"Authorization": {"Bearer A"}, "Cookie": {"session=A"}, "X-Unnamed": {"A"}, "User-Agent": {"browser-UA"}, "Accept": {"*/*"}, "Referer": {"https://watch.example.test/player?sig=A#fragment"}}
			if e := outboundCredentialGuard(tc.root, nil, newOriginCookieJar())(r); e != nil {
				t.Fatal(e)
			}
			for _, key := range []string{"Authorization", "Cookie", "X-Unnamed"} {
				if (r.Header.Get(key) == "") != tc.foreign {
					t.Fatal("wrong credential boundary")
				}
			}
			if r.UserAgent() != "browser-UA" || r.Header.Get("Accept") != "*/*" {
				t.Fatal("negotiation context lost")
			}
			if tc.downgrade && r.Referer() != "" {
				t.Fatal("downgrade carried Referer")
			}
			if tc.foreign && strings.Contains(r.Referer(), "sig=A") {
				t.Fatal("signed Referer crossed origin")
			}
			if tc.foreign && r.Host != "" {
				t.Fatal("old Host override crossed origin")
			}
		})
	}
}

func TestAuthRefusalDoesNotExposeSignedRedirectURL(t *testing.T) {
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("foreign POST reached target") }))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/file?token=synthetic-B", http.StatusTemporaryRedirect)
	}))
	defer a.Close()
	f := buildFetcher()
	defer f.Close()
	e := f.Resolve(&base.Request{URL: a.URL + "/file?token=synthetic-A", Extra: &model.ReqExtra{Method: "POST", Body: "A-secret-body"}}, &base.Options{Path: t.TempDir()})
	if !errors.Is(e, transfer.ErrScopedContext) {
		t.Fatalf("wrong refusal classification: %v", e)
	}
	if strings.Contains(e.Error(), "synthetic-") || strings.Contains(e.Error(), "127.0.0.1") {
		t.Fatal("scoped refusal exposes signed URL")
	}
}

func TestAuthDomainCookiePartition(t *testing.T) {
	jar := newOriginCookieJar()
	a, _ := url.Parse("https://a.example.test/file")
	b, _ := url.Parse("https://b.example.test/file")
	jar.SetCookies(a, []*http.Cookie{{Name: "session", Value: "A", Domain: "example.test", Path: "/"}})
	if c := jar.Cookies(a); len(c) != 1 || c[0].Value != "A" {
		t.Fatal("A own domain cookie lost")
	}
	if len(jar.Cookies(b)) != 0 {
		t.Fatal("A parent-domain cookie reached B")
	}
	jar.SetCookies(b, []*http.Cookie{{Name: "session", Value: "B", Domain: "example.test", Path: "/"}})
	if c := jar.Cookies(b); len(c) != 1 || c[0].Value != "B" {
		t.Fatal("B own domain cookie lost")
	}
	if c := jar.Cookies(a); len(c) != 1 || c[0].Value != "A" {
		t.Fatal("B domain cookie replaced A context")
	}
}
func TestAuthProxyCredentialsRemainSeparate(t *testing.T) {
	seen := make(chan http.Header, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		if r.URL.Host == "a.example.test" {
			http.Redirect(w, r, "http://b.example.test/file", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("B"))
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("proxy-user", "proxy-password")
	f := buildFetcher()
	defer f.Close()
	f.ctl.GetProxy = func(*base.RequestProxy) func(*http.Request) (*url.URL, error) {
		return func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	}
	f.meta.Req = &base.Request{URL: "http://a.example.test/file", Extra: &model.ReqExtra{Header: map[string]string{"Authorization": "Bearer A", "X-Private": "A", "Proxy-Authorization": "bad-target-proxy-value"}}}
	req, e := f.buildRequest(context.Background(), f.meta.Req)
	if e != nil {
		t.Fatal(e)
	}
	res, e := doCredentialRequest(f.buildClient(), req)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-password"))
	first, second := <-seen, <-seen
	if first.Get("Proxy-Authorization") != expected || second.Get("Proxy-Authorization") != expected {
		t.Fatal("target filtering broke proxy auth")
	}
	if second.Get("Authorization") != "" || second.Get("X-Private") != "" {
		t.Fatal("proxy redirect carried old target credentials")
	}
}

func TestAuthExplicitCookiePatchInvalidatesOlderJarValue(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Cookie")
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	f := buildFetcher()
	defer f.Close()
	f.meta.Req = &base.Request{URL: server.URL, Extra: &model.ReqExtra{Header: map[string]string{"Cookie": "session=old-header"}}}
	u, _ := url.Parse(server.URL)
	f.cookieJar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "old-response", Path: "/"}})
	if e := f.Patch(&base.Request{Extra: &model.ReqExtra{Header: map[string]string{"Cookie": "session=new-explicit"}}}, nil); e != nil {
		t.Fatal(e)
	}
	req, e := f.buildRequest(context.Background(), f.meta.Req)
	if e != nil {
		t.Fatal(e)
	}
	res, e := doCredentialRequest(f.buildClient(), req)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if h := <-received; h != "session=new-explicit" {
		t.Fatalf("older cookie overrode explicit patch: %s", h)
	}
}

func TestAuthCookiePatchReplacesProviderJarSnapshot(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Cookie")
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	f := buildFetcher()
	defer f.Close()
	oldHeader := map[string]string{"Cookie": "session=provider-old"}
	f.meta.Req = &base.Request{URL: server.URL, Extra: &model.ReqExtra{Header: oldHeader}}
	u, _ := url.Parse(server.URL)
	providerJar := newOriginCookieJar()
	providerJar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "provider-jar-old", Path: "/"}})
	c, e := transfer.NewTargetCredentials(server.URL, oldHeader, providerJar)
	if e != nil {
		t.Fatal(e)
	}
	f.SetTargetCredentials(c)
	if e = f.Patch(&base.Request{Extra: &model.ReqExtra{Header: map[string]string{"Cookie": "session=new-explicit"}}}, nil); e != nil {
		t.Fatal(e)
	}
	req, e := f.buildRequest(context.Background(), f.meta.Req)
	if e != nil {
		t.Fatal(e)
	}
	res, e := doCredentialRequest(f.buildClient(), req)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if h := <-received; h != "session=new-explicit" {
		t.Fatal("provider jar survived explicit replacement")
	}
}
