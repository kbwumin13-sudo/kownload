package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

type Inspection = model.Inspection
type Format = model.Format
type Stats = model.Stats

type subtitleFormat struct {
	Ext  string `json:"ext"`
	Name string `json:"name"`
}

type videoInfo struct {
	Thumbnail         string                      `json:"thumbnail"`
	Subtitles         map[string][]subtitleFormat `json:"subtitles"`
	AutomaticCaptions map[string][]subtitleFormat `json:"automatic_captions"`
	Type              string                      `json:"_type"`
	Entries           json.RawMessage             `json:"entries"`
	ID                string                      `json:"id"`
	WebpageURL        string                      `json:"webpage_url"`
	URL               string                      `json:"url"`
	ManifestURL       string                      `json:"manifest_url"`
	Protocol          string                      `json:"protocol"`
	Title             string                      `json:"title"`
	Duration          float64                     `json:"duration"`
	Ext               string                      `json:"ext"`
	VCodec            string                      `json:"vcodec"`
	ACodec            string                      `json:"acodec"`
	FormatID          string                      `json:"format_id"`
	Filesize          int64                       `json:"filesize"`
	FilesizeApprox    int64                       `json:"filesize_approx"`
	IsLive            bool                        `json:"is_live"`
	LiveStatus        string                      `json:"live_status"`
	HasDRM            bool                        `json:"has_drm"`
	Formats           []videoFormat               `json:"formats"`
	RequestedFormats  []videoFormat               `json:"requested_formats"`
}

type videoFormat struct {
	URL            string  `json:"url"`
	ManifestURL    string  `json:"manifest_url"`
	Protocol       string  `json:"protocol"`
	Language       string  `json:"language"`
	FPS            float64 `json:"fps"`
	TBR            float64 `json:"tbr"`
	FormatID       string  `json:"format_id"`
	FormatNote     string  `json:"format_note"`
	Ext            string  `json:"ext"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	VCodec         string  `json:"vcodec"`
	ACodec         string  `json:"acodec"`
	Filesize       int64   `json:"filesize"`
	FilesizeApprox int64   `json:"filesize_approx"`
	HasDRM         bool    `json:"has_drm"`
}

func configFor(ctl *controller.Controller) model.Config {
	c := model.Config{Connections: 4}
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			c.YtDlpPath = filepath.Join(home, ".local", "bin", "yt-dlp")
		}
		c.FFmpegPath = "/opt/homebrew/bin/ffmpeg"
		c.JsRuntimePath = "/opt/homebrew/bin/node"
	}
	if ctl != nil && ctl.GetConfig != nil {
		ctl.GetConfig(&c)
	}
	if c.YtDlpPath == "" {
		c.YtDlpPath = "yt-dlp"
	}
	if c.FFmpegPath == "" {
		if runtime.GOOS == "darwin" {
			c.FFmpegPath = "/opt/homebrew/bin/ffmpeg"
		} else {
			c.FFmpegPath = "ffmpeg"
		}
	}
	if runtime.GOOS == "darwin" && !filepath.IsAbs(c.FFmpegPath) {
		c.FFmpegPath = "/opt/homebrew/bin/ffmpeg"
	}
	if c.Connections < 1 {
		c.Connections = 4
	}
	return c
}

func extraFor(req *base.Request) (*model.ReqExtra, error) {
	if req == nil {
		return nil, errors.New("missing media request")
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("media URL must be an HTTP(S) resource without embedded credentials")
	}
	if err := base.ParseReqExtra[model.ReqExtra](req); err != nil {
		return nil, errors.New("invalid media request extra")
	}
	if req.Extra == nil {
		req.Extra = &model.ReqExtra{}
	}
	extra := req.Extra.(*model.ReqExtra)
	if extra.MediaKind != "" && extra.MediaKind != "video" && extra.MediaKind != "audio" {
		return nil, errors.New("invalid media kind")
	}
	return extra, nil
}

type inspectionEntry struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	info    *videoInfo
	result  *model.Inspection
	err     error
	expires time.Time
}

var inspections = struct {
	sync.Mutex
	items map[[32]byte]*inspectionEntry
}{items: make(map[[32]byte]*inspectionEntry)}

const inspectionTTL = 30 * time.Second
const inspectionLimit = 64

func toolIdentity(path string) any {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	stat, err := os.Stat(resolved)
	if err != nil {
		return []any{path, resolved}
	}
	return []any{path, resolved, stat.Size(), stat.ModTime().UnixNano(), stat.Mode()}
}

func inspectionKey(req *base.Request, extra *model.ReqExtra, cfg model.Config) [32]byte {
	// Hash the complete signed URL and request context; never retain them in cache keys.
	data, _ := json.Marshal([]any{req.URL, req.Protocol, req.Proxy, req.SkipVerifyCert, extra, cfg, toolIdentity(cfg.YtDlpPath), toolIdentity(cfg.FFmpegPath), toolIdentity(cfg.JsRuntimePath)})
	return sha256.Sum256(data)
}

func baseInspectionKey(req *base.Request, extra *model.ReqExtra, cfg model.Config) [32]byte {
	baseExtra := *extra
	baseExtra.Format, baseExtra.Container = "", ""
	return inspectionKey(req, &baseExtra, cfg)
}

func invalidateInspection(req *base.Request, cfg model.Config) {
	if req == nil {
		return
	}
	requestCopy := *req
	extra, err := extraFor(&requestCopy)
	if err != nil {
		return
	}
	exactKey := inspectionKey(&requestCopy, extra, cfg)
	baseKey := baseInspectionKey(&requestCopy, extra, cfg)
	inspections.Lock()
	delete(inspections.items, exactKey)
	delete(inspections.items, baseKey)
	inspections.Unlock()
}

func inspectInfo(ctx context.Context, req *base.Request, ctl *controller.Controller, cfg model.Config) (*videoInfo, *model.Inspection, error) {
	if req == nil {
		return nil, nil, errors.New("missing media request")
	}
	snapshot, err := cloneRequest(req)
	if err != nil {
		return nil, nil, err
	}
	requestCopy := *snapshot
	extra, err := extraFor(&requestCopy)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	baseKey := baseInspectionKey(&requestCopy, extra, cfg)
	if extra.Format != "" {
		if baseInfo, baseResult, baseErr, found := cachedInspection(ctx, baseKey, nil); found {
			if baseErr != nil {
				return nil, nil, baseErr
			}
			if selected, ok := selectedInfo(baseInfo, baseResult, extra.Format); ok {
				return selected, baseResult, nil
			}
		}
	}
	key := baseKey
	if extra.Format != "" {
		key = inspectionKey(&requestCopy, extra, cfg)
	}
	// Default metadata is extracted without a container choice so it can serve
	// the later popup selection regardless of the requested output container.
	if extra.Format == "" {
		baseExtra := *extra
		baseExtra.Container = ""
		extra = &baseExtra
	}
	info, result, err, _ := cachedInspection(ctx, key, func(workCtx context.Context) (*videoInfo, *model.Inspection, error) {
		return extractInfo(workCtx, &requestCopy, extra, ctl, cfg)
	})
	return info, result, err
}

type inspectionWork func(context.Context) (*videoInfo, *model.Inspection, error)

// nil work only joins an existing entry; it never starts a new extraction.
func cachedInspection(ctx context.Context, key [32]byte, work inspectionWork) (*videoInfo, *model.Inspection, error, bool) {
	inspections.Lock()
	entry := inspections.items[key]
	if entry != nil && entry.done != nil {
		select {
		case <-entry.done:
			if time.Now().After(entry.expires) || entry.err != nil {
				delete(inspections.items, key)
				entry = nil
			}
		default:
		}
	}
	if entry == nil {
		if work == nil {
			inspections.Unlock()
			return nil, nil, nil, false
		}
		if len(inspections.items) >= inspectionLimit {
			for k, old := range inspections.items {
				select {
				case <-old.done:
					delete(inspections.items, k)
				default:
				}
				if len(inspections.items) < inspectionLimit {
					break
				}
			}
		}
		// When all entries are active, run this request uncached rather than evict an active extraction.
		workCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		entry = &inspectionEntry{done: make(chan struct{}), cancel: cancel}
		if len(inspections.items) < inspectionLimit {
			inspections.items[key] = entry
		}
		go func() {
			info, result, err := work(workCtx)
			inspections.Lock()
			entry.info, entry.result, entry.err, entry.expires = info, result, err, time.Now().Add(inspectionTTL)
			if err != nil && inspections.items[key] == entry {
				delete(inspections.items, key)
			}
			close(entry.done)
			inspections.Unlock()
			cancel()
		}()
	}
	entry.waiters++
	inspections.Unlock()
	select {
	case <-ctx.Done():
		inspections.Lock()
		entry.waiters--
		select {
		case <-entry.done:
		default:
			if entry.waiters == 0 {
				entry.cancel()
				if inspections.items[key] == entry {
					delete(inspections.items, key)
				}
			}
		}
		inspections.Unlock()
		return nil, nil, ctx.Err(), true
	case <-entry.done:
		inspections.Lock()
		entry.waiters--
		info, result, err := entry.info, entry.result, entry.err
		inspections.Unlock()
		return cloneVideoInfo(info), cloneInspection(result), err, true
	}
}

func cloneVideoInfo(info *videoInfo) *videoInfo {
	if info == nil {
		return nil
	}
	copy := *info
	copy.Subtitles = cloneSubtitleMap(info.Subtitles)
	copy.AutomaticCaptions = cloneSubtitleMap(info.AutomaticCaptions)
	copy.Formats = append([]videoFormat(nil), info.Formats...)
	copy.RequestedFormats = append([]videoFormat(nil), info.RequestedFormats...)
	copy.Entries = append(json.RawMessage(nil), info.Entries...)
	return &copy
}

func cloneInspection(result *model.Inspection) *model.Inspection {
	if result == nil {
		return nil
	}
	copy := *result
	copy.ContextURLs = append([]string(nil), result.ContextURLs...)
	copy.Formats = append([]model.Format(nil), result.Formats...)
	copy.Subtitles = append([]model.Subtitle{}, result.Subtitles...)
	for i := range copy.Subtitles {
		copy.Subtitles[i].Formats = append([]string(nil), result.Subtitles[i].Formats...)
	}
	return &copy
}

func selectedInfo(base *videoInfo, result *model.Inspection, choice string) (*videoInfo, bool) {
	if base == nil || result == nil || !result.Supported {
		return nil, false
	}
	allowed := make(map[string]bool, len(result.Formats))
	for _, f := range result.Formats {
		allowed[f.ID] = true
	}
	ids := []string{choice}
	singleRaw := false
	for _, f := range base.Formats {
		if f.FormatID == choice {
			singleRaw = true
			break
		}
	}
	if !singleRaw {
		ids = strings.Split(choice, "+")
		if len(ids) != 2 || ids[0] == "" || ids[1] == "" || ids[0] == ids[1] {
			return nil, false
		}
	}
	selected := make([]videoFormat, 0, len(ids))
	for _, id := range ids {
		if !allowed[id] {
			return nil, false
		}
		found := false
		for _, f := range base.Formats {
			if f.FormatID == id && !f.HasDRM {
				selected = append(selected, f)
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	copy := cloneVideoInfo(base)
	copy.RequestedFormats = selected
	copy.FormatID = choice
	copy.Filesize, copy.FilesizeApprox = 0, 0
	if len(selected) == 1 {
		f := selected[0]
		copy.Ext, copy.VCodec, copy.ACodec = f.Ext, f.VCodec, f.ACodec
		copy.Filesize, copy.FilesizeApprox = f.Filesize, f.FilesizeApprox
		return copy, true
	}
	var video, audio videoFormat
	for _, f := range selected {
		if f.VCodec != "" && f.VCodec != "none" && f.ACodec == "none" {
			video = f
		}
		if f.VCodec == "none" && f.ACodec != "" && f.ACodec != "none" {
			audio = f
		}
	}
	if video.FormatID == "" || audio.FormatID == "" {
		return nil, false
	}
	copy.VCodec, copy.ACodec = video.VCodec, audio.ACodec
	if video.Ext == "webm" && (audio.Ext == "webm" || audio.Ext == "opus") {
		copy.Ext = "webm"
	} else if (video.Ext == "mp4" || video.Ext == "m4v") && (audio.Ext == "m4a" || audio.Ext == "mp4") {
		copy.Ext = "mp4"
	} else {
		copy.Ext = "mkv"
	}
	var size int64
	exactSize := true
	for _, f := range selected {
		if f.Filesize > 0 {
			size += f.Filesize
		} else {
			size += f.FilesizeApprox
			exactSize = false
		}
	}
	if exactSize {
		copy.Filesize = size
	} else {
		copy.FilesizeApprox = size
	}
	return copy, true
}

func extractInfo(ctx context.Context, req *base.Request, extra *model.ReqExtra, ctl *controller.Controller, cfg model.Config) (*videoInfo, *model.Inspection, error) {
	args, cleanup, err := requestArgs(req, extra, ctl, cfg)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()
	args = append(args, formatArgs(extra)...)
	args = append(args, "--dump-single-json", "--skip-download", req.URL)
	cmd, commandCleanup, err := mediaCommand(ctx, cfg, req, extra, args)
	if err != nil {
		return nil, nil, err
	}
	defer commandCleanup()
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if strings.Contains(string(exit.Stderr), "403") {
				invalidateInspection(req, cfg)
			}
			return nil, nil, mediaError(exit.Stderr)
		}
		if ctx.Err() != nil {
			return nil, nil, errors.New("媒体解析已取消或超时")
		}
		return nil, nil, errors.New("视频解析工具无法启动，请检查设置中的工具路径")
	}
	var info videoInfo
	if json.Unmarshal(out, &info) != nil {
		return nil, nil, errors.New("invalid media inspection JSON")
	}
	if info.Type == "playlist" || info.Type == "multi_video" || (len(info.Entries) != 0 && string(info.Entries) != "null") {
		return &info, &model.Inspection{Formats: []model.Format{}, Reason: "playlist or multi-video media is unsupported"}, nil
	}
	result := inspectionFromInfo(&info)
	if extra.SourceType == "page" && extra.ExpectedMediaID != "" && info.ID != "" && result.Supported && verifiedYouTubeID(req.URL, extra.ExpectedMediaID) && info.ID != extra.ExpectedMediaID {
		result.Supported = false
		result.Reason = "selected media does not match the requested video"
		result.Formats = []model.Format{}
	}
	return &info, result, nil
}

func verifiedYouTubeID(rawURL, expected string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "youtube.com" || strings.HasSuffix(host, ".youtube.com") {
		return u.Query().Get("v") == expected
	}
	if host == "youtu.be" {
		return strings.TrimPrefix(u.Path, "/") == expected
	}
	return false
}

// Inspect and Fetcher.Resolve share the same extraction and eligibility checks.
func Inspect(ctx context.Context, req *base.Request, ctl *controller.Controller) (*Inspection, error) {
	snapshot, err := cloneRequest(req)
	if err != nil {
		return nil, err
	}
	_, result, err := inspectInfo(ctx, snapshot, ctl, configFor(ctl))
	if err == nil && result.Supported && snapshot.Extra.(*model.ReqExtra).CollectContext {
		result.ContextURLs = discoverContext(ctx, snapshot, ctl, result)
	}
	return result, err
}

func inspectionFromInfo(info *videoInfo) *model.Inspection {
	r := &model.Inspection{Thumbnail: info.Thumbnail, Subtitles: subtitleMetadata(info), ID: info.ID, MediaID: info.ID, SourceURL: info.URL, WebpageURL: info.WebpageURL, ManifestURL: info.ManifestURL, Protocol: info.Protocol, Title: info.Title, Duration: info.Duration, IsLive: info.IsLive || info.LiveStatus == "is_live" || info.LiveStatus == "is_upcoming", Formats: []model.Format{}}
	if info.Type == "playlist" || info.Type == "multi_video" || (len(info.Entries) != 0 && string(info.Entries) != "null") {
		r.Reason = "playlist or multi-video media is unsupported"
		return r
	}
	if r.IsLive {
		r.Reason = "live media is unsupported"
		return r
	}
	if info.HasDRM {
		r.Reason = "DRM media is unsupported"
		return r
	}
	for _, selected := range info.RequestedFormats {
		if selected.HasDRM {
			r.Reason = "DRM media is unsupported"
			return r
		}
	}
	for _, f := range info.Formats {
		if f.HasDRM || f.FormatID == "" {
			continue
		}
		if f.VCodec == "none" && f.ACodec == "none" {
			continue
		}
		container := f.Ext
		if container == "" {
			container = info.Ext
		}
		size := f.Filesize
		if size == 0 {
			size = f.FilesizeApprox
		}
		r.Formats = append(r.Formats, model.Format{ID: f.FormatID, Label: strings.TrimSpace(strings.Join([]string{f.FormatID, f.FormatNote, f.Language}, " ")), Ext: f.Ext, Width: f.Width, Height: f.Height, VCodec: f.VCodec, ACodec: f.ACodec, FileSize: size, Container: container, Protocol: f.Protocol, Language: f.Language, FPS: f.FPS, TBR: f.TBR, ManifestURL: f.ManifestURL, SourceURL: f.URL})
	}
	// yt-dlp can return a selected format without an expanded format list.
	if len(r.Formats) == 0 && info.FormatID != "" {
		r.Formats = append(r.Formats, model.Format{ID: info.FormatID, Ext: info.Ext, VCodec: info.VCodec, ACodec: info.ACodec, Container: info.Ext, Protocol: info.Protocol, ManifestURL: info.ManifestURL, SourceURL: info.URL})
	}
	if strings.Contains(info.FormatID, "+") {
		r.Formats = append(r.Formats, model.Format{ID: info.FormatID, Label: info.FormatID, Ext: info.Ext, VCodec: info.VCodec, ACodec: info.ACodec, Container: info.Ext, Protocol: info.Protocol, ManifestURL: info.ManifestURL, SourceURL: info.URL})
	}
	if len(r.Formats) == 0 {
		r.Reason = "no unprotected downloadable format"
		return r
	}
	r.Supported = true
	return r
}

func formatArgs(extra *model.ReqExtra) []string {
	format := extra.Format
	if format == "" {
		format = "bv*+ba/b"
		if extra.MediaKind == "audio" {
			format = "ba/b"
		}
	}
	args := []string{"-f", format, "-S", "ext:mp4:m4a"}
	if extra.Container == "mp4" || extra.Container == "mkv" || extra.Container == "webm" {
		args = append(args, "--merge-output-format", extra.Container)
	}
	return args
}

func requestArgs(req *base.Request, extra *model.ReqExtra, ctl *controller.Controller, cfg model.Config) ([]string, func(), error) {
	args := []string{"--ignore-config", "--no-playlist", "--abort-on-unavailable-fragments", "--no-cache-dir", "--no-warnings", "--no-check-certificates"}
	if !req.SkipVerifyCert {
		args = args[:len(args)-1]
	}
	args = append(args, "--ffmpeg-location", cfg.FFmpegPath)
	if executablePath(cfg.JsRuntimePath) {
		args = append(args, "--js-runtimes", "node:"+cfg.JsRuntimePath)
	}
	if ctl != nil && ctl.GetProxy != nil {
		proxyFn := ctl.GetProxy(req.Proxy)
		if proxyFn != nil {
			httpReq := &httpRequestForProxy{raw: req.URL}
			proxyURL, err := proxyFn(httpReq.request())
			if err != nil {
				return nil, nil, errors.New("media proxy configuration failed")
			}
			if proxyURL != nil {
				args = append(args, "--proxy", proxyURL.String())
			} else {
				args = append(args, "--proxy", "")
			}
		} else {
			args = append(args, "--proxy", "")
		}
	} else {
		args = append(args, "--proxy", "")
	}
	cleanup := func() {}
	if len(extra.Cookies) != 0 {
		cookiePath, err := writeCookieJar(extra.Cookies, req.URL)
		if err != nil {
			return nil, nil, err
		}
		cleanup = func() { _ = os.Remove(cookiePath) }
		args = append(args, "--cookies", cookiePath)
	}
	return args, cleanup, nil
}

func executablePath(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	stat, err := os.Stat(path)
	return err == nil && stat.Mode().IsRegular() && stat.Mode().Perm()&0111 != 0
}

// Keep the proxy callback's request shape identical to the HTTP fetcher.
type httpRequestForProxy struct{ raw string }

func (r *httpRequestForProxy) request() *http.Request {
	q, _ := http.NewRequest("GET", r.raw, nil)
	return q
}

func cleanEnvironment() []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch strings.ToLower(key) {
		case "http_proxy", "https_proxy", "all_proxy", "no_proxy":
			continue
		}
		env = append(env, item)
	}
	return env
}

func writeCookieJar(cookies []model.Cookie, rawURL string) (string, error) {
	u, _ := url.Parse(rawURL)
	f, err := os.CreateTemp("", "gopeed-media-cookies-*")
	if err != nil {
		return "", errors.New("cannot create media cookie file")
	}
	path := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return "", errors.New("cannot secure media cookie file")
	}
	if _, err := f.WriteString("# Netscape HTTP Cookie File\n"); err != nil {
		return "", errors.New("cannot write media cookie file")
	}
	for _, c := range cookies {
		domain := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		if domain == "" {
			domain = u.Hostname()
		}
		if !c.HostOnly {
			domain = "." + domain
		}
		cookiePath := c.Path
		if cookiePath == "" {
			cookiePath = "/"
		}
		if !strings.HasPrefix(cookiePath, "/") || strings.ContainsAny(domain+c.Name+c.Value+cookiePath, "\r\n\t") {
			continue
		}
		secure := "FALSE"
		if c.Secure {
			secure = "TRUE"
		}
		subdomain := "FALSE"
		if !c.HostOnly {
			subdomain = "TRUE"
		}
		if c.HTTPOnly {
			domain = "#HttpOnly_" + domain
		}
		if _, err := fmt.Fprintf(f, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", domain, subdomain, cookiePath, secure, int64(c.ExpirationDate), c.Name, c.Value); err != nil {
			return "", errors.New("cannot write media cookie file")
		}
	}
	if err := f.Close(); err != nil {
		return "", errors.New("cannot close media cookie file")
	}
	ok = true
	return filepath.Clean(path), nil
}

// Error categories are useful to the user without disclosing signed URLs or credentials.
func mediaError(raw []byte) error {
	message := strings.ToLower(string(raw))
	switch {
	case strings.Contains(message, "403"), strings.Contains(message, "http error 401"), strings.Contains(message, "401 unauthorized"):
		return errors.New("资源登录态或链接已失效，请返回网页重新捕获并更新任务")
	case strings.Contains(message, "drm"), strings.Contains(message, "protected"):
		return errors.New("此资源受保护，无法下载")
	case strings.Contains(message, "no space left"), strings.Contains(message, "disk full"), strings.Contains(message, "errno 28"), strings.Contains(message, "enospc"):
		return errors.New("下载目录磁盘空间不足")
	case strings.Contains(message, "ffmpeg") && strings.Contains(message, "not found"):
		return errors.New("音视频合并工具无法启动，请检查设置中的工具路径")
	default:
		return errors.New("媒体下载失败，请检查网络或返回网页重新捕获资源")
	}
}

func cloneSubtitleMap(src map[string][]subtitleFormat) map[string][]subtitleFormat {
	dst := make(map[string][]subtitleFormat, len(src))
	for k, v := range src {
		dst[k] = append([]subtitleFormat(nil), v...)
	}
	return dst
}

// Limit popup payloads; entries contain metadata only, never signed subtitle URLs.
func subtitleMetadata(info *videoInfo) []model.Subtitle {
	out := []model.Subtitle{}
	for i, tracks := range []map[string][]subtitleFormat{info.Subtitles, info.AutomaticCaptions} {
		keys := make([]string, 0, len(tracks))
		for key := range tracks {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, lang := range keys {
			if len(out) >= 128 {
				return out
			}
			if len(lang) > 64 || len(tracks[lang]) == 0 {
				continue
			}
			sub := model.Subtitle{Language: lang, Name: lang, Automatic: i == 1}
			seen := map[string]bool{}
			for _, f := range tracks[lang] {
				if f.Name != "" && sub.Name == lang {
					sub.Name = f.Name
				}
				if f.Ext != "" && !seen[f.Ext] && len(sub.Formats) < 16 {
					sub.Formats = append(sub.Formats, f.Ext)
					seen[f.Ext] = true
				}
			}
			out = append(out, sub)
		}
	}
	return out
}

func subtitleArgs(extra *model.ReqExtra) ([]string, error) {
	args := []string{"--no-write-subs", "--no-write-auto-subs", "--no-embed-subs"}
	if len(extra.SubtitleLanguages) == 0 {
		return args, nil
	}
	if len(extra.SubtitleLanguages) > 128 {
		return nil, errors.New("too many subtitle languages")
	}
	langs := make([]string, 0, len(extra.SubtitleLanguages))
	for _, lang := range extra.SubtitleLanguages {
		if lang == "" || len(lang) > 64 || strings.ContainsAny(lang, ",/\\\r\n") {
			return nil, errors.New("invalid subtitle language")
		}
		langs = append(langs, "^"+regexp.QuoteMeta(lang)+"$")
	}
	args = append(args, "--write-subs", "--sub-langs", strings.Join(langs, ","), "--sub-format", "srt/vtt/ass/ssa")
	if extra.WriteAutomaticSubtitles {
		args = append(args, "--write-auto-subs")
	}
	if extra.EmbedSubtitles {
		args = append(args, "--embed-subs")
	}
	return args, nil
}

func validateSelectedSubtitles(extra *model.ReqExtra, info *videoInfo) error {
	if _, err := subtitleArgs(extra); err != nil {
		return err
	}
	for _, language := range extra.SubtitleLanguages {
		formats := info.Subtitles[language]
		if len(formats) == 0 && extra.WriteAutomaticSubtitles {
			formats = info.AutomaticCaptions[language]
		}
		if len(formats) == 0 {
			return fmt.Errorf("requested subtitle language %q is unavailable", language)
		}
		supported := false
		for _, format := range formats {
			switch format.Ext {
			case "srt", "vtt", "ass", "ssa":
				supported = true
			}
		}
		if !supported {
			return fmt.Errorf("requested subtitle language %q has unsupported format (supported: srt, vtt, ass, ssa)", language)
		}
	}
	return nil
}

func validateSubtitleFiles(prefix string, languages []string) error {
	for _, language := range languages {
		found := false
		for _, ext := range []string{"srt", "vtt", "ass", "ssa"} {
			stat, err := os.Stat(prefix + "." + language + "." + ext)
			if err == nil && stat.Mode().IsRegular() && stat.Size() > 0 {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("requested subtitle language %q is missing or empty", language)
		}
	}
	return nil
}
