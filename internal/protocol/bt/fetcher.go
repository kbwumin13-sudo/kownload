package bt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/protocol/bt"
	"github.com/GopeedLab/gopeed/pkg/util"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

var (
	cfg         *torrent.ClientConfig
	client      *torrent.Client
	lock        sync.Mutex
	closeCtx    context.Context
	closeFunc   func()
	torrentRefs = make(map[*torrent.Torrent]*torrentRef)
	// configureClient lets tests keep every client this package creates off
	// the network. Production leaves it nil.
	configureClient func(*torrent.ClientConfig)
)

// resolveTimeout bounds only the interactive resolve behind the create panel,
// so a magnet nobody seeds cannot spin there forever. Created tasks and seeding
// wait for metadata until they are paused, deleted or shut down.
var resolveTimeout = 45 * time.Second

var errMetadataNotReady = errors.New("种子信息尚未就绪")

type metadataResolveError struct{ cause error }

func (e *metadataResolveError) Error() string {
	if errors.Is(e.cause, context.DeadlineExceeded) {
		return "获取种子信息超时，请检查网络或稍后重试"
	}
	return "已取消获取种子信息"
}

func (e *metadataResolveError) Unwrap() error { return e.cause }

type torrentRef struct {
	count int
	owned bool
}

type Fetcher struct {
	ctl    *controller.Controller
	config *config

	torrent *torrent.Torrent
	meta    *fetcher.FetcherMeta
	data    *fetcherData

	torrentReady    atomic.Bool
	torrentUpload   atomic.Bool
	torrentDropCtx  context.Context
	torrentDropFunc func()
	uploadDoneCh    chan any
	opMu            sync.Mutex
	closeOnce       sync.Once
	heldTorrent     *torrent.Torrent // protected by opMu; references use the global lock
	resolveMu       sync.Mutex
	resolveCancel   context.CancelFunc
	seedMu          sync.Mutex // guards data.SeedBytes and data.SeedTime, updated by doUpload
}

func (f *Fetcher) Setup(ctl *controller.Controller) {
	f.ctl = ctl
	if f.meta == nil {
		f.meta = &fetcher.FetcherMeta{}
	}
	if f.data == nil {
		f.data = &fetcherData{}
	}
	f.uploadDoneCh = make(chan any, 1)
	f.torrentDropCtx, f.torrentDropFunc = context.WithCancel(context.Background())
	f.ctl.GetConfig(&f.config)
	return
}

// initClientLocked must be called under lock, together with AddTorrentSpec.
func (f *Fetcher) initClientLocked() (err error) {
	if client != nil {
		return
	}
	if closeCtx == nil {
		closeCtx, closeFunc = context.WithCancel(context.Background())
	}

	cfg = torrent.NewDefaultClientConfig()
	cfg.Seed = true
	cfg.Bep20 = fmt.Sprintf("-GP%s-", parseBep20())
	cfg.ExtendedHandshakeClientVersion = fmt.Sprintf("Gopeed %s", base.Version)
	cfg.ListenPort = f.config.ListenPort
	cfg.HTTPProxy = f.ctl.GetProxy(f.meta.Req.Proxy)
	dnsResolver := &DnsCacheResolver{RefreshTimeout: 5 * time.Minute}
	cfg.TrackerDialContext = dnsResolver.DialContext
	if configureClient != nil {
		configureClient(cfg)
	}
	client, err = torrent.NewClient(cfg)
	if err != nil {
		return
	}

	closeCtx, closeFunc = context.WithCancel(context.Background())
	go dnsResolver.Run(closeCtx)
	return
}

func (f *Fetcher) Resolve(req *base.Request, opts *base.Options) error {
	return f.ResolveContext(context.Background(), req, opts)
}

// ResolveContext is the interactive resolve: it gives up after resolveTimeout.
func (f *Fetcher) ResolveContext(parent context.Context, req *base.Request, opts *base.Options) error {
	ctx, cancel := context.WithTimeout(parent, resolveTimeout)
	defer cancel()
	return f.resolve(ctx, cancel, req, opts)
}

// ResolveTask resolves a created task, such as a magnet added for direct
// download. Finding a peer that has the metadata can take minutes, so there is
// no deadline: Pause (ctx or CancelResolve) and Delete (Close) end the wait.
func (f *Fetcher) ResolveTask(parent context.Context, req *base.Request, opts *base.Options) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	return f.resolve(ctx, cancel, req, opts)
}

func (f *Fetcher) resolve(ctx context.Context, cancel context.CancelFunc, req *base.Request, opts *base.Options) error {
	f.setResolveCancel(cancel)
	defer f.setResolveCancel(nil)
	f.opMu.Lock()
	f.meta.Req = req
	f.meta.Opts = opts
	if f.meta.Opts == nil {
		f.meta.Opts = &base.Options{}
	}
	t, err := f.acquireLocked(ctx, req)
	f.opMu.Unlock()
	if err != nil {
		return err
	}
	return f.awaitInfo(ctx, t, f.updateRes)
}

// PrepareContext restores a task whose metadata is already persisted. A magnet
// task fetches its info from peers again, without a deadline.
func (f *Fetcher) PrepareContext(parent context.Context, req *base.Request, opts *base.Options) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	f.setResolveCancel(cancel)
	defer f.setResolveCancel(nil)
	f.opMu.Lock()
	if f.torrentReady.Load() {
		f.opMu.Unlock()
		return nil
	}
	if f.torrentDropCtx.Err() != nil {
		f.opMu.Unlock()
		return context.Canceled
	}
	f.meta.Req = req
	f.meta.Opts = opts
	t, err := f.acquireLocked(ctx, req)
	f.opMu.Unlock()
	if err != nil {
		return err
	}
	return f.awaitInfo(ctx, t, nil)
}

func (f *Fetcher) Start() (err error) {
	f.opMu.Lock()
	defer f.opMu.Unlock()
	if f.torrentDropCtx.Err() != nil {
		return context.Canceled
	}
	// The downloader resolves or prepares first, without its task locks. Start
	// runs under those locks, so it must never wait for peers.
	if !f.torrentReady.Load() {
		return errMetadataNotReady
	}

	files := f.torrent.Files()
	// If the user does not specify the file to download, all files will be downloaded by default
	if f.data.Progress == nil {
		if len(f.meta.Opts.SelectFiles) == 0 {
			f.meta.Opts.SelectFiles = make([]int, len(files))
			for i := range files {
				f.meta.Opts.SelectFiles[i] = i
			}
		}
		f.data.Progress = make(fetcher.Progress, len(f.meta.Opts.SelectFiles))
	}
	if len(f.meta.Opts.SelectFiles) == len(files) {
		f.torrent.DownloadAll()
	} else {
		for _, selectIndex := range f.meta.Opts.SelectFiles {
			file := files[selectIndex]
			file.Download()
		}
	}
	f.torrent.AllowDataDownload()
	go f.doUpload(false)
	return
}

func (f *Fetcher) Pause() (err error) {
	f.opMu.Lock()
	defer f.opMu.Unlock()
	if f.heldTorrent != nil {
		f.torrent.DisallowDataDownload()
	}
	return
}

func (f *Fetcher) setResolveCancel(cancel context.CancelFunc) {
	f.resolveMu.Lock()
	f.resolveCancel = cancel
	f.resolveMu.Unlock()
}

func (f *Fetcher) CancelResolve() {
	f.resolveMu.Lock()
	defer f.resolveMu.Unlock()
	if f.resolveCancel != nil {
		f.resolveCancel()
	}
}

func (f *Fetcher) Close() (err error) {
	f.closeOnce.Do(func() {
		if f.torrentDropFunc != nil {
			f.torrentDropFunc()
		}
		if f.uploadDoneCh != nil {
			close(f.uploadDoneCh)
		}
	})
	f.opMu.Lock()
	defer f.opMu.Unlock()
	return f.releaseTorrent()
}

// releaseTorrent only drops the last reference created by our shared client.
// Another resolver or an existing task can hold the same Torrent pointer.
func (f *Fetcher) releaseTorrent() error {
	lock.Lock()
	defer lock.Unlock()
	if f.heldTorrent == nil {
		return nil
	}
	releaseTorrentRefLocked(f.heldTorrent)
	f.heldTorrent = nil
	if client != nil && len(client.Torrents()) == 0 {
		return closeClientLocked()
	}
	return nil
}

func releaseTorrentRefLocked(t *torrent.Torrent) {
	if ref := torrentRefs[t]; ref != nil {
		ref.count--
		if ref.count == 0 {
			delete(torrentRefs, t)
			if ref.owned {
				t.Drop()
			}
		}
	}
}

func (f *Fetcher) Meta() *fetcher.FetcherMeta {
	return f.meta
}

func (f *Fetcher) Stats() any {
	var stats torrent.TorrentStats
	if f.torrent != nil {
		stats = f.torrent.Stats()
	} else {
		stats = torrent.TorrentStats{}
	}
	f.seedMu.Lock()
	seedBytes, seedTime := f.data.SeedBytes, f.data.SeedTime
	f.seedMu.Unlock()
	return &bt.Stats{
		TotalPeers:       stats.TotalPeers,
		ActivePeers:      stats.ActivePeers,
		ConnectedSeeders: stats.ConnectedSeeders,
		SeedBytes:        seedBytes,
		SeedRatio:        f.seedRadio(),
		SeedTime:         seedTime,
	}
}

func (f *Fetcher) Progress() fetcher.Progress {
	if !f.torrentReady.Load() {
		return f.data.Progress
	}
	for i := range f.data.Progress {
		selectIndex := f.meta.Opts.SelectFiles[i]
		file := f.torrent.Files()[selectIndex]
		f.data.Progress[i] = file.BytesCompleted()
	}
	return f.data.Progress
}

func (f *Fetcher) Wait() (err error) {
	for {
		select {
		case <-f.torrentDropCtx.Done():
			return
		case <-time.After(time.Second):
			if f.torrentReady.Load() && len(f.meta.Opts.SelectFiles) > 0 {
				if f.isDone() {
					// remove unselected files
					for i, file := range f.torrent.Files() {
						selected := false
						for _, selectIndex := range f.meta.Opts.SelectFiles {
							if i == selectIndex {
								selected = true
								break
							}
						}
						if !selected {
							util.SafeRemove(filepath.Join(f.meta.Opts.Path, f.meta.Res.Name, file.Path()))
						}
					}
					return
				}
			}
		}
	}
}

func (f *Fetcher) isDone() bool {
	if f.meta.Opts == nil {
		return false
	}
	for _, selectIndex := range f.meta.Opts.SelectFiles {
		file := f.torrent.Files()[selectIndex]
		if file.BytesCompleted() < file.Length() {
			return false
		}
	}
	return true
}

// Patch modifies the BT task settings.
// Invalid file indices are silently ignored.
func (f *Fetcher) Patch(req *base.Request, opts *base.Options) error {
	if opts == nil {
		return nil
	}

	if opts.SelectFiles != nil {
		selectFiles := opts.SelectFiles

		// Get file count from resource metadata
		fileCount := 0
		if f.meta.Res != nil {
			fileCount = len(f.meta.Res.Files)
		}

		// Filter out invalid indices (silently ignore)
		validSelectFiles := make([]int, 0, len(selectFiles))
		for _, idx := range selectFiles {
			if idx >= 0 && idx < fileCount {
				validSelectFiles = append(validSelectFiles, idx)
			}
		}

		if f.torrent != nil {
			files := f.torrent.Files()

			// Cancel all current file downloads first
			f.torrent.CancelPieces(0, f.torrent.NumPieces())

			// Apply new file selection
			if len(validSelectFiles) == len(files) {
				f.torrent.DownloadAll()
			} else {
				for _, selectIndex := range validSelectFiles {
					file := files[selectIndex]
					file.Download()
				}
			}
		}

		f.meta.Opts.SelectFiles = validSelectFiles
		// Recalculate the resource size based on new selection
		if f.meta.Res != nil {
			f.meta.Res.CalcSize(validSelectFiles)
		}
		// Reset progress tracking for new file selection
		f.data.Progress = make(fetcher.Progress, len(validSelectFiles))
	}

	return nil
}

func (f *Fetcher) updateRes() {
	res := &base.Resource{
		Range: true,
		Files: make([]*base.FileInfo, len(f.torrent.Files())),
		Hash:  f.torrent.InfoHash().String(),
	}
	// Directory torrent
	if f.torrent.Info().Length == 0 {
		res.Name = f.torrent.Name()
	}
	for i, file := range f.torrent.Files() {
		res.Files[i] = &base.FileInfo{
			Name: filepath.Base(file.DisplayPath()),
			Path: util.Dir(file.DisplayPath()),
			Size: file.Length(),
		}
	}
	res.CalcSize(nil)
	f.meta.Res = res
	if f.meta.Opts != nil {
		f.meta.Opts.InitSelectFiles(len(res.Files))
	}
}

// Upload resumes seeding a finished task. A magnet task fetches its info from
// peers again; that wait has no deadline and runs in the background, because
// the downloader calls Upload while holding the task's locks. Close or client
// shutdown ends it.
func (f *Fetcher) Upload() error {
	f.opMu.Lock()
	defer f.opMu.Unlock()
	t, err := f.acquireLocked(f.torrentDropCtx, f.meta.Req)
	if err != nil {
		return err
	}
	go func() {
		if f.awaitInfo(f.torrentDropCtx, t, nil) == nil {
			f.doUpload(true)
		}
	}()
	return nil
}

func (f *Fetcher) doUpload(fromUpload bool) {
	if !f.torrentUpload.CompareAndSwap(false, true) {
		return
	}

	// Check and update seed data
	f.seedMu.Lock()
	lastData := &fetcherData{
		SeedBytes: f.data.SeedBytes,
		SeedTime:  f.data.SeedTime,
	}
	f.seedMu.Unlock()
	var doneTime int64 = 0
	for {
		select {
		case <-f.torrentDropCtx.Done():
			return
		case <-time.After(time.Second):
			if !f.torrentReady.Load() {
				continue
			}

			stats := f.torrentStats()
			f.seedMu.Lock()
			f.data.SeedBytes = lastData.SeedBytes + stats.BytesWrittenData.Int64()
			f.seedMu.Unlock()

			// Check is download complete, if not don't check and stop seeding
			if !fromUpload && !f.isDone() {
				continue
			}
			if doneTime == 0 {
				doneTime = time.Now().Unix()
			}
			seedTime := lastData.SeedTime + time.Now().Unix() - doneTime
			f.seedMu.Lock()
			f.data.SeedTime = seedTime
			f.seedMu.Unlock()

			// If the seed forever is true, keep seeding
			if f.config.SeedKeep {
				continue
			}

			// If the seed ratio is reached, stop seeding
			if f.config.SeedRatio > 0 {
				seedRadio := f.seedRadio()
				if seedRadio >= f.config.SeedRatio {
					f.Close()
					break
				}
			}

			// If the seed time is reached, stop seeding
			if f.config.SeedTime > 0 {
				if seedTime >= f.config.SeedTime {
					f.Close()
					break
				}
			}
		}
	}
}

// Get torrent stats maybe panic, see https://github.com/anacrolix/torrent/issues/972
func (f *Fetcher) torrentStats() torrent.TorrentStats {
	defer func() {
		if r := recover(); r != nil {
			// ignore panic
		}
	}()

	return f.torrent.Stats()
}

func (f *Fetcher) UploadedBytes() int64 {
	f.seedMu.Lock()
	defer f.seedMu.Unlock()
	return f.data.SeedBytes
}

func (f *Fetcher) WaitUpload() (err error) {
	<-f.uploadDoneCh
	return nil
}

// acquireLocked adds req's torrent to the shared client and takes this
// fetcher's reference to it. The caller holds opMu; waiting for the metadata
// is left to awaitInfo.
func (f *Fetcher) acquireLocked(ctx context.Context, req *base.Request) (t *torrent.Torrent, err error) {
	defer func() {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			err = &metadataResolveError{cause: err}
		}
	}()
	if err = ctx.Err(); err != nil {
		return
	}
	if f.torrentDropCtx.Err() != nil {
		return nil, context.Canceled
	}
	if err = base.ParseReqExtra[bt.ReqExtra](req); err != nil {
		return
	}
	schema := util.ParseSchema(req.URL)
	privateTorrent := false
	var spec *torrent.TorrentSpec
	if schema == "MAGNET" {
		spec, err = torrent.TorrentSpecFromMagnetUri(req.URL)
		if err != nil {
			return
		}
	} else {
		var reader io.Reader
		if schema == "FILE" {
			fileUrl, _ := url.Parse(req.URL)
			filePath := fileUrl.Path[1:]
			reader, err = os.Open(filePath)
			if err != nil {
				return
			}
			defer reader.(io.Closer).Close()
		} else if schema == "DATA" {
			_, data := util.ParseDataUri(req.URL)
			reader = bytes.NewBuffer(data)
		} else {
			reader, err = os.Open(req.URL)
			if err != nil {
				return
			}
			defer reader.(io.Closer).Close()
		}

		var metaInfo *metainfo.MetaInfo
		metaInfo, err = metainfo.Load(reader)
		// Hotfix for https://github.com/anacrolix/torrent/issues/992, ignore "expected EOF" error
		// TODO remove this after the issue is fixed
		if err != nil && !strings.Contains(err.Error(), "expected EOF") {
			return nil, err
		}

		info, er := metaInfo.UnmarshalInfo()
		if er != nil {
			return nil, er
		}

		if info.Private != nil && *info.Private {
			privateTorrent = true
		}
		spec, err = torrent.TorrentSpecFromMetaInfoErr(metaInfo)
		if err != nil {
			return
		}
	}
	// Serialize client creation, acquisition and final release. A timeout must
	// never drop a torrent which another fetcher acquired in the meantime.
	f.torrentReady.Store(false)
	lock.Lock()
	if err = f.initClientLocked(); err != nil {
		lock.Unlock()
		return
	}
	downloadPath := f.meta.Opts.Path
	spec.Storage = storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir: cfg.DataDir,
		TorrentDirMaker: func(baseDir string, info *metainfo.Info, infoHash metainfo.Hash) string {
			return downloadPath
		},
	})
	var added bool
	t, added, err = client.AddTorrentSpec(spec)
	if err != nil {
		if len(client.Torrents()) == 0 {
			_ = closeClientLocked()
		}
		lock.Unlock()
		return nil, err
	}
	ref := torrentRefs[t]
	if ref == nil {
		ref = &torrentRef{owned: added}
		torrentRefs[t] = ref
	}
	ref.count++
	// Acquire first: a repeated resolve/upload of the same hash must retain
	// its metadata even if this fetcher was previously the only owner.
	releaseTorrentRefLocked(f.heldTorrent)
	f.heldTorrent = t
	f.torrent = t
	lock.Unlock()

	// Do not add external tracker to a private torrent.
	if !privateTorrent {
		// use map to deduplicate
		trackers := make(map[string]bool)
		if req.Extra != nil {
			extra := req.Extra.(*bt.ReqExtra)
			if len(extra.Trackers) > 0 {
				for _, tracker := range extra.Trackers {
					trackers[tracker] = true
				}
			}
		}
		if len(f.config.Trackers) > 0 {
			for _, tracker := range f.config.Trackers {
				trackers[tracker] = true
			}
		}
		if len(trackers) > 0 {
			announceList := make([][]string, 0)
			for tracker := range trackers {
				announceList = append(announceList, []string{tracker})
			}
			t.AddTrackers(announceList)
		}
	}
	return t, nil
}

// awaitInfo waits for t's metadata without holding opMu, so Pause, Close and
// Start never queue behind a slow magnet; peers found meanwhile stay with t.
// On success ready runs under opMu before the fetcher is marked ready, so
// Wait and Progress never see half-written metadata. On failure this
// fetcher's reference is released.
func (f *Fetcher) awaitInfo(ctx context.Context, t *torrent.Torrent, ready func()) (err error) {
	defer func() {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			err = &metadataResolveError{cause: err}
		}
	}()
	select {
	case <-ctx.Done():
	case <-f.torrentDropCtx.Done():
	case <-t.Closed():
	case <-t.GotInfo():
	}
	f.opMu.Lock()
	defer f.opMu.Unlock()
	// Several cases can be ready at once: Close also drops the torrent, and a
	// cancellation racing with the metadata wins.
	switch {
	case ctx.Err() != nil:
		err = ctx.Err()
	case f.torrentDropCtx.Err() != nil:
		err = context.Canceled
	case f.heldTorrent != t:
		// A newer acquisition by this fetcher replaced the reference.
		err = context.Canceled
	default:
		select {
		case <-t.GotInfo():
		default:
			err = errors.New("torrent closed while resolving metadata")
		}
	}
	if f.heldTorrent != t {
		// Close or the newer acquisition already released this reference.
		return err
	}
	if err != nil {
		_ = f.releaseTorrent()
		return err
	}
	if ready != nil {
		ready()
	}
	f.torrentReady.Store(true)
	return nil
}

func (f *Fetcher) seedRadio() float64 {
	var bytesRead int64
	if f.Meta().Res != nil {
		bytesRead = f.Meta().Res.Size
	} else {
		bytesRead = 0
	}
	if bytesRead <= 0 {
		return 0
	}

	f.seedMu.Lock()
	seedBytes := f.data.SeedBytes
	f.seedMu.Unlock()
	return float64(seedBytes) / float64(bytesRead)
}

type fetcherData struct {
	Progress  fetcher.Progress
	SeedBytes int64
	// SeedTime is the time in seconds to seed after downloading is complete.
	SeedTime int64
}

func closeClient() error {
	lock.Lock()
	defer lock.Unlock()
	return closeClientLocked()
}

func closeClientLocked() error {
	if closeFunc != nil {
		closeFunc()
	}
	if client != nil {
		errs := client.Close()
		client = nil
		closeCtx = nil
		closeFunc = nil
		torrentRefs = make(map[*torrent.Torrent]*torrentRef)
		if len(errs) > 0 {
			return errs[0]
		}
	}
	return nil
}

type FetcherManager struct {
}

func (fm *FetcherManager) Name() string {
	return "bt"
}

func (fm *FetcherManager) Filters() []*fetcher.SchemeFilter {
	return []*fetcher.SchemeFilter{
		{
			Type:    fetcher.FilterTypeUrl,
			Pattern: "MAGNET",
		},
		{
			Type:    fetcher.FilterTypeFile,
			Pattern: "TORRENT",
		},
		{
			Type:    fetcher.FilterTypeBase64,
			Pattern: "APPLICATION/X-BITTORRENT",
		},
	}
}

func (fm *FetcherManager) Build() fetcher.Fetcher {
	return &Fetcher{}
}

func (fm *FetcherManager) ParseName(u string) string {
	var name string
	url, err := url.Parse(u)
	if err != nil {
		return ""
	}

	params := url.Query()
	if params.Get("dn") != "" {
		return params.Get("dn")
	}
	if params.Get("xt") != "" {
		xt := strings.Split(params.Get("xt"), ":")
		return xt[len(xt)-1]
	}
	return name
}

func (fm *FetcherManager) AutoRename() bool {
	return false
}

func (fm *FetcherManager) DefaultConfig() any {
	return &config{
		ListenPort: 0,
		Trackers:   []string{},
		SeedKeep:   false,
		SeedRatio:  1.0,
		SeedTime:   120 * 60,
	}
}

func (fm *FetcherManager) Store(f fetcher.Fetcher) (data any, err error) {
	_f := f.(*Fetcher)
	// Persist a snapshot: doUpload keeps updating the seeding counters while
	// the storage serializes.
	_f.seedMu.Lock()
	defer _f.seedMu.Unlock()
	return &fetcherData{
		Progress:  slices.Clone(_f.data.Progress),
		SeedBytes: _f.data.SeedBytes,
		SeedTime:  _f.data.SeedTime,
	}, nil
}

func (fm *FetcherManager) Restore() (v any, f func(meta *fetcher.FetcherMeta, v any) fetcher.Fetcher) {
	return &fetcherData{}, func(meta *fetcher.FetcherMeta, v any) fetcher.Fetcher {
		return &Fetcher{
			meta: meta,
			data: v.(*fetcherData),
		}
	}
}

func (fm *FetcherManager) Close() error {
	return closeClient()
}

// parse version to bep20 format, fixed length 4, if not enough, fill 0
func parseBep20() string {
	s := strings.ReplaceAll(base.Version, ".", "")
	if len(s) < 4 {
		s += strings.Repeat("0", 4-len(s))
	}
	return s
}
