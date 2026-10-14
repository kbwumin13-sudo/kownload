package netdisk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

type pending struct {
	Request   model.BrowserRequest
	Nonce     string
	ContextID string
}
type Service struct {
	mu        sync.Mutex
	providers map[string]Provider
	sessions  map[string]*Session
	requests  map[string]*pending
	transfers map[string]*model.TransferReceipt
	downloads map[string]*Download
	state     fetcher.ProtocolStateStore
}

func NewService(providers ...Provider) *Service {
	if len(providers) == 0 {
		providers = BuiltinProviders()
	}
	s := &Service{providers: map[string]Provider{}, sessions: map[string]*Session{}, requests: map[string]*pending{}, transfers: map[string]*model.TransferReceipt{}, downloads: map[string]*Download{}}
	for _, p := range providers {
		s.providers[p.Info().ID] = p
	}
	return s
}
func randomID() string {
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func clone[T any](v T) T { b, _ := json.Marshal(v); var c T; _ = json.Unmarshal(b, &c); return c }
func (s *Service) Infos() []model.ProviderInfo {
	ids := []string{"baidu", "aliyun", "mcloud", "tcloud", "xunlei", "quark", "uc", "123pan", "guangya"}
	out := []model.ProviderInfo{}
	for _, id := range ids {
		if p := s.providers[id]; p != nil {
			out = append(out, p.Info())
		}
	}
	return out
}
func (s *Service) Detect(raw string) (model.Source, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Scheme != "https" && u.Scheme != "http" {
		return model.Source{}, Fail("unsupported", "请输入有效的网盘分享链接")
	}
	for _, info := range s.Infos() {
		for _, h := range info.Hosts {
			if HostMatches(u.Hostname(), h) {
				q := u.Query()
				q.Del("gopeed_request")
				u.RawQuery = q.Encode()
				src := model.Source{Provider: info.ID, URL: u.String()}
				fragment, _ := url.Parse(u.EscapedFragment())
				parts := strings.Split(strings.Trim(u.Path, "/"), "/")
				for i, v := range parts {
					if (v == "s" || v == "share" || v == "t") && i+1 < len(parts) {
						src.ShareID = parts[i+1]
						break
					}
				}
				if info.ID == "baidu" && strings.Trim(u.Path, "/") == "share/init" {
					// surl omits the leading 1 used by the public /s/ link.
					src.ShareID = ""
					if v := q.Get("surl"); v != "" {
						src.ShareID = "1" + v
					}
				}
				if info.ID == "mcloud" && strings.Trim(u.Path, "/") == "shareweb" && fragment != nil {
					parts := strings.Split(strings.Trim(fragment.Path, "/"), "/")
					if len(parts) >= 3 && parts[0] == "w" && parts[1] == "i" {
						src.ShareID = parts[2]
					}
				}
				if src.ShareID == "" {
					for _, k := range []string{"shareId", "shareid", "linkID", "code"} {
						if v := u.Query().Get(k); v != "" {
							src.ShareID = v
							break
						}
					}
				}
				for _, k := range []string{"pwd", "password", "pass_code", "receive_code"} {
					if v := u.Query().Get(k); v != "" {
						src.Password = v
						break
					}
				}
				if src.Password == "" && strings.HasPrefix(u.EscapedFragment(), "pwd=") {
					fq, _ := url.ParseQuery(u.EscapedFragment())
					src.Password = fq.Get("pwd")
				}
				return src, nil
			}
		}
	}
	return model.Source{}, Fail("unsupported", "该链接不属于内置支持的九类网盘")
}
func (s *Service) ValidateSource(src model.Source) (model.Source, error) {
	detected, err := s.Detect(src.URL)
	if err != nil {
		return src, err
	}
	if src.Provider != "" && src.Provider != detected.Provider {
		return src, Fail("invalid_source", "网盘类型与来源页面不一致")
	}
	src.Provider = detected.Provider
	return src, nil
}
func sameSource(a, b model.Source) bool {
	if a.Provider != b.Provider {
		return false
	}
	if a.ShareID != "" || b.ShareID != "" {
		return a.ShareID == b.ShareID
	}
	x, e1 := url.Parse(a.URL)
	y, e2 := url.Parse(b.URL)
	return e1 == nil && e2 == nil && x.Host == y.Host && x.Path == y.Path
}
func (s *Service) RequestBrowser(src model.Source, instance string) (*model.BrowserRequest, error) {
	if src.ShareID == "" {
		if detected, e := s.Detect(src.URL); e == nil {
			src.ShareID = detected.ShareID
			if src.Password == "" {
				src.Password = detected.Password
			}
		}
	}
	var err error
	src, err = s.ValidateSource(src)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.requests {
		if time.Now().After(p.Request.ExpiresAt) {
			delete(s.requests, id)
		}
	}
	if len(s.requests) >= 64 {
		return nil, Fail("busy", "待同步的网盘页面过多，请完成或取消已有请求")
	}
	r := model.BrowserRequest{ID: randomID(), Source: src, InstanceID: instance, State: "waiting", ExpiresAt: time.Now().Add(10 * time.Minute)}
	s.requests[r.ID] = &pending{Request: r}
	c := clone(r)
	return &c, nil
}
func (s *Service) BrowserStatus(id string) (*model.BrowserRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.requests[id]
	if p == nil {
		return nil, Fail("expired", "页面同步请求不存在或已取消")
	}
	if time.Now().After(p.Request.ExpiresAt) && p.Request.State != "ready" {
		p.Request.State = "expired"
		p.Request.Message = "页面同步已超时，请重试"
	}
	r := clone(p.Request)
	return &r, nil
}
func (s *Service) CancelBrowser(id string) { s.mu.Lock(); delete(s.requests, id); s.mu.Unlock() }

type ClaimResult struct {
	Request model.BrowserRequest `json:"request"`
	Nonce   string               `json:"nonce"`
}

func (s *Service) Claim(raw, instance, browser, contextID string, requestID ...string) (*ClaimResult, error) {
	src, err := s.Detect(raw)
	if err != nil {
		return nil, err
	}
	if instance == "" || contextID == "" {
		return nil, Fail("invalid_context", "无法确认浏览器窗口")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.requests {
		if len(requestID) > 0 && requestID[0] != "" {
			if requestID[0] != id {
				continue
			}
		} else if p.Request.InstanceID == "" {
			continue
		}
		if time.Now().After(p.Request.ExpiresAt) || p.Request.State == "ready" || !sameSource(src, p.Request.Source) {
			continue
		}
		if p.Request.InstanceID != "" && p.Request.InstanceID != instance {
			continue
		}
		if p.ContextID != "" && p.ContextID != contextID {
			if !sameBrowserTab(p.ContextID, contextID) {
				continue
			}
			p.Nonce = randomID()
		}
		p.Request.InstanceID = instance
		p.ContextID = contextID
		p.Request.State = "capturing"
		if p.Nonce == "" {
			p.Nonce = randomID()
		}
		return &ClaimResult{clone(p.Request), p.Nonce}, nil
	}
	return nil, nil
}

// A page reload may replace its document, but another tab cannot take ownership.
func sameBrowserTab(a, b string) bool {
	x, y := strings.Split(a, ":"), strings.Split(b, ":")
	return len(x) == 5 && len(y) == 5 && x[0] == y[0] && x[1] == y[1] && x[4] == y[4]
}

type CaptureResult struct {
	SessionID string       `json:"sessionId"`
	Source    model.Source `json:"source"`
	Files     []model.File `json:"files"`
}

// CaptureContext uses the official current-user response where a browser page
// does not expose a stable UID. Transient cookies cannot identify an account.
func (s *Service) CaptureContext(ctx context.Context, ctl *controller.Controller, requestID, nonce string, c model.Capture) (*CaptureResult, error) {
	src, err := s.Detect(c.SourcePage)
	if err != nil {
		return nil, err
	}
	if c.Provider == "quark" && len(c.Cookies) > 0 {
		p := s.providers[c.Provider]
		client := NewAPIClient(ctl, &base.Request{URL: src.URL}, p.Info(), &Session{Capture: c})
		authCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		if account := quarkAccountID(authCtx, client, src); account != "" {
			c.AccountID = account
		}
		cancel()
	}
	return s.Capture(requestID, nonce, c)
}

func quarkAccountID(ctx context.Context, client *APIClient, src model.Source) string {
	info, err := apiJSON(ctx, client, "GET", "https://pan.quark.cn/account/info", nil, requestHeaders(nil, src))
	if err != nil || !yes(info["success"]) {
		return ""
	}
	return quarkIdentityData(obj(info["data"]))
}
func quarkIdentityData(data object) string {
	if id := first(data, "ucid", "uid", "user_id", "userId"); id != "" {
		return id
	}
	return first(obj(at(data, "config", "cloudDriveConfig", "paramInfo")), "ucid")
}

func (s *Service) Capture(requestID, nonce string, c model.Capture) (*CaptureResult, error) {
	src, err := s.Detect(c.SourcePage)
	if err != nil {
		return nil, err
	}
	if c.Provider != src.Provider || c.InstanceID == "" || c.ContextID == "" {
		return nil, Fail("invalid_context", "网盘页面上下文无效")
	}
	actualSource := src
	if len(c.Files) > 10000 || len(c.Cookies) > 512 || len(c.Tokens) > 64 {
		return nil, Fail("invalid_context", "网盘页面数据超出范围")
	}
	for _, f := range c.Files {
		if err := validateFile(f); err != nil {
			return nil, err
		}
	}
	if v := c.Share["shareId"]; v != "" {
		src.ShareID = v
	}
	if v := c.Share["password"]; v != "" {
		src.Password = v
	}
	if v := c.Share["parentId"]; v != "" {
		src.ParentID = v
	}
	info := s.providers[src.Provider].Info()
	hosts := append(append([]string{}, info.Hosts...), info.APIHosts...)
	for _, cookie := range c.Cookies {
		ok := false
		for _, h := range hosts {
			if HostMatches(h, cookie.Domain) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, Fail("invalid_context", "登录信息与网盘域名不一致")
		}
	}
	delete(c.Headers, "Cookie")
	delete(c.Headers, "cookie")
	s.mu.Lock()
	defer s.mu.Unlock()
	if requestID != "" {
		p := s.requests[requestID]
		if p == nil || p.Nonce == "" || p.Nonce != nonce || p.ContextID != c.ContextID || p.Request.InstanceID != c.InstanceID || time.Now().After(p.Request.ExpiresAt) || !sameSource(actualSource, p.Request.Source) || !sameSource(src, p.Request.Source) {
			return nil, Fail("expired", "来源页面已变化，请重新同步")
		}
		if p.Request.State == "ready" {
			ss := s.sessions[p.Request.SessionID]
			if ss != nil {
				return &CaptureResult{SessionID: ss.ID, Source: src, Files: publicFiles(ss.Capture.Files)}, nil
			}
		}
	}
	for id, ss := range s.sessions {
		if time.Since(ss.CreatedAt) > 8*time.Hour {
			delete(s.sessions, id)
		}
	}
	if len(s.sessions) >= 128 {
		return nil, Fail("busy", "浏览器会话过多，请重启后重新同步")
	}
	if c.AccountID == "" {
		b, _ := json.Marshal([]any{c.InstanceID, c.Cookies, c.Tokens})
		h := sha256.Sum256(b)
		c.AccountID = hex.EncodeToString(h[:16])
	}
	ss := &Session{ID: randomID(), Capture: clone(c), CreatedAt: time.Now()}
	s.sessions[ss.ID] = ss
	if requestID != "" {
		p := s.requests[requestID]
		p.Request.State = "ready"
		p.Request.SessionID = ss.ID
	}
	return &CaptureResult{SessionID: ss.ID, Source: src, Files: publicFiles(c.Files)}, nil
}
func validateFile(f model.File) error {
	if f.ID == "" || f.Name == "" || f.Size < 0 || strings.ContainsAny(f.Name, "/\\\x00") || f.Name == "." || f.Name == ".." {
		return Fail("invalid_file", "网盘返回了无效的文件信息")
	}
	if strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "\\") {
		return Fail("invalid_path", "文件相对路径无效")
	}
	for _, p := range strings.Split(f.Path, "/") {
		if p == ".." || strings.ContainsRune(p, 0) {
			return Fail("invalid_path", "文件相对路径无效")
		}
	}
	return nil
}
func publicFiles(files []model.File) []model.File {
	out := clone(files)
	for i := range out {
		for k, v := range out[i].Meta {
			lower := strings.ToLower(k)
			if strings.Contains(lower, "token") || strings.Contains(lower, "cookie") || strings.Contains(lower, "authorization") || strings.Contains(lower, "url") || strings.Contains(v, "https://") || strings.Contains(v, "http://") {
				delete(out[i].Meta, k)
			}
		}
	}
	return out
}
func (s *Service) session(provider, id, account string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss := s.sessions[id]; ss != nil && ss.Capture.Provider == provider && (account == "" || ss.Capture.AccountID == account) && time.Since(ss.CreatedAt) < 8*time.Hour {
		return clone(ss), nil
	}
	if account != "" {
		var newest *Session
		for _, ss := range s.sessions {
			if ss.Capture.Provider == provider && ss.Capture.AccountID == account && time.Since(ss.CreatedAt) < 8*time.Hour && (newest == nil || ss.CreatedAt.After(newest.CreatedAt)) {
				newest = ss
			}
		}
		if newest != nil {
			return clone(newest), nil
		}
	}
	return nil, LoginRequired(provider)
}

// Original files are identified by source and file IDs. The provider verifies
// permission to that exact file; transferred copies retain their account check.
func (s *Service) sourceSession(e *model.ReqExtra) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transfers[e.BatchID] != nil {
		return nil
	}
	var newest *Session
	for _, ss := range s.sessions {
		if ss.Capture.Provider != e.Provider || time.Since(ss.CreatedAt) >= 8*time.Hour {
			continue
		}
		src, err := s.Detect(ss.Capture.SourcePage)
		if err != nil {
			continue
		}
		if share := ss.Capture.Share["shareId"]; share != "" {
			src.ShareID = share
		}
		if !sameSource(src, e.Source) {
			continue
		}
		if newest == nil || ss.CreatedAt.After(newest.CreatedAt) {
			newest = ss
		}
	}
	if newest == nil {
		return nil
	}
	return clone(newest)
}

func (s *Service) Resolve(ctx context.Context, ctl *controller.Controller, req *base.Request, e *model.ReqExtra) (*Download, error) {
	if err := validateFile(e.File); err != nil {
		return nil, err
	}
	source, err := s.ValidateSource(e.Source)
	if err != nil {
		return nil, err
	}
	if source.Provider != e.Provider {
		return nil, Fail("invalid_source", "网盘类型与任务不匹配")
	}
	p := s.providers[e.Provider]
	if p == nil {
		return nil, Fail("unsupported", "不支持的网盘类型")
	}
	ss, err := s.session(e.Provider, e.SessionID, e.AccountID)
	if err != nil {
		ss = s.sourceSession(e)
		if ss != nil {
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	key := e.BatchID + ":" + e.File.ID
	cached := s.downloads[key]
	delete(s.downloads, key)
	s.mu.Unlock()
	if cached != nil && (cached.ExpiresAt.IsZero() || time.Now().Before(cached.ExpiresAt)) {
		return cached, nil
	}
	return p.Resolve(ctx, NewAPIClient(ctl, req, p.Info(), ss), ss, e.Source, e.File)
}
func (s *Service) List(ctx context.Context, ctl *controller.Controller, req *base.Request, src model.Source, sessionID, cursor string) (*model.Listing, error) {
	var err error
	src, err = s.ValidateSource(src)
	if err != nil {
		return nil, err
	}
	ss, err := s.session(src.Provider, sessionID, "")
	if err != nil {
		return nil, err
	}
	p := s.providers[src.Provider]
	l, err := p.List(ctx, NewAPIClient(ctl, req, p.Info(), ss), ss, src, cursor)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, Fail("invalid_response", "网盘没有返回文件列表")
	}
	for _, f := range l.Files {
		if err := validateFile(f); err != nil {
			return nil, err
		}
	}
	l.Files = publicFiles(l.Files)
	l.SessionID = ss.ID
	l.Source = src
	return l, nil
}

type Prepared struct {
	Reqs    []*base.CreateTaskBatchItem `json:"reqs"`
	BatchID string                      `json:"batchId"`
}

func (s *Service) Prepare(ctx context.Context, ctl *controller.Controller, req *base.Request, src model.Source, sessionID string, selected []model.File, opts *base.Options) (*Prepared, error) {
	var err error
	src, err = s.ValidateSource(src)
	if err != nil {
		return nil, err
	}
	ss, err := s.session(src.Provider, sessionID, "")
	if err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, Fail("empty_selection", "请先选择要下载的文件")
	}
	p := s.providers[src.Provider]
	client := NewAPIClient(ctl, req, p.Info(), ss)
	files := []model.File{}
	seen := map[string]bool{}
	var walk func(model.File, int) error
	walk = func(f model.File, depth int) error {
		if err := validateFile(f); err != nil {
			return err
		}
		if depth > 64 || len(files) >= 10000 {
			return Fail("too_many_files", "目录层级或文件数量超出本次下载范围")
		}
		key := f.ID
		if seen[key] {
			return nil
		}
		seen[key] = true
		if !f.IsDir {
			files = append(files, f)
			return nil
		}
		sub := src
		sub.ParentID = f.ID
		cursor := ""
		pages := map[string]bool{}
		for {
			if pages[cursor] {
				return Fail("invalid_pagination", "网盘目录分页重复，请重试")
			}
			pages[cursor] = true
			l, e := p.List(ctx, client, ss, sub, cursor)
			if e != nil {
				return e
			}
			if l == nil {
				return Fail("invalid_response", "目录文件列表为空")
			}
			for _, child := range l.Files {
				child.Path = path.Join(f.Path, f.Name, child.Path)
				if e = walk(child, depth+1); e != nil {
					return e
				}
			}
			if l.NextPage == "" {
				break
			}
			cursor = l.NextPage
		}
		return nil
	}
	for _, f := range selected {
		if err = walk(f, 0); err != nil {
			return nil, err
		}
	}
	if len(files) == 0 {
		return nil, Fail("empty_selection", "所选目录没有可下载文件")
	}
	batch := randomID()
	needTransfer := false
	resolved := map[string]*Download{}
	for _, f := range files {
		var download *Download
		download, err = p.Resolve(ctx, client, ss, src, f)
		if err != nil {
			var typed *Error
			if errors.As(err, &typed) && typed.Code == "transfer_required" {
				needTransfer = true
				break
			}
			return nil, err
		}
		resolved[f.ID] = download
		if download != nil && download.ExpiresAt.IsZero() {
			download.ExpiresAt = time.Now().Add(30 * time.Second)
		}
	}
	if needTransfer {
		receipt, e := p.Transfer(ctx, client, ss, src, files, batch)
		if receipt != nil && len(receipt.CreatedIDs) > 0 {
			receipt.BatchID = batch
			receipt.Provider = src.Provider
			receipt.AccountID = ss.Capture.AccountID
			receipt.Source = src
			receipt.Files = publicFiles(receipt.Files)
			receipt.State = "transfer_failed"
			receipt.Remaining = []string{}
			for _, f := range files {
				receipt.Remaining = append(receipt.Remaining, f.ID)
			}
			if e != nil {
				receipt.Message = e.Error()
			}
			s.mu.Lock()
			s.transfers[batch] = clone(receipt)
			saveErr := s.persistLocked()
			s.mu.Unlock()
			if saveErr != nil {
				return nil, saveErr
			}
		}
		if e != nil {
			return nil, e
		}
		if receipt == nil || len(receipt.CreatedIDs) == 0 || len(receipt.Files) != len(files) {
			return nil, Fail("invalid_transfer", "转存结果缺少文件身份，副本已保留，请在网盘确认")
		}
		receipt.BatchID = batch
		receipt.Files = publicFiles(receipt.Files)
		receipt.Provider = src.Provider
		receipt.AccountID = ss.Capture.AccountID
		receipt.Source = src
		receipt.State = "downloading"
		receipt.Remaining = []string{}
		mapped := map[string]bool{}
		for _, f := range receipt.Files {
			if err := validateFile(f); err != nil {
				return nil, err
			}
			if mapped[f.ID] {
				return nil, Fail("invalid_transfer", "转存返回了重复文件身份，副本已保留，请在网盘确认")
			}
			mapped[f.ID] = true
			receipt.Remaining = append(receipt.Remaining, f.ID)
		}
		s.mu.Lock()
		s.transfers[batch] = clone(receipt)
		err = s.persistLocked()
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		files = receipt.Files
		src.ShareID = ""
		src.ParentID = receipt.FolderID
	}
	if opts == nil {
		opts = &base.Options{}
	}
	if !needTransfer {
		s.mu.Lock()
		for id, d := range resolved {
			s.downloads[batch+":"+id] = d
		}
		s.mu.Unlock()
	}
	out := &Prepared{BatchID: batch, Reqs: []*base.CreateTaskBatchItem{}}
	for _, f := range publicFiles(files) {
		e := &model.ReqExtra{Provider: src.Provider, Source: src, File: f, SessionID: ss.ID, AccountID: ss.Capture.AccountID, BatchID: batch}
		out.Reqs = append(out.Reqs, &base.CreateTaskBatchItem{Req: &base.Request{URL: src.URL, Protocol: "netdisk", Extra: e, Proxy: req.Proxy}, Opts: opts.Clone()})
	}
	return out, nil
}
func (s *Service) SetStateStore(store fetcher.ProtocolStateStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = store
	var v struct {
		Transfers map[string]*model.TransferReceipt `json:"transfers"`
	}
	if ok, err := store.Load(&v); ok && err == nil && v.Transfers != nil {
		s.transfers = v.Transfers
		for _, r := range s.transfers {
			if r.State == "cleaning" {
				r.State = "cleanup_failed"
				r.Message = "上次清理中断，可重新同步登录后重试"
			}
		}
	}
}

// PublicFile strips ephemeral provider authorization from persisted recipes.
func PublicFile(file model.File) model.File { return publicFiles([]model.File{file})[0] }
func (s *Service) persistLocked() error {
	if s.state == nil {
		return nil
	}
	return s.state.Save(struct {
		Transfers map[string]*model.TransferReceipt `json:"transfers"`
	}{clone(s.transfers)})
}
func (s *Service) Transfers() []*model.TransferReceipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := []*model.TransferReceipt{}
	for _, r := range s.transfers {
		v = append(v, clone(r))
	}
	return v
}
func (s *Service) Complete(ctx context.Context, ctl *controller.Controller, req *base.Request, e *model.ReqExtra) {
	s.mu.Lock()
	r := s.transfers[e.BatchID]
	if r == nil {
		s.mu.Unlock()
		return
	}
	left := []string{}
	for _, id := range r.Remaining {
		if id != e.File.ID {
			left = append(left, id)
		}
	}
	r.Remaining = left
	_ = s.persistLocked()
	ready := len(left) == 0
	s.mu.Unlock()
	if ready {
		_ = s.Cleanup(ctx, ctl, req, e.BatchID)
	}
}
func (s *Service) Cleanup(ctx context.Context, ctl *controller.Controller, req *base.Request, batch string) error {
	s.mu.Lock()
	r := clone(s.transfers[batch])
	if r == nil {
		s.mu.Unlock()
		return Fail("not_found", "转存记录不存在")
	}
	if len(r.Remaining) > 0 {
		s.mu.Unlock()
		return Fail("in_use", "此批次还有文件未下载完成，临时副本继续保留")
	}
	if r.State == "cleaned" || r.State == "cleaning" {
		s.mu.Unlock()
		return nil
	}
	s.transfers[batch].State = "cleaning"
	s.mu.Unlock()
	ss, err := s.session(r.Provider, "", r.AccountID)
	if err == nil {
		p := s.providers[r.Provider]
		if p == nil {
			err = fmt.Errorf("不支持的网盘类型")
		} else {
			err = p.Cleanup(ctx, NewAPIClient(ctl, req, p.Info(), ss), ss, r)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.transfers[batch]
	if err != nil {
		current.State = "cleanup_failed"
		current.Message = err.Error()
	} else {
		current.State = "cleaned"
		current.Message = ""
	}
	_ = s.persistLocked()
	return err
}
func (s *Service) Close() {
	s.mu.Lock()
	s.sessions = map[string]*Session{}
	s.requests = map[string]*pending{}
	s.downloads = map[string]*Download{}
	s.mu.Unlock()
}
