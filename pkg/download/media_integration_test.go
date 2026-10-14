package download

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	mediaModel "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

type mediaFixture struct {
	root          string
	server        *httptest.Server
	segmentSeen   atomic.Int32
	failSegments  atomic.Bool
	delaySegments atomic.Bool
}

func mediaTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s unavailable: %v", name, err)
	}
	return path
}

func mediaFFmpeg(t *testing.T, binary string, args ...string) {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v: %s", args, err, output)
	}
}

func makeMediaFixture(t *testing.T, ffmpeg string) *mediaFixture {
	t.Helper()
	f := &mediaFixture{root: t.TempDir()}
	for _, sub := range []string{"hls", "dash"} {
		if err := os.Mkdir(filepath.Join(f.root, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	video := []string{"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "8", "-shortest", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac"}
	mediaFFmpeg(t, ffmpeg, append(append([]string{}, video...), "-movflags", "+faststart", filepath.Join(f.root, "sample.mp4"))...)
	key := filepath.Join(f.root, "hls", "key.bin")
	if err := os.WriteFile(key, bytes.Repeat([]byte{0x42}, 16), 0600); err != nil {
		t.Fatal(err)
	}
	keyInfo := filepath.Join(f.root, "hls", "key_info.txt")
	if err := os.WriteFile(keyInfo, []byte("key.bin\n"+key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mediaFFmpeg(t, ffmpeg, append(append([]string{}, video...), "-g", "24", "-hls_time", "1", "-hls_list_size", "0", "-hls_key_info_file", keyInfo, filepath.Join(f.root, "hls", "stream.m3u8"))...)
	mediaFFmpeg(t, ffmpeg, append(append([]string{}, video...), "-seg_duration", "1", "-use_template", "1", "-use_timeline", "1", "-adaptation_sets", "id=0,streams=v id=1,streams=a", "-f", "dash", filepath.Join(f.root, "dash", "stream.mpd"))...)
	fileServer := http.FileServer(http.Dir(f.root))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/protected/") {
			cookie, err := r.Cookie("fixture_session")
			if err != nil || cookie.Value != "synthetic-ok" || r.Referer() != f.server.URL+"/index.html" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/protected")
		}
		if strings.HasSuffix(r.URL.Path, ".ts") || strings.HasSuffix(r.URL.Path, ".m4s") {
			f.segmentSeen.Add(1)
			if f.failSegments.Load() {
				http.Error(w, "synthetic fragment failure", http.StatusServiceUnavailable)
				return
			}
			if f.delaySegments.Load() {
				time.Sleep(350 * time.Millisecond)
			}
		}
		fileServer.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func mediaDownloader(t *testing.T, store Storage, dir, ytdlp, ffmpeg string) *Downloader {
	t.Helper()
	d := NewDownloader(&DownloaderConfig{Storage: store, StorageDir: dir})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	cfg, err := d.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProtocolConfig["media"] = &mediaModel.Config{YtDlpPath: ytdlp, FFmpegPath: ffmpeg, Connections: 2}
	if err := d.PutConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return d
}

func mediaWaitStatus(t *testing.T, d *Downloader, id string, wanted ...base.Status) base.Status {
	t.Helper()
	deadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		task := d.GetTask(id)
		if task == nil {
			t.Fatalf("task %s disappeared", id)
		}
		for _, status := range wanted {
			if task.Status == status {
				return status
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task %s timed out; status=%s", id, d.GetTask(id).Status)
	return ""
}

func mediaProbeAV(t *testing.T, ffprobe, path string) {
	t.Helper()
	cmd := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_type", "-of", "json", path)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var result struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	var audio, video bool
	for _, stream := range result.Streams {
		audio = audio || stream.CodecType == "audio"
		video = video || stream.CodecType == "video"
	}
	if !audio || !video {
		t.Fatalf("single output lacks audio/video: %s", out)
	}
}

func mediaOutput(t *testing.T, d *Downloader, id string) string {
	t.Helper()
	task := d.GetTask(id)
	if task == nil || task.Meta == nil || task.Meta.Res == nil || len(task.Meta.Res.Files) != 1 {
		t.Fatalf("expected one media file: %#v", task)
	}
	return task.Meta.SingleFilepath()
}

func TestMediaDownloaderIntegration(t *testing.T) {
	ytdlp := mediaTool(t, "yt-dlp")
	ffmpeg := mediaTool(t, "ffmpeg")
	ffprobe := mediaTool(t, "ffprobe")
	f := makeMediaFixture(t, ffmpeg)

	t.Run("HLS encrypted fragments and browser state", func(t *testing.T) {
		dir := t.TempDir()
		d := mediaDownloader(t, NewMemStorage(), dir, ytdlp, ffmpeg)
		defer d.Close()
		req := &base.Request{URL: f.server.URL + "/protected/hls/stream.m3u8", Protocol: "media", Extra: &mediaModel.ReqExtra{
			SourcePage: f.server.URL + "/index.html", Cookies: []mediaModel.Cookie{{Name: "fixture_session", Value: "synthetic-ok", Domain: "127.0.0.1", Path: "/"}}, Title: "encrypted",
		}}
		inspection, err := d.InspectMedia(context.Background(), req)
		if err != nil || !inspection.Supported || len(inspection.Formats) == 0 {
			t.Fatalf("media inspection: result=%+v err=%v", inspection, err)
		}
		id, err := d.CreateDirect(req, &base.Options{Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		if got := mediaWaitStatus(t, d, id, base.DownloadStatusDone, base.DownloadStatusError); got != base.DownloadStatusDone {
			t.Fatalf("encrypted HLS ended %s", got)
		}
		mediaProbeAV(t, ffprobe, mediaOutput(t, d, id))
	})

	t.Run("DASH separate tracks", func(t *testing.T) {
		dir := t.TempDir()
		d := mediaDownloader(t, NewMemStorage(), dir, ytdlp, ffmpeg)
		defer d.Close()
		id, err := d.CreateDirect(&base.Request{URL: f.server.URL + "/dash/stream.mpd", Protocol: "media"}, &base.Options{Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		if got := mediaWaitStatus(t, d, id, base.DownloadStatusDone, base.DownloadStatusError); got != base.DownloadStatusDone {
			t.Fatalf("DASH ended %s", got)
		}
		mediaProbeAV(t, ffprobe, mediaOutput(t, d, id))
	})

	t.Run("pause continue and Bolt reopen", func(t *testing.T) {
		f.delaySegments.Store(true)
		defer f.delaySegments.Store(false)
		seenBefore := f.segmentSeen.Load()
		dir := t.TempDir()
		storeDir := t.TempDir()
		d := mediaDownloader(t, NewBoltStorage(storeDir), dir, ytdlp, ffmpeg)
		cfg, err := d.GetConfig()
		if err != nil {
			t.Fatal(err)
		}
		cfg.AutoDeleteMissingFileTasks = true
		if err := d.PutConfig(cfg); err != nil {
			t.Fatal(err)
		}
		id, err := d.CreateDirect(&base.Request{URL: f.server.URL + "/hls/stream.m3u8", Protocol: "media"}, &base.Options{Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		for until := time.Now().Add(10 * time.Second); f.segmentSeen.Load() == seenBefore && time.Now().Before(until); {
			time.Sleep(25 * time.Millisecond)
		}
		if f.segmentSeen.Load() == seenBefore {
			t.Fatal("no HLS fragment requested")
		}
		filter := &TaskFilter{IDs: []string{id}}
		if err := d.Pause(filter); err != nil {
			t.Fatal(err)
		}
		if got := d.GetTask(id).Status; got != base.DownloadStatusPause {
			t.Fatalf("pause status: %s", got)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		d = mediaDownloader(t, NewBoltStorage(storeDir), dir, ytdlp, ffmpeg)
		defer d.Close()
		reopenedCfg, err := d.GetConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !reopenedCfg.AutoDeleteMissingFileTasks {
			t.Fatal("auto-delete setting was not restored")
		}
		if d.GetTask(id) == nil {
			t.Fatal("Bolt task was not restored")
		}
		if err := d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
			t.Fatal(err)
		}
		if got := mediaWaitStatus(t, d, id, base.DownloadStatusDone, base.DownloadStatusError); got != base.DownloadStatusDone {
			t.Fatalf("resumed HLS ended %s", got)
		}
		mediaProbeAV(t, ffprobe, mediaOutput(t, d, id))
	})

	t.Run("failed fragment is never done", func(t *testing.T) {
		f.failSegments.Store(true)
		defer f.failSegments.Store(false)
		dir := t.TempDir()
		storeDir := t.TempDir()
		d := mediaDownloader(t, NewBoltStorage(storeDir), dir, ytdlp, ffmpeg)
		id, err := d.CreateDirect(&base.Request{URL: f.server.URL + "/hls/stream.m3u8", Protocol: "media"}, &base.Options{Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		if got := mediaWaitStatus(t, d, id, base.DownloadStatusDone, base.DownloadStatusError); got != base.DownloadStatusError {
			t.Fatalf("failed fragment became %s", got)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		d = mediaDownloader(t, NewBoltStorage(storeDir), dir, ytdlp, ffmpeg)
		defer d.Close()
		if task := d.GetTask(id); task == nil || task.Status != base.DownloadStatusError {
			t.Fatalf("failed fragment status after reopen: %#v", task)
		}
	})

	t.Run("pause while resolving slow manifest", func(t *testing.T) {
		started := make(chan struct{})
		canceled := make(chan struct{})
		var startOnce, cancelOnce sync.Once
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			startOnce.Do(func() { close(started) })
			select {
			case <-r.Context().Done():
				cancelOnce.Do(func() { close(canceled) })
			case <-time.After(8 * time.Second):
				http.Error(w, "synthetic slow manifest timeout", http.StatusGatewayTimeout)
			}
		}))
		defer server.Close()
		dir := t.TempDir()
		d := mediaDownloader(t, NewMemStorage(), dir, ytdlp, ffmpeg)
		defer d.Close()
		id, err := d.CreateDirect(&base.Request{URL: server.URL + "/slow.m3u8", Protocol: "media"}, &base.Options{Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("media resolver never requested manifest")
		}
		began := time.Now()
		if err := d.Pause(&TaskFilter{IDs: []string{id}}); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(began); elapsed >= 2*time.Second {
			t.Fatalf("Pause during resolve took %s", elapsed)
		}
		select {
		case <-canceled:
		case <-time.After(2 * time.Second):
			t.Fatal("manifest request was not canceled")
		}
		if got := d.GetTask(id).Status; got != base.DownloadStatusPause {
			t.Fatalf("Pause became %s", got)
		}
		time.Sleep(150 * time.Millisecond)
		if got := d.GetTask(id).Status; got != base.DownloadStatusPause {
			t.Fatalf("resolve cancellation changed status to %s", got)
		}
	})

	t.Run("ordinary HTTP routing", func(t *testing.T) {
		dir := t.TempDir()
		d := mediaDownloader(t, NewMemStorage(), dir, ytdlp, ffmpeg)
		defer d.Close()
		id, err := d.CreateDirect(&base.Request{URL: f.server.URL + "/sample.mp4"}, &base.Options{Path: dir})
		if err != nil {
			t.Fatal(err)
		}
		if got := mediaWaitStatus(t, d, id, base.DownloadStatusDone, base.DownloadStatusError); got != base.DownloadStatusDone {
			t.Fatalf("HTTP ended %s", got)
		}
		source, err := os.Open(filepath.Join(f.root, "sample.mp4"))
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		actual, err := os.Open(mediaOutput(t, d, id))
		if err != nil {
			t.Fatal(err)
		}
		defer actual.Close()
		want, err := io.ReadAll(source)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(actual)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("ordinary HTTP download differs from fixture")
		}
	})
}
