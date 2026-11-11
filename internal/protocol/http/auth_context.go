package http

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

type originCookieJar struct {
	mu   sync.Mutex
	jars map[string]http.CookieJar
}

func newOriginCookieJar() *originCookieJar {
	return &originCookieJar{jars: map[string]http.CookieJar{}}
}
func (j *originCookieJar) forURL(u *url.URL) http.CookieJar {
	origin, err := transfer.CredentialOrigin(u.String())
	if err != nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	jar := j.jars[origin]
	if jar == nil {
		jar, _ = cookiejar.New(nil)
		j.jars[origin] = jar
	}
	return jar
}
func (j *originCookieJar) Cookies(u *url.URL) []*http.Cookie {
	if jar := j.forURL(u); jar != nil {
		return jar.Cookies(u)
	}
	return nil
}
func (j *originCookieJar) SetCookies(u *url.URL, c []*http.Cookie) {
	if jar := j.forURL(u); jar != nil {
		jar.SetCookies(u, c)
	}
}

func (j *originCookieJar) clearOrigin(raw string) {
	origin, err := transfer.CredentialOrigin(raw)
	if err != nil {
		return
	}
	j.mu.Lock()
	delete(j.jars, origin)
	j.mu.Unlock()
}

// SetTargetCredentials transfers a new provider context while inactive. It is
// in-memory only, like SetRequestPolicy, and cannot be set by JSON metadata.
func (f *Fetcher) SetTargetCredentials(c *transfer.TargetCredentials) { f.targetCredentials = c }
func outboundCredentialGuard(root string, ctx *transfer.TargetCredentials, jar http.CookieJar) func(*http.Request) error {
	return func(r *http.Request) error {
		old, err := transfer.CredentialOrigin(root)
		if err != nil {
			return err
		}
		next, err := transfer.CredentialOrigin(r.URL.String())
		if err != nil {
			return err
		}
		foreign := old != next
		if foreign {
			if r.Method != "GET" && r.Method != "HEAD" || r.Body != nil && r.Body != http.NoBody || r.ContentLength > 0 {
				return transfer.ErrScopedContext
			}
			if r.URL.User != nil && !ctx.Matches(r.URL.String()) {
				return transfer.ErrScopedContext
			}
			r.Header = transfer.InheritedHeaders(r.Header)
			from, _ := url.Parse(root)
			if from.Scheme == "https" && r.URL.Scheme == "http" {
				r.Header.Del("Referer")
			}
			r.URL.User = nil
			r.Host = ""
			// Client.send adds jar cookies after CheckRedirect. Rebuild foreign Cookie
			// only from this task's target-origin jar, never the inherited Cookie line.
			if jar != nil {
				for _, cookie := range jar.Cookies(r.URL) {
					r.AddCookie(cookie)
				}
			}
		}
		if ctx.Matches(r.URL.String()) {
			if err = ctx.Apply(r); err != nil {
				return err
			}
		}
		// Target responses may rotate a cookie supplied by a provider. Prefer
		// those exact-origin cookies without reviving any inherited A cookie.
		if jar != nil {
			freshCookies := jar.Cookies(r.URL)
			if len(freshCookies) > 0 {
				names := map[string]bool{}
				for _, c := range freshCookies {
					names[c.Name] = true
				}
				oldCookies := r.Cookies()
				r.Header.Del("Cookie")
				for _, c := range oldCookies {
					if !names[c.Name] {
						r.AddCookie(c)
					}
				}
				for _, c := range freshCookies {
					r.AddCookie(c)
				}
			}
		}
		transfer.DropProxyHeaders(r.Header)
		return nil
	}
}

// Client.Do wraps transport errors with the request URL. New policy refusals
// carry a fixed category so signed URLs do not reach task errors or logs.
func credentialPolicyError(err error) error {
	for _, category := range []error{transfer.ErrScopedContext, transfer.ErrCredentialContext} {
		if errors.Is(err, category) {
			return category
		}
	}
	return nil
}

func doCredentialRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	response, err := client.Do(req)
	if category := credentialPolicyError(err); category != nil {
		return response, category
	}
	return response, err
}

type credentialFailure struct{ err error }

func (f *Fetcher) stopForCredentialFailure(conn *connection, err error) {
	f.failSingleStream(conn, err)
	f.policyFailure.CompareAndSwap(nil, &credentialFailure{err: err})
	if f.cancel != nil {
		f.cancel()
	}
}
func normalizedExtra(req *base.Request) (*model.ReqExtra, error) {
	if err := base.ParseReqExtra[model.ReqExtra](req); err != nil {
		return nil, transfer.ErrCredentialContext
	}
	if req.Extra == nil {
		req.Extra = &model.ReqExtra{}
	}
	extra := req.Extra.(*model.ReqExtra)
	h, err := transfer.CredentialHeaders(extra.Header)
	if err != nil {
		return nil, err
	}
	transfer.DropProxyHeaders(h)
	extra.Header = transfer.HeaderMap(h)
	return extra, nil
}
