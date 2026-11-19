package http

import (
	"mime"
	"net/http"
	"strings"
)

// InlineHTML identifies a resolved webpage without another request to the source.
func (f *Fetcher) InlineHTML() bool {
	f.resolveRespLock.Lock()
	defer f.resolveRespLock.Unlock()
	return f.inlineHTML
}

func inlineHTMLResponse(resp *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (mediaType != "text/html" && mediaType != "application/xhtml+xml") {
		return false
	}
	disposition, _, _ := strings.Cut(resp.Header.Get("Content-Disposition"), ";")
	disposition = strings.TrimSpace(disposition)
	return disposition == "" || strings.EqualFold(disposition, "inline")
}

// PathFreezeMIME returns only the actual resolved response's Content-Type.
// It performs no sniffing, extension inference, network or persistence.
func (f *Fetcher) PathFreezeMIME() string {
	f.resolveRespLock.Lock()
	defer f.resolveRespLock.Unlock()
	if f.resolveResp == nil {
		return ""
	}
	return f.resolveResp.Header.Get("Content-Type")
}
