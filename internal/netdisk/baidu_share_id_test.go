package netdisk

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

// These fictitious links exercise Detect's canonical ID before the actual
// provider builds its wxlist request; no live share or credentials are used.
func TestBaiduShareListPreservesCanonicalID(t *testing.T) {
	cases := []struct {
		name, raw, shareID, parent, cursor, root, dir, page string
	}{
		{"public link", "https://pan.baidu.com/s/1fixture?pwd=Test", "1fixture", "", "", "1", "", "1"},
		{"init link", "https://pan.baidu.com/share/init?surl=fixture&pwd=Test", "1fixture", "0", "", "1", "", "1"},
		{"init suffix starts with one", "https://pan.baidu.com/share/init?surl=1fixture&pwd=Test", "11fixture", "", "", "1", "", "1"},
		{"child path", "https://yun.baidu.com/s/1fixture?pwd=Test", "1fixture", "/shared folder/子目录", "", "0", "/shared folder/子目录", "1"},
		{"cached child ID", "https://pan.baidu.com/s/1fixture?pwd=Test", "1fixture", "42", "", "0", "/shared folder/子目录", "1"},
		{"next page", "https://pan.baidu.com/s/1fixture?pwd=Test", "1fixture", "/shared folder/子目录", "2", "0", "/shared folder/子目录", "2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, err := NewService().Detect(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if src.ShareID != tc.shareID {
				t.Fatalf("canonical share ID = %q, want %q", src.ShareID, tc.shareID)
			}
			src.ParentID = tc.parent
			p := adapterProvider(t, "baidu")
			s := &Session{Capture: model.Capture{Share: map[string]string{"baidu_path.42": "/shared folder/子目录"}}}
			calls := 0
			c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
				calls++
				form := baiduShareListForm(t, r)
				if got := form.Get("shorturl"); got != tc.shareID {
					t.Errorf("wxlist shorturl = %q, want full canonical ID %q", got, tc.shareID)
					// Match the observed API failure when the leading 1 is lost.
					return adapterJSON(`{"errno":2}`), nil
				}
				want := url.Values{"shorturl": {tc.shareID}, "pwd": {"Test"}, "root": {tc.root}, "dir": {tc.dir}, "num": {"100"}, "order": {"time"}, "page": {tc.page}}
				if !reflect.DeepEqual(form, want) {
					t.Fatalf("wxlist form = %v, want %v", form, want)
				}
				return adapterJSON(`{"errno":0,"data":{"uk":123,"shareid":456,"seckey":"fixture-key","has_more":1,"list":[{"fs_id":9007199254740993,"server_filename":"folder","path":"/shared folder/子目录/folder","isdir":1}]}}`), nil
			})
			listing, err := p.List(context.Background(), c, s, src, tc.cursor)
			if err != nil {
				t.Fatalf("valid share listing failed: %v", err)
			}
			wantNext := "2"
			if tc.cursor == "2" {
				wantNext = "3"
			}
			if calls != 1 || len(listing.Files) != 1 || listing.Source != src || listing.NextPage != wantNext {
				t.Fatalf("unexpected listing: calls=%d, listing=%+v", calls, listing)
			}
			f := listing.Files[0]
			if f.ID != "9007199254740993" || f.Name != "folder" || !f.IsDir || f.Meta["remote_path"] != "/shared folder/子目录/folder" {
				t.Fatalf("unexpected directory: %+v", f)
			}
			if sessionValue(s, "uk") != "123" || sessionValue(s, "shareid") != "456" || sessionValue(s, "seckey") != "fixture-key" || sessionValue(s, "baidu_path."+f.ID) != f.Meta["remote_path"] {
				t.Fatal("share context or directory path was not retained")
			}
		})
	}
}

func TestBaiduShareListRediscoversChildAcrossPages(t *testing.T) {
	src, err := NewService().Detect("https://pan.baidu.com/share/init?surl=fixture&pwd=Test")
	if err != nil {
		t.Fatal(err)
	}
	src.ParentID = "42"
	p := adapterProvider(t, "baidu")
	s := &Session{}
	steps := []struct{ root, dir, page, response string }{
		{"1", "", "1", `{"errno":0,"data":{"has_more":1,"list":[]}}`},
		{"1", "", "2", `{"errno":0,"data":{"has_more":0,"list":[{"fs_id":42,"server_filename":"child","path":"/shared folder/child","isdir":1}]}}`},
		{"0", "/shared folder/child", "1", `{"errno":0,"data":{"has_more":0,"list":[{"fs_id":43,"server_filename":"file.txt","path":"/shared folder/child/file.txt","size":7,"isdir":0}]}}`},
	}
	calls := 0
	c := adapterClient(p, s, func(r *http.Request) (*http.Response, error) {
		if calls >= len(steps) {
			t.Fatal("unexpected extra directory discovery request")
		}
		step := steps[calls]
		calls++
		form := baiduShareListForm(t, r)
		if form.Get("shorturl") != "1fixture" {
			t.Errorf("discovery lost canonical share ID: %q", form.Get("shorturl"))
			return adapterJSON(`{"errno":2}`), nil
		}
		if form.Get("pwd") != "Test" || form.Get("root") != step.root || form.Get("dir") != step.dir || form.Get("page") != step.page {
			t.Fatalf("discovery step %d form = %v", calls, form)
		}
		return adapterJSON(step.response), nil
	})
	listing, err := p.List(context.Background(), c, s, src, "")
	if err != nil {
		t.Fatalf("child discovery failed: %v", err)
	}
	if calls != len(steps) || len(listing.Files) != 1 || listing.Files[0].ID != "43" || listing.Files[0].IsDir || listing.NextPage != "" || listing.Source != src {
		t.Fatalf("unexpected discovered child listing: calls=%d, listing=%+v", calls, listing)
	}
}

func baiduShareListForm(t *testing.T, r *http.Request) url.Values {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Scheme != "https" || r.URL.Host != "pan.baidu.com" || r.URL.Path != "/share/wxlist" {
		t.Fatalf("unexpected share request: %s %s", r.Method, r.URL)
	}
	wantQuery := url.Values{"channel": {"weixin"}, "version": {"2.2.2"}, "clienttype": {"25"}, "web": {"1"}}
	if !reflect.DeepEqual(r.URL.Query(), wantQuery) || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("unexpected share query or content type: %s, %q", r.URL.RawQuery, r.Header.Get("Content-Type"))
	}
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	return r.PostForm
}
