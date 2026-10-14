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
