//go:build !windows

package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
	"github.com/GopeedLab/gopeed/pkg/util"
)

func toolFixture(t *testing.T, directory, name, body string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMediaToolStatusMatchesDaemonSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	yt := toolFixture(t, dir, "yt-dlp", "#!/bin/sh\n[ \"$*\" = '--ignore-config --version' ] || exit 1\nprintf '%s\\n' 2026.10.26")
	ff := toolFixture(t, dir, "ffmpeg", "#!/bin/sh\n[ \"$*\" = '-version' ] || exit 1\nprintf '%s\\n' 'ffmpeg version 8.2.1 Copyright (c)'")
	node := toolFixture(t, dir, "node", "#!/bin/sh\n[ \"$*\" = '--version' ] || exit 1\nprintf '%s\\n' v24.9.0")
	stored := map[string]any{"ytDlpPath": "", "ffmpegPath": dir, "jsRuntimePath": node}
	status, err := CheckTools(context.Background(), stored)
	if err != nil {
		t.Fatal(err)
	}
	if status.YtDlp.Path != yt || status.YtDlp.Version != "2026.10.26" ||
		status.YtDlp.State != toolReady || status.YtDlp.Custom || status.Bridge.State != toolUnsupported {
		t.Fatalf("PATH executable and bridge: %+v / %+v", status.YtDlp, status.Bridge)
	}
	if status.FFmpeg.Path != ff || status.FFmpeg.Version != "8.2.1" ||
		status.FFmpeg.State != toolReady || !status.FFmpeg.Custom {
		t.Fatalf("ffmpeg from configured directory: %+v", status.FFmpeg)
	}
	if status.Node.Path != node || status.Node.Version != "v24.9.0" ||
		status.Node.State != toolReady || !status.Node.Enabled {
		t.Fatalf("actual enabled Node: %+v", status.Node)
	}
	ctl := controller.NewController()
	ctl.GetConfig = func(dst any) {
		if err := util.MapToStruct(stored, dst); err != nil {
			t.Fatal(err)
		}
	}
	if cfg := configFor(ctl); cfg.YtDlpPath != "yt-dlp" || cfg.FFmpegPath != dir || enabledNodePath(cfg) != node {
		t.Fatalf("different effective daemon config: %+v", cfg)
	}

	// A configured absolute yt-dlp outranks the daemon PATH.
	custom := toolFixture(t, dir, "selected", "#!/bin/sh\necho 2026.10.20")
	stored["ytDlpPath"], stored["jsRuntimePath"] = custom, "node"
	status, err = CheckTools(context.Background(), stored)
	if err != nil || status.YtDlp.Path != custom || !status.YtDlp.Custom ||
		status.YtDlp.Version != "2026.10.20" || status.Node.Enabled || status.Node.State != toolInactive ||
		status.Node.Path != "" {
		t.Fatalf("explicit yt-dlp and inactive relative Node: %+v (%v)", status, err)
	}

	stored["ytDlpPath"], stored["jsRuntimePath"] = filepath.Join(dir, "missing"), ""
	status, err = CheckTools(context.Background(), stored)
	if err != nil || status.YtDlp.State != toolMissing || status.Node.State != toolNotConfigured {
		t.Fatalf("missing yt-dlp and unconfigured Node: %+v (%v)", status, err)
	}
	if runtime.GOOS == "darwin" && status.YtDlp.DefaultPath != filepath.Join(os.Getenv("HOME"), ".local/bin/yt-dlp") {
		t.Fatalf("reset does not restore Darwin's actual default: %+v", status.YtDlp)
	}
	if _, err := CheckTools(context.Background(), map[string]any{"ytDlpPath": 7}); err == nil {
		t.Fatal("malformed configuration must be a query failure")
	}
}

func TestMediaToolStatusBoundsAndCheckFailures(t *testing.T) {
	dir := t.TempDir()
	slow := toolFixture(t, dir, "yt-dlp", "#!/bin/sh\n/bin/sleep 10")
	noExec := toolFixture(t, dir, "node", "#!/bin/sh\necho v24.9.0")
	if err := os.Chmod(noExec, 0600); err != nil {
		t.Fatal(err)
	}
	noisy := toolFixture(t, dir, "ffmpeg", "#!/bin/sh\nprintf '%s\\n' 'ffmpeg version 8.2.1'; printf '%4500s' 'private output'")
	start := time.Now()
	status, err := CheckTools(context.Background(), model.Config{
		YtDlpPath: slow, FFmpegPath: noisy, JsRuntimePath: noExec,
	})
	if err != nil || time.Since(start) > 4*time.Second {
		t.Fatalf("query should return quickly: %+v (%v)", status, err)
	}
	if status.YtDlp.State != toolTimedOut || status.Node.State != toolNotRunnable ||
		status.Node.Enabled || status.FFmpeg.State != toolUnrecognized || status.FFmpeg.Version != "" {
		t.Fatalf("timeout, permission, and unrecognized output: %+v", status)
	}

	hangingPipe := toolFixture(t, dir, "descendant", "#!/bin/sh\n(sleep 0.5) &\necho 2026.10.26")
	if _, state := readToolVersion(context.Background(), time.Second, hangingPipe, ytdlpVersion); state != toolTimedOut {
		t.Fatalf("inherited stdout must not block the probe: %s", state)
	}
}

func TestMediaToolStatusRunsTheExistingBridgeOffline(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("isolated bridge fixture requires Python 3:", err)
	}
	dir := t.TempDir()
	tool := toolFixture(t, dir, "configured-yt-dlp", "#!"+python+"\nprint('2026.10.10')")
	t.Setenv("PYTHONPATH", dir)
	t.Setenv("PYTHONNOUSERSITE", "1")
	// Mask every external installation, including a package beside the
	// system interpreter. A CLI version cannot satisfy the bridge check.
	writeModule := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		toolFixture(t, filepath.Dir(path), filepath.Base(path), content)
	}
	writeModule("yt_dlp/__init__.py", "raise ImportError('fixture import failure')\n")
	cfg := model.Config{YtDlpPath: tool}
	status := checkBridge(context.Background(), toolTimeout, cfg)
	if status.State != toolIncompatible || status.Interpreter != python {
		t.Fatalf("missing module reported compatible: %+v", status)
	}

	writeModule("yt_dlp/__init__.py", `class YoutubeDL:
    def build_request_director(self): pass
    def process_video_result(self): pass

def main(*args):
    raise AssertionError('the bridge probe must never dispatch media')
`)
	writeModule("yt_dlp/version.py", "__version__ = '2026.10.26'\n")
	writeModule("yt_dlp/networking/__init__.py", "class Request: pass\n")
	writeModule("yt_dlp/networking/_urllib.py", `class UrllibRH:
    def _prepare_headers(self, *args): pass

class HTTPHandler:
    def http_request(self, *args): pass
    def do_open(self, *args): pass
`)
	writeModule("yt_dlp/networking/exceptions.py", "class HTTPError(Exception): pass\nclass TransportError(Exception): pass\n")
	writeModule("yt_dlp/downloader/external.py", "class ExternalFD: pass\n")
	writeModule("yt_dlp/utils.py", "class DownloadError(Exception): pass\ndef parse_m3u8_attributes(*args): pass\n")
	status = checkBridge(context.Background(), toolTimeout, cfg)
	if status.State != toolReady || status.Version != "2026.10.26" || status.Interpreter != python {
		t.Fatalf("same-environment bridge failed to boot: %+v", status)
	}
	writeModule("yt_dlp/networking/_urllib.py", `class UrllibRH:
    def _prepare_headers(self, *args): pass
class HTTPHandler:
    def http_request(self, *args): pass
`)
	if status := checkBridge(context.Background(), toolTimeout, cfg); status.State != toolIncompatible {
		t.Fatalf("module import succeeded but recovery hook was missing: %+v", status)
	}
}
