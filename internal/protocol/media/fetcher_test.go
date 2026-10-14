package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

func TestInspectRejectsLiveAndDRM(t *testing.T) {
	for _, info := range []*videoInfo{
		{Title: "live", IsLive: true, Formats: []videoFormat{{FormatID: "hls", Ext: "mp4"}}},
		{Title: "drm", HasDRM: true, Formats: []videoFormat{{FormatID: "dash", Ext: "mp4"}}},
		{Title: "protected formats", Formats: []videoFormat{{FormatID: "dash", Ext: "mp4", HasDRM: true}}},
	} {
		result := inspectionFromInfo(info)
		if result.Supported || result.Reason == "" {
			t.Fatalf("accepted unsupported media: %+v", result)
		}
	}
}

func TestCookieJarAndHeaders(t *testing.T) {
	req := &base.Request{URL: "http://media.example.test/video", Proxy: &base.RequestProxy{Mode: base.RequestProxyModeNone}}
	extra := &model.ReqExtra{Header: map[string]string{"Authorization": "secret", "Cookie": "secret", "User-Agent": "agent"}, Cookies: []model.Cookie{{Name: "session", Value: "value", Domain: "media.example.test", Path: "/video", HostOnly: true, HTTPOnly: true}}}
	args, cleanup, err := requestArgs(req, extra, controller.NewController(), model.Config{FFmpegPath: "ffmpeg"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "secret") || strings.Contains(joined, "User-Agent:agent") || !strings.Contains(joined, "--proxy ") {
		t.Fatalf("unexpected CLI args: %v", args)
	}
	for i, arg := range args {
		if arg != "--cookies" {
			continue
		}
		stat, err := os.Stat(args[i+1])
		if err != nil {
			t.Fatal(err)
		}
		if stat.Mode().Perm() != 0600 {
			t.Fatalf("cookie permissions: %v", stat.Mode())
		}
		body, err := os.ReadFile(args[i+1])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "#HttpOnly_media.example.test\tFALSE\t/video") {
			t.Fatalf("cookie domain/path lost: %q", body)
		}
		return
	}
	t.Fatal("missing cookie jar")
}

func TestLocalHLSAndDASHDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("ffmpeg fixture")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("yt-dlp"); err != nil {
		t.Skip("yt-dlp unavailable")
	}
	for _, format := range []struct {
		name, ext string
		args      []string
	}{
		{"hls", "m3u8", []string{"-f", "hls", "-hls_time", "1", "-hls_list_size", "0"}},
		{"dash", "mpd", []string{"-f", "dash", "-seg_duration", "1", "-use_template", "1", "-use_timeline", "1"}},
	} {
		t.Run(format.name, func(t *testing.T) {
			dir := t.TempDir()
			manifest := filepath.Join(dir, "index."+format.ext)
			args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10", "-f", "lavfi", "-i", "sine=frequency=440", "-t", "3", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac"}
			args = append(args, format.args...)
			args = append(args, manifest)
			if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, out)
			}
			server := httptest.NewServer(http.FileServer(http.Dir(dir)))
			defer server.Close()
			req := &base.Request{URL: server.URL + "/index." + format.ext, Proxy: &base.RequestProxy{Mode: base.RequestProxyModeNone}}
			ctl := controller.NewController()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			inspected, err := Inspect(ctx, req, ctl)
			if err != nil || !inspected.Supported || len(inspected.Formats) == 0 {
				t.Fatalf("inspect: %+v %v", inspected, err)
			}
			f := (&FetcherManager{}).Build().(*Fetcher)
			f.Setup(ctl)
			if err := f.Resolve(req, &base.Options{Path: filepath.Join(dir, "output")}); err != nil {
				t.Fatal(err)
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- f.Wait() }()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				_ = f.Close()
				t.Fatal("download timeout")
			}
			stat, err := os.Stat(f.Meta().SingleFilepath())
			if err != nil || stat.Size() == 0 {
				t.Fatalf("final media: %v %v", stat, err)
			}
			if f.MediaPhase() != "done" {
				t.Fatalf("phase: %s", f.MediaPhase())
			}
			if format.name == "dash" && len(f.tracks) < 2 {
				t.Fatalf("DASH did not report separate track progress: %+v", f.tracks)
			}
		})
	}
}

func TestAudioOnlyHLSAndDASHDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("ffmpeg fixture")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe", "yt-dlp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	for _, format := range []struct {
		name, ext string
		args      []string
	}{
		{"hls", "m3u8", []string{"-f", "hls", "-hls_time", "1", "-hls_list_size", "0"}},
		{"dash", "mpd", []string{"-f", "dash", "-seg_duration", "1", "-use_template", "1", "-use_timeline", "1"}},
	} {
		t.Run(format.name, func(t *testing.T) {
			dir := t.TempDir()
			manifest := filepath.Join(dir, "audio."+format.ext)
			args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440", "-t", "3", "-c:a", "aac", "-b:a", "96k"}
			args = append(args, format.args...)
			args = append(args, manifest)
			if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, out)
			}
			server := httptest.NewServer(http.FileServer(http.Dir(dir)))
			defer server.Close()
			req := &base.Request{URL: server.URL + "/audio." + format.ext, Proxy: &base.RequestProxy{Mode: base.RequestProxyModeNone}}
			ctl := controller.NewController()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			info, inspected, err := inspectInfo(ctx, req, ctl, configFor(ctl))
			if err != nil || !inspected.Supported {
				t.Fatalf("audio inspect: %+v %+v %v", info, inspected, err)
			}
			if info.Ext == "m4a" {
				req.Extra = &model.ReqExtra{Container: "m4a"}
			}
			f := (&FetcherManager{}).Build().(*Fetcher)
			f.Setup(ctl)
			if err := f.Resolve(req, &base.Options{Path: filepath.Join(dir, "out")}); err != nil {
				t.Fatal(err)
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- f.Wait() }()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				_ = f.Close()
				t.Fatal("audio download timeout")
			}
			target := f.Meta().SingleFilepath()
			stat, err := os.Stat(target)
			if err != nil || stat.Size() == 0 {
				t.Fatalf("audio output: %v %v", stat, err)
			}
			if got := f.Progress().TotalDownloaded(); got != stat.Size() {
				t.Fatalf("progress %d != actual %d", got, stat.Size())
			}
			if f.Meta().Res.Size != stat.Size() {
				t.Fatalf("resource size %d != actual %d", f.Meta().Res.Size, stat.Size())
			}
			probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", target).Output()
			if err != nil || strings.Contains(string(probe), "video") || !strings.Contains(string(probe), "audio") {
				t.Fatalf("not audio only: %q %v", probe, err)
			}
		})
	}
}

func TestM4AContainerAndTrackProgress(t *testing.T) {
	info := &videoInfo{Ext: "m4a", VCodec: "none", ACodec: "mp4a.40.2", FormatID: "audio", Formats: []videoFormat{{FormatID: "audio", Ext: "m4a", VCodec: "none", ACodec: "mp4a.40.2"}}}
	if err := validateContainer("m4a", info); err != nil {
		t.Fatal(err)
	}
	if got := formatArgs(&model.ReqExtra{Container: "m4a"}); strings.Contains(strings.Join(got, " "), "--merge-output-format") {
		t.Fatalf("audio container passed as merge argument: %v", got)
	}
	if err := validateContainer("m4a", &videoInfo{Ext: "m4a", VCodec: "avc1"}); err == nil {
		t.Fatal("video accepted as m4a")
	}
	if err := validateContainer("mp4", &videoInfo{Ext: "webm", RequestedFormats: []videoFormat{{VCodec: "vp9", ACodec: "none"}, {VCodec: "none", ACodec: "opus"}}}); err == nil {
		t.Fatal("incompatible MP4 merge accepted")
	}
	f := &Fetcher{}
	f.acceptProgress("\"video\"\t{\"downloaded_bytes\":10,\"total_bytes_estimate\":200.0}")
	if got := f.Progress().TotalDownloaded(); got != 10 {
		t.Fatalf("fractional estimate discarded live progress: %d", got)
	}
	f.acceptProgress("\"video\"\t{\"downloaded_bytes\":100,\"total_bytes\":200}")
	f.acceptProgress("\"audio\"\t{\"downloaded_bytes\":30,\"total_bytes\":50}")
	f.acceptProgress("\"video\"\t{\"downloaded_bytes\":80,\"total_bytes\":200}")
	stats := f.Stats().(*model.Stats)
	if stats.Downloaded != 130 || stats.Total != 250 {
		t.Fatalf("track progress was not accumulated: %+v", stats)
	}
	saved, err := (&FetcherManager{}).Store(f)
	if err != nil {
		t.Fatal(err)
	}
	_, restore := (&FetcherManager{}).Restore()
	other := restore(&fetcher.FetcherMeta{}, saved).(*Fetcher)
	other.acceptProgress("\"audio\"\t{\"downloaded_bytes\":40,\"total_bytes\":50}")
	if got := other.Progress().TotalDownloaded(); got != 140 {
		t.Fatalf("restored tracks lost: %d", got)
	}
}

func TestDirectM4AContainerDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("ffmpeg fixture")
	}
	for _, tool := range []string{"ffmpeg", "yt-dlp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.m4a")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440", "-t", "2", "-c:a", "aac", source).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer server.Close()
	req := &base.Request{URL: server.URL + "/source.m4a", Extra: &model.ReqExtra{Container: "m4a"}, Proxy: &base.RequestProxy{Mode: base.RequestProxyModeNone}}
	ctl := controller.NewController()
	f := (&FetcherManager{}).Build().(*Fetcher)
	f.Setup(ctl)
	if err := f.Resolve(req, &base.Options{Path: filepath.Join(dir, "out")}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(f.Meta().SingleFilepath(), ".m4a") {
		t.Fatalf("wrong extension: %s", f.Meta().SingleFilepath())
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- f.Wait() }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		_ = f.Close()
		t.Fatal("m4a download timeout")
	}
	stat, err := os.Stat(f.Meta().SingleFilepath())
	if err != nil || stat.Size() == 0 {
		t.Fatalf("m4a output: %v %v", stat, err)
	}
}

func TestDownloadErrorsUseRedactedCategories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix executable fixture")
	}
	for _, test := range []struct{ name, detail, want string }{
		{"forbidden", "HTTP Error 403: Forbidden https://example.test/?token=secret", "登录态"},
		{"disk", "[Errno 28] No space left on device /private/secret", "磁盘空间不足"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			program := filepath.Join(dir, "fake-yt-dlp")
			if err := os.WriteFile(program, []byte("#!/bin/sh\necho '"+test.detail+"' >&2\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			ctl := controller.NewController()
			ctl.GetConfig = func(v any) { v.(*model.Config).YtDlpPath = program }
			f := (&FetcherManager{}).Build().(*Fetcher)
			f.Setup(ctl)
			f.meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "http://example.test/video"}, Res: &base.Resource{Files: []*base.FileInfo{{Name: "video.mp4"}}}, Opts: &base.Options{Path: dir}}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- f.Wait() }()
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "secret") {
					t.Fatalf("error category/leak: %v", err)
				}
			case <-time.After(5 * time.Second):
				_ = f.Close()
				t.Fatal("error capture timeout")
			}
		})
	}
}

func TestMediaErrorCategoriesAreRedacted(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{"ERROR: HTTP Error 403: Forbidden https://example.test/?token=secret", "登录态"},
		{"ERROR: [Errno 28] No space left on device /private/secret", "磁盘空间不足"},
	} {
		got := mediaError([]byte(test.raw)).Error()
		if !strings.Contains(got, test.want) || strings.Contains(got, "secret") {
			t.Fatalf("category/leak: %q", got)
		}
	}
}

func TestPauseStoreRestorePatchAndClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process group fixture")
	}
	dir := t.TempDir()
	program := filepath.Join(dir, "fake-yt-dlp")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctl := controller.NewController()
	ctl.GetConfig = func(v any) { v.(*model.Config).YtDlpPath = program }
	f := (&FetcherManager{}).Build().(*Fetcher)
	f.Setup(ctl)
	f.meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "http://example.test/video", Extra: &model.ReqExtra{}}, Res: &base.Resource{Files: []*base.FileInfo{{Name: "video.mp4"}}}, Opts: &base.Options{Path: dir}}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := f.Pause(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.done:
		t.Fatal("pause notified Wait as completion")
	default:
	}
	f.meta.Req.Extra.(*model.ReqExtra).Header = map[string]string{"User-Agent": "first"}
	part := f.Meta().SingleFilepath() + ".gopeed-media.mp4.part"
	if err := os.WriteFile(part, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Patch(&base.Request{URL: "http://example.test/new", Extra: &model.ReqExtra{Header: map[string]string{"Referer": "new"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if f.meta.Req.Extra.(*model.ReqExtra).Header["User-Agent"] != "" || f.meta.Req.Extra.(*model.ReqExtra).Header["Referer"] != "new" || f.meta.Req.URL != "http://example.test/new" {
		t.Fatal("patch did not replace explicit headers and update URL")
	}
	manager := &FetcherManager{}
	saved, err := manager.Store(f)
	if err != nil {
		t.Fatal(err)
	}
	_, restore := manager.Restore()
	other := restore(f.Meta(), saved).(*Fetcher)
	other.Setup(ctl)
	if other.Meta().Req.URL != "http://example.test/new" {
		t.Fatal("restore lost patched URL")
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf("close left task cache: %v", err)
	}
}
