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
	"github.com/GopeedLab/gopeed/internal/transfer"
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
	"sync/atomic"
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
	linkTime   time.Duration
}

type Fetcher struct {
	recovery atomic.Pointer[transfer.RecoveryLease]
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
	operation  uint64 // mu; rejects results from a cancelled/replaced operation
	resolving  bool
	resolveDone chan struct{}
	running    bool
	closed     bool
	cancel     context.CancelFunc
	done       chan error
	runDone    chan struct{}
	phase      string
	// Time the latest link resolution took (取链), for Stats.
	linkTime time.Duration
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

// deferrable marks a task run's resolve context: an API call that would first
// have to wait out a long cooldown ends the run with a deferral instead, so
// the downloader gives the running slot to other tasks meanwhile.
func (f *Fetcher) deferrable(ctx context.Context) context.Context {
	var queueWaiting func() bool
	var queueChanged func() <-chan struct{}
	if f.ctl != nil {
		queueWaiting, queueChanged = f.ctl.QueueWaiting, f.ctl.QueueChanged
	}
	return transfer.WithDeferral(ctx, queueWaiting, queueChanged)
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
func resolveBudget(e *model.ReqExtra) time.Duration {
	if e.Deferred {
		// A shared directory may need one batch transfer. Preserve Prepare's
		// five-minute operation budget while CancelResolve still cancels it.
		return 5 * time.Minute
	}
	return time.Minute
}

func (f *Fetcher) Resolve(req *base.Request, opts *base.Options) (result error) {
	defer func() { result = safeError(result) }()
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
	f.mu.Lock()
	if f.closed || f.running || f.resolving {
		f.mu.Unlock()
		return errors.New("请先暂停网盘任务")
	}
	f.operation++
	operation := f.operation
	f.resolving = true
	done := make(chan struct{})
	f.resolveDone = done
	f.meta.Req, f.meta.Opts = req, opts
	ctx, cancel := context.WithTimeout(f.deferrable(context.Background()), resolveBudget(e))
	f.cancelMu.Lock()
	f.cancel = cancel
	f.cancelMu.Unlock()
	f.phase = "解析网盘文件"
	f.publishViewLocked()
	f.mu.Unlock()
	defer func() {
		cancel()
		f.mu.Lock()
		f.resolving = false
		close(done)
		f.mu.Unlock()
	}()
	_, err = f.resolveForRun(ctx, operation, nil)
	return err
}

// resolveForRun owns its request and candidate child. Network IO, owner calls
// and child joins must never happen while holding the published-view mutex.
func (f *Fetcher) resolveForRun(ctx context.Context, operation uint64, plan *netdiskRefreshPlan) (context.Context, error) {
	f.mu.Lock()
	if err := f.checkOperationLocked(ctx, operation); err != nil {
		f.mu.Unlock()
		return ctx, err
	}
	req, opts := clone(f.meta.Req), f.meta.Opts.Clone()
	old, resume := f.child, clone(f.resume)
	f.mu.Unlock()
	e, err := parseExtra(req)
	if err != nil {
		return ctx, err
	}
	started := time.Now()
	providerCtx, providerCancel := context.WithTimeout(f.deferrable(ctx), resolveBudget(e))
	d, err := f.service.Resolve(providerCtx, f.ctl, req, e)
	providerCancel()
	linkTime := time.Since(started)
	if err != nil {
		return ctx, err
	}
	if err := f.checkOperation(ctx, operation); err != nil {
		return ctx, err
	}
	if d == nil || d.URL == "" {
		return ctx, errors.New("网盘没有返回下载地址")
	}
	if d.File.ID != "" && d.File.ID != e.File.ID {
		return ctx, errors.New("网盘返回的文件身份不匹配")
	}
	if e.File.Size > 0 && d.File.Size > 0 && e.File.Size != d.File.Size {
		return ctx, errors.New("网盘文件大小已经变化，请重新创建下载")
	}
	if e.File.Hash != "" && d.File.Hash != "" && !strings.EqualFold(e.File.Hash, d.File.Hash) {
		return ctx, errors.New("网盘文件内容已经变化，请重新创建下载")
	}
	if e.File.Size == 0 && d.File.Size > 0 {
		e.File.Size = d.File.Size
	}
	if e.File.Hash == "" && d.File.Hash != "" {
		e.File.Hash = d.File.Hash
	}
	u, err := url.Parse(d.URL)
	if err != nil || u.User != nil || u.Scheme != "http" && u.Scheme != "https" {
		return ctx, errors.New("网盘下载地址无效")
	}
	if old != nil {
		if err = old.Pause(); err != nil {
			return ctx, err
		}
		if resume == nil {
			data, e := (&httpfetch.FetcherManager{}).Store(old)
			if e != nil {
				return ctx, e
			}
			b, _ := json.Marshal(data)
			var clean map[string]json.RawMessage
			_ = json.Unmarshal(b, &clean)
			delete(clean, "RedirectURL")
			b, _ = json.Marshal(clean)
			resume = &saved{Child: b, Resource: clone(old.Meta().Res), Downloaded: old.Progress().TotalDownloaded()}
		}
	}
	if resume != nil && resume.Downloaded == 0 {
		resume = nil
	}
	if plan != nil {
		var commitErr error
		ctx, commitErr = f.commitRefresh(ctx, operation, plan)
		if commitErr != nil {
			return ctx, commitErr
		}
	}
	jar, _ := cookiejar.New(nil)
	cloud.SeedCookies(jar, d.Cookies, []string{u.Hostname()})
	credentials, err := transfer.NewTargetCredentials(d.URL, d.Header, jar)
	if err != nil {
		return ctx, errors.New("网盘下载认证上下文无效")
	}

	hreq := &base.Request{URL: d.URL, Extra: &httpmodel.ReqExtra{}, Proxy: req.Proxy, SkipVerifyCert: req.SkipVerifyCert}
	hm := &httpfetch.FetcherManager{}
	httpCtl := *f.ctl
	// Streams of one provider account share its download limit, whatever CDN
	// host a link points to; without an account ID the origin decides.
	if e.AccountID != "" {
		httpCtl.SourceScope = "netdisk|" + e.Provider + "|" + e.AccountID
	}
	httpCtl.GetConfig = func(v any) {
		b, _ := json.Marshal(hm.DefaultConfig())
		_ = json.Unmarshal(b, v)
		if f.ctl.GetConfig != nil {
			f.ctl.GetConfig(v)
		}
	}
	integrity := &baiduResponseDigest{enabled: e.Provider == "baidu" && !req.SkipVerifyCert, total: e.File.Size}
	var child *httpfetch.Fetcher
	published := false
	defer func() {
		if child != nil && !published {
			_ = child.Close()
		}
	}()
	if resume != nil && len(resume.Child) > 0 && resume.Resource != nil {
		// Probe the refreshed file before using saved byte ranges.
		probe := hm.Build().(*httpfetch.Fetcher)
		probe.SetTargetCredentials(credentials)
		probe.SetResponsePolicy(integrity.policy)
		probe.Setup(&httpCtl)
		probeCtx, bindErr := f.bindHTTPChild(ctx, probe, hreq)
		if bindErr != nil {
			_ = probe.Close()
			return ctx, bindErr
		}
		if err = probe.ResolveContext(probeCtx, hreq, opts.Clone()); err != nil {
			_ = probe.Close()
			return ctx, err
		}
		newRes := clone(probe.Meta().Res)
		_ = probe.Close()
		if newRes.Size != resume.Resource.Size || !newRes.Range {
			return ctx, errors.New("文件长度或断点下载能力已经变化，请重新创建下载")
		}
		template, restore := hm.Restore()
		if err = json.Unmarshal(resume.Child, template); err != nil {
			return ctx, err
		}
		meta := &fetcher.FetcherMeta{Req: hreq, Res: clone(resume.Resource), Opts: opts.Clone()}
		child = restore(meta, template).(*httpfetch.Fetcher)
		child.SetTargetCredentials(credentials)
		child.SetResponsePolicy(integrity.policy)
		child.Setup(&httpCtl)
		if _, err = f.bindHTTPChild(ctx, child, hreq); err != nil {
			return ctx, err
		}
	} else {
		child = hm.Build().(*httpfetch.Fetcher)
		child.SetTargetCredentials(credentials)
		child.SetResponsePolicy(integrity.policy)
		child.Setup(&httpCtl)
		childCtx, bindErr := f.bindHTTPChild(ctx, child, hreq)
		if bindErr != nil {
			return ctx, bindErr
		}
		if err = child.ResolveContext(childCtx, hreq, opts.Clone()); err != nil {
			_ = child.Close()
			return ctx, err
		}
	}
	res := child.Meta().Res
	if e.Provider != "baidu" && res.Hash == "" {
		res.Hash = e.File.Hash
	}
	if e.File.Size > 0 && res.Size > 0 && e.File.Size != res.Size {
		_ = child.Close()
		return ctx, errors.New("下载响应与网盘文件大小不符")
	}
	if e.Provider == "baidu" {
		digest := integrity.value()
		if resume != nil && resume.Resource != nil && digest != "" {
			if previous, ok := storedResponseMD5(resume.Resource.Hash); ok && previous != digest {
				_ = child.Close()
				return ctx, errors.New("网盘文件内容已经变化，已保留现有文件，请重新下载")
			}
		}
		// A restored resource must not retain a digest from an earlier response.
		res.Hash = ""
		if digest != "" {
			res.Hash = "md5:" + digest
		}
	}
	if err := child.SetResourceKey(e.Provider + "|" + e.Source.URL + "|" + e.File.ID); err != nil {
		_ = child.Close()
		return ctx, err
	}
	if err := child.SetResumeHash(resumeDigest(e, res)); err != nil {
		_ = child.Close()
		return ctx, err
	}
	res.Name = ""
	res.Files[0].Name = e.File.Name
	res.Files[0].Path = e.File.Path
	f.mu.Lock()
	if err := f.checkOperationLocked(ctx, operation); err != nil {
		f.mu.Unlock()
		return ctx, err
	}
	f.child = child
	if transfer.RecoveryLeaseFromContext(ctx) == nil {
		// Interactive resolution may enrich its public file selection. A
		// managed parent keeps the exact raw request frozen by Core.
		f.meta.Req.Extra = e
	}
	f.meta.Res = clone(res)
	f.resume = nil
	f.refresh = false
	f.phase = "等待下载"
	f.linkTime = linkTime
	f.publishViewLocked()
	published = true
	f.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return ctx, nil
}
func (f *Fetcher) Start() error {
	f.mu.Lock()
	if f.closed || f.resolving {
		f.mu.Unlock()
		return errors.New("网盘任务已关闭或正在解析")
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
	f.operation++
	operation := f.operation
	parent := context.Background()
	if lease := f.recovery.Load(); lease != nil {
		if lease.Context == nil {
			f.mu.Unlock()
			return transfer.ErrRecoveryHeld
		}
		parent = transfer.WithRecoveryLease(lease.Context, lease)
	}
	ctx, cancel := context.WithCancel(parent)
	f.cancelMu.Lock()
	f.cancel = cancel
	f.cancelMu.Unlock()
	f.running = true
	f.phase = "准备网盘下载"
	f.publishViewLocked()
	f.runDone = make(chan struct{})
	runDone := f.runDone
	result := f.done
	select {
	case <-f.done:
	default:
	}
	f.mu.Unlock()
	go func() {
		defer close(runDone)
		child, ctx, err := f.startChild(ctx, operation, e, nil)
		for attempt := 0; attempt < 2; attempt++ {
			if err == nil {
				err = child.WaitContext(ctx)
				if err == nil && e.File.Size > 0 && child.Progress().TotalDownloaded() < e.File.Size {
					err = errors.New("网盘下载尚未完整，链接可能已过期")
				}
			}
			if ctx.Err() != nil {
				return
			}
			if attempt == 0 && refreshable(err) {
				f.mu.Lock()
				if f.checkOperationLocked(ctx, operation) == nil {
					f.phase, f.refresh = "刷新网盘下载地址", true
					f.publishViewLocked()
				}
				f.mu.Unlock()
				var plan *netdiskRefreshPlan
				plan, err = f.prepareRefresh(ctx, operation)
				if err == nil {
					child, ctx, err = f.startChild(ctx, operation, e, plan)
				}
				if err == nil {
					continue
				}
			}
			break
		}
		if f.checkOperation(ctx, operation) != nil {
			return
		}
		if err == nil {
			err = f.verifyFile(e)
		}
		if f.checkOperation(ctx, operation) != nil {
			return
		}
		if err == nil {
			cleanctx, c := context.WithTimeout(ctx, 40*time.Second)
			f.service.Complete(cleanctx, f.ctl, f.meta.Req, e)
			c()
		}
		f.mu.Lock()
		if f.checkOperationLocked(ctx, operation) != nil {
			f.mu.Unlock()
			return
		}
		f.running = false
		if child != nil {
			f.downloaded = child.Progress().TotalDownloaded()
		}
		if err == nil {
			f.phase = "已完成"
		} else {
			f.phase = "下载失败"
			if _, deferred := transfer.AsDeferred(err); deferred {
				f.phase = "等待网盘接口冷却"
			}
			f.refresh = true
		}
		f.publishViewLocked()
		f.mu.Unlock()
		if f.checkOperation(ctx, operation) != nil {
			return
		}
		select {
		case result <- safeError(err):
		default:
		}
	}()
	return nil
}

func (f *Fetcher) startChild(ctx context.Context, operation uint64, e *model.ReqExtra, plan *netdiskRefreshPlan) (*httpfetch.Fetcher, context.Context, error) {
	f.mu.Lock()
	if err := f.checkOperationLocked(ctx, operation); err != nil {
		f.mu.Unlock()
		return nil, ctx, err
	}
	resolve := f.child == nil || f.refresh
	f.mu.Unlock()
	if resolve {
		var err error
		ctx, err = f.resolveForRun(ctx, operation, plan)
		if err != nil {
			return nil, ctx, err
		}
	}
	f.mu.Lock()
	if err := f.checkOperationLocked(ctx, operation); err != nil {
		f.mu.Unlock()
		return nil, ctx, err
	}
	child := f.child
	current, err := parseExtra(f.meta.Req)
	if err != nil {
		f.mu.Unlock()
		return nil, ctx, err
	}
	*e = *clone(current)
	err = f.syncOptions(child)
	if err == nil {
		child.Meta().Res = clone(f.meta.Res)
	}
	f.mu.Unlock()
	if err != nil {
		return nil, ctx, err
	}
	// Bind cancellation before Start so a late start cannot acquire a new
	// background lifetime after Pause has cancelled this parent run.
	if transfer.RecoveryLeaseFromContext(ctx) == nil {
		child.BindRunWait(ctx)
	}
	if err := f.checkOperation(ctx, operation); err != nil {
		return nil, ctx, err
	}
	return child, ctx, child.Start()
}

func refreshable(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "尚未完整") || httpfetch.IsExpiredLinkError(err))
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
	digest := e.File.Hash
	if digest == "" {
		digest = f.meta.Res.Hash
	}
	if e.Provider == "baidu" {
		if responseDigest, ok := storedResponseMD5(f.meta.Res.Hash); ok {
			digest = "md5:" + responseDigest
		} else if len(digest) == 32 {
			digest, err = cloud.BaiduMD5(digest)
			if err != nil {
				return err
			}
		}
	}
	parts := strings.SplitN(digest, ":", 2)
	if len(parts) != 2 {
		algorithm := ""
		if (e.Provider == "baidu" || e.Provider == "quark" || e.Provider == "uc" || e.Provider == "tcloud") && len(digest) == 32 {
			algorithm = "md5"
		}
		if e.Provider == "aliyun" && len(digest) == 40 {
			algorithm = "sha1"
		}
		if algorithm == "" {
			return nil
		}
		parts = []string{algorithm, digest}
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
	f.mu.Lock()
	f.cancelMu.Lock()
	if f.cancel != nil {
		f.cancel()
	}
	f.cancelMu.Unlock()
	f.operation++
	operation := f.operation
	child, run, resolve := f.child, f.runDone, f.resolveDone
	f.mu.Unlock()
	// Neither a child join nor the parent run wait may hold f.mu.
	var pauseErr error
	if child != nil {
		pauseErr = child.Pause()
	}
	if run != nil {
		<-run
	}
	if resolve != nil {
		<-resolve
	}
	if pauseErr != nil {
		return pauseErr
	}
	var checkpoint *saved
	if child != nil {
		var err error
		checkpoint, err = snapshotChild(child)
		if err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.operation != operation {
		return transfer.ErrRecoveryStale
	}
	f.running = false
	f.refresh = true
	if checkpoint != nil {
		f.downloaded, f.resume = checkpoint.Downloaded, checkpoint
	}
	f.publishViewLocked()
	return nil
}
func (f *Fetcher) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	if err := f.Pause(); err != nil {
		return err
	}
	f.mu.Lock()
	child := f.child
	select {
	case f.done <- context.Canceled:
	default:
	}
	f.mu.Unlock()
	if child != nil {
		return child.Close()
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
		// Client follows these responses and reapplies the scoped request policy.
		// Validate the final byte response after redirects have been followed.
		switch res.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
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
	f.view = fetcherView{f.child, f.downloaded, f.phase, out, f.linkTime}
	f.viewMu.Unlock()
}

// Stats keeps phase and transfers for older readers and adds the HTTP
// transfer's diagnostics plus the latest link resolution time.
func (f *Fetcher) Stats() any {
	f.viewMu.RLock()
	phase, child, linkTime := f.view.phase, f.view.child, f.view.linkTime
	f.viewMu.RUnlock()
	stats := map[string]any{"phase": phase, "transfers": f.service.Transfers(), "linkMs": linkTime.Milliseconds()}
	if child != nil {
		stats["http"] = child.Stats()
	}
	return stats
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
