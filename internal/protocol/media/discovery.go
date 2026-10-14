package media

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
	"golang.org/x/net/publicsuffix"
)

type scopedTransport struct {
	base   http.RoundTripper
	scopes []model.HeaderScope
}

func (t scopedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.Header = req.Header.Clone()
	copy.Header.Del("Authorization")
	copy.Header.Del("Proxy-Authorization")
	for _, scope := range t.scopes {
		for key := range scope.Headers {
			copy.Header.Del(key)
		}
	}
	// More specific scopes override broad scopes per header.
	priorities := map[string]int{}
	targetOrigin, _ := headerOrigin(req.URL.String())
	for _, scope := range t.scopes {
		priority := 0
		if scope.URL != "" {
			if scope.URL != req.URL.String() {
				continue
			}
			priority = 1 << 20
		} else {
			if scope.Origin != targetOrigin || !strings.HasPrefix(req.URL.EscapedPath(), scope.PathPrefix) {
				continue
			}
			priority = len(scope.PathPrefix) + 1
		}
		for key, value := range scope.Headers {
			key = http.CanonicalHeaderKey(key)
			if priority >= priorities[key] {
				copy.Header.Set(key, value)
				priorities[key] = priority
			}
		}
	}
	return t.base.RoundTrip(copy)
}

type manifestRef struct {
	url      string
	manifest bool
}

var hlsURI = regexp.MustCompile(`URI="([^"]+)"`)

func manifestRefs(raw string, baseURL *url.URL) []manifestRef {
	refs := []manifestRef{}
	add := func(value string, manifest bool, base *url.URL) {
		if len(refs) >= 64 {
			return
		}
		u, err := url.Parse(strings.TrimSpace(value))
		if err != nil {
			return
		}
		u = base.ResolveReference(u)
		if u.User != nil || u.Scheme != "http" && u.Scheme != "https" {
			return
		}
		refs = append(refs, manifestRef{u.String(), manifest})
	}
	if strings.HasPrefix(strings.TrimSpace(raw), "#EXTM3U") {
		scanner := bufio.NewScanner(strings.NewReader(raw))
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		child := false
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "#") {
				isManifest := strings.HasPrefix(line, "#EXT-X-MEDIA:") || strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:")
				for _, match := range hlsURI.FindAllStringSubmatch(line, -1) {
					add(match[1], isManifest, baseURL)
				}
				child = strings.HasPrefix(line, "#EXT-X-STREAM-INF:")
			} else {
				add(line, child, baseURL)
				child = false
			}
		}
		return refs
	}
	decoder := xml.NewDecoder(strings.NewReader(raw))
	bases := []*url.URL{baseURL}
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch node := token.(type) {
		case xml.StartElement:
			parent := bases[len(bases)-1]
			if node.Name.Local == "BaseURL" {
				var value string
				if decoder.DecodeElement(&value, &node) != nil {
					return refs
				}
				u, err := url.Parse(strings.TrimSpace(value))
				if err == nil {
					resolved := parent.ResolveReference(u)
					add(value, false, parent)
					bases[len(bases)-1] = resolved
				}
				continue
			}
			bases = append(bases, parent)
			for _, attr := range node.Attr {
				switch attr.Name.Local {
				case "media", "initialization", "sourceURL", "index":
					add(attr.Value, false, parent)
				}
			}
		case xml.EndElement:
			if len(bases) > 1 {
				bases = bases[:len(bases)-1]
			}
		}
	}
	return refs
}

func discoverContext(ctx context.Context, req *base.Request, ctl *controller.Controller, info *model.Inspection) []string {
	extra := req.Extra.(*model.ReqExtra)
	scopes, err := contextScopes(req, extra)
	if err != nil {
		return nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: req.SkipVerifyCert} // Explicit request setting; verification is the default.
	if ctl != nil && ctl.GetProxy != nil {
		transport.Proxy = ctl.GetProxy(req.Proxy)
	}
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	root, _ := url.Parse(req.URL)
	for _, c := range extra.Cookies {
		domain := strings.TrimPrefix(c.Domain, ".")
		if domain == "" {
			domain = root.Hostname()
		}
		scheme := "http"
		if c.Secure {
			scheme = "https"
		}
		u := &url.URL{Scheme: scheme, Host: domain, Path: "/"}
		cookie := &http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Secure: c.Secure, HttpOnly: c.HTTPOnly}
		if !c.HostOnly {
			cookie.Domain = domain
		}
		if c.ExpirationDate > 0 {
			cookie.Expires = time.Unix(int64(c.ExpirationDate), 0)
		}
		jar.SetCookies(u, []*http.Cookie{cookie})
	}
	client := &http.Client{Transport: scopedTransport{transport, scopes}, Jar: jar, Timeout: 3 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return http.ErrUseLastResponse
		}
		r.Header.Del("Authorization")
		return nil
	}}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type pending struct {
		raw   string
		depth int
	}
	queue := []pending{}
	seen := map[string]bool{}
	output := []string{}
	add := func(raw string) bool {
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || seen[raw] || len(output) >= 64 {
			return false
		}
		seen[raw] = true
		output = append(output, raw)
		return true
	}
	enqueue := func(raw string) {
		if add(raw) {
			queue = append(queue, pending{raw, 0})
		}
	}
	if info.ManifestURL != "" {
		enqueue(info.ManifestURL)
	}
	for _, f := range info.Formats {
		if f.ManifestURL != "" {
			enqueue(f.ManifestURL)
		}
	}
	ext := strings.ToLower(path.Ext(root.Path))
	if ext == ".m3u8" || ext == ".mpd" {
		enqueue(req.URL)
	}
	if len(queue) == 0 && (strings.Contains(info.Protocol, "m3u8") || strings.Contains(info.Protocol, "dash")) {
		enqueue(req.URL)
	}
	total := int64(4 * 1024 * 1024)
	fetched := 0
	for len(queue) > 0 && total > 0 && fetched < 5 && ctx.Err() == nil {
		item := queue[0]
		queue = queue[1:]
		fetched++
		request, err := http.NewRequestWithContext(ctx, "GET", item.raw, nil)
		if err != nil {
			continue
		}
		response, err := client.Do(request)
		if err != nil {
			continue
		}
		limit := int64(1024 * 1024)
		if total < limit {
			limit = total
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, limit))
		_ = response.Body.Close()
		total -= int64(len(data))
		if readErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 || int64(len(data)) >= limit {
			continue
		}
		for _, ref := range manifestRefs(string(data), response.Request.URL) {
			if add(ref.url) && ref.manifest && item.depth < 2 {
				queue = append(queue, pending{ref.url, item.depth + 1})
			}
		}
	}
	return output
}
