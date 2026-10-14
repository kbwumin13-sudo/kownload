package netdisk

import (
	"context"
	"encoding/json"
	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"strings"
	"testing"
)

type testProvider struct {
	duplicate     bool
	needsTransfer bool
	resolved      []string
	cleaned       []string
	lists         int
}

func (*testProvider) Info() model.ProviderInfo {
	return model.ProviderInfo{ID: "quark", Name: "夸克", Hosts: []string{"pan.quark.cn"}, APIHosts: []string{"drive-pc.quark.cn"}}
}
func (p *testProvider) List(_ context.Context, _ *APIClient, _ *Session, src model.Source, cursor string) (*model.Listing, error) {
	p.lists++
	if cursor == "" {
		return &model.Listing{Files: []model.File{{ID: "A", Name: "first.bin", Size: 3}}, NextPage: "next"}, nil
	}
	return &model.Listing{Files: []model.File{{ID: "B", Name: "second.bin", Size: 4}}}, nil
}
func (p *testProvider) Resolve(_ context.Context, _ *APIClient, _ *Session, src model.Source, f model.File) (*Download, error) {
	p.resolved = append(p.resolved, f.ID)
	if p.needsTransfer && src.ShareID != "" {
		return nil, TransferRequired()
	}
	return &Download{URL: "https://cdn.quark.cn/" + f.ID, File: f}, nil
}
func (p *testProvider) Transfer(_ context.Context, _ *APIClient, _ *Session, src model.Source, fs []model.File, batch string) (*model.TransferReceipt, error) {
	out := clone(fs)
	for i := range out {
		out[i].ID = "copy-" + out[i].ID
		if p.duplicate {
			out[i].ID = "copy-A"
		}
	}
	return &model.TransferReceipt{CreatedIDs: []string{"temporary-folder"}, FolderID: "temporary-folder", Files: out}, nil
}
func TestCaptureCannotMaskDifferentShareWithPageData(t *testing.T) {
	s := NewService(&testProvider{})
	src, _ := s.Detect("https://pan.quark.cn/s/A")
	r, _ := s.RequestBrowser(src, "browser")
	claim, _ := s.Claim(src.URL, "browser", "chrome", "tab")
	_, e := s.Capture(r.ID, claim.Nonce, model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/B", InstanceID: "browser", ContextID: "tab", Share: map[string]string{"shareId": "A"}})
	if e == nil {
		t.Fatal("different actual share accepted")
	}
}
func TestUnpairedRequestNeedsCorrelatedBrowserLaunch(t *testing.T) {
	s := NewService(&testProvider{})
	src, _ := s.Detect("https://pan.quark.cn/s/A")
	r, _ := s.RequestBrowser(src, "")
	c, e := s.Claim(src.URL, "unselected-profile", "chrome", "old-tab")
	if e != nil || c != nil {
		t.Fatal("old page claimed unpaired request")
	}
	c, e = s.Claim(src.URL+"?gopeed_request="+r.ID, "selected-profile", "chrome", "new-tab", r.ID)
	if e != nil || c == nil {
		t.Fatal(e)
	}
	if strings.Contains(c.Request.Source.URL, "gopeed_request") {
		t.Fatal("handoff marker persisted")
	}
}
func TestDuplicateTransferFileIDsNeverEnableCleanup(t *testing.T) {
	p := &testProvider{needsTransfer: true, duplicate: true}
	s := NewService(p)
	cap := testCapture(t, s)
	_, e := s.Prepare(context.Background(), controller.NewController(), &base.Request{URL: cap.Source.URL}, cap.Source, cap.SessionID, []model.File{{ID: "A", Name: "a", Size: 3}, {ID: "B", Name: "b", Size: 4}}, nil)
	if e == nil {
		t.Fatal("duplicate transfer accepted")
	}
	if len(p.cleaned) > 0 {
		t.Fatal("bad batch cleaned")
	}
	if len(s.Transfers()) != 1 || s.Transfers()[0].State != "transfer_failed" {
		t.Fatal("bad transfer lost its recovery receipt")
	}
}
func (p *testProvider) Cleanup(_ context.Context, _ *APIClient, _ *Session, r *model.TransferReceipt) error {
	p.cleaned = append(p.cleaned, r.CreatedIDs...)
	return nil
}
func testCapture(t *testing.T, s *Service) *CaptureResult {
	t.Helper()
	r, e := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/share", InstanceID: "browser-one", ContextID: "tab-one", AccountID: "account-one", Tokens: map[string]string{"token": "private-auth"}, Files: []model.File{{ID: "A", Name: "a.bin", Size: 3, Meta: map[string]string{"share_fid_token": "private-file-auth", "etag": "valid"}}}, Cookies: []model.Cookie{{Name: "session", Value: "private-cookie", Domain: ".quark.cn", Path: "/", Secure: true}}})
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestBrowserClaimBoundToShareAndContext(t *testing.T) {
	s := NewService(&testProvider{})
	src, _ := s.Detect("https://pan.quark.cn/s/share")
	r, e := s.RequestBrowser(src, "browser-one")
	if e != nil {
		t.Fatal(e)
	}
	c, e := s.Claim(src.URL, "browser-two", "Chrome", "tab-one")
	if e != nil || c != nil {
		t.Fatalf("other profile claimed: %v %v", c, e)
	}
	c, e = s.Claim(src.URL, "browser-one", "Chrome", "tab-one")
	if e != nil || c == nil {
		t.Fatal(e)
	}
	cap := model.Capture{Provider: "quark", SourcePage: src.URL, InstanceID: "browser-one", ContextID: "tab-two"}
	if _, e = s.Capture(r.ID, c.Nonce, cap); e == nil {
		t.Fatal("other tab accepted")
	}
	cap.ContextID = "tab-one"
	if _, e = s.Capture(r.ID, "wrong", cap); e == nil {
		t.Fatal("wrong nonce accepted")
	}
	if _, e = s.Capture(r.ID, c.Nonce, cap); e != nil {
		t.Fatal(e)
	}
	status, _ := s.BrowserStatus(r.ID)
	if status.State != "ready" {
		t.Fatal(status)
	}
}
func TestCaptureRejectsForeignCookiesAndTraversal(t *testing.T) {
	s := NewService(&testProvider{})
	cap := model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/", InstanceID: "browser", ContextID: "tab", Cookies: []model.Cookie{{Name: "session", Domain: "foreign.example", Value: "secret"}}}
	if _, e := s.Capture("", "", cap); e == nil {
		t.Fatal("foreign cookie accepted")
	}
	cap.Cookies = nil
	cap.Files = []model.File{{ID: "A", Name: "a", Path: "../escape"}}
	if _, e := s.Capture("", "", cap); e == nil {
		t.Fatal("traversal accepted")
	}
}
func TestPreparePaginationKeepsDistinctFilesAndCachesLinks(t *testing.T) {
	p := &testProvider{}
	s := NewService(p)
	cap := testCapture(t, s)
	src := cap.Source
	req := &base.Request{URL: src.URL}
	out, e := s.Prepare(context.Background(), controller.NewController(), req, src, cap.SessionID, []model.File{{ID: "folder", Name: "Folder", IsDir: true}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Reqs) != 2 || p.lists != 2 {
		t.Fatal(out, p.lists)
	}
	for i, item := range out.Reqs {
		extra := item.Req.Extra.(*model.ReqExtra)
		if extra.File.Path != "Folder" {
			t.Fatal(extra.File)
		}
		d, e := s.Resolve(context.Background(), controller.NewController(), item.Req, extra)
		if e != nil {
			t.Fatal(e)
		}
		if !strings.HasSuffix(d.URL, []string{"A", "B"}[i]) {
			t.Fatal(d)
		}
	}
	if len(p.resolved) != 2 {
		t.Fatalf("links resolved twice: %v", p.resolved)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "private-") {
		t.Fatal("auth leaked into recipes")
	}
}
func TestTransferCleanupWaitsForWholeBatch(t *testing.T) {
	p := &testProvider{needsTransfer: true}
	s := NewService(p)
	cap := testCapture(t, s)
	req := &base.Request{URL: cap.Source.URL}
	out, e := s.Prepare(context.Background(), controller.NewController(), req, cap.Source, cap.SessionID, []model.File{{ID: "A", Name: "a", Size: 3}, {ID: "B", Name: "b", Size: 4}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Cleanup(context.Background(), controller.NewController(), req, out.BatchID); e == nil {
		t.Fatal("active batch cleaned")
	}
	s.Complete(context.Background(), controller.NewController(), req, out.Reqs[0].Req.Extra.(*model.ReqExtra))
	if len(p.cleaned) != 0 {
		t.Fatal("partial batch cleaned")
	}
	s.Complete(context.Background(), controller.NewController(), req, out.Reqs[1].Req.Extra.(*model.ReqExtra))
	if len(p.cleaned) != 1 || p.cleaned[0] != "temporary-folder" {
		t.Fatal(p.cleaned)
	}
	s.Complete(context.Background(), controller.NewController(), req, out.Reqs[1].Req.Extra.(*model.ReqExtra))
	if len(p.cleaned) != 1 {
		t.Fatal("duplicate cleanup")
	}
}

func TestClaimCannotReplaceAnotherTabButAllowsReload(t *testing.T) {
	s := NewService(&testProvider{})
	src, _ := s.Detect("https://pan.quark.cn/s/share")
	r, _ := s.RequestBrowser(src, "")
	a, err := s.Claim(src.URL, "browser", "Chrome", "1:0:doc1:0:store", r.ID)
	if err != nil || a == nil {
		t.Fatal(err)
	}
	b, err := s.Claim(src.URL, "browser", "Chrome", "2:0:doc2:0:store")
	if err != nil || b != nil {
		t.Fatal("another tab replaced owner", err)
	}
	reloaded, err := s.Claim(src.URL, "browser", "Chrome", "1:0:doc3:1:store")
	if err != nil || reloaded == nil || reloaded.Nonce == a.Nonce {
		t.Fatal("reload did not rotate document nonce", err)
	}
}

func TestSharedOriginalReopensExactShareWhileOwnedCopiesKeepAccount(t *testing.T) {
	s := NewService(&testProvider{})
	cap := testCapture(t, s)
	e := &model.ReqExtra{Provider: "quark", Source: cap.Source, SessionID: cap.SessionID, AccountID: "old-account", BatchID: "untransferred", File: model.File{ID: "file", Name: "file", Size: 3}}
	if got := s.sourceSession(e); got == nil {
		t.Fatal("exact shared original lost its fresh permission")
	}
	wrong := clone(e)
	wrong.Source.ShareID = "another-share"
	if s.sourceSession(wrong) != nil {
		t.Fatal("another share accepted")
	}
	own := clone(e)
	own.Source.ShareID = ""
	if s.sourceSession(own) != nil {
		t.Fatal("owned copy bypassed account check")
	}
	s.mu.Lock()
	s.transfers[e.BatchID] = &model.TransferReceipt{BatchID: e.BatchID}
	s.mu.Unlock()
	if s.sourceSession(e) != nil {
		t.Fatal("transfer ownership bypassed")
	}
}

func TestOriginalPrivateFileCanRecheckPermissionAfterCredentialRotation(t *testing.T) {
	s := NewService(&testProvider{})
	cap, err := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/list", InstanceID: "browser", ContextID: "tab", AccountID: "new-session-fingerprint"})
	if err != nil {
		t.Fatal(err)
	}
	e := &model.ReqExtra{Provider: "quark", Source: cap.Source, AccountID: "old-session-fingerprint", BatchID: "original-file", File: model.File{ID: "original", Name: "file", Size: 3}}
	if s.sourceSession(e) == nil {
		t.Fatal("fresh source could not recheck original file permission")
	}
	s.mu.Lock()
	s.transfers[e.BatchID] = &model.TransferReceipt{BatchID: e.BatchID}
	s.mu.Unlock()
	if s.sourceSession(e) != nil {
		t.Fatal("copied file bypassed account check")
	}
}
