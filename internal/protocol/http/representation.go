package http

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/netdisk/mailru"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// ErrUnsafeResume never authorizes replacing any byte in the existing file.
var ErrUnsafeResume = errors.New("cannot verify HTTP resource identity; partial file preserved")

type representation struct {
	ResourceKey  string `json:"resourceKey,omitempty"`
	QueryKey     string `json:"queryKey,omitempty"`
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Size         int64  `json:"size,omitempty"`
	Encoding     string `json:"encoding,omitempty"`
	Hash         string `json:"hash,omitempty"`
}

func responseRepresentation(resp *http.Response, size int64) representation {
	r := representation{Size: size, Encoding: normalizedEncoding(resp.Header.Get("Content-Encoding"))}
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' && !strings.ContainsAny(etag, "\r\n") {
		r.ETag = etag
	}
	// A modification time at least one minute older than Date can be used as
	// an If-Range validator. Weak ETags alone never authorize a resume.
	modified, e1 := http.ParseTime(resp.Header.Get("Last-Modified"))
	date, e2 := http.ParseTime(resp.Header.Get("Date"))
	if e1 == nil && e2 == nil && date.Sub(modified) >= time.Minute {
		r.LastModified = modified.UTC().Format(http.TimeFormat)
	}
	return r
}

func normalizedEncoding(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "identity" {
		return ""
	}
	return value
}

func (f *Fetcher) representationSnapshot() representation {
	f.identityMu.RLock()
	defer f.identityMu.RUnlock()
	return f.identity
}

func (f *Fetcher) rememberRepresentation(resp *http.Response, size int64) {
	f.identityMu.Lock()
	f.identity = responseRepresentation(resp, size)
	f.identity.ResourceKey = requestResourceKey(f.meta.Req)
	f.identity.QueryKey = requestQueryKey(f.meta.Req)
	f.identityMu.Unlock()
}

// resourceURL is the URL that names the resource: the request URL, or the
// public share page of a Mail.ru link.
func resourceURL(req *base.Request) string {
	raw := req.URL
	if extra, ok := req.Extra.(*fhttp.ReqExtra); ok && extra != nil {
		header := http.Header{}
		for k, v := range extra.Header {
			header.Set(k, v)
		}
		if source, ok := mailru.Match(raw, header.Get("Referer")); ok {
			raw = source.Page()
		}
	} else if source, ok := mailru.Match(raw, ""); ok {
		raw = source.Page()
	}
	return raw
}

// requestResourceKey scopes saved validators to one resource: the scheme,
// host and path of the URL, or a Mail.ru link's public share page. It is only
// compared when a validator was saved. The query may change, as it does for
// renewed signed links of the same file; If-Range and the validator check on
// every response decide whether the saved bytes still match, together with a
// comparison of saved bytes when more than signatures changed (see
// requestQueryKey). Without a validator the saved-overlap comparison decides.
func requestResourceKey(req *base.Request) string {
	raw := resourceURL(req)
	if u, err := url.Parse(raw); err == nil {
		raw = u.Scheme + "://" + strings.ToLower(u.Host) + u.EscapedPath()
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
}

// signatureParams sign or expire a link without choosing the file wherever
// they appear: generic names, CloudFront's key pair, Alibaba Cloud OSS and
// CDN, Akamai, Google Cloud V2 and Tencent Cloud's session token, besides the
// x-amz-* (Amazon S3) and x-goog-* (Google Cloud V4) prefixes. A parameter
// missing here only costs one comparison of saved bytes on resume, never a
// refusal.
var signatureParams = map[string]bool{
	"sig": true, "signature": true, "expires": true, "expire": true, "key-pair-id": true,
	"ossaccesskeyid": true, "security-token": true, "auth_key": true,
	"x-oss-signature-version": true, "x-oss-credential": true, "x-oss-date": true, "x-oss-expires": true,
	"x-oss-signature": true, "x-oss-security-token": true, "x-oss-additional-headers": true,
	"hdnts": true, "hdnea": true, "__token__": true,
	"googleaccessid": true, "x-cos-security-token": true,
}

var azureSASParams = map[string]bool{
	"sv": true, "ss": true, "srt": true, "sp": true, "se": true, "st": true, "spr": true, "sip": true,
	"sr": true, "si": true, "sdd": true, "skoid": true, "sktid": true, "skt": true, "ske": true,
	"sks": true, "skv": true, "saoid": true, "suoid": true, "scid": true, "ses": true,
	"rscc": true, "rscd": true, "rsce": true, "rscl": true, "rsct": true,
}

// providerSignatures are short names that sign a link only beside all of
// their provider's markers in the same query. Elsewhere a site may use them
// to choose the file, as ?e=2 for episode 2.
var providerSignatures = []struct {
	markers []string
	signs   func(name string) bool
}{
	{[]string{"e", "token"}, func(name string) bool { return name == "e" || name == "token" }}, // Qiniu
	{[]string{"sig"}, func(name string) bool { return azureSASParams[name] }},                  // Azure SAS
	{[]string{"q-signature"}, func(name string) bool { return strings.HasPrefix(name, "q-") }}, // Tencent Cloud COS
	{[]string{"signature", "key-pair-id"}, func(name string) bool { return name == "policy" }}, // CloudFront
}

// signatureParam reports whether the lower-case parameter name signs or
// expires the link, given the names present in its query.
func signatureParam(name string, present map[string]bool) bool {
	if signatureParams[name] || strings.HasPrefix(name, "x-amz-") || strings.HasPrefix(name, "x-goog-") {
		return true
	}
	for _, provider := range providerSignatures {
		if !provider.signs(name) {
			continue
		}
		signed := true
		for _, marker := range provider.markers {
			signed = signed && present[marker]
		}
		if signed {
			return true
		}
	}
	return false
}

// requestQueryKey identifies the query of the resource URL without its
// signature and expiry parameters. Renewed signed links of one file share it.
// When it changed since the saved bytes were confirmed, such as
// download?id=2 patched into the task of id=1, a resume with a saved
// validator compares saved bytes as well: another file can share a validator.
func requestQueryKey(req *base.Request) string {
	var parts, names []string
	present := map[string]bool{}
	if u, err := url.Parse(resourceURL(req)); err == nil {
		for _, part := range strings.Split(u.RawQuery, "&") {
			if part == "" {
				continue
			}
			name, _, _ := strings.Cut(part, "=")
			if decoded, err := url.QueryUnescape(name); err == nil {
				name = decoded
			}
			name = strings.ToLower(name)
			parts, names = append(parts, part), append(names, name)
			present[name] = true
		}
	}
	var kept []string
	for i, part := range parts {
		if !signatureParam(names[i], present) {
			kept = append(kept, part)
		}
	}
	sort.Strings(kept)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(kept, "&"))))
}

// SetResourceKey binds a child to a file whose provider/source/ID the wrapper
// has already checked. It scopes opaque HTTP validators across CDN URL changes.
func (f *Fetcher) SetResourceKey(key string) error {
	key = fmt.Sprintf("%x", sha256.Sum256([]byte("provider|"+key)))
	f.identityMu.Lock()
	defer f.identityMu.Unlock()
	if f.restored && f.identity.ResourceKey != "" && f.identity.ResourceKey != key {
		return fmt.Errorf("%w: provider file identity changed", ErrUnsafeResume)
	}
	f.identity.ResourceKey = key
	f.resourceKey = key
	return nil
}

func (f *Fetcher) applyIfRange(req *http.Request) {
	r := f.representationSnapshot()
	validator := r.ETag
	if validator == "" {
		validator = r.LastModified
	}
	if validator == "" {
		validator = f.rangeResumeValidator // Legacy prefix upgrade checkpoint.
	}
	if validator != "" {
		req.Header.Set("If-Range", validator)
		req.Header.Set("Accept-Encoding", "identity")
	}
}

func (f *Fetcher) validateRepresentation(resp *http.Response) error {
	r := f.representationSnapshot()
	if r.ETag != "" && resp.Header.Get("ETag") != r.ETag {
		return fmt.Errorf("%w: ETag changed or missing", ErrUnsafeResume)
	}
	if r.ETag == "" && r.LastModified != "" {
		modified, err := http.ParseTime(resp.Header.Get("Last-Modified"))
		if err != nil || modified.UTC().Format(http.TimeFormat) != r.LastModified {
			return fmt.Errorf("%w: Last-Modified changed or missing", ErrUnsafeResume)
		}
	}
	if f.usesRanges() && (r.ETag != "" || r.LastModified != "") && (resp.Uncompressed || normalizedEncoding(resp.Header.Get("Content-Encoding")) != r.Encoding) {
		return fmt.Errorf("%w: content encoding changed", ErrUnsafeResume)
	}
	return nil
}

// SetResumeHash is for a wrapper that independently verifies the complete file,
// such as netdisk. A persisted hash alone is not trusted by ordinary HTTP tasks:
// the current wrapper must provide that same expected digest after Restore.
func (f *Fetcher) SetResumeHash(digest string) error {
	digest = strings.ToLower(strings.TrimSpace(digest))
	algorithm, value, ok := strings.Cut(digest, ":")
	length := map[string]int{"md5": 32, "sha1": 40, "sha256": 64}[algorithm]
	if !ok || length == 0 || len(value) != length {
		return nil
	}
	if _, err := hex.DecodeString(value); err != nil {
		return nil
	}
	f.identityMu.Lock()
	defer f.identityMu.Unlock()
	if f.identity.Hash != "" && f.identity.Hash != digest {
		return fmt.Errorf("%w: expected content hash changed", ErrUnsafeResume)
	}
	f.identity.Hash, f.resumeHash = digest, digest
	return nil
}

func (f *Fetcher) hasResumeData() bool {
	f.connMu.Lock()
	defer f.connMu.Unlock()
	if f.resolveDataPos.Load() > 0 || f.prefetchSize.Load() > 0 {
		return true
	}
	for _, c := range f.connections {
		if c.Downloaded > 0 {
			return true
		}
	}
	return false
}

// An intact, already completed local checkpoint needs no new response and
// therefore no validator. Missing/truncated targets are reset before this check.
func (f *Fetcher) completedLocalCheckpoint() bool {
	if f.meta.Res.Size <= 0 {
		return false
	}
	f.connMu.Lock()
	total := int64(0)
	complete := len(f.connections) > 0 || f.resolveConn != nil
	if f.resolveConn != nil {
		complete = complete && f.resolveConn.Completed
		total += f.resolveConn.Downloaded
	}
	for _, c := range f.connections {
		complete = complete && c.Completed
		if f.usesRanges() {
			complete = complete && c.Chunk != nil && c.Chunk.remain() <= 0
		}
		total += c.Downloaded
	}
	f.connMu.Unlock()
	if !complete || total != f.meta.Res.Size {
		return false
	}
	info, err := f.localOutputInfo()
	return err == nil && info.Size() == f.meta.Res.Size
}

// checkResumeIdentity checks what the checkpoint itself can prove, without a
// request. overlap reports saved ranges that only a comparison with the
// server can confirm: no validator or trusted digest was saved, or the link
// changed in more than its signatures. The download loop runs it before any
// stream starts (verifySavedOverlaps).
func (f *Fetcher) checkResumeIdentity() (overlap bool, err error) {
	if !f.usesRanges() || !f.hasResumeData() {
		return false, nil // Full GET restarts from zero; it cannot reuse an old prefix.
	}
	if f.completedLocalCheckpoint() {
		return false, nil
	}
	f.identityMu.RLock()
	r, external, expectedKey := f.identity, f.resumeHash, f.resourceKey
	f.identityMu.RUnlock()
	if expectedKey == "" {
		expectedKey = requestResourceKey(f.meta.Req)
	}
	if r.ResourceKey != "" && r.ResourceKey != expectedKey && f.hasResumeValidator() && !(r.Hash != "" && r.Hash == external) {
		return false, fmt.Errorf("%w: source resource changed", ErrUnsafeResume)
	}
	if r.Size > 0 && r.Size != f.meta.Res.Size {
		return false, fmt.Errorf("%w: saved total length changed", ErrUnsafeResume)
	}
	if r.ETag != "" || r.LastModified != "" || f.rangeResumeValidator != "" || r.Hash != "" && r.Hash == external {
		// A link changed in more than its signatures may name another file
		// that shares the validator. A provider key already names the file.
		queryChanged := f.resourceKey == "" && r.QueryKey != "" && r.QueryKey != requestQueryKey(f.meta.Req)
		return queryChanged, nil
	}
	return true, nil
}

func (f *Fetcher) hasResumeValidator() bool {
	r := f.representationSnapshot()
	f.identityMu.RLock()
	external := f.resumeHash
	f.identityMu.RUnlock()
	return r.ETag != "" || r.LastModified != "" || f.rangeResumeValidator != "" || r.Hash != "" && r.Hash == external
}
