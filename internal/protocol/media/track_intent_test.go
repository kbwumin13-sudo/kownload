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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

func TestVideoIntentRejectsAudioFormatWithoutTopLevelCodec(t *testing.T) {
	fixture := videoInfo{ID: "audio", Title: "audio", Ext: "mp4", FormatID: "0", Formats: []videoFormat{{FormatID: "0", Ext: "mp4", VCodec: "none", ACodec: "aac"}}}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	program, _ := fakeInspector(t, "printf '%s\\n' '"+string(data)+"'")
	ctl := configuredInspector(program)
	req := &base.Request{URL: "https://example.test/audio.m3u8", Extra: &model.ReqExtra{MediaKind: "video"}}
	inspection, err := Inspect(context.Background(), req, ctl)
	if err != nil || len(inspection.Formats) != 1 || inspection.Formats[0].VCodec != "none" {
		t.Fatalf("audio inspection: %+v %v", inspection, err)
	}
	f := &Fetcher{}
	f.Setup(ctl)
	if err := f.Resolve(req, &base.Options{Path: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "没有视频轨道") {
		t.Fatalf("known audio format was accepted as video: %v", err)
	}
}

func trackFixture(t *testing.T, video, fragmented bool) string {
	t.Helper()
	if testing.Short() {
		t.Skip("real HLS track fixture")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe", "yt-dlp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	dir := t.TempDir()
	args := []string{}
	if video {
		args = append(args, "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10")
	}
	args = append(args, "-f", "lavfi", "-i", "sine=frequency=440", "-t", "3")
	if video {
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-g", "10")
	}
	args = append(args, "-c:a", "aac", "-f", "hls", "-hls_time", "1", "-hls_list_size", "0")
	if fragmented {
		args = append(args, "-hls_segment_type", "fmp4")
	}
	// The URL deliberately carries no track hint.
	matrixFFmpeg(t, append(args, filepath.Join(dir, "index.m3u8"))...)
	return dir
}

func waitTrackDownload(t *testing.T, f *Fetcher) error {
	t.Helper()
	if err := f.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- f.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(25 * time.Second):
		_ = f.Pause()
		t.Fatal("track download timeout")
		return nil
	}
}

func TestRealHLSTrackIntent(t *testing.T) {
	for _, tc := range []struct {
		name              string
		video, fragmented bool
	}{{"audio_TS", false, false}, {"audio_fMP4_init", false, true}, {"video_audio_TS", true, false}, {"video_audio_fMP4_init", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := trackFixture(t, tc.video, tc.fragmented)
			server := httptest.NewServer(http.FileServer(http.Dir(dir)))
			defer server.Close()
			req := &base.Request{URL: server.URL + "/index.m3u8"}
			ctl := configuredInspector("yt-dlp")
			result, err := Inspect(context.Background(), req, ctl)
			if err != nil || !result.Supported || len(result.Formats) != 1 {
				t.Fatalf("inspect: %+v %v", result, err)
			}
			format := result.Formats[0]
			if format.ACodec != "aac" || (tc.video && format.VCodec != "h264") || (!tc.video && format.VCodec != "none") {
				t.Errorf("actual tracks absent from metadata: %+v", format)
			}
			kind := "audio"
			if tc.video {
				kind = "video"
			}
			f := &Fetcher{}
			f.Setup(ctl)
			req.Extra = map[string]any{"mediaKind": kind, "format": format.ID}
			if err := f.Resolve(req, &base.Options{Path: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			if err := waitTrackDownload(t, f); err != nil {
				t.Fatal(err)
			}
			if f.MediaPhase() != "done" {
				t.Fatal("matching intent did not complete")
			}
			t.Logf("%s intent completed; metadata vcodec=%q acodec=%q", kind, format.VCodec, format.ACodec)
			if !tc.video {
				wrong := &Fetcher{}
				wrong.Setup(ctl)
				req.Extra = map[string]any{"mediaKind": "video", "format": format.ID}
				err := wrong.Resolve(req, &base.Options{Path: t.TempDir()})
				if err == nil {
					err = waitTrackDownload(t, wrong)
				}
				if err == nil || wrong.MediaPhase() == "done" {
					t.Fatal("audio-only resource satisfied video intent")
				}
				if wrong.Meta().Res != nil {
					if _, err := os.Stat(wrong.Meta().SingleFilepath()); !os.IsNotExist(err) {
						t.Fatal("video intent published audio")
					}
				}
			}
		})
	}
}

func TestRealHLSFinalTrackGateWithUnknownMetadata(t *testing.T) {
	dir := trackFixture(t, false, true)
	server := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer server.Close()
	f := &Fetcher{}
	f.Setup(configuredInspector("yt-dlp"))
	// A persisted/previously resolved task cannot rely on inspection metadata.
	f.meta = &fetcher.FetcherMeta{Req: &base.Request{URL: server.URL + "/index.m3u8", Extra: map[string]any{"mediaKind": "video"}}, Opts: &base.Options{Path: t.TempDir()}, Res: &base.Resource{Files: []*base.FileInfo{{Name: "expected-video.mp4"}}}}
	if err := waitTrackDownload(t, f); err == nil {
		t.Fatal("unknown metadata published audio as video")
	}
	if f.MediaPhase() == "done" {
		t.Fatal("mismatch marked done")
	}
	if _, err := os.Stat(f.Meta().SingleFilepath()); !os.IsNotExist(err) {
		t.Fatal("mismatch published file")
	}
}

func TestUnknownTrackMetadataAndSelectedFallback(t *testing.T) {
	for _, raw := range []string{
		`{"format_id":"0","ext":"mp4","formats":[{"format_id":"0","ext":"mp4","vcodec":"","acodec":""}]}`,
		`{"format_id":"0","ext":"mp4","formats":[{"format_id":"0","ext":"mp4","vcodec":"unknown","acodec":"unknown"}]}`,
	} {
		var info videoInfo
		if err := json.Unmarshal([]byte(raw), &info); err != nil {
			t.Fatal(err)
		}
		result := inspectionFromInfo(&info)
		if len(result.Formats) != 1 || result.Formats[0].VCodec != info.Formats[0].VCodec || result.Formats[0].ACodec != info.Formats[0].ACodec {
			t.Fatal("unknown tracks invented")
		}
	}
	result := inspectionFromInfo(&videoInfo{FormatID: "a", Ext: "mp4", VCodec: "none", ACodec: "aac"})
	if result.Formats[0].VCodec != "none" || !strings.HasPrefix(result.Formats[0].ACodec, "aac") {
		t.Fatal("selected track codecs discarded")
	}
}

func TestRealHLSProbePreservesCredentialScopes(t *testing.T) {
	dir := trackFixture(t, false, true)
	var mu sync.Mutex
	seen := map[string][]string{}
	files := http.FileServer(http.Dir(dir))
	foreignServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen["foreign"+r.URL.Path] = append(seen["foreign"+r.URL.Path], r.Header.Get("Authorization")+"|"+r.Header.Get("Cookie")+"|"+r.Referer())
		mu.Unlock()
		files.ServeHTTP(w, r)
	}))
	defer foreignServer.Close()
	foreign := strings.Replace(foreignServer.URL, "127.0.0.1", "localhost", 1)
	manifest, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.ReplaceAll(string(manifest), `URI="init.mp4"`, `URI="redirect-init"`))
	manifest = []byte(strings.ReplaceAll(string(manifest), "index0.m4s", foreign+"/index0.m4s"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = append(seen[r.URL.Path], r.Header.Get("Authorization")+"|"+r.Header.Get("Cookie"))
		mu.Unlock()
		switch r.URL.Path {
		case "/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write(manifest)
		case "/redirect-init":
			http.Redirect(w, r, foreign+"/init.mp4", http.StatusFound)
		default:
			files.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	req := &base.Request{URL: server.URL + "/index.m3u8", Extra: &model.ReqExtra{
		SourcePage: server.URL + "/page",
		Cookies:    []model.Cookie{{Name: "fixture", Value: "private", Domain: "127.0.0.1", Path: "/", HostOnly: true}},
		ScopedHeaders: []model.HeaderScope{
			{URL: server.URL + "/index.m3u8", Headers: map[string]string{"Authorization": "Bearer manifest"}},
			{URL: foreign + "/init.mp4", Headers: map[string]string{"Authorization": "Bearer init"}},
		},
	}}
	result, err := Inspect(context.Background(), req, configuredInspector("yt-dlp"))
	if err != nil || !result.Supported || result.Formats[0].VCodec != "none" || result.Formats[0].ACodec != "aac" {
		t.Fatalf("scoped sample: %+v %v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for path, values := range seen {
		for _, value := range values {
			want := ""
			switch path {
			case "/index.m3u8":
				want = "Bearer manifest|fixture=private"
			case "/redirect-init":
				want = "|fixture=private"
			case "foreign/init.mp4":
				want = "Bearer init||"
			case "foreign/index0.m4s":
				want = "||"
			default:
				t.Fatalf("unexpected probe request %s", path)
			}
			if value != want {
				t.Fatalf("%s scope: %q", path, value)
			}
		}
	}
	if len(seen["foreign/init.mp4"]) != 1 || len(seen["foreign/index0.m4s"]) != 1 {
		t.Fatal("sample did not exercise init redirect and foreign media fragment")
	}
}

func TestFinalProbeCannotFetchPlaylistResources(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "media.mp4")
	if err := os.WriteFile(file, []byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\n"+server.URL+"/private.ts\n#EXT-X-ENDLIST\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateFinalTracks(context.Background(), configFor(nil), "video", file); err == nil {
		t.Fatal("playlist accepted as local final file")
	}
	if requests.Load() != 0 {
		t.Fatal("ffprobe fetched remote playlist dependency")
	}
}

func TestHLSUnrecognizedSampleRemainsUnknown(t *testing.T) {
	if testing.Short() {
		t.Skip("real yt-dlp inspection")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe", "yt-dlp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	var sampleRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.m3u8" {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nunknown.ts\n#EXT-X-ENDLIST\n"))
		} else {
			sampleRequests.Add(1)
			_, _ = w.Write([]byte("unrecognized media bytes"))
		}
	}))
	defer server.Close()
	result, err := Inspect(context.Background(), &base.Request{URL: server.URL + "/index.m3u8"}, configuredInspector("yt-dlp"))
	if err != nil || !result.Supported || len(result.Formats) != 1 {
		t.Fatalf("unknown inspection: %+v %v", result, err)
	}
	if sampleRequests.Load() != 1 || result.Formats[0].VCodec != "" || result.Formats[0].ACodec != "" {
		t.Fatalf("probe failure invented tracks: %+v", result.Formats[0])
	}
}
