package media

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
)

func matrixFFmpeg(t *testing.T, args ...string) {
	t.Helper()
	args = append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("matrix fixture: %v %s", err, out)
	}
}

func matrixBox(t *testing.T, path, kind string) (int, int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		if size < 8 || off+size > len(data) {
			t.Fatal("invalid MP4 box")
		}
		if string(data[off+4:off+8]) == kind {
			return off, off + size - 1
		}
		off += size
	}
	t.Fatalf("missing %s box", kind)
	return 0, 0
}

func TestProtocolMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("real FFmpeg/yt-dlp protocol matrix")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe", "yt-dlp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	for _, kind := range []string{"HLS_fMP4", "HLS_byte_range", "DASH_SegmentBase"} {
		t.Run(kind, func(t *testing.T) {
			source := t.TempDir()
			video := []string{"-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "3", "-c:v", "libx264", "-preset", "ultrafast", "-g", "10", "-sc_threshold", "0", "-c:a", "aac"}
			manifest := "index.m3u8"
			switch kind {
			case "HLS_fMP4":
				matrixFFmpeg(t, append(video, "-f", "hls", "-hls_time", "1", "-hls_list_size", "0", "-hls_segment_type", "fmp4", filepath.Join(source, manifest))...)
			case "HLS_byte_range":
				matrixFFmpeg(t, append(video, "-f", "hls", "-hls_time", "1", "-hls_list_size", "0", "-hls_flags", "single_file", filepath.Join(source, manifest))...)
			case "DASH_SegmentBase":
				manifest = "index.mpd"
				vpath, apath := filepath.Join(source, "video.mp4"), filepath.Join(source, "audio.m4a")
				matrixFFmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=10", "-t", "3", "-c:v", "libx264", "-g", "10", "-movflags", "+dash+global_sidx", vpath)
				matrixFFmpeg(t, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "3", "-c:a", "aac", "-movflags", "+dash+global_sidx", apath)
				vs, ve := matrixBox(t, vpath, "sidx")
				_, vi := matrixBox(t, vpath, "moov")
				as, ae := matrixBox(t, apath, "sidx")
				_, ai := matrixBox(t, apath, "moov")
				mpd := fmt.Sprintf(`<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT3S" minBufferTime="PT1S"><Period duration="PT3S"><AdaptationSet mimeType="video/mp4" contentType="video"><Representation id="v" bandwidth="200000" width="160" height="90" codecs="avc1.42e00b"><BaseURL>video.mp4</BaseURL><SegmentBase indexRange="%d-%d"><Initialization range="0-%d"/></SegmentBase></Representation></AdaptationSet><AdaptationSet mimeType="audio/mp4" contentType="audio" lang="en"><Representation id="a" bandwidth="64000" codecs="mp4a.40.2" audioSamplingRate="44100"><BaseURL>audio.m4a</BaseURL><SegmentBase indexRange="%d-%d"><Initialization range="0-%d"/></SegmentBase></Representation></AdaptationSet></Period></MPD>`, vs, ve, vi, as, ae, ai)
				if err := os.WriteFile(filepath.Join(source, manifest), []byte(mpd), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var ranges atomic.Int64
			files := http.FileServer(http.Dir(source))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "" {
					ranges.Add(1)
				}
				files.ServeHTTP(w, r)
			}))
			defer server.Close()
			f := &Fetcher{}
			f.Setup(nil)
			req := &base.Request{URL: server.URL + "/" + manifest, Proxy: &base.RequestProxy{Mode: base.RequestProxyModeNone}}
			if err := f.Resolve(req, &base.Options{Path: t.TempDir()}); err != nil {
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
			case <-time.After(30 * time.Second):
				_ = f.Pause()
				t.Fatal("matrix download timeout")
			}
			data, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type:format=duration", "-of", "json", f.Meta().SingleFilepath()).Output()
			if err != nil {
				t.Fatal(err)
			}
			var probe struct {
				Streams []struct {
					Kind string `json:"codec_type"`
				}
				Format struct {
					Duration string `json:"duration"`
				}
			}
			if err = json.Unmarshal(data, &probe); err != nil {
				t.Fatal(err)
			}
			tracks := map[string]bool{}
			for _, s := range probe.Streams {
				tracks[s.Kind] = true
			}
			duration, _ := strconv.ParseFloat(probe.Format.Duration, 64)
			if !tracks["audio"] || !tracks["video"] || duration < 2.8 || duration > 3.3 {
				t.Fatalf("incomplete output %s", data)
			}
			if kind == "HLS_byte_range" && ranges.Load() == 0 {
				t.Fatal("byte-range fixture did not exercise ranged requests")
			}
			t.Logf("audio/video complete, %.3fs, %d ranged requests", duration, ranges.Load())
		})
	}
}
