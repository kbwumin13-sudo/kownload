package netdisk

import (
	"net/url"
	"strings"
	"testing"

	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

func TestSourceURLIdentityFormats(t *testing.T) {
	s := NewService()
	cases := []struct {
		name, raw, provider, shareID, password string
	}{
		{"mobile fragment", "https://yun.139.com/shareweb/#/w/i/abc", "mcloud", "abc", ""},
		{"mobile alias and query password", "https://caiyun.139.com/shareweb/?password=A1b2#/w/i/abc", "mcloud", "abc", "A1b2"},
		{"mobile URL identity wins", "https://yun.139.com/shareweb/?pwd=A1b2&shareId=wrong#/w/i/abc", "mcloud", "abc", "A1b2"},
		{"mobile encoded identity", "https://yun.139.com/shareweb/?pass_code=A1b2#/w/i/%61bc", "mcloud", "abc", "A1b2"},
		{"baidu init", "https://pan.baidu.com/share/init?surl=abc", "baidu", "1abc", ""},
		{"baidu init alias", "https://yun.baidu.com/share/init/?surl=abc&pwd=A1b2", "baidu", "1abc", "A1b2"},
		{"baidu canonical", "https://pan.baidu.com/s/1abc#pwd=A1b2", "baidu", "1abc", "A1b2"},
		{"baidu surl starts with 1", "https://pan.baidu.com/share/init?surl=1abc", "baidu", "11abc", ""},
		{"baidu init query precedence", "https://pan.baidu.com/share/init?surl=abc&shareId=wrong&receive_code=A1b2", "baidu", "1abc", "A1b2"},
		{"baidu init missing surl", "https://pan.baidu.com/share/init", "baidu", "", ""},
		{"baidu init existing alias", "https://pan.baidu.com/share/init?shareid=1abc", "baidu", "1abc", ""},
		{"tcloud query", "https://cloud.189.cn/web/share?code=abc&password=A1b2", "tcloud", "abc", "A1b2"},
		{"aliyun", "https://www.alipan.com/s/abc?pass_code=A1b2", "aliyun", "abc", "A1b2"},
		{"xunlei", "https://pan.xunlei.com/s/abc?receive_code=A1b2", "xunlei", "abc", "A1b2"},
		{"quark", "https://pan.quark.cn/s/abc?pwd=A1b2", "quark", "abc", "A1b2"},
		{"uc", "https://drive.uc.cn/s/abc?password=A1b2", "uc", "abc", "A1b2"},
		{"123 alias", "https://www.123912.com/s/abc?pwd=A1b2", "123pan", "abc", "A1b2"},
		{"guangya", "https://www.guangyapan.com/s/abc?pwd=A1b2", "guangya", "abc", "A1b2"},
		{"mobile personal route", "https://yun.139.com/drive#/w/i/abc", "mcloud", "", ""},
		{"quark route unaffected", "https://pan.quark.cn/shareweb/#/w/i/abc", "quark", "", ""},
		{"other provider init unaffected", "https://pan.quark.cn/share/init?surl=abc", "quark", "init", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, err := s.Detect(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if src.Provider != tc.provider || src.ShareID != tc.shareID || src.Password != tc.password {
				t.Fatalf("identity = (%q, %q, %q), want (%q, %q, %q)", src.Provider, src.ShareID, src.Password, tc.provider, tc.shareID, tc.password)
			}
		})
	}
}

func TestSourceURLExistingQueryAliases(t *testing.T) {
	s := NewService()
	for _, shareKey := range []string{"shareId", "shareid", "linkID", "code"} {
		for _, passwordKey := range []string{"pwd", "password", "pass_code", "receive_code"} {
			t.Run(shareKey+"/"+passwordKey, func(t *testing.T) {
				q := url.Values{shareKey: {"abc"}, passwordKey: {"A1b2"}}
				src, err := s.Detect("https://pan.quark.cn/web?" + q.Encode())
				if err != nil || src.ShareID != "abc" || src.Password != "A1b2" {
					t.Fatalf("alias identity = %+v, err = %v", src, err)
				}
			})
		}
	}
	for _, key := range []string{"pwd", "password", "pass_code", "receive_code"} {
		src, err := s.Detect("https://yun.139.com/shareweb/?" + key + "=A1b2#/w/i/abc")
		if err != nil || src.ShareID != "abc" || src.Password != "A1b2" {
			t.Fatalf("mobile query alias %s = %+v, err = %v", key, src, err)
		}
	}
	src, err := s.Detect("https://pan.quark.cn/s/abc?pwd=A1b2&password=other&shareId=wrong&gopeed_request=public-marker#pwd=third")
	if err != nil || src.ShareID != "abc" || src.Password != "A1b2" || strings.Contains(src.URL, "gopeed_request") {
		t.Fatalf("URL precedence or marker removal = %+v, err = %v", src, err)
	}
}

func TestSourceURLAliasesClaimSameIdentity(t *testing.T) {
	cases := []struct{ expected, current string }{
		{"https://pan.baidu.com/share/init?surl=abc", "https://yun.baidu.com/s/1abc"},
		{"https://pan.baidu.com/s/1abc", "https://pan.baidu.com/share/init?surl=abc"},
		{"https://yun.139.com/shareweb/#/w/i/abc", "https://caiyun.139.com/shareweb/#/w/i/abc"},
		{"https://yun.139.com/shareweb/#/w/i/abc", "https://yun.139.com/s/abc"},
		{"https://cloud.189.cn/web/share?code=abc", "https://cloud.189.cn/t/abc"},
	}
	for _, tc := range cases {
		t.Run(tc.expected, func(t *testing.T) {
			s := NewService()
			src, err := s.Detect(tc.expected)
			if err != nil {
				t.Fatal(err)
			}
			request, err := s.RequestBrowser(src, "url-test-browser")
			if err != nil {
				t.Fatal(err)
			}
			claim, err := s.Claim(tc.current, "url-test-browser", "chrome", "url-test-tab")
			if err != nil || claim == nil || claim.Request.ID != request.ID {
				t.Fatalf("equivalent URL failed claim: %+v, %v", claim, err)
			}
		})
	}
}

func TestSourceURLDifferentSharesCannotClaim(t *testing.T) {
	for _, raw := range []string{"https://yun.139.com/shareweb/#/w/i/abc", "https://pan.baidu.com/share/init?surl=abc"} {
		s := NewService()
		src, err := s.Detect(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.RequestBrowser(src, "url-test-browser"); err != nil {
			t.Fatal(err)
		}
		claim, err := s.Claim(strings.Replace(raw, "abc", "other", 1), "url-test-browser", "chrome", "url-test-tab")
		if err != nil || claim != nil {
			t.Fatalf("different share claimed request: %+v, %v", claim, err)
		}
	}
}

func TestSourceURLProtectedDomains(t *testing.T) {
	s := NewService()
	for _, raw := range []string{
		"https://yun.139.com.evil.test/shareweb/#/w/i/abc",
		"https://evilyun.139.com/shareweb/#/w/i/abc",
		"https://pan.baidu.com.evil.test/share/init?surl=abc",
		"https://evilpan.baidu.com/share/init?surl=abc",
		"https://evil.test/?next=https://pan.baidu.com/share/init?surl=abc",
		"https://user:password@pan.baidu.com/share/init?surl=abc",
		"file://pan.baidu.com/share/init?surl=abc",
		"https://drive-pc.quark.cn/1/clouddrive/file/sort",
	} {
		if _, err := s.Detect(raw); err == nil {
			t.Fatalf("protected-domain URL accepted: %s", raw)
		}
	}
}

func TestSourceURLValidatePreservesTransferredPersonalIdentity(t *testing.T) {
	s := NewService()
	for _, raw := range []string{"https://yun.139.com/shareweb/#/w/i/abc", "https://pan.baidu.com/share/init?surl=abc"} {
		original := model.Source{URL: raw, ParentID: "own-copy-folder"}
		validated, err := s.ValidateSource(original)
		if err != nil || validated.ShareID != "" || validated.Password != "" || validated.ParentID != original.ParentID || validated.URL != raw {
			t.Fatalf("transferred personal identity changed: %+v, %v", validated, err)
		}
		if validated.Provider == "" {
			t.Fatal("provider was not validated")
		}
	}
}

func TestSourceURLPersonalHomeRedirectClaimAndCapture(t *testing.T) {
	for _, tc := range []struct{ entry, home string }{
		{"https://pan.quark.cn/", "https://pan.quark.cn/list?gopeed_request=marker#/list/all"},
		{"https://pan.quark.cn", "https://pan.quark.cn/list#/list/all"},
		{"https://drive.uc.cn/", "https://drive.uc.cn/list?gopeed_request=marker#/list/all"},
		{"https://www.alipan.com/drive/file/all", "https://alipan.com/drive/file/all?gopeed_request=marker"},
		{"https://yun.139.com/", "https://yun.139.com/w/#/index?gopeed_request=marker"},
		{"https://yun.139.com", "https://yun.139.com/w/#/index"},
		{"https://yun.139.com/", "https://yun.139.com/w/#/?gopeed_request=marker"},
		{"https://cloud.189.cn/web/main/", "https://cloud.189.cn/web/main/file/folder/-11?gopeed_request=marker"},
	} {
		t.Run(tc.entry, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				entry, home := tc.entry, tc.home
				if reverse {
					entry, home = home, entry
				}
				s := NewService()
				src, err := s.Detect(entry)
				if err != nil {
					t.Fatal(err)
				}
				request, err := s.RequestBrowser(src, "")
				if err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"", "wrong-request"} {
					claim, err := s.Claim(home, "browser", "chrome", "7:0:doc:0:store", id)
					if err != nil || claim != nil {
						t.Fatalf("unbound request claimed without its ID: %+v, %v", claim, err)
					}
				}
				claim, err := s.Claim(home, "browser", "chrome", "7:0:doc:0:store", request.ID)
				if err != nil || claim == nil || claim.Request.ID != request.ID {
					t.Fatalf("home redirect failed claim: %+v, %v", claim, err)
				}
				unknownPath, _ := url.Parse(entry)
				unknownPath.Path, unknownPath.RawQuery, unknownPath.Fragment = "/unknown", "", ""
				unknownRoute, _ := url.Parse(home)
				unknownRoute.Fragment = "/unknown"
				for _, page := range []string{unknownPath.String(), unknownRoute.String()} {
					badRequest, err := s.RequestBrowser(src, "browser")
					if err != nil {
						t.Fatal(err)
					}
					badClaim, err := s.Claim(home, "browser", "chrome", "9:0:bad-doc:0:store", badRequest.ID)
					if err != nil || badClaim == nil {
						t.Fatalf("failed to claim boundary fixture: %v", err)
					}
					if _, err := s.Capture(badRequest.ID, badClaim.Nonce, model.Capture{Provider: src.Provider, SourcePage: page, InstanceID: "browser", ContextID: "9:0:bad-doc:0:store"}); err == nil {
						t.Fatalf("unknown page captured home request: %s", page)
					}
					s.CancelBrowser(badRequest.ID)
				}
				for _, context := range []struct{ instance, tab string }{
					{"browser", "8:0:doc:0:store"},
					{"other-browser", "7:0:doc:0:store"},
					{"browser", "7:0:doc:0:other-store"},
				} {
					other, err := s.Claim(entry, context.instance, "chrome", context.tab, request.ID)
					if err != nil || other != nil {
						t.Fatalf("another context took home request: %+v, %v", other, err)
					}
					if _, err := s.Capture(request.ID, claim.Nonce, model.Capture{Provider: src.Provider, SourcePage: home, InstanceID: context.instance, ContextID: context.tab}); err == nil {
						t.Fatal("another context captured home request")
					}
				}
				reloaded, err := s.Claim(home, "browser", "chrome", "7:0:new-doc:1:store", request.ID)
				if err != nil || reloaded == nil || reloaded.Nonce == claim.Nonce {
					t.Fatalf("same-tab reload failed to renew claim: %+v, %v", reloaded, err)
				}
				capture := model.Capture{Provider: src.Provider, SourcePage: home, InstanceID: "browser", ContextID: "7:0:new-doc:1:store"}
				if _, err := s.Capture(request.ID, claim.Nonce, capture); err == nil {
					t.Fatal("stale nonce captured home request")
				}
				result, err := s.Capture(request.ID, reloaded.Nonce, capture)
				if err != nil || result.SessionID == "" {
					t.Fatalf("home redirect failed capture: %+v, %v", result, err)
				}
				status, err := s.BrowserStatus(request.ID)
				if err != nil || status.State != "ready" {
					t.Fatalf("home request did not leave waiting: %+v, %v", status, err)
				}
			}
		})
	}
}

func TestSourceURLPersonalHomeBoundaries(t *testing.T) {
	for _, tc := range []struct{ expected, current string }{
		{"https://pan.quark.cn/", "https://pan.quark.cn/unknown"},
		{"https://pan.quark.cn/", "https://pan.quark.cn/list/unknown#/list/all"},
		{"https://pan.quark.cn/", "https://pan.quark.cn/list#/list/share"},
		{"https://pan.quark.cn/list#/list/all", "https://pan.quark.cn/list#/unknown"},
		{"https://pan.quark.cn/", "https://pan.quark.cn/s/abc"},
		{"https://pan.quark.cn/s/abc", "https://pan.quark.cn/s/other"},
		{"https://yun.139.com/", "https://yun.139.com/unknown"},
		{"https://yun.139.com/", "https://yun.139.com/w/#/unknown"},
		{"https://yun.139.com/w/#/index", "https://yun.139.com/w/#/w/i/abc"},
		{"https://yun.139.com/", "https://yun.139.com/shareweb/#/w/i/abc"},
		{"https://yun.139.com/shareweb/#/w/i/abc", "https://yun.139.com/shareweb/#/w/i/other"},
		{"https://pan.quark.cn/", "https://yun.139.com/w/#/index"},
		{"https://pan.quark.cn/", "https://drive.uc.cn/list#/list/all"},
		{"https://drive.uc.cn/", "https://drive.uc.cn/unknown"},
		{"https://drive.uc.cn/list#/list/all", "https://drive.uc.cn/list#/unknown"},
		{"https://drive.uc.cn/s/abc", "https://drive.uc.cn/s/other"},
		{"https://www.alipan.com/drive/file/all", "https://alipan.com/unknown"},
		{"https://www.alipan.com/unknown", "https://alipan.com/unknown"},
		{"https://www.alipan.com/drive/file/all", "https://other.alipan.com/drive/file/all"},
		{"https://www.alipan.com/drive/file/all", "https://api.alipan.com/drive/file/all"},
		{"https://www.alipan.com/drive/file/all", "https://www.aliyundrive.com/drive/file/all"},
		{"https://www.alipan.com/drive/file/all", "https://alipan.com/drive/file/all#/unknown"},
		{"https://www.alipan.com/s/abc", "https://alipan.com/s/other"},
	} {
		t.Run(tc.expected+" to "+tc.current, func(t *testing.T) {
			s := NewService()
			src, err := s.Detect(tc.expected)
			if err != nil {
				t.Fatal(err)
			}
			request, err := s.RequestBrowser(src, "browser")
			if err != nil {
				t.Fatal(err)
			}
			claim, err := s.Claim(tc.current, "browser", "chrome", "tab", request.ID)
			if err != nil || claim != nil {
				t.Fatalf("unrelated page claimed home request: %+v, %v", claim, err)
			}
		})
	}
}
