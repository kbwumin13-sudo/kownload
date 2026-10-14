package media

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

// Use the ffprobe shipped beside the configured ffmpeg, including on Windows.
func probePath(cfg model.Config) string {
	ffmpeg, err := exec.LookPath(cfg.FFmpegPath)
	if err != nil {
		return ""
	}
	name := "ffprobe"
	if strings.EqualFold(filepath.Ext(ffmpeg), ".exe") {
		name += ".exe"
	}
	return filepath.Join(filepath.Dir(ffmpeg), name)
}

// An explicit intent must be proven against the finished local file before any
// media or subtitle is published. ffprobe cannot open HTTP, HLS or DASH inputs.
func validateFinalTracks(ctx context.Context, cfg model.Config, kind, filename string) error {
	if kind == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, probePath(cfg), "-v", "error", "-protocol_whitelist", "file,pipe", "-format_whitelist", "mov,mpegts,matroska,mp3,aac,wav,flac,ogg,aiff", "-show_entries", "stream=codec_type:stream_disposition=attached_pic", "-of", "json", filename)
	cmd.Env = cleanEnvironment()
	out, err := cmd.Output()
	if err != nil {
		return errors.New("无法验证下载文件的音视频轨道，请检查媒体工具或重新捕获资源")
	}
	var probe struct {
		Streams []struct {
			Kind        string `json:"codec_type"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if json.Unmarshal(out, &probe) != nil {
		return errors.New("无法验证下载文件的音视频轨道")
	}
	video, audio := false, false
	for _, stream := range probe.Streams {
		video = video || stream.Kind == "video" && stream.Disposition.AttachedPic == 0
		audio = audio || stream.Kind == "audio"
	}
	if kind == "video" && !video {
		return errors.New("所选视频资源没有视频轨道，请选择视频流或改为音频下载")
	}
	if kind == "audio" && (!audio || video) {
		return errors.New("所选音频资源不是独立音频，请重新选择音频流")
	}
	return ctx.Err()
}
