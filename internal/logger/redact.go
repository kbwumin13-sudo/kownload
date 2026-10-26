package logger

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const redacted = "[REDACTED]"

// A credential value in text: an escaped JSON string, a JSON string, or for
// headers the rest of the line (cookies carry several pairs and may hold
// quotes), for parameters the rest of the parameter.
const (
	// In escaped JSON, a value's escaped quote is three backslashes plus a
	// quote; its closing delimiter is one. Consume whole escape sequences.
	quotedValue = `\\"(?:[^"\\]|\\\\(?:[^"\\]|\\["\\]))*\\"|"(?:[^"\\]|\\.)*"`
	headerValue = `(` + quotedValue + `|[^\r\n]*)`
	paramValue  = `(` + quotedValue + `|[^&\s"'\\;,]+)`
)

var (
	// scheme://[userinfo@]host[:port][path][?query][#fragment], ending at
	// whitespace, a quote or a backslash. JSON \uXXXX escapes such as &
	// stay inside the URL.
	urlPattern = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^\s"'<>\\/?#]*@)?([^\s"'<>\\/?#@]*)((?:[^\s"'<>\\]|\\u[0-9a-f]{4})*)`)
	// Credential headers, as "Name: value", name=value or (escaped) JSON
	// "name":"value".
	headerPattern = regexp.MustCompile(`(?im)\b(set-cookie|cookie|proxy-authorization|authorization|x-captcha-token|accesstoken|access-token|x-auth-token|x-api-key|x-device-id)(\\?"?\s*[:=]\s*)` + headerValue)
	// Credential parameters outside a URL, such as a request URI or form body.
	paramPattern = regexp.MustCompile(`(?i)\b([\w-]*(?:token|secret|signature|password|passwd|ticket|session|sessionid|credential|apikey|api_key)|sign|sig|pwd|sid|auth|bduss|stoken|ptoken|accesskeyid|ossaccesskeyid)=` + paramValue)
	authPattern  = regexp.MustCompile(`(?i)\b(bearer\s+)[a-z0-9._~+/=-]{8,}|\b(basic\s+)[a-z0-9+/=]{8,}`)
	// Structured fields whose whole value is a credential.
	secretKey = regexp.MustCompile(`(?i)^(?:set-?cookie|cookies?|(?:proxy-?)?authorization|auth|x-captcha-token|x-device-id|bduss|stoken|ptoken|.*(?:token|secret|password|passwd|signature|credential|session|api[-_]?key).*)$`)
)

// secretHints are the lower-case fragments every rule above needs to match:
// the scheme separator, credential header, field and parameter names, and
// the Authorization schemes. Most log lines contain none of them.
var secretHints = []string{"://", "cookie", "auth", "token", "secret", "passw", "sig", "credential",
	"session", "ticket", "api-key", "api_key", "apikey", "x-device-id", "bduss", "pwd", "sid",
	"accesskeyid", "bearer", "basic"}

// mayHoldSecret reports whether a rule could match text. Non-ASCII text is
// always checked in full, since case folding can turn it into a hint.
func mayHoldSecret(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			return true
		}
	}
	lower := strings.ToLower(text)
	for _, hint := range secretHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// Redact removes credentials from text: URLs keep only their scheme and host,
// and the values of credential headers, parameters and Authorization schemes
// are replaced. The rest of the text is kept.
func Redact(text string) string {
	if !mayHoldSecret(text) {
		return text
	}
	text = urlPattern.ReplaceAllStringFunc(text, func(u string) string {
		m := urlPattern.FindStringSubmatch(u)
		out := m[1] + m[3]
		if m[2] != "" || m[4] != "" {
			out += "/…"
		}
		return out
	})
	text = replaceValue(headerPattern, text)
	text = replaceValue(paramPattern, text)
	return authPattern.ReplaceAllStringFunc(text, func(s string) string {
		return s[:strings.IndexAny(s, " \t")+1] + redacted
	})
}

// replaceValue replaces the value group of each match, keeping the quotes of
// a quoted value so the surrounding text stays well formed.
func replaceValue(pattern *regexp.Regexp, text string) string {
	return pattern.ReplaceAllStringFunc(text, func(s string) string {
		m := pattern.FindStringSubmatch(s)
		value := m[len(m)-1]
		prefix := s[:len(s)-len(value)]
		switch {
		case strings.HasPrefix(value, `\"`):
			return prefix + `\"` + redacted + `\"`
		case strings.HasPrefix(value, `"`):
			return prefix + `"` + redacted + `"`
		}
		return prefix + redacted
	})
}

// fieldRedactor removes credentials from a zerolog JSON event before the
// console writer formats it: fields named like credentials lose their whole
// value, and every other string is redacted as text. The console writer adds
// quotes and escapes, so the decoded values are where redaction is exact.
// Events without any hint pass through unchanged.
type fieldRedactor struct{ next io.Writer }

func (w fieldRedactor) Write(p []byte) (int, error) {
	out := p
	if mayHoldSecret(string(p)) {
		var event map[string]any
		dec := json.NewDecoder(bytes.NewReader(p))
		dec.UseNumber()
		if dec.Decode(&event) == nil {
			encoded, err := json.Marshal(redactValue("", event))
			if err != nil {
				return 0, err
			}
			out = append(encoded, '\n')
		} else {
			out = []byte(Redact(string(p)))
		}
	}
	if _, err := w.next.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func redactValue(key string, v any) any {
	if key != "" && secretKey.MatchString(key) {
		return redacted
	}
	switch v := v.(type) {
	case string:
		return Redact(v)
	case map[string]any:
		for k, x := range v {
			v[k] = redactValue(k, x)
		}
	case []any:
		for i, x := range v {
			v[i] = redactValue("", x)
		}
	}
	return v
}
