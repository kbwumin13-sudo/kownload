package netdisk

import (
	"context"
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
