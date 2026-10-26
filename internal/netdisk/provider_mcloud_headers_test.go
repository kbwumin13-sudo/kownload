package netdisk

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

// Public contract checked on 2026-10-15:
// https://yun.139.com/w/static/js/app.56b4a805.js (request interceptors and
// personal getDisk -> /file/list converter), together with the fixed
// artifacts/linkswift-2026-10-14/upstream/LinkSwift-stable.user.js.
// The official error table calls 04000003 invalid headers; a synthetic-auth
// request reproduces that code without X-Yun-Client-Info, and passes that
// validation when only this header is added. No real session is used here.
func TestMcloudPersonalListWebHeaders(t *testing.T) {
	for _, tc := range []struct {
		name, parent, cursor string
	}{
		{name: "root"},
		{name: "nested", parent: "folder-id"},
		{name: "next page", parent: "folder-id", cursor: "cursor+/=next"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := adapterProvider(t, "mcloud")
			auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("pc:fixture-account:fixture-token"))
			s := &Session{Capture: model.Capture{Headers: map[string]string{"Authorization": auth}}}
			calls := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodPost || r.URL.String() != "https://personal-kd-njs.yun.139.com/hcy/file/list" {
					t.Fatalf("unexpected list endpoint: %s %s", r.Method, r.URL)
				}
				// These are web protocol fields, independent of the user's token.
				for k, want := range map[string]string{
					"Content-Type":         "application/json;charset=UTF-8",
					"X-Huawei-ChannelSrc":  "10000034",
					"X-Inner-Ntwk":         "2",
					"X-Yun-Client-Info":    "||9|7.17.0|edge||||windows 10||zh-CN|||||",
					"X-Yun-Module-Type":    "100",
					"X-Yun-Svc-Type":       "1",
					"X-Yun-Api-Version":    "v1",
					"X-Yun-App-Channel":    "10000034",
					"X-Yun-Channel-Source": "10000034",
				} {
					if r.Header.Get(k) != want {
						t.Errorf("incorrect web header %s", k)
					}
				}
				if r.Header.Get("Authorization") != auth {
					t.Error("captured Basic authorization changed")
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				body, err := decodeObject(raw)
				if err != nil {
					t.Fatal(err)
				}
				parent := tc.parent
				if parent == "" {
					parent = "/"
				}
				want := object{
					"parentFileId": parent, "orderBy": "updated_at", "orderDirection": "DESC",
					"pageInfo":                object{"pageCursor": tc.cursor, "pageSize": 100},
					"imageThumbnailStyleList": []string{"Small", "Large"},
				}
				// Compare decoded objects so the fixture does not depend on key order.
				if body["parentFileId"] != want["parentFileId"] || body["orderBy"] != want["orderBy"] || body["orderDirection"] != want["orderDirection"] ||
					at(body, "pageInfo", "pageCursor") != tc.cursor || num(at(body, "pageInfo", "pageSize")) != 100 ||
					!reflect.DeepEqual(body["imageThumbnailStyleList"], []any{"Small", "Large"}) || len(body) != len(want) {
					t.Error("personal list body differs from the web contract")
				}
				sign := strings.Split(r.Header.Get("Mcloud-Sign"), ",")
				if len(sign) != 3 || len(sign[1]) != 16 || sign[2] != mcloudSign(raw, sign[0], sign[1]) {
					t.Error("signature does not cover the transmitted list body")
				}
				return adapterJSON(`{"code":"0000","data":{"items":[{"fileId":"dir","name":"文件夹","type":"folder"},{"fileId":"9007199254740993","name":"a.bin","type":"file","size":42}],"nextPageCursor":"next"}}`), nil
			})
			listing, err := p.List(context.Background(), c, s, model.Source{Provider: "mcloud", ParentID: tc.parent}, tc.cursor)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(listing.Files) != 2 || !listing.Files[0].IsDir || listing.Files[1].ID != "9007199254740993" || listing.Files[1].Size != 42 || listing.NextPage != "next" {
				t.Fatal("list response lost file identity or pagination")
			}
		})
	}
}

func TestMcloudPersonalAuthorization(t *testing.T) {
	p := adapterProvider(t, "mcloud")
	for _, tc := range []struct {
		name    string
		capture model.Capture
		want    string
	}{
		{"header", model.Capture{Headers: map[string]string{"authorization": "Basic fixture"}}, "Basic fixture"},
		{"bare token", model.Capture{Tokens: map[string]string{"authorization": "fixture"}}, "Basic fixture"},
		{"cookie", model.Capture{Cookies: []model.Cookie{{Name: "authorization", Value: "Basic fixture"}}}, "Basic fixture"},
		{"missing", model.Capture{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{Capture: tc.capture}
			calls := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("Authorization") != tc.want {
					t.Error("incorrect Basic authorization")
				}
				return adapterJSON(`{"code":"0000","data":{"items":[]}}`), nil
			})
			_, err := p.List(context.Background(), c, s, model.Source{Provider: "mcloud"}, "")
			if tc.want == "" {
				adapterError(t, err, "login_required")
				if calls != 0 {
					t.Error("sent personal request without authorization")
				}
			} else if err != nil || calls != 1 {
				t.Fatalf("personal request failed: %v", err)
			}
		})
	}
}

func TestMcloudConcurrentPersonalList(t *testing.T) {
	p := adapterProvider(t, "mcloud")
	s := &Session{Capture: model.Capture{Headers: map[string]string{"Authorization": "Basic fixture"}}}
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Yun-Client-Info") == "" {
			return adapterJSON(`{"success":false,"code":"04000003"}`), nil
		}
		return adapterJSON(`{"code":"0000","data":{"items":[]}}`), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.List(context.Background(), c, s, model.Source{Provider: "mcloud"}, ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(s.Capture.Headers) != 1 {
		t.Error("request headers mutated the captured session")
	}
}
