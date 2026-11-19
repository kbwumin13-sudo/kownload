package media

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

func TestPastedPageRejectsUnsafeFormats(t *testing.T) {
	const origin = "https://example.test"
	const page = origin + "/watch"
	valid := videoFormat{URL: origin + "/clip.mp4", Protocol: "https", Ext: "mp4", HTTPHeaders: map[string]string{"Referer": page}}
	if !directPageFormat(valid, page, origin) {
		t.Fatal("rejected same-origin progressive format")
	}
	for name, mutate := range map[string]func(*videoFormat){
		"navigation":      func(f *videoFormat) { f.URL = "https://foreign.test/clip.mp4" },
		"credentials":     func(f *videoFormat) { f.URL = "https://user:password@example.test/clip.mp4" },
		"signed query":    func(f *videoFormat) { f.URL += "?credential=secret" },
		"DRM":             func(f *videoFormat) { f.HasDRM = true },
		"HLS":             func(f *videoFormat) { f.Protocol = "m3u8_native" },
		"DASH":            func(f *videoFormat) { f.ManifestURL = origin + "/clip.mpd" },
		"hidden playlist": func(f *videoFormat) { f.URL = origin + "/clip.m3u8" },
		"extra headers":   func(f *videoFormat) { f.HTTPHeaders = map[string]string{"Authorization": "Bearer secret"} },
	} {
		t.Run(name, func(t *testing.T) {
			format := valid
			mutate(&format)
			if directPageFormat(format, page, origin) {
				t.Fatal("accepted unsafe format")
			}
		})
	}
}

func TestPastedPageSingleVideoChecksExpectedID(t *testing.T) {
	program, _ := fakeInspector(t, `printf '%s\n' '{"id":"actual","ext":"mp4","format_id":"best","formats":[{"format_id":"best","ext":"mp4"}]}'`)
	ctl := configuredInspector(program)
	extra := &model.ReqExtra{SourceType: pastedPageSource, ExpectedMediaID: "previous", Format: "best"}
	req := &base.Request{URL: "https://example.test/watch", Extra: extra}
	f := (&FetcherManager{}).Build().(*Fetcher)
	f.Setup(ctl)
	if err := f.Resolve(req, &base.Options{Path: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("stale single video resolved: %v", err)
	}
	extra.ExpectedMediaID = "actual"
	if err := f.Resolve(req, &base.Options{Path: t.TempDir()}); err != nil {
		t.Fatalf("expected single video rejected: %v", err)
	}
}

func TestPastedPageSelectsExactPlayerAndFormat(t *testing.T) {
	if testing.Short() {
		t.Skip("real yt-dlp and FFmpeg fixture")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe", "yt-dlp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	ctl := configuredInspector("yt-dlp")
	cfg := configFor(ctl)
	if interpreter, err := mediaBridgeInterpreter(cfg); err != nil || interpreter == "" {
		t.Skip("requires a Python yt-dlp entrypoint")
	}
	fixtureDir := t.TempDir()
	clip := filepath.Join(fixtureDir, "chosen.mp4")
	matrixFFmpeg(t, "-f", "lavfi", "-i", "color=red:s=64x64:r=10", "-t", "1", "-c:v", "libx264", clip)
	clipBytes, err := os.ReadFile(clip)
	if err != nil {
		t.Fatal(err)
	}
	var foreignHits, directHits, otherHits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		_, _ = w.Write(clipBytes)
	}))
	defer foreign.Close()
	const first = `<video controls src="/wrong.mp4"></video>`
	const second = `<video controls><source src="/chosen.mp4" type="video/mp4" height="64"/><source src="/alternate.mp4" type="video/mp4" height="128"/></video>`
	var mode atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/watch":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<!doctype html><title>Sample players</title>`)
			switch mode.Load() {
			case 1:
				_, _ = io.WriteString(w, second+first)
			case 2:
				_, _ = io.WriteString(w, second+second)
			default:
				_, _ = io.WriteString(w, first+second)
			}
		case "/chosen.mp4":
			directHits.Add(1)
			if referer := r.Header.Get("Referer"); referer != "http://"+r.Host+"/watch" {
				t.Errorf("selected format Referer = %q", referer)
				http.Error(w, "missing page referer", http.StatusForbidden)
				return
			}
			if mode.Load() == 3 {
				http.Redirect(w, r, foreign.URL+"/away.mp4", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			http.ServeContent(w, r, "chosen.mp4", time.Time{}, bytes.NewReader(clipBytes))
		default:
			otherHits.Add(1)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	extra := &model.ReqExtra{SourceType: pastedPageSource, MediaKind: "video"}
	req := &base.Request{URL: server.URL + "/watch", Extra: extra}
	legacy, err := Inspect(ctx, &base.Request{URL: req.URL}, ctl)
	if err != nil || legacy.Supported || len(legacy.Candidates) != 0 {
		t.Fatalf("legacy inspection unexpectedly supported a list: %+v, %v", legacy, err)
	}
	parent, err := Inspect(ctx, req, ctl)
	if err != nil || parent.Supported || parent.Formats == nil || len(parent.Formats) != 0 || len(parent.Candidates) != 2 {
		t.Fatalf("parent inspection: %+v, %v", parent, err)
	}
	manager := &FetcherManager{}
	f := manager.Build().(*Fetcher)
	f.Setup(ctl)
	if err := f.Resolve(req, &base.Options{Path: t.TempDir()}); err == nil {
		t.Fatal("resolved an unselected playlist")
	}

	mode.Store(1)
	extra.PageCandidateID = parent.Candidates[1].ID
	child, err := Inspect(ctx, req, ctl)
	if err != nil || !child.Supported || len(child.Formats) != 2 || child.ID != extra.PageCandidateID {
		t.Fatalf("child inspection after moving player: %+v, %v", child, err)
	}
	for _, format := range child.Formats {
		if strings.HasSuffix(format.SourceURL, "/chosen.mp4") && format.Height == 64 && format.Label == "chosen.mp4" {
			extra.Format, extra.Container = format.ID, format.Container
		}
	}
	if extra.Format == "" {
		t.Fatal("selected player's lower resolution unavailable")
	}
	if err := f.Resolve(req, &base.Options{Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	assertRun := func(fetcher *Fetcher) error {
		t.Helper()
		wait := fetcher.BindRunWait(ctx)
		if err := fetcher.Start(); err != nil {
			t.Fatal(err)
		}
		defer fetcher.Pause()
		return wait()
	}
	data, err := manager.Store(f)
	if err != nil {
		t.Fatal(err)
	}
	_, restore := manager.Restore()
	f = restore(f.Meta(), data).(*Fetcher)
	f.Setup(ctl)
	invalidateInspection(req, cfg)
	if err := assertRun(f); err != nil {
		t.Fatal(err)
	}
	result, err := os.ReadFile(f.Meta().SingleFilepath())
	if err != nil || !bytes.Equal(result, clipBytes) || directHits.Load() == 0 || otherHits.Load() != 0 {
		t.Fatalf("selected restored task: file err=%v, bytes equal=%t, selected requests=%d, other requests=%d", err, bytes.Equal(result, clipBytes), directHits.Load(), otherHits.Load())
	}

	mode.Store(3)
	redirected := manager.Build().(*Fetcher)
	redirected.Setup(ctl)
	if err := redirected.Resolve(req, &base.Options{Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := assertRun(redirected); err == nil || foreignHits.Load() != 0 || otherHits.Load() != 0 {
		t.Fatalf("cross-origin redirect: download err=%v, foreign=%d, other=%d", err, foreignHits.Load(), otherHits.Load())
	}

	mode.Store(2)
	extra.PageCandidateID, extra.Format, extra.Container = "", "", ""
	invalidateInspection(req, cfg)
	duplicate, err := Inspect(ctx, req, ctl)
	if err != nil || len(duplicate.Candidates) != 2 || duplicate.Candidates[0].ID == duplicate.Candidates[1].ID {
		t.Fatalf("identical files in independent players: %+v, %v", duplicate, err)
	}
}
