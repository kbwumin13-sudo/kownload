package netdisk

import (
	"context"
	"encoding/json"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPErrorRetainsProviderLoginReason(t *testing.T) {
	p := adapterProvider(t, "quark")
	s := adapterSession("quark")
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":23018,"message":"private https://cdn.invalid/?token=hidden"}`))}, nil
	})
	_, err := c.Do(context.Background(), "POST", "https://drive-pc.quark.cn/1/clouddrive/file/download", nil, nil)
	adapterError(t, err, "login_required")
	if strings.Contains(err.Error(), "hidden") {
		t.Fatal("raw upstream body exposed")
	}
}

func TestHTTPErrorFixedProviderMappings(t *testing.T) {
	tests := []struct {
		value   any
		code    string
		message string
	}{
		{23018, "login_required", "请在浏览器登录网盘后重新同步"},
		{"unauthorized", "login_required", "请在浏览器登录网盘后重新同步"},
		{"unauthenticated", "login_required", "请在浏览器登录网盘后重新同步"},
		{9, "expired", "网盘登录或分享凭据已过期，请重新同步浏览器"},
		{"invalid_token", "expired", "网盘登录或分享凭据已过期，请重新同步浏览器"},
		{132, "verification_required", "网盘需要浏览器验证，请完成验证后重新同步"},
		{"8001", "verification_required", "网盘需要浏览器验证，请完成验证后重新同步"},
		{"captcha_required", "verification_required", "网盘需要浏览器验证，请完成验证后重新同步"},
		{"captcha_invalid", "verification_required", "网盘需要浏览器验证，请完成验证后重新同步"},
		{"captcha_expired", "verification_required", "网盘需要浏览器验证，请完成验证后重新同步"},
		{"invalid_captcha", "verification_required", "网盘需要浏览器验证，请完成验证后重新同步"},
		{"INVALID_CAPTCHA_TOKEN", "verification_required", "网盘需要浏览器验证，请完成验证后重新同步"},
		{5113, "quota_exceeded", "网盘下载流量不足"},
		{"not_found", "share_expired", "分享不存在、已取消或已过期"},
		{"password_error", "password_required", "提取码不正确，请在浏览器验证分享"},
		{"save_required", "transfer_required", "此分享需要先转存到网盘"},
	}
	for _, field := range []string{"code", "errno", "errorCode", "error_code", "error"} {
		for _, tc := range tests {
			t.Run(field+"/"+str(tc.value), func(t *testing.T) {
				payload := map[string]any{field: tc.value, "message": "private-response", "error_description": "Bearer private-token", "signedURL": "https://cdn.invalid/?token=private-signature"}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				p := adapterProvider(t, "xunlei")
				c := adapterClient(p, adapterSession("xunlei"), func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
				})
				res, err := c.Do(context.Background(), "GET", xunleiAPI+"/files", nil, nil)
				adapterError(t, err, tc.code)
				if res != nil || err.Error() != tc.message {
					t.Fatalf("want fixed message %q, got %v", tc.message, err)
				}
			})
		}
	}
}

func TestHTTPErrorUnknownValuesNeverExposeResponse(t *testing.T) {
	const secret = "https://cdn.invalid/file?token=private-token&signature=private-signature"
	for _, field := range []string{"code", "errno", "errorCode", "error_code", "error"} {
		for _, value := range []any{secret, "captcha_required " + secret, "unknown_code", 123456789, nil, "", 0, false, map[string]any{"error": secret}, []string{secret}} {
			body, err := json.Marshal(map[string]any{field: value, "message": secret, "access_token": "private-token", "signedURL": secret})
			if err != nil {
				t.Fatal(err)
			}
			assertHTTPErrorMessage(t, string(body), "api_error", "网盘接口返回 HTTP 403")
		}
	}
	for _, body := range []string{
		`{"message":"captcha_required","error_description":"unauthorized","signedURL":"` + secret + `"}`,
		`{"data":{"error":"captcha_required"}}`,
		`{"error_code":`,
		`<html>` + secret + `</html>`,
		`{"padding":"` + strings.Repeat("x", 64<<10) + `","error":"captcha_required"}`,
	} {
		assertHTTPErrorMessage(t, body, "api_error", "网盘接口返回 HTTP 403")
	}
}

func TestHTTPErrorUsesKnownReasonAfterUnknownCode(t *testing.T) {
	for _, body := range []string{
		`{"error_code":4002,"error":"captcha_required"}`,
		`{"error_code":9,"error":"captcha_invalid"}`,
		`{"code":0,"error_code":0,"error":"unauthenticated"}`,
		`{"code":"private-token","errno":null,"errorCode":false,"error_code":{},"error":"unauthorized"}`,
	} {
		code, message := "login_required", "请在浏览器登录网盘后重新同步"
		if strings.Contains(body, "captcha_") {
			code, message = "verification_required", "网盘需要浏览器验证，请完成验证后重新同步"
		}
		assertHTTPErrorMessage(t, body, code, message)
	}
}

func assertHTTPErrorMessage(t *testing.T, body, code, message string) {
	t.Helper()
	p := adapterProvider(t, "xunlei")
	c := adapterClient(p, adapterSession("xunlei"), func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	res, err := c.Do(context.Background(), "GET", xunleiAPI+"/files", nil, nil)
	adapterError(t, err, code)
	if res != nil || err.Error() != message {
		t.Fatalf("want fixed message %q, got %v", message, err)
	}
	encoded, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "https://") {
		t.Fatal("serialized error exposed upstream data")
	}
}

func TestQuarkAccountIdentityUsesCurrentUserNotRotatingCookies(t *testing.T) {
	p := adapterProvider(t, "quark")
	for _, cookie := range []string{"old-session", "refreshed-session"} {
		s := adapterSession("quark")
		s.Capture.Cookies = []model.Cookie{{Name: "__puus", Value: cookie, Domain: ".quark.cn", Path: "/", Secure: true}}
		c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "pan.quark.cn" || r.URL.Path != "/account/info" || r.Method != "GET" {
				t.Fatal("wrong current-user endpoint")
			}
			return adapterJSON(`{"success":true,"data":{"ucid":"stable-user","nickname":"display-name"}}`), nil
		})
		if got := quarkAccountID(context.Background(), c, model.Source{URL: "https://pan.quark.cn/s/share"}); got != "stable-user" {
			t.Fatalf("unstable account %q", got)
		}
	}
}

func TestQuarkCurrentUserConfigContainsIdentity(t *testing.T) {
	data := object{"nickname": "shown-name", "avatarUri": "avatar", "mobilekps": "never-an-account-id", "config": object{"cloudDriveConfig": object{"paramInfo": object{"ucid": "stable-account", "pr": "ucpro"}}}}
	if quarkIdentityData(data) != "stable-account" {
		t.Fatal("current-user config identity missing")
	}
	delete(data, "config")
	if quarkIdentityData(data) != "" {
		t.Fatal("nickname or auth material treated as account identity")
	}
}
