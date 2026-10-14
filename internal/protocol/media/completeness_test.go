package media

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

func TestPatchSnapshotIdentityAndReinspection(t *testing.T) {
	program, count := fakeInspector(t, `case "$*" in *"-f b"*) selected=b; size=200;; *) selected=a; size=100;; esac
case "$*" in
 *--dump-single-json*) printf '{"id":"one","title":"video","ext":"mp4","format_id":"%s","filesize":%s,"formats":[{"format_id":"a","ext":"mp4","filesize":100},{"format_id":"b","ext":"mp4","filesize":200}]}\n' "$selected" "$size" ;;
 *) sleep 20 ;;
 esac`)
	f := &Fetcher{}
	f.Setup(configuredInspector(program))
	req := &base.Request{URL: "https://example.test/v?sig=one", Extra: &model.ReqExtra{Format: "a", Cookies: []model.Cookie{{Name: "one", Value: "first"}}}, Proxy: &base.RequestProxy{Mode: base.RequestProxyModeNone}}
	opts := &base.Options{Path: t.TempDir()}
	if err := f.Resolve(req, opts); err != nil {
		t.Fatal(err)
	}
	req.Extra.(*model.ReqExtra).Cookies[0].Value = "mutated"
	req.Proxy.Host = "mutated"
	opts.Path = "mutated"
	if f.meta.Req.Extra.(*model.ReqExtra).Cookies[0].Value != "first" || f.meta.Req.Proxy.Host != "" || f.meta.Opts.Path == "mutated" {
		t.Fatal("resolve retained mutable inputs")
	}
	f.downloaded = 50
	f.tracks = map[string]trackProgress{"a": {Downloaded: 50, Total: 100}}
	identity := f.identity
	part := f.meta.SingleFilepath() + ".gopeed-media.mp4.part"
	if err := os.WriteFile(part, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Patch(&base.Request{URL: "https://example.test/v?sig=two"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := f.Patch(&base.Request{URL: "https://example.test/other"}, nil); err == nil {
		t.Fatal("active patch accepted")
	}
	_ = f.Pause()
	if f.identity != identity || f.workID != "" || f.downloaded != 50 {
		t.Fatal("signed refresh lost resume state")
	}
	oldPrefix := f.workPrefix
	if data, err := os.ReadFile(part); err != nil || string(data) != "partial" {
		t.Fatal("signed refresh discarded part")
	}
	incoming := &model.ReqExtra{Format: "b", Cookies: []model.Cookie{{Name: "one", Value: "second"}}}
	if err := f.Patch(&base.Request{Extra: incoming}, nil); err != nil {
		t.Fatal(err)
	}
	incoming.Cookies[0].Value = "mutated"
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	_ = f.Pause()
	if f.workPrefix == oldPrefix || f.workID == "" || f.downloaded != 0 || len(f.tracks) != 0 || f.meta.Res.Size != 200 || f.format != "b" {
		t.Fatalf("stale resumed state: %+v %+v", f.Stats(), f.meta.Res)
	}
	if f.meta.Req.Extra.(*model.ReqExtra).Cookies[0].Value != "second" {
		t.Fatal("patch retained mutable inputs")
	}
	if invocationCount(t, count) < 3 {
		t.Fatal("resume did not inspect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.resolveCancel = cancel
	if err := f.Patch(nil, nil); err == nil {
		t.Fatal("resolving patch accepted")
	}
	_ = ctx
	f.resolveCancel = nil
	old := f.meta.Req.URL
	if err := f.Patch(&base.Request{URL: "file:///bad", Extra: map[string]any{"format": "bad"}}, nil); err == nil || f.meta.Req.URL != old {
		t.Fatal("invalid patch partially applied")
	}
}

func TestSubtitleMetadataOptionsAndCacheIsolation(t *testing.T) {
	var info videoInfo
	if err := json.Unmarshal([]byte(`{"thumbnail":"thumb","subtitles":{"en":[{"ext":"vtt","name":"English"},{"ext":"srt"}]},"automatic_captions":{"zh":[{"ext":"vtt"}]}}`), &info); err != nil {
		t.Fatal(err)
	}
	result := inspectionFromInfo(&info)
	cloned := cloneInspection(result)
	if result.Thumbnail != "thumb" || len(result.Subtitles) != 2 || !result.Subtitles[1].Automatic {
		t.Fatalf("metadata: %+v", result)
	}
	cloned.Subtitles[0].Formats[0] = "mutated"
	if result.Subtitles[0].Formats[0] != "vtt" {
		t.Fatal("cached subtitle alias")
	}
	args, err := subtitleArgs(&model.ReqExtra{WriteAutomaticSubtitles: true, EmbedSubtitles: true})
	if err != nil || strings.Contains(strings.Join(args, " "), "--write-auto-subs") {
		t.Fatal("unselected subtitles enabled")
	}
	args, err = subtitleArgs(&model.ReqExtra{SubtitleLanguages: []string{"en", "zh-Hans"}, WriteAutomaticSubtitles: true, EmbedSubtitles: true})
	if err != nil || !strings.Contains(strings.Join(args, " "), "--embed-subs") {
		t.Fatal("subtitle flags missing")
	}
	req := &base.Request{URL: "https://example.test/v"}
	cfg := model.Config{}
	baseline := inspectionKey(req, &model.ReqExtra{}, cfg)
	for _, extra := range []*model.ReqExtra{{CollectContext: true}, {SubtitleLanguages: []string{"en"}}, {WriteAutomaticSubtitles: true}, {EmbedSubtitles: true}, {ScopedHeaders: []model.HeaderScope{{URL: req.URL, Headers: map[string]string{"Authorization": "Bearer test"}}}}} {
		if inspectionKey(req, extra, cfg) == baseline {
			t.Fatal("new context omitted from cache key")
		}
	}
}

func TestPythonScopesRedirectsAndPrivateContext(t *testing.T) {
	cfg := configFor(nil)
	if _, err := os.Stat(cfg.YtDlpPath); err != nil {
		t.Skip("configured Python yt-dlp unavailable")
	}
	var mu sync.Mutex
	got := map[string]string{}
	cookies := map[string]string{}
	referers := map[string]string{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got["foreign"+r.URL.Path] = r.Header.Get("Authorization")
		cookies["foreign"+r.URL.Path] = r.Header.Get("Cookie")
		referers["foreign"+r.URL.Path] = r.Referer()
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()
	foreign := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path] = r.Header.Get("Authorization")
		cookies[r.URL.Path] = r.Header.Get("Cookie")
		referers[r.URL.Path] = r.Referer()
		mu.Unlock()
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, foreign+"/landing", 302)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer source.Close()
	extra := &model.ReqExtra{SourcePage: "about:srcdoc", Header: map[string]string{"rEfErEr": source.URL + "/page"}, ScopedHeaders: []model.HeaderScope{
		{Origin: source.URL, PathPrefix: "/path/", Headers: map[string]string{"Authorization": "Bearer path"}},
		{URL: source.URL + "/path/exact", Headers: map[string]string{"Authorization": "Bearer exact"}},
		{URL: source.URL + "/redirect", Headers: map[string]string{"Authorization": "Bearer redirect"}},
		{URL: foreign + "/key", Headers: map[string]string{"Authorization": "Bearer key"}},
	}}
	jar, err := writeCookieJar([]model.Cookie{{Domain: "127.0.0.1", HostOnly: true, Path: "/path", Name: "path", Value: "cookie"}}, source.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(jar)
	req := &base.Request{URL: source.URL + "/redirect", Extra: extra}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{jar, source.URL + "/redirect", source.URL + "/path/exact", source.URL + "/path/other", source.URL + "/outside", foreign + "/key"}
	cmd, cleanup, err := mediaCommand(ctx, cfg, req, extra, args)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if strings.Contains(strings.Join(cmd.Args, " "), "Bearer exact") {
		t.Fatal("credential in argv")
	}
	contextPath := cmd.Args[len(cmd.Args)-1]
	stat, err := os.Stat(contextPath)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("insecure context file")
	}
	cmd.Args[2] = strings.Replace(contextWrapper, "yt_dlp.main(context['args'] + ['--downloader', 'native'])", `with yt_dlp.YoutubeDL({'quiet': True, 'proxy': '', 'cookiefile': context['args'][0]}) as ydl:
    for target_url in context['args'][1:]:
        with ydl.urlopen(target_url) as response:
            response.read()`, 1)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper: %v %s", err, out)
	}
	for key, want := range map[string]string{"/redirect": "Bearer redirect", "/path/exact": "Bearer exact", "/path/other": "Bearer path", "/outside": "", "foreign/landing": "", "foreign/key": "Bearer key"} {
		if got[key] != want {
			t.Fatalf("%s scope: %q", key, got[key])
		}
	}
	if cookies["/path/exact"] != "path=cookie" || cookies["foreign/landing"] != "" || cookies["/outside"] != "" {
		t.Fatalf("cookie scope: %v", cookies)
	}
	for _, path := range []string{"/redirect", "/path/exact", "/path/other", "/outside"} {
		if referers[path] != source.URL+"/page" {
			t.Fatalf("same-origin Referer lost on %s: %q", path, referers[path])
		}
	}
	if referers["foreign/landing"] != "" || referers["foreign/key"] != "" {
		t.Fatalf("cross-origin Referer leaked: %v", referers)
	}
	cleanup()
	if _, err := os.Stat(contextPath); !os.IsNotExist(err) {
		t.Fatal("context retained")
	}
}

func TestContextDiscoveryDoesNotFetchKeysAndKeepsPathScope(t *testing.T) {
	keyRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/root.m3u8":
			if r.Header.Get("Authorization") != "Bearer root" {
				http.Error(w, "auth", 401)
				return
			}
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nchild.m3u8\n"))
		case "/child.m3u8":
			if r.Header.Get("Authorization") != "" {
				t.Error("root auth leaked")
			}
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"/secret/key\"\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1,\nseg.ts\n"))
		default:
			keyRequests++
			http.Error(w, "must not fetch", 500)
		}
	}))
	defer server.Close()
	req := &base.Request{URL: server.URL + "/root.m3u8", Extra: &model.ReqExtra{CollectContext: true, ScopedHeaders: []model.HeaderScope{{URL: server.URL + "/root.m3u8", Headers: map[string]string{"Authorization": "Bearer root"}}}}}
	refs := discoverContext(context.Background(), req, nil, &model.Inspection{ManifestURL: req.URL})
	if keyRequests != 0 || len(refs) != 5 || !strings.Contains(strings.Join(refs, " "), "/secret/key") {
		t.Fatalf("refs=%v fetched=%d", refs, keyRequests)
	}
	baseURL, _ := http.NewRequest("GET", server.URL+"/x/main.mpd", nil)
	dash := manifestRefs(`<MPD><Period><BaseURL>nested/</BaseURL><AdaptationSet><SegmentTemplate initialization="init.mp4" media="seg-$Number$.m4s"/></AdaptationSet></Period></MPD>`, baseURL.URL)
	if len(dash) != 3 || !strings.Contains(dash[1].url, "/x/nested/init.mp4") {
		t.Fatalf("DASH refs=%+v", dash)
	}
}

func TestFailedMergeNeverCompletes(t *testing.T) {
	program, _ := fakeInspector(t, `printf 'media-merge:{}\n'; printf 'ffmpeg merge failed\n' >&2; exit 1`)
	f := &Fetcher{}
	f.Setup(configuredInspector(program))
	// A resolved snapshot sufficient to exercise the postprocess completion gate.
	req := &base.Request{URL: "https://example.test/video"}
	f.meta = &fetcher.FetcherMeta{Req: req, Opts: &base.Options{Path: t.TempDir()}, Res: &base.Resource{Files: []*base.FileInfo{{Name: "video.mp4"}}}}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := f.Wait(); err == nil || f.MediaPhase() == "done" {
		t.Fatal("failed merge completed")
	}
	if _, err := os.Stat(f.meta.SingleFilepath()); !os.IsNotExist(err) {
		t.Fatal("failed merge published output")
	}
}

func TestSelectedSubtitleDownloadAndEmbed(t *testing.T) {
	if testing.Short() {
		t.Skip("ffmpeg fixture")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=size=64x64:rate=5", "-t", "1", "-c:v", "libx264", filepath.Join(dir, "clip.mp4")).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "captions.vtt"), []byte("WEBVTT\n\n00:00.000 --> 00:00.900\nhello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	page := `<html><head><title>subtitles</title></head><body><video src="/clip.mp4"><track kind="subtitles" src="/captions.vtt" srclang="en" label="English"></video></body></html>`
	if err := os.WriteFile(filepath.Join(dir, "watch.html"), []byte(page), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer server.Close()
	req := &base.Request{URL: server.URL + "/watch.html", Extra: &model.ReqExtra{SubtitleLanguages: []string{"en"}, EmbedSubtitles: true}}
	f := &Fetcher{}
	f.Setup(nil)
	if err := f.Resolve(req, &base.Options{Path: filepath.Join(dir, "output")}); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		_ = f.Pause()
		t.Fatal("subtitle download timeout")
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "s", "-show_entries", "stream=codec_name", "-of", "csv=p=0", f.Meta().SingleFilepath()).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "mov_text") {
		t.Fatalf("missing embedded subtitle: %v %s", err, out)
	}
	sidecars, _ := filepath.Glob(filepath.Join(dir, "output", "*.en.vtt"))
	if len(sidecars) != 1 || strings.Contains(sidecars[0], "gopeed-media") {
		t.Fatalf("subtitle sidecars not published: %v", sidecars)
	}
}
