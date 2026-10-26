package netdisk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

// Only the transport destination changes. All parsing, pagination, token
// lookup, request construction and public recipes use the real Quark provider.
type sessionCacheHTTPProvider struct {
	*quarkProvider
	server *httptest.Server
}

func (p *sessionCacheHTTPProvider) route(c *APIClient) {
	destination, _ := url.Parse(p.server.URL)
	c.Client.Transport = adapterTransport(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		u := *r.URL
		r.Host = u.Host
		u.Scheme, u.Host = destination.Scheme, destination.Host
		r.URL = &u
		return p.server.Client().Transport.RoundTrip(r)
	})
}

func (p *sessionCacheHTTPProvider) List(ctx context.Context, c *APIClient, ss *Session, src model.Source, cursor string) (*model.Listing, error) {
	p.route(c)
	return p.quarkProvider.List(ctx, c, ss, src, cursor)
}

func (p *sessionCacheHTTPProvider) Resolve(ctx context.Context, c *APIClient, ss *Session, src model.Source, f model.File) (*Download, error) {
	p.route(c)
	return p.quarkProvider.Resolve(ctx, c, ss, src, f)
}

type sessionCacheHTTPFixture struct {
	pages, tokens, downloads atomic.Int64
	generation               atomic.Int64
}

type sessionCacheStore struct{ data []byte }

func (s *sessionCacheStore) Load(value any) (bool, error) {
	if len(s.data) == 0 {
		return false, nil
	}
	return true, json.Unmarshal(s.data, value)
}
func (s *sessionCacheStore) Save(value any) (err error) {
	s.data, err = json.Marshal(value)
	return err
}
func (s *sessionCacheStore) Delete() error { s.data = nil; return nil }

func newSessionCacheHTTPFixture(t *testing.T) (*sessionCacheHTTPProvider, *sessionCacheHTTPFixture) {
	t.Helper()
	f := &sessionCacheHTTPFixture{}
	f.generation.Store(1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		generation := f.generation.Load()
		stoken := fmt.Sprintf("private-stoken-%d", generation)
		switch r.URL.Path {
		case "/1/clouddrive/share/sharepage/token":
			f.tokens.Add(1)
			var b map[string]any
			if json.NewDecoder(r.Body).Decode(&b) != nil || b["pwd_id"] != "share" {
				t.Error("share-token request lost source")
			}
			_ = json.NewEncoder(w).Encode(object{"code": 0, "data": object{"stoken": stoken}})
		case "/1/clouddrive/share/sharepage/detail":
			f.pages.Add(1)
			q := r.URL.Query()
			page, err := strconv.Atoi(q.Get("_page"))
			if err != nil || page < 1 || page > 3 || q.Get("_size") != "100" || q.Get("pwd_id") != "share" || q.Get("pdir_fid") != "folder" || q.Get("stoken") != stoken {
				t.Error("directory request lost page, parent or authorization")
				http.Error(w, "bad fixture request", http.StatusBadRequest)
				return
			}
			list := []object{}
			for i := (page-1)*100 + 1; i <= page*100 && i <= 201; i++ {
				list = append(list, object{"fid": strconv.Itoa(i), "file_name": fmt.Sprintf("%03d.bin", i), "file": true, "size": i,
					"share_fid_token": fmt.Sprintf("private-fid-%d-%d", generation, i)})
			}
			_ = json.NewEncoder(w).Encode(object{"code": 0, "data": object{"list": list}, "metadata": object{"_total": 201}})
		case "/1/clouddrive/chat/conv/file/acquire_dl_token":
			_ = json.NewEncoder(w).Encode(object{"code": 0, "data": object{"token": "private-social", "expired_timestamp": time.Now().Add(time.Hour).UnixMilli()}})
		case "/1/clouddrive/file/download":
			f.downloads.Add(1)
			var b map[string]any
			if json.NewDecoder(r.Body).Decode(&b) != nil {
				t.Error("invalid download request")
			}
			ids, fidtokens := items(b["fids"]), items(b["fids_token"])
			if len(ids) != 1 || len(fidtokens) != 1 || b["pwd_id"] != "share" || b["stoken"] != stoken || b["token"] != "private-social" {
				t.Error("download lost scoped authorization")
				http.Error(w, "bad fixture request", http.StatusBadRequest)
				return
			}
			id := str(ids[0])
			if str(fidtokens[0]) != fmt.Sprintf("private-fid-%d-%s", generation, id) {
				t.Error("download reused wrong token generation")
			}
			_ = json.NewEncoder(w).Encode(object{"code": 0, "data": []object{{"fid": id, "download_url": "https://cdn.quark.cn/" + id}}})
		default:
			t.Errorf("unexpected fixture endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &sessionCacheHTTPProvider{quarkProvider: newQuarkProvider(false), server: srv}, f
}

func sessionCacheCapture(t *testing.T, s *Service) *CaptureResult {
	t.Helper()
	result, err := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/share", AccountID: "account", InstanceID: "browser", ContextID: "tab"})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertSessionCachePublic(t *testing.T, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-", "stoken", "share_fid_token", "social_token"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("provider authorization escaped into public state: %s", secret)
		}
	}
}

func TestSessionCacheQuarkPrepareHTTP(t *testing.T) {
	for _, retain := range []bool{true, false} {
		t.Run(fmt.Sprintf("retain=%t", retain), func(t *testing.T) {
			p, fixture := newSessionCacheHTTPFixture(t)
			s := NewService(p)
			store := &sessionCacheStore{}
			s.SetStateStore(store)
			cap := sessionCacheCapture(t, s)
			req := &base.Request{URL: cap.Source.URL}
			prepared, err := s.Prepare(context.Background(), nil, req, cap.Source, cap.SessionID, []model.File{{ID: "folder", Name: "Folder", IsDir: true}}, nil)
			if err != nil || prepared == nil || len(prepared.Reqs) != 201 {
				t.Fatalf("Prepare: %v", err)
			}
			if fixture.pages.Load() != 3 || fixture.tokens.Load() != 1 || fixture.downloads.Load() != 0 {
				t.Fatal("Prepare must list three pages and resolve no links")
			}
			assertSessionCachePublic(t, prepared)
			var durable any
			if err := json.Unmarshal(store.data, &durable); err != nil {
				t.Fatal(err)
			}
			assertSessionCachePublic(t, durable)
			for _, index := range []int{0, 100, 200} {
				if !retain {
					// Regression control: losing the learned Capture exactly
					// reproduces the old 1 + 2 + 3 page rescans.
					s.mu.Lock()
					s.sessions[cap.SessionID].Capture.Share = nil
					s.mu.Unlock()
				}
				e := prepared.Reqs[index].Req.Extra.(*model.ReqExtra)
				d, err := s.Resolve(context.Background(), nil, req, e)
				if err != nil || d == nil || d.File.ID != strconv.Itoa(index+1) || d.File.Path != "Folder" {
					t.Fatalf("Resolve file %d: %v", index+1, err)
				}
				assertSessionCachePublic(t, e)
			}
			wantPages := int64(3)
			if !retain {
				wantPages = 9
			}
			if fixture.pages.Load() != wantPages || fixture.downloads.Load() != 3 {
				t.Fatalf("pages=%d want=%d downloads=%d", fixture.pages.Load(), wantPages, fixture.downloads.Load())
			}
			t.Logf("201 files: Prepare=3 pages; Resolve 1/101/201 adds %d pages", fixture.pages.Load()-3)
		})
	}
}

func TestSessionCacheQuarkListAndResolveHTTP(t *testing.T) {
	for _, method := range []string{"List", "Resolve"} {
		t.Run(method, func(t *testing.T) {
			p, fixture := newSessionCacheHTTPFixture(t)
			s := NewService(p)
			cap := sessionCacheCapture(t, s)
			src := cap.Source
			src.ParentID = "folder"
			req := &base.Request{URL: src.URL}
			e := &model.ReqExtra{Provider: "quark", SessionID: cap.SessionID, AccountID: "account", Source: src,
				File: model.File{ID: "101", Name: "101.bin", Meta: map[string]string{"parent_id": "folder"}}}
			if method == "List" {
				l, err := s.List(context.Background(), nil, req, src, cap.SessionID, "2")
				if err != nil {
					t.Fatal(err)
				}
				assertSessionCachePublic(t, l)
			} else if _, err := s.Resolve(context.Background(), nil, req, e); err != nil {
				t.Fatal(err)
			}
			pages := fixture.pages.Load()
			if _, err := s.Resolve(context.Background(), nil, req, e); err != nil {
				t.Fatal(err)
			}
			if fixture.pages.Load() != pages || fixture.tokens.Load() != 1 {
				t.Fatal("successful operation discarded learned authorization")
			}
		})
	}
}

func TestSessionCacheClearAndRestartRefreshHTTP(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			p, fixture := newSessionCacheHTTPFixture(t)
			s := NewService(p)
			cap := sessionCacheCapture(t, s)
			src := cap.Source
			src.ParentID = "folder"
			req := &base.Request{URL: src.URL}
			e := &model.ReqExtra{Provider: "quark", SessionID: cap.SessionID, AccountID: "account", Source: src, File: model.File{ID: "201", Name: "201.bin", Meta: map[string]string{"parent_id": "folder"}}}
			if _, err := s.Resolve(context.Background(), nil, req, e); err != nil {
				t.Fatal(err)
			}
			late, _ := s.session("quark", cap.SessionID, "account")
			s.Close()
			if restart {
				s = NewService(p)
			}
			s.rememberSessionCache(late)
			if len(s.sessions) != 0 {
				t.Fatal("late operation resurrected cleared session")
			}
			fixture.generation.Store(2)
			fresh := sessionCacheCapture(t, s)
			s.rememberSessionCache(late)
			if _, err := s.Resolve(context.Background(), nil, req, e); err != nil {
				t.Fatal("fresh same-account sync did not refresh authorization", err)
			}
			if fixture.pages.Load() != 6 || fixture.tokens.Load() != 2 {
				t.Fatal("fresh sync reused cleared credentials")
			}
			ss, _ := s.session("quark", fresh.SessionID, "account")
			if sessionValue(ss, "share_fid_token.201") != "private-fid-2-201" {
				t.Fatal("new sync did not retain its own token generation")
			}
			t.Log("clear/restart + fresh sync fetched a new stoken and three new pages")
		})
	}
}

func TestSessionCacheMergeIdentityAndWhitelist(t *testing.T) {
	for _, change := range []string{"none", "id", "created", "account", "provider", "source", "share", "password", "expired", "new_stoken", "operation_source"} {
		t.Run(change, func(t *testing.T) {
			s := NewService(newQuarkProvider(false))
			cap := sessionCacheCapture(t, s)
			ss, _ := s.session("quark", cap.SessionID, "account")
			ss.Capture.Share = map[string]string{"stoken": "learned-share", "share_fid_token.new": "learned-fid", "share_fid_token.": "invalid",
				"social_token": "unbounded-social", "social_token_expired_timestamp": "1", "Authorization": "unexpected", "share_token": "other-provider"}
			ss.Capture.Tokens = map[string]string{"access_token": "unexpected"}
			operation := cap.Source
			current := s.sessions[cap.SessionID]
			switch change {
			case "id":
				ss.ID = "different-session"
			case "created":
				current.CreatedAt = current.CreatedAt.Add(time.Second)
			case "account":
				current.Capture.AccountID = "other-account"
			case "provider":
				current.Capture.Provider = "uc"
			case "source":
				current.Capture.SourcePage = "https://pan.quark.cn/s/other"
			case "share":
				current.Capture.Share = map[string]string{"shareId": "other"}
			case "password":
				current.Capture.Share = map[string]string{"password": "other"}
			case "expired":
				current.CreatedAt = time.Now().Add(-9 * time.Hour)
				ss.CreatedAt = current.CreatedAt
			case "new_stoken":
				current.Capture.Share = map[string]string{"stoken": "new-capture-stoken"}
			case "operation_source":
				operation.ShareID = "other"
			}
			s.rememberSessionCache(ss, operation)
			got := current.Capture.Share["share_fid_token.new"]
			if (got == "learned-fid") != (change == "none") {
				t.Fatalf("merge crossed session identity boundary %s", change)
			}
			for _, key := range []string{"social_token", "social_token_expired_timestamp", "Authorization", "share_token", "share_fid_token."} {
				if current.Capture.Share[key] != "" {
					t.Fatalf("unapproved key merged: %s", key)
				}
			}
			if current.Capture.Tokens["access_token"] != "" {
				t.Fatal("whole token map replaced")
			}
		})
	}
}

func TestSessionCacheConcurrentMergeKeepsCapturedValues(t *testing.T) {
	s := NewService(newQuarkProvider(false))
	cap := sessionCacheCapture(t, s)
	current := s.sessions[cap.SessionID]
	current.Capture.Tokens = map[string]string{"stoken": "captured-stoken", "share_fid_token.kept": "captured-fid"}
	current.Capture.Files = []model.File{{ID: "browser", Name: "browser.bin", Meta: map[string]string{"share_fid_token": "browser-fid"}}}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ss, err := s.session("quark", cap.SessionID, "account")
			if err != nil {
				t.Error(err)
				return
			}
			remember(ss, "stoken", "captured-stoken")
			remember(ss, "share_fid_token.kept", "late-fid")
			remember(ss, "share_fid_token.browser", "late-browser-fid")
			remember(ss, "share_fid_token."+strconv.Itoa(i), "learned-fid")
			s.rememberSessionCache(ss)
		}(i)
	}
	wg.Wait()
	ss, _ := s.session("quark", cap.SessionID, "account")
	if shareFID(ss, "kept") != "captured-fid" || shareFID(ss, "browser") != "browser-fid" {
		t.Fatal("late operation replaced browser capture")
	}
	for i := 0; i < 64; i++ {
		if shareFID(ss, strconv.Itoa(i)) != "learned-fid" {
			t.Fatal("concurrent merge lost a disjoint learned key")
		}
	}
}

type sessionCacheFailedProvider struct {
	*quarkProvider
	invalidListing bool
}

func (p *sessionCacheFailedProvider) List(_ context.Context, _ *APIClient, ss *Session, _ model.Source, _ string) (*model.Listing, error) {
	remember(ss, "stoken", "partial-share")
	remember(ss, "share_fid_token.partial", "partial-fid")
	if p.invalidListing {
		return &model.Listing{Files: []model.File{{ID: "partial", Name: "../invalid"}}}, nil
	}
	return nil, Fail("network_error", "fixture failure")
}

func (p *sessionCacheFailedProvider) Resolve(_ context.Context, _ *APIClient, ss *Session, _ model.Source, _ model.File) (*Download, error) {
	remember(ss, "stoken", "partial-share")
	remember(ss, "share_fid_token.partial", "partial-fid")
	return nil, Fail("network_error", "fixture failure")
}

func TestSessionCacheFailedOperationsDoNotMerge(t *testing.T) {
	for _, method := range []string{"List", "invalid_List", "Prepare", "Resolve"} {
		t.Run(method, func(t *testing.T) {
			p := &sessionCacheFailedProvider{quarkProvider: newQuarkProvider(false), invalidListing: method == "invalid_List"}
			s := NewService(p)
			cap := sessionCacheCapture(t, s)
			req := &base.Request{URL: cap.Source.URL}
			var err error
			switch method {
			case "List", "invalid_List":
				_, err = s.List(context.Background(), nil, req, cap.Source, cap.SessionID, "")
			case "Prepare":
				_, err = s.Prepare(context.Background(), nil, req, cap.Source, cap.SessionID, []model.File{{ID: "folder", Name: "Folder", IsDir: true}}, nil)
			case "Resolve":
				_, err = s.Resolve(context.Background(), nil, req, &model.ReqExtra{Provider: "quark", Source: cap.Source, SessionID: cap.SessionID, File: model.File{ID: "partial", Name: "a.bin"}})
			}
			if err == nil {
				t.Fatal("fixture must fail")
			}
			ss, _ := s.session("quark", cap.SessionID, "account")
			if sessionValue(ss, "stoken", "share_fid_token.partial") != "" {
				t.Fatal("failed operation merged a partially learned Capture")
			}
		})
	}
}
