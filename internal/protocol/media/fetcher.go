package media

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

type fetcherData struct {
	WorkPrefix   string                   `json:"workPrefix,omitempty"`
	Identity     string                   `json:"identity,omitempty"`
	WorkID       string                   `json:"workId,omitempty"`
	NeedsResolve bool                     `json:"needsResolve,omitempty"`
	Downloaded   int64                    `json:"downloaded"`
	Total        int64                    `json:"total"`
	Format       string                   `json:"format"`
	Tracks       map[string]trackProgress `json:"tracks,omitempty"`
}

type trackProgress struct {
	Downloaded int64 `json:"downloaded"`
	Total      int64 `json:"total"`
}

type FetcherManager struct{}

func (*FetcherManager) Name() string                     { return "media" }
func (*FetcherManager) Filters() []*fetcher.SchemeFilter { return nil }
func (*FetcherManager) Build() fetcher.Fetcher           { return &Fetcher{} }
func (*FetcherManager) ParseName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "media"
	}
	return path.Base(u.Path)
}
func (*FetcherManager) AutoRename() bool   { return true }
func (*FetcherManager) DefaultConfig() any { c := configFor(nil); return &c }
func (*FetcherManager) Store(item fetcher.Fetcher) (any, error) {
	f := item.(*Fetcher)
	f.mu.Lock()
	defer f.mu.Unlock()
	tracks := make(map[string]trackProgress, len(f.tracks))
	for key, value := range f.tracks {
		tracks[key] = value
	}
	return &fetcherData{Downloaded: f.downloaded, Total: f.total, Format: f.format, Tracks: tracks, Identity: f.identity, WorkID: f.workID, NeedsResolve: f.needsResolve, WorkPrefix: f.workPrefix}, nil
}
func (*FetcherManager) Restore() (any, func(*fetcher.FetcherMeta, any) fetcher.Fetcher) {
	return &fetcherData{}, func(meta *fetcher.FetcherMeta, data any) fetcher.Fetcher {
		d := data.(*fetcherData)
		return &Fetcher{meta: meta, downloaded: d.Downloaded, total: d.Total, format: d.Format, tracks: d.Tracks, identity: d.Identity, workID: d.WorkID, needsResolve: d.NeedsResolve, workPrefix: d.WorkPrefix}
	}
}
func (*FetcherManager) Close() error { return nil }

type Fetcher struct {
	workPrefix    string
	starting      bool
	needsResolve  bool
	identity      string
	workID        string
	mu            sync.Mutex
	ctl           *controller.Controller
	cfg           model.Config
	meta          *fetcher.FetcherMeta
	done          chan error
	runDone       chan struct{}
	cmd           *exec.Cmd
	stop          context.CancelFunc
	resolveCancel context.CancelFunc
	running       bool
	downloaded    int64
	total         int64
	format        string
	phase         string
	tracks        map[string]trackProgress
}

func (f *Fetcher) Setup(ctl *controller.Controller) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctl = ctl
	f.cfg = configFor(ctl)
	if f.meta == nil {
		f.meta = &fetcher.FetcherMeta{}
	}
	f.done = make(chan error, 1)
}

func (f *Fetcher) Resolve(req *base.Request, opts *base.Options) error {
	f.mu.Lock()
	if f.running || f.starting || f.resolveCancel != nil {
		f.mu.Unlock()
		return errors.New("media task is active or resolving")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	f.resolveCancel = cancel
	ctl, cfg := f.ctl, f.cfg
	f.mu.Unlock()
	defer func() { cancel(); f.mu.Lock(); f.resolveCancel = nil; f.mu.Unlock() }()
	return f.resolve(ctx, req, opts, ctl, cfg)
}

func (f *Fetcher) resolve(ctx context.Context, req *base.Request, opts *base.Options, ctl *controller.Controller, cfg model.Config) error {
	var err error
	req, err = cloneRequest(req)
	if err != nil {
		return err
	}
	if opts == nil {
		opts = &base.Options{}
	} else {
		opts = opts.Clone()
	}
	info, result, err := inspectInfo(ctx, req, ctl, cfg)
	if err != nil {
		return err
	}
	if !result.Supported {
		return errors.New(result.Reason)
	}
	requestCopy := *req
	extra, err := extraFor(&requestCopy)
	if err != nil {
		return err
	}
	if extra.MediaKind == "video" && isAudioOnly(info) {
		return errors.New("所选视频资源没有视频轨道，请选择视频流或改为音频下载")
	}
	if extra.MediaKind == "audio" && info.VCodec != "" && info.VCodec != "none" && info.VCodec != "unknown" {
		return errors.New("所选音频资源不是独立音频，请重新选择音频流")
	}
	if info.HasDRM || info.IsLive {
		return errors.New("unsupported media")
	}
	for _, selected := range info.RequestedFormats {
		if selected.HasDRM {
			return errors.New("DRM media is unsupported")
		}
	}
	if err := validateContainer(extra.Container, info); err != nil {
		return err
	}
	if err := validateSelectedSubtitles(extra, info); err != nil {
		return err
	}
	if opts == nil {
		opts = &base.Options{}
	}
	name := extra.Title
	if name == "" {
		name = info.Title
	}
	if name == "" {
		name = "media"
	}
	name = safeName(name)
	ext := info.Ext
	if extra.Container != "" {
		ext = extra.Container
	}
	if ext == "" {
		ext = "mp4"
	}
	if !strings.EqualFold(filepath.Ext(name), "."+ext) {
		name += "." + ext
	}
	size := info.Filesize
	if size == 0 {
		size = info.FilesizeApprox
	}
	if size == 0 {
		for _, selected := range info.RequestedFormats {
			n := selected.Filesize
			if n == 0 {
				n = selected.FilesizeApprox
			}
			size += n
		}
	}
	identity := mediaIdentity(req, extra, info)
	f.mu.Lock()
	if err := ctx.Err(); err != nil {
		f.mu.Unlock()
		return err
	}
	if f.identity != "" && f.identity != identity || f.identity == "" && f.needsResolve {
		f.workID = fmt.Sprintf("%x", time.Now().UnixNano())
		f.workPrefix = ""
		f.downloaded = 0
		f.tracks = nil
		f.phase = ""
	}
	f.identity = identity
	f.needsResolve = false
	f.meta = &fetcher.FetcherMeta{Req: req, Opts: opts, Res: &base.Resource{Files: []*base.FileInfo{{Name: name, Size: size}}, Size: size}}
	f.total = size
	f.format = info.FormatID
	if extra.Format != "" {
		f.format = extra.Format
	}
	f.mu.Unlock()
	return nil
}

func compatibleContainer(container string, formats []videoFormat) bool {
	if container == "mkv" {
		return true
	}
	for _, f := range formats {
		v := strings.ToLower(f.VCodec)
		a := strings.ToLower(f.ACodec)
		if container == "webm" {
			if v != "" && v != "none" && !strings.HasPrefix(v, "vp8") && !strings.HasPrefix(v, "vp9") && !strings.HasPrefix(v, "av01") {
				return false
			}
			if a != "" && a != "none" && !strings.HasPrefix(a, "opus") && !strings.HasPrefix(a, "vorbis") {
				return false
			}
			continue
		}
		if v != "" && v != "none" && !strings.HasPrefix(v, "avc") && !strings.HasPrefix(v, "h264") && !strings.HasPrefix(v, "hev") && !strings.HasPrefix(v, "h265") && !strings.HasPrefix(v, "av01") {
			return false
		}
		if a != "" && a != "none" && !strings.HasPrefix(a, "mp4a") && !strings.HasPrefix(a, "aac") && !strings.HasPrefix(a, "alac") {
			return false
		}
	}
	return true
}

func validateContainer(container string, info *videoInfo) error {
	if container == "" {
		return nil
	}
	switch container {
	case "m4a", "mp3", "aac", "ogg", "opus", "flac", "wav", "aiff", "alac":
		if container == info.Ext && isAudioOnly(info) {
			return nil
		}
		return errors.New("selected audio container does not match the source format")
	case "mp4", "mkv", "webm":
		if container == info.Ext {
			return nil
		}
		if len(info.RequestedFormats) >= 2 && compatibleContainer(container, info.RequestedFormats) {
			return nil
		}
		return errors.New("selected container is incompatible with streams")
	default:
		return errors.New("unsupported media container")
	}
}

func isAudioOnly(info *videoInfo) bool {
	if info.VCodec != "" {
		return info.VCodec == "none"
	}
	if len(info.RequestedFormats) > 0 {
		for _, selected := range info.RequestedFormats {
			if selected.VCodec != "none" {
				return false
			}
		}
		return true
	}
	for _, format := range info.Formats {
		if format.FormatID == info.FormatID {
			if format.VCodec != "" {
				return format.VCodec == "none"
			}
		}
	}
	switch info.Ext {
	case "m4a", "mp3", "aac", "ogg", "opus", "flac", "wav", "aiff", "alac":
		return true
	}
	return false
}

func safeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == 0 || r < 32 {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "media"
	}
	return name
}

func (f *Fetcher) Start() error {
	f.mu.Lock()
	if f.running {
		f.mu.Unlock()
		return nil
	}
	if f.starting || f.resolveCancel != nil {
		f.mu.Unlock()
		return errors.New("media task is resolving")
	}
	if f.meta == nil || f.meta.Req == nil || f.meta.Res == nil || f.meta.Opts == nil {
		f.mu.Unlock()
		return errors.New("media is unresolved")
	}
	if f.needsResolve {
		f.starting = true
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		f.resolveCancel = cancel
		req, opts, ctl, cfg := f.meta.Req, f.meta.Opts, f.ctl, f.cfg
		f.mu.Unlock()
		err := f.resolve(ctx, req, opts, ctl, cfg)
		cancel()
		f.mu.Lock()
		f.resolveCancel = nil
		f.starting = false
		if err != nil {
			f.mu.Unlock()
			return err
		}
	}
	defer f.mu.Unlock()
	req, err := cloneRequest(f.meta.Req)
	if err != nil {
		return err
	}
	ctl, cfg := f.ctl, f.cfg
	target := f.meta.SingleFilepath()
	extra, err := extraFor(req)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	args, cleanup, err := requestArgs(req, extra, ctl, cfg)
	if err != nil {
		return err
	}
	// A fixed work prefix retains .part/.ytdl across pause and process restart.
	workPrefix := f.workPrefix
	if workPrefix == "" {
		workPrefix = target + ".gopeed-media"
		if f.workID != "" {
			workPrefix += "." + f.workID
		}
		f.workPrefix = workPrefix
	}
	if err := os.MkdirAll(filepath.Dir(workPrefix), 0755); err != nil {
		cleanup()
		return err
	}
	subArgs, err := subtitleArgs(extra)
	if err != nil {
		cleanup()
		return err
	}
	args = append(args, subArgs...)
	args = append(args, formatArgs(extra)...)
	args = append(args, "--no-simulate", "--continue", "--newline", "--progress", "--concurrent-fragments", strconv.Itoa(cfg.Connections))
	args = append(args, "--progress-template", "download:media-progress:%(info.format_id)j\t%(progress)j", "--print", "before_postprocess:media-merge:{}", "--print", "after_move:media-final:%(filepath)j")
	args = append(args, "-o", strings.ReplaceAll(workPrefix, "%", "%%")+".%(ext)s", req.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cmd, commandCleanup, err := mediaCommand(ctx, cfg, req, extra, args)
	if err != nil {
		cancel()
		cleanup()
		return err
	}
	requestCleanup := cleanup
	cleanup = func() { commandCleanup(); requestCleanup() }
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		cleanup()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		cleanup()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		cleanup()
		return errors.New("media downloader could not start")
	}
	f.cmd, f.stop, f.running, f.phase = cmd, cancel, true, "download"
	f.runDone = make(chan struct{})
	go f.run(ctx, cmd, stdout, stderr, target, workPrefix, extra, cfg, cleanup, cancel)
	return nil
}

func (f *Fetcher) run(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.Reader, target, workPrefix string, extra *model.ReqExtra, cfg model.Config, cleanup func(), cancel context.CancelFunc) {
	var finalPath string
	var errorTail boundedTail
	var scanWG sync.WaitGroup
	scanWG.Add(2)
	go func() {
		defer scanWG.Done()
		s := bufio.NewScanner(stdout)
		s.Buffer(make([]byte, 4096), 2*1024*1024)
		for s.Scan() {
			line := s.Text()
			switch {
			case strings.HasPrefix(line, "media-progress:"):
				f.acceptProgress(strings.TrimPrefix(line, "media-progress:"))
			case strings.HasPrefix(line, "media-merge:"):
				f.mu.Lock()
				f.phase = "merge"
				f.mu.Unlock()
			case strings.HasPrefix(line, "media-final:"):
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "media-final:")), &finalPath)
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	go func() {
		defer scanWG.Done()
		s := bufio.NewScanner(stderr)
		s.Buffer(make([]byte, 4096), 2*1024*1024)
		for s.Scan() {
			line := s.Text()
			if strings.HasPrefix(line, "media-progress:") {
				f.acceptProgress(strings.TrimPrefix(line, "media-progress:"))
			} else {
				_, _ = errorTail.Write(append([]byte(line), '\n'))
			}
		}
		_, _ = io.Copy(&errorTail, stderr)
	}()
	scanWG.Wait()
	err := cmd.Wait()
	cleanup()
	if err == nil {
		if finalPath == "" || !strings.HasPrefix(filepath.Clean(finalPath), filepath.Clean(workPrefix)+".") {
			err = errors.New("media downloader did not report a final file")
		} else {
			stat, statErr := os.Stat(finalPath)
			if statErr != nil || !stat.Mode().IsRegular() {
				err = errors.New("media final file is missing")
			} else if trackErr := validateFinalTracks(ctx, cfg, extra.MediaKind, finalPath); trackErr != nil {
				err = trackErr
			} else if ctx.Err() != nil {
				err = ctx.Err()
			} else if subtitleErr := validateSubtitleFiles(workPrefix, extra.SubtitleLanguages); subtitleErr != nil {
				err = subtitleErr
			} else if subtitleErr := moveSubtitleFiles(workPrefix, target); subtitleErr != nil {
				err = subtitleErr
			} else if renameErr := os.Rename(finalPath, target); renameErr != nil {
				err = errors.New("media final file could not be moved")
			} else {
				f.mu.Lock()
				f.downloaded = stat.Size()
				f.total = stat.Size()
				f.phase = "done"
				f.meta.Res.Size = stat.Size()
				f.meta.Res.Files[0].Size = stat.Size()
				f.mu.Unlock()
			}
		}
	} else {
		if strings.Contains(string(errorTail.data), "403") {
			f.mu.Lock()
			req, cfg := f.meta.Req, f.cfg
			f.mu.Unlock()
			invalidateInspection(req, cfg)
		}
		err = mediaError(errorTail.data)
	}
	paused := ctx.Err() != nil
	cancel()
	f.mu.Lock()
	f.cmd, f.stop, f.running = nil, nil, false
	runDone := f.runDone
	f.mu.Unlock()
	if !paused {
		select {
		case f.done <- err:
		default:
		}
	}
	close(runDone)
}

type boundedTail struct{ data []byte }

func (t *boundedTail) Write(p []byte) (int, error) {
	n := len(p)
	const limit = 32 * 1024
	if len(p) >= limit {
		t.data = append(t.data[:0], p[len(p)-limit:]...)
		return n, nil
	}
	if len(t.data)+len(p) > limit {
		t.data = append(t.data[:0], t.data[len(t.data)+len(p)-limit:]...)
	}
	t.data = append(t.data, p...)
	return n, nil
}

func (f *Fetcher) acceptProgress(raw string) {
	trackJSON, progressJSON, found := strings.Cut(raw, "\t")
	if !found {
		trackJSON, progressJSON = "", raw
	}
	var p struct {
		Downloaded int64   `json:"downloaded_bytes"`
		Total      int64   `json:"total_bytes"`
		Estimate   float64 `json:"total_bytes_estimate"`
		Filename   string  `json:"filename"`
	}
	if json.Unmarshal([]byte(progressJSON), &p) != nil {
		return
	}
	var track string
	_ = json.Unmarshal([]byte(trackJSON), &track)
	if track == "" {
		track = p.Filename
	}
	if track == "" {
		track = "single"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	total := p.Total
	if total == 0 {
		total = int64(p.Estimate)
	}
	if f.tracks == nil {
		f.tracks = make(map[string]trackProgress)
	}
	state := f.tracks[track]
	if p.Downloaded > state.Downloaded {
		state.Downloaded = p.Downloaded
	}
	if total > state.Total {
		state.Total = total
	}
	f.tracks[track] = state
	var downloaded, knownTotal int64
	for _, value := range f.tracks {
		downloaded += value.Downloaded
		knownTotal += value.Total
	}
	if downloaded > f.downloaded {
		f.downloaded = downloaded
	}
	if knownTotal > f.total {
		f.total = knownTotal
	}
}

func (f *Fetcher) Patch(req *base.Request, opts *base.Options) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running || f.starting || f.resolveCancel != nil {
		return errors.New("media task is active or resolving")
	}
	if f.meta == nil || f.meta.Req == nil {
		return errors.New("media is unresolved")
	}
	candidate, err := cloneRequest(f.meta.Req)
	if err != nil {
		return err
	}
	options := f.meta.Opts.Clone()
	if req != nil {
		if req.URL != "" {
			oldOrigin, _ := headerOrigin(candidate.URL)
			newOrigin, err := headerOrigin(req.URL)
			if err != nil {
				return err
			}
			if oldOrigin != newOrigin {
				extra, err := extraFor(candidate)
				if err != nil {
					return err
				}
				for key := range extra.Header {
					if !strings.EqualFold(key, "Referer") && !strings.EqualFold(key, "User-Agent") {
						delete(extra.Header, key)
					}
				}
				// Domainless cookies were implicitly bound to the old request host.
				for i := range extra.Cookies {
					if extra.Cookies[i].Domain == "" {
						u, _ := url.Parse(candidate.URL)
						extra.Cookies[i].Domain = u.Hostname()
					}
				}
			}
			candidate.URL = req.URL
		}
		if req.Proxy != nil {
			proxy := *req.Proxy
			candidate.Proxy = &proxy
		}
		if req.Extra != nil {
			// Merge JSON fields to retain explicit false/empty array changes from API callers.
			current, _ := json.Marshal(candidate.Extra)
			incoming, err := json.Marshal(req.Extra)
			if err != nil {
				return errors.New("invalid media patch")
			}
			var old, patch map[string]json.RawMessage
			if json.Unmarshal(current, &old) != nil || json.Unmarshal(incoming, &patch) != nil {
				return errors.New("invalid media patch")
			}
			// A nil typed map is omitted context, while an explicit JSON null or
			// empty map can clear headers. Cookie-only refreshes retain headers.
			if typed, ok := req.Extra.(*model.ReqExtra); ok && typed != nil && typed.Header == nil {
				delete(patch, "header")
			}
			if typed, ok := req.Extra.(model.ReqExtra); ok && typed.Header == nil {
				delete(patch, "header")
			}
			for key, value := range patch {
				if key == "header" {
					var headers map[string]string
					if json.Unmarshal(value, &headers) != nil {
						return errors.New("invalid media patch")
					}
					old[key] = value
					continue
				}
				if string(value) == "null" || string(value) == `""` {
					continue
				}
				old[key] = value
			}
			merged, _ := json.Marshal(old)
			var extra model.ReqExtra
			if json.Unmarshal(merged, &extra) != nil {
				return errors.New("invalid media patch")
			}
			candidate.Extra = &extra
		}
	}
	extra, err := extraFor(candidate)
	if err != nil {
		return err
	}
	if _, err = contextScopes(candidate, extra); err != nil {
		return err
	}
	if _, err = subtitleArgs(extra); err != nil {
		return err
	}
	if opts != nil {
		if opts.Name != "" {
			options.Name = opts.Name
		}
		if opts.Path != "" {
			options.Path = opts.Path
		}
	}
	f.meta = &fetcher.FetcherMeta{Req: candidate, Opts: options, Res: f.meta.Res}
	if req != nil {
		f.needsResolve = true
	}
	return nil
}

func mediaIdentity(req *base.Request, extra *model.ReqExtra, info *videoInfo) string {
	u, _ := url.Parse(req.URL)
	// Keep semantic query parameters (v/id/episode); strip only known signing fields.
	query := u.Query()
	for key := range query {
		k := strings.ToLower(key)
		if k == "sig" || k == "signature" || k == "token" || k == "expires" || k == "expire" || k == "policy" || k == "key-pair-id" || strings.HasPrefix(k, "x-amz-") || strings.HasPrefix(k, "x-goog-") {
			query.Del(key)
		}
	}
	u.RawQuery = query.Encode()
	u.Fragment = ""
	identity := []any{u.String(), info.ID, info.FormatID, info.Ext, extra.Format, extra.Container, extra.ExpectedMediaID, extra.SubtitleLanguages, extra.WriteAutomaticSubtitles, extra.EmbedSubtitles}
	// Keep persisted legacy identities stable through signed-URL refreshes.
	if extra.MediaKind != "" {
		identity = append(identity, extra.MediaKind)
	}
	data, _ := json.Marshal(identity)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (f *Fetcher) Pause() error {
	f.CancelResolve()
	f.mu.Lock()
	stop, cmd, running, runDone := f.stop, f.cmd, f.running, f.runDone
	f.mu.Unlock()
	if !running {
		return nil
	}
	if stop != nil {
		stop()
	}
	if cmd != nil {
		_ = killProcessGroup(cmd)
	}
	// run closes this only after cmd.Wait and pipe readers terminate.
	<-runDone
	f.mu.Lock()
	f.phase = "pause"
	f.mu.Unlock()
	return nil
}

// CancelResolve lets the downloader interrupt inspection before acquiring its
// task lock, which Resolve currently owns.
func (f *Fetcher) CancelResolve() {
	f.mu.Lock()
	cancel := f.resolveCancel
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (f *Fetcher) Close() error {
	if err := f.Pause(); err != nil {
		return err
	}
	f.mu.Lock()
	meta := f.meta
	f.mu.Unlock()
	if meta == nil || meta.Res == nil || meta.Opts == nil || len(meta.Res.Files) == 0 {
		return nil
	}
	prefixes := []string{meta.SingleFilepath() + ".gopeed-media"}
	f.mu.Lock()
	if f.workPrefix != "" {
		prefixes = append(prefixes, f.workPrefix)
	}
	f.mu.Unlock()
	for _, prefix := range prefixes {
		entries, err := os.ReadDir(filepath.Dir(prefix))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), filepath.Base(prefix)+".") {
				continue
			}
			if err := os.Remove(filepath.Join(filepath.Dir(prefix), entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (f *Fetcher) Meta() *fetcher.FetcherMeta { f.mu.Lock(); defer f.mu.Unlock(); return f.meta }
func (f *Fetcher) Stats() any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &model.Stats{Phase: f.phase, Downloaded: f.downloaded, Total: f.total, Format: f.format}
}
func (f *Fetcher) MediaPhase() string { f.mu.Lock(); defer f.mu.Unlock(); return f.phase }
func (f *Fetcher) Progress() fetcher.Progress {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fetcher.Progress{f.downloaded}
}
func (f *Fetcher) Wait() error { return <-f.done }

// Publish selected sidecars beside the final media, including when embedding was disabled.
func moveSubtitleFiles(prefix, target string) error {
	entries, err := os.ReadDir(filepath.Dir(prefix))
	if err != nil {
		return errors.New("media subtitles could not be read")
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), filepath.Base(prefix)+".") {
			continue
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".srt", ".vtt", ".ass", ".ssa", ".ttml", ".srv1", ".srv2", ".srv3", ".json3":
		default:
			continue
		}
		suffix := strings.TrimPrefix(entry.Name(), filepath.Base(prefix))
		destination := strings.TrimSuffix(target, filepath.Ext(target)) + suffix
		if err := os.Rename(filepath.Join(filepath.Dir(prefix), entry.Name()), destination); err != nil {
			return errors.New("media subtitles could not be moved")
		}
	}
	return nil
}
