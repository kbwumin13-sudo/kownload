// Package mailru resolves only anonymous cloud.mail.ru public shares.
package mailru

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

type Source struct {
	ShareID string `json:"shareId"`
}

func share(raw string) (Source, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Scheme != "https" || u.Host != "cloud.mail.ru" {
		return Source{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "public" {
		return Source{}, false
	}
	for _, value := range parts[1:] {
		if value == "" || len(value) > 128 {
			return Source{}, false
		}
		for _, c := range value {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				return Source{}, false
			}
		}
	}
	return Source{ShareID: parts[1] + "/" + parts[2]}, true
}

func clocloHost(host, suffix string) bool {
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	label := strings.TrimSuffix(host, suffix)
	return strings.HasPrefix(label, "cloclo") && !strings.Contains(label, ".")
}

func Direct(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.User == nil && u.Scheme == "https" && (u.Port() == "" || u.Port() == "443") &&
		clocloHost(u.Hostname(), ".datacloudmail.ru") && strings.HasPrefix(u.Path, "/public/get/")
}

// A signed CDN path cannot reconstruct a public share ID. The caller must have
// either the public page itself or its explicit Referer; no guessing by filename.
func Match(raw, referer string) (Source, bool) {
	if source, ok := share(raw); ok {
		return source, true
	}
	if Direct(raw) {
		return share(referer)
	}
	return Source{}, false
}

func ClassifyError(status int) bool { return transfer.IsExpiredLinkStatus(status) }

func (s Source) Page() string { return "https://cloud.mail.ru/public/" + s.ShareID }

func (s Source) Refresh(ctx context.Context, original *http.Client) (string, error) {
	if checked, ok := share(s.Page()); !ok || checked != s {
		return "", errors.New("invalid Mail.ru public source")
	}
	client := *original
	client.Jar = nil // Public-only; never reuse login cookies.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	get := func(raw string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return nil, errors.New("invalid Mail.ru public resolver response")
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("Mail.ru public resolver request failed")
		}
		return resp, nil
	}
	resp, err := get("https://cloud.mail.ru/api/v2/dispatcher?api=2")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("Mail.ru public resolver requires authentication or is unavailable")
	}
	var result struct {
		Body struct {
			Links []struct {
				URL string `json:"url"`
			} `json:"weblink_get"`
		} `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil || len(result.Body.Links) == 0 {
		return "", errors.New("Mail.ru public resolver returned no usable download service")
	}
	base, err := url.Parse(result.Body.Links[0].URL)
	if err != nil || base.User != nil || base.Scheme != "https" || base.Port() != "" ||
		!clocloHost(base.Hostname(), ".cloud.mail.ru") || !strings.HasPrefix(base.Path, "/public/") {
		return "", errors.New("Mail.ru public resolver returned an unsupported download service")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + s.ShareID
	link, err := get(base.String())
	if err != nil {
		return "", err
	}
	defer link.Body.Close()
	if link.StatusCode != 301 && link.StatusCode != 302 && link.StatusCode != 303 && link.StatusCode != 307 && link.StatusCode != 308 {
		return "", errors.New("Mail.ru public share requires authentication or is unavailable")
	}
	target, err := link.Location()
	if err != nil || !Direct(target.String()) {
		return "", errors.New("Mail.ru public share did not return an anonymous download link")
	}
	return target.String(), nil
}
