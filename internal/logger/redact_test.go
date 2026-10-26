package logger

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// All credentials below are synthetic.
func TestRedactRemovesCredentials(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`Get "https://user:SYNTH-PASS@cdn.example.test/private/a.bin?X-Amz-Signature=SYNTH-SIG&token=SYNTH-TOKEN": EOF`,
			`Get "https://cdn.example.test/…": EOF`},
		{`Get \"https://cdn.example.test/a.bin?sig=SYNTH-SIG\": EOF`, `Get \"https://cdn.example.test/…\": EOF`},
		{`url=http://127.0.0.1:8080/f.bin?sign=SYNTH-SIGN#frag`, `url=http://127.0.0.1:8080/…`},
		{`{"url":"https://cdn.example.test/a?x=1&sig=SYNTH-SIG"}`, `{"url":"https://cdn.example.test/…"}`},
		{`{"url":"https://cdn.example.test/a?x=1\u0026sig=SYNTH-SIG"}`, `{"url":"https://cdn.example.test/…"}`},
		{`webhook: failed url=https://hooks.example.test/services/SYNTH-PATH-SECRET`, `webhook: failed url=https://hooks.example.test/…`},
		{"request header Cookie: session=SYNTH-COOKIE; uid=SYNTH-UID\nnext line", "request header Cookie: " + redacted + "\nnext line"},
		{`Cookie: sid="SYNTH-QUOTED"; other=SYNTH-OTHER`, `Cookie: ` + redacted},
		{`Set-Cookie: sid=SYNTH-SET; Path=/; HttpOnly`, `Set-Cookie: ` + redacted},
		{`Authorization: Bearer SYNTH-BEARER-TOKEN`, `Authorization: ` + redacted},
		{`headers={"Authorization":"Bearer SYNTH-BEARER-TOKEN","Accept":"*/*"}`, `headers={"Authorization":"` + redacted + `","Accept":"*/*"}`},
		{`{"Cookie":"sid=\"SYNTH-Q\"; other=SYNTH-O","x":1}`, `{"Cookie":"` + redacted + `","x":1}`},
		{`error="headers {\"Cookie\":\"SYNTH-COOKIE\"}"`, `error="headers {\"Cookie\":\"` + redacted + `\"}"`},
		{`http server panic: GET /api/v1/tasks?access_token=SYNTH-TOKEN&page=2`, `http server panic: GET /api/v1/tasks?access_token=` + redacted + `&page=2`},
		{`form token="SYNTH \"QUOTED\" TOKEN" next`, `form token="` + redacted + `" next`},
		{`token refused: bearer SYNTH-BEARER-TOKEN`, `token refused: bearer ` + redacted},
		{`proxy basic U1lOVEgtVVNFUjpTWU5USC1QQVNT failed`, `proxy basic ` + redacted + ` failed`},
	} {
		if got := Redact(c.in); got != c.want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRedactKeepsOrdinaryLines(t *testing.T) {
	for _, line := range []string{
		"2026-10-26 07:58:59 WRN task download failed, task id: T1 error=\"mkdir w12: read-only file system\"\n",
		"INF task deferred by source rate limit until 2026-10-26T13:32:22+08:00, task id: T2",
		"ERR open /Users/someone/Downloads/a b.zip: permission denied",
		"WRN basic auth failed for proxy",
		"INF 3 tokens left, session cookie expired",
	} {
		if got := Redact(line); got != line {
			t.Errorf("an ordinary line changed:\n got %q\nwant %q", got, line)
		}
	}
}

// The real file logger, as core.log and extension.log are written: credential
// fields, quoted or not, nested, and credentials inside messages and errors.
func TestFileLoggerRedacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "core.log")
	l := NewLogger(true, path)
	l.Warn().
		Err(errors.New(`Get "https://cdn.example.test/private/a.bin?X-Amz-Signature=SYNTH-SIG": EOF; Cookie: sid="SYNTH-ERR-COOKIE"; uid=SYNTH-ERR-UID`)).
		Str("Cookie", `sid="SYNTH-QUOTED-COOKIE"; other=SYNTH-OTHER-COOKIE`).
		Str("cookie", "a=SYNTH-A; b=SYNTH-B").
		Str("Set-Cookie", `sid=SYNTH-SET; Path=/; HttpOnly`).
		Str("Authorization", "Bearer SYNTH-BEARER").
		Str("proxy_authorization", "Basic U1lOVEgtVVNFUjpTWU5USC1QQVNT").
		Str("access_token", "SYNTH-TOKEN").
		Str("X-Captcha-Token", `"SYNTH-CAPTCHA"`).
		Interface("headers", map[string]any{"Cookie": `x="SYNTH-NESTED"`, "Accept": "*/*", "list": []any{"token=SYNTH-LIST"}}).
		Msgf("task download failed, task id: %s, form token=%s", "T1", "SYNTH-MSG")
	l.CLose()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, leak := range []string{"SYNTH", "U1lOVEgtVVNFUjpTWU5USC1QQVNT"} {
		if strings.Contains(log, leak) {
			t.Fatalf("%s survived the file logger:\n%s", leak, log)
		}
	}
	for _, kept := range []string{"task download failed, task id: T1", "https://cdn.example.test/…", "Accept", "*/*"} {
		if !strings.Contains(log, kept) {
			t.Fatalf("%q missing from the redacted log:\n%s", kept, log)
		}
	}
}

// Taken from the build 17 re-acceptance (R03): a quoted Cookie field written
// through the real logger keeps no secret.
func TestQuotedCookieFieldIsRedacted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "core.log")
	l := NewLogger(true, p)
	l.Warn().Str("Cookie", `sid="SYNTH-QUOTED-COOKIE"; other=SYNTH-OTHER-COOKIE`).Msg("request failed")
	l.CLose()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SYNTH-") {
		t.Fatalf("credential survived actual logger: %s", data)
	}
}

// Text without a hint skips redaction, so every credential form the rules
// handle, in any letter case, and every credential field name must contain
// one; non-ASCII text is always redacted in full.
func TestSecretHintsCoverEveryRule(t *testing.T) {
	for _, sample := range []string{
		"get https://h.example.test/p", "Cookie: a=b", "set-cookie: a=b", "Authorization: x",
		"proxy-authorization: x", "x-captcha-token: x", "accesstoken: x", "access-token: x",
		"x-auth-token: x", "x-api-key: x", "x-device-id: x", "token=x", "client_secret=x",
		"signature=x", "password=x", "passwd=x", "ticket=x", "session=x", "sessionid=x",
		"credential=x", "apikey=x", "api_key=x", "sign=x", "sig=x", "pwd=x", "sid=x", "auth=x",
		"bduss=x", "stoken=x", "ptoken=x", "accesskeyid=x", "ossaccesskeyid=x",
		"bearer SYNTHTOKEN1", "basic U1lOVEgxMjM=", "to\u212aen=x", "x\u017fession=x",
	} {
		for _, text := range []string{sample, strings.ToUpper(sample)} {
			if Redact(text) == text {
				t.Fatalf("sample %q matches no rule", text)
			}
			if !mayHoldSecret(text) {
				t.Errorf("%q holds a credential but would skip redaction", text)
			}
		}
	}
	for _, key := range []string{"set-cookie", "setcookie", "cookie", "cookies", "authorization",
		"proxy-authorization", "proxyauthorization", "auth", "x-captcha-token", "x-device-id", "bduss",
		"stoken", "ptoken", "my_token", "client_secret", "password", "passwd", "signature",
		"credential", "session", "api-key", "api_key", "apikey"} {
		for _, k := range []string{key, strings.ToUpper(key)} {
			if !secretKey.MatchString(k) {
				t.Fatalf("%q is not a credential field", k)
			}
			if !mayHoldSecret(`{"` + k + `":"v"}`) {
				t.Errorf("credential field %q would skip redaction", k)
			}
		}
	}
	if mayHoldSecret(`{"level":"info","time":"2026-10-26 18:00:00","message":"task done, task id: T1"}`) {
		t.Error("an ordinary event is checked in full")
	}
}
