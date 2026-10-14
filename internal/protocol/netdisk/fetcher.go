package netdisk

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	httpfetch "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
	httpmodel "github.com/GopeedLab/gopeed/pkg/protocol/http"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"hash"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

type FetcherManager struct{ Service *cloud.Service }

func (m *FetcherManager) service() *cloud.Service {
	if m.Service == nil {
		m.Service = cloud.NewService()
	}
	return m.Service
}
func (m *FetcherManager) Name() string                     { return "netdisk" }
func (m *FetcherManager) Filters() []*fetcher.SchemeFilter { return nil }
func (m *FetcherManager) Build() fetcher.Fetcher           { return &Fetcher{service: m.service()} }
func (m *FetcherManager) ParseName(raw string) string {
	u, _ := url.Parse(raw)
	if u == nil {
		return "网盘文件"
	}
	return path.Base(u.Path)
}
func (m *FetcherManager) AutoRename() bool   { return true }
func (m *FetcherManager) DefaultConfig() any { return (&httpfetch.FetcherManager{}).DefaultConfig() }
func (m *FetcherManager) SetStateStore(store fetcher.ProtocolStateStore) {
	m.service().SetStateStore(store)
}
func (m *FetcherManager) Close() error { m.service().Close(); return nil }

type saved struct {
	Child      json.RawMessage `json:"child,omitempty"`
	Resource   *base.Resource  `json:"resource,omitempty"`
	Downloaded int64           `json:"downloaded"`
}

func (m *FetcherManager) Store(item fetcher.Fetcher) (any, error) {
	f := item.(*Fetcher)
	if !f.mu.TryLock() {
		f.viewMu.RLock()
		out := clone(f.view.saved)
		f.viewMu.RUnlock()
		return out, nil
	}
	defer f.mu.Unlock()
	out := &saved{Resource: clone(f.meta.Res), Downloaded: f.downloaded}
	if f.child != nil {
		data, err := (&httpfetch.FetcherManager{}).Store(f.child)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		var clean map[string]json.RawMessage
		_ = json.Unmarshal(b, &clean)
		delete(clean, "RedirectURL")
		out.Child, _ = json.Marshal(clean)
		out.Downloaded = f.child.Progress().TotalDownloaded()
	}
	f.viewMu.Lock()
	f.view.saved = clone(out)
	f.viewMu.Unlock()
	return out, nil
}
func (m *FetcherManager) Restore() (any, func(*fetcher.FetcherMeta, any) fetcher.Fetcher) {
	return &saved{}, func(meta *fetcher.FetcherMeta, v any) fetcher.Fetcher {
		d := v.(*saved)
		return &Fetcher{service: m.service(), meta: meta, resume: clone(d), downloaded: d.Downloaded, refresh: true}
	}
}
func clone[T any](v T) T { b, _ := json.Marshal(v); var c T; _ = json.Unmarshal(b, &c); return c }

type fetcherView struct {
	child      *httpfetch.Fetcher
	downloaded int64
	phase      string
	saved      *saved
}

type Fetcher struct {
	viewMu     sync.RWMutex
	view       fetcherView
	mu         sync.Mutex
	cancelMu   sync.Mutex
	service    *cloud.Service
	ctl        *controller.Controller
	meta       *fetcher.FetcherMeta
	child      *httpfetch.Fetcher
	resume     *saved
	downloaded int64
	refresh    bool
	running    bool
	closed     bool
	cancel     context.CancelFunc
	done       chan error
	runDone    chan struct{}
	phase      string
}

func (f *Fetcher) Setup(ctl *controller.Controller) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctl = ctl
	if f.meta == nil {
		f.meta = &fetcher.FetcherMeta{}
	}
	f.done = make(chan error, 1)
	f.publishViewLocked()
}
func parseExtra(req *base.Request) (*model.ReqExtra, error) {
	if req == nil {
		return nil, errors.New("缺少网盘请求")
	}
	if err := base.ParseReqExtra[model.ReqExtra](req); err != nil {
		return nil, err
	}
	e, ok := req.Extra.(*model.ReqExtra)
	if !ok || e.Provider == "" || e.File.ID == "" || e.File.IsDir {
		return nil, errors.New("请先在网盘文件列表选择要下载的文件")
	}
	e.File = cloud.PublicFile(e.File)
	return e, nil
}
func (f *Fetcher) Resolve(req *base.Request, opts *base.Options) (result error) {
	defer func() { result = safeError(result) }()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return errors.New("请先暂停网盘任务")
	}
	req = clone(req)
	if opts == nil {
		opts = &base.Options{}
	} else {
		opts = opts.Clone()
	}
	e, err := parseExtra(req)
	if err != nil {
		return err
	}
	f.meta.Req = req
	f.meta.Opts = opts
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	f.cancelMu.Lock()
	f.cancel = cancel
	f.cancelMu.Unlock()
	defer cancel()
	f.phase = "解析网盘文件"
	f.publishViewLocked()
	return f.resolveLocked(ctx, e)
}
func (f *Fetcher) resolveLocked(ctx context.Context, e *model.ReqExtra) error {
	f.publishViewLocked()
	d, err := f.service.Resolve(ctx, f.ctl, f.meta.Req, e)
	if err != nil {
		return err
	}
	if d == nil || d.URL == "" {
		return errors.New("网盘没有返回下载地址")
	}
	if d.File.ID != "" && d.File.ID != e.File.ID {
		return errors.New("网盘返回的文件身份不匹配")
	}
	if e.File.Size > 0 && d.File.Size > 0 && e.File.Size != d.File.Size {
		return errors.New("网盘文件大小已经变化，请重新创建下载")
	}
	if e.File.Hash != "" && d.File.Hash != "" && !strings.EqualFold(e.File.Hash, d.File.Hash) {
		return errors.New("网盘文件内容已经变化，请重新创建下载")
	}
	if e.File.Size == 0 && d.File.Size > 0 {
		e.File.Size = d.File.Size
	}
	if e.File.Hash == "" && d.File.Hash != "" {
		e.File.Hash = d.File.Hash
	}
	u, err := url.Parse(d.URL)
	if err != nil || u.User != nil || u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("网盘下载地址无效")
	}
	if f.child != nil {
		if err = f.child.Pause(); err != nil {
			return err
		}
		if f.resume == nil {
			data, e := (&httpfetch.FetcherManager{}).Store(f.child)
			if e != nil {
				return e
			}
			b, _ := json.Marshal(data)
			var clean map[string]json.RawMessage
			_ = json.Unmarshal(b, &clean)
			delete(clean, "RedirectURL")
			b, _ = json.Marshal(clean)
			f.resume = &saved{Child: b, Resource: clone(f.child.Meta().Res), Downloaded: f.child.Progress().TotalDownloaded()}
		}
	}
	if f.resume != nil && f.resume.Downloaded == 0 {
		f.resume = nil
	}
	jar, _ := cookiejar.New(nil)
	cloud.SeedCookies(jar, d.Cookies, []string{u.Hostname()})
	policy := func(r *http.Request) {
		for _, key := range []string{"Authorization", "Cookie", "AccessToken", "X-Captcha-Token", "X-Device-Id"} {
			r.Header.Del(key)
		}
		for k, v := range d.Header {
			if strings.EqualFold(k, "Cookie") {
				continue
			}
			if (r.URL.Scheme != u.Scheme || r.URL.Host != u.Host) && (strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Device-Id") || strings.Contains(strings.ToLower(k), "token")) {
				continue
			}
			r.Header.Set(k, v)
		}
		for _, cookie := range jar.Cookies(r.URL) {
			r.AddCookie(cookie)
		}
	}
	hreq := &base.Request{URL: d.URL, Extra: &httpmodel.ReqExtra{}, Proxy: f.meta.Req.Proxy, SkipVerifyCert: f.meta.Req.SkipVerifyCert}
	hm := &httpfetch.FetcherManager{}
	httpCtl := *f.ctl
	httpCtl.GetConfig = func(v any) {
		b, _ := json.Marshal(hm.DefaultConfig())
		_ = json.Unmarshal(b, v)
		if f.ctl.GetConfig != nil {
			f.ctl.GetConfig(v)
		}
	}
	var child *httpfetch.Fetcher
	if f.resume != nil && len(f.resume.Child) > 0 && f.resume.Resource != nil {
		// Probe the refreshed file before using saved byte ranges.
		probe := hm.Build().(*httpfetch.Fetcher)
		probe.SetRequestPolicy(policy)
		probe.SetResponsePolicy(rangeResponse(e.File.Size))
		probe.Setup(&httpCtl)
		if err = probe.ResolveContext(ctx, hreq, f.meta.Opts.Clone()); err != nil {
			return err
		}
		newRes := clone(probe.Meta().Res)
		_ = probe.Close()
		if newRes.Size != f.resume.Resource.Size || !newRes.Range {
			return errors.New("文件长度或断点下载能力已经变化，请重新创建下载")
		}
		template, restore := hm.Restore()
		if err = json.Unmarshal(f.resume.Child, template); err != nil {
			return err
		}
		meta := &fetcher.FetcherMeta{Req: hreq, Res: clone(f.resume.Resource), Opts: f.meta.Opts.Clone()}
		child = restore(meta, template).(*httpfetch.Fetcher)
		child.SetRequestPolicy(policy)
		child.SetResponsePolicy(rangeResponse(e.File.Size))
		child.Setup(&httpCtl)
	} else {
		child = hm.Build().(*httpfetch.Fetcher)
		child.SetRequestPolicy(policy)
		child.SetResponsePolicy(rangeResponse(e.File.Size))
		child.Setup(&httpCtl)
		if err = child.ResolveContext(ctx, hreq, f.meta.Opts.Clone()); err != nil {
			_ = child.Close()
			return err
		}
	}
	res := child.Meta().Res
	if e.File.Size > 0 && res.Size > 0 && e.File.Size != res.Size {
		_ = child.Close()
		return errors.New("下载响应与网盘文件大小不符")
	}
	res.Name = ""
	res.Files[0].Name = e.File.Name
	res.Files[0].Path = e.File.Path
	if f.child != nil {
		_ = f.child.Close()
	}
	f.child = child
	f.meta.Res = clone(res)
	f.resume = nil
	f.refresh = false
	f.phase = "等待下载"
	f.publishViewLocked()
	return nil
}
func (f *Fetcher) Start() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return errors.New("网盘任务已关闭")
	}
	if f.running {
		f.mu.Unlock()
		return nil
	}
	e, err := parseExtra(f.meta.Req)
	if err != nil {
		f.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancelMu.Lock()
	f.cancel = cancel
	f.cancelMu.Unlock()
	f.running = true
	f.phase = "准备网盘下载"
	f.publishViewLocked()
	f.runDone = make(chan struct{})
	runDone := f.runDone
	select {
	case <-f.done:
	default:
	}
	f.mu.Unlock()
	go func() {
		defer close(runDone)
		f.mu.Lock()
		var err error
		if f.child == nil || f.refresh {
			resolveCtx, c := context.WithTimeout(ctx, 60*time.Second)
			err = f.resolveLocked(resolveCtx, e)
			c()
		}
		child := f.child
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = f.syncOptions(child)
			if err == nil {
				child.Meta().Res = clone(f.meta.Res)
				err = child.Start()
			}
		}
		f.mu.Unlock()
		if err == nil {
			for attempt := 0; attempt < 2; attempt++ {
				err = child.WaitContext(ctx)
				if err == nil && e.File.Size > 0 && child.Progress().TotalDownloaded() < e.File.Size {
					err = errors.New("网盘下载尚未完整，链接可能已过期")
				}
				if ctx.Err() != nil {
					return
				}
				if attempt == 0 && refreshable(err) {
					f.mu.Lock()
					f.phase = "刷新网盘下载地址"
					resolveCtx, c := context.WithTimeout(ctx, 60*time.Second)
					err = f.resolveLocked(resolveCtx, e)
					c()
					if err == nil {
						err = ctx.Err()
					}
					if err == nil {
						child = f.child
						err = f.syncOptions(child)
						if err == nil {
							err = child.Start()
						}
					}
					f.mu.Unlock()
					if err == nil {
						continue
					}
				}
				break
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = f.verifyFile(e)
		}
		if ctx.Err() != nil {
			return
		}
		f.mu.Lock()
		f.running = false
		if child != nil {
			f.downloaded = child.Progress().TotalDownloaded()
		}
		if err == nil {
			f.phase = "已完成"
		} else {
			f.phase = "下载失败"
			f.refresh = true
		}
		f.publishViewLocked()
		f.mu.Unlock()
		if err == nil {
			cleanctx, c := context.WithTimeout(ctx, 40*time.Second)
			f.service.Complete(cleanctx, f.ctl, f.meta.Req, e)
			c()
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case f.done <- safeError(err):
		default:
		}
	}()
	return nil
}
func refreshable(err error) bool {
	if err == nil {
		return false
	}
	var re *httpfetch.RequestError
	if errors.As(err, &re) {
		return re.Code == 401 || re.Code == 403 || re.Code == 410 || re.Code == 412
	}
	text := err.Error()
	return strings.Contains(text, "尚未完整") || strings.Contains(text, "status=401") || strings.Contains(text, "status=403") || strings.Contains(text, "status=410") || strings.Contains(text, "status=412")
}
func (f *Fetcher) syncOptions(child *httpfetch.Fetcher) error {
	opts := f.meta.Opts.Clone()
	if opts.Extra == nil && child.Meta().Opts != nil {
		opts.Extra = child.Meta().Opts.Extra
	}
	if err := base.ParseOptExtra[httpmodel.OptsExtra](opts); err != nil {
		return err
	}
	if opts.Extra == nil {
		opts.Extra = &httpmodel.OptsExtra{Connections: 16}
	}
	child.Meta().Opts = opts
	return nil
}
func (f *Fetcher) verifyFile(e *model.ReqExtra) error {
	info, err := os.Stat(f.meta.SingleFilepath())
	if err != nil {
		return err
	}
	if e.File.Size > 0 && info.Size() != e.File.Size {
		return errors.New("下载文件不完整，临时网盘副本已保留")
	}
	parts := strings.SplitN(e.File.Hash, ":", 2)
	if len(parts) != 2 {
		algorithm := ""
		if (e.Provider == "baidu" || e.Provider == "quark" || e.Provider == "uc" || e.Provider == "tcloud") && len(e.File.Hash) == 32 {
			algorithm = "md5"
		}
		if e.Provider == "aliyun" && len(e.File.Hash) == 40 {
			algorithm = "sha1"
		}
		if algorithm == "" {
			return nil
		}
		parts = []string{algorithm, e.File.Hash}
	}
	var h hash.Hash
	switch strings.ToLower(parts[0]) {
	case "md5":
		h = md5.New()
	case "sha1":
		h = sha1.New()
	case "sha256":
		h = sha256.New()
	default:
		return nil
	}
	file, err := os.Open(f.meta.SingleFilepath())
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err = io.Copy(h, file); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), parts[1]) {
		return errors.New("下载文件校验不一致，临时网盘副本已保留")
	}
	return nil
}
func (f *Fetcher) Pause() error {
	f.cancelMu.Lock()
	if f.cancel != nil {
		f.cancel()
	}
	f.cancelMu.Unlock()
	f.mu.Lock()
	child := f.child
	run := f.runDone
	var pauseErr error
	if child != nil {
		pauseErr = child.Pause()
	}
	f.mu.Unlock()
	if pauseErr != nil {
		return pauseErr
	}
	if run != nil {
		<-run
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
	f.refresh = true
	if child != nil {
		d, err := (&httpfetch.FetcherManager{}).Store(child)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(d)
		var clean map[string]json.RawMessage
		_ = json.Unmarshal(b, &clean)
		delete(clean, "RedirectURL")
		b, _ = json.Marshal(clean)
		f.downloaded = child.Progress().TotalDownloaded()
		f.resume = &saved{Child: b, Resource: clone(child.Meta().Res), Downloaded: f.downloaded}
	}
	f.publishViewLocked()
	return nil
}
func (f *Fetcher) Close() error {
	if err := f.Pause(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	select {
	case f.done <- context.Canceled:
	default:
	}
	if f.child != nil {
		return f.child.Close()
	}
	return nil
}
func (f *Fetcher) CancelResolve() {
	f.cancelMu.Lock()
	if f.cancel != nil {
		f.cancel()
	}
	f.cancelMu.Unlock()
}

type redactedError struct {
	cause   error
	message string
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

var privateURL = regexp.MustCompile(`https?://[^\s"'<>]+`)

func safeError(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{err, privateURL.ReplaceAllString(err.Error(), "[下载地址]")}
}
func rangeResponse(total int64) func(*http.Request, *http.Response) error {
	return func(req *http.Request, res *http.Response) error {
		if req.Header.Get("Range") == "" || res.StatusCode >= 400 {
			return nil
		}
		var begin, end, gotBegin, gotEnd, gotTotal int64
		if res.StatusCode != http.StatusPartialContent {
			return errors.New("网盘服务器没有正确处理断点请求，已保留现有文件，请重新下载")
		}
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &begin, &end); err != nil {
			return errors.New("无效的断点请求")
		}
		if _, err := fmt.Sscanf(res.Header.Get("Content-Range"), "bytes %d-%d/%d", &gotBegin, &gotEnd, &gotTotal); err != nil || gotBegin != begin || gotEnd != end || total > 0 && gotTotal != total || gotEnd < gotBegin {
			return errors.New("网盘服务器返回的断点范围不匹配，现有文件已保留")
		}
		return nil
	}
}
func (f *Fetcher) Patch(req *base.Request, opts *base.Options) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return errors.New("请先暂停任务再同步登录")
	}
	if req != nil {
		e, err := parseExtra(req)
		if err != nil {
			return err
		}
		old, err := parseExtra(f.meta.Req)
		if err != nil {
			return err
		}
		if e.Provider != old.Provider || !reflect.DeepEqual(e.File, old.File) || e.AccountID != old.AccountID || !reflect.DeepEqual(e.Source, old.Source) || e.BatchID != old.BatchID {
			return errors.New("不能将已有任务替换为其他账号或文件")
		}
		updated := clone(f.meta.Req)
		old.SessionID = e.SessionID
		updated.Extra = clone(old)
		if req.Proxy != nil {
			updated.Proxy = clone(req.Proxy)
		}
		f.meta.Req = updated
	}
	if opts != nil {
		f.meta.Opts = opts.Clone()
	}
	f.refresh = true
	return nil
}

// publishViewLocked keeps polling and persistence independent of API waits.
func (f *Fetcher) publishViewLocked() {
	out := &saved{Resource: clone(f.meta.Res), Downloaded: f.downloaded}
	if f.resume != nil {
		out = clone(f.resume)
	}
	if f.child != nil {
		if data, err := (&httpfetch.FetcherManager{}).Store(f.child); err == nil {
			b, _ := json.Marshal(data)
			var clean map[string]json.RawMessage
			_ = json.Unmarshal(b, &clean)
			delete(clean, "RedirectURL")
			out.Child, _ = json.Marshal(clean)
			out.Downloaded = f.child.Progress().TotalDownloaded()
		}
	}
	f.viewMu.Lock()
	f.view = fetcherView{f.child, f.downloaded, f.phase, out}
	f.viewMu.Unlock()
}
func (f *Fetcher) Stats() any {
	f.viewMu.RLock()
	phase := f.view.phase
	f.viewMu.RUnlock()
	return map[string]any{"phase": phase, "transfers": f.service.Transfers()}
}
func (f *Fetcher) Meta() *fetcher.FetcherMeta { return f.meta }
func (f *Fetcher) Progress() fetcher.Progress {
	f.viewMu.RLock()
	child, downloaded := f.view.child, f.view.downloaded
	f.viewMu.RUnlock()
	if child != nil {
		return child.Progress()
	}
	return fetcher.Progress{downloaded}
}
func (f *Fetcher) Wait() error { return <-f.done }

var _ fetcher.Fetcher = (*Fetcher)(nil)
