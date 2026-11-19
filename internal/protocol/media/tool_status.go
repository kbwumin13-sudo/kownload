package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
	"github.com/GopeedLab/gopeed/pkg/util"
)

type toolState string

const (
	toolReady         toolState = "ready"
	toolMissing       toolState = "missing"
	toolNotRunnable   toolState = "not_runnable"
	toolTimedOut      toolState = "timed_out"
	toolNotConfigured toolState = "not_configured"
	toolInactive      toolState = "inactive"
	toolUnrecognized  toolState = "unrecognized"
	toolUnavailable   toolState = "unavailable"
	toolUnsupported   toolState = "unsupported"
	toolIncompatible  toolState = "incompatible"
)

type toolStatus struct {
	State       toolState `json:"state"`
	Setting     string    `json:"setting"`
	DefaultPath string    `json:"defaultPath"`
	Path        string    `json:"path"`
	Version     string    `json:"version"`
	Enabled     bool      `json:"enabled"`
	Custom      bool      `json:"custom"`
	Fallback    bool      `json:"fallback"`
}

type bridgeStatus struct {
	State       toolState `json:"state"`
	Interpreter string    `json:"interpreter"`
	Version     string    `json:"version"`
}

type ToolStatus struct {
	YtDlp  toolStatus   `json:"ytDlp"`
	FFmpeg toolStatus   `json:"ffmpeg"`
	Node   toolStatus   `json:"node"`
	Bridge bridgeStatus `json:"bridge"`
}

const (
	toolTimeout   = 2 * time.Second
	maxToolPath   = 4096
	maxToolOutput = 4096
)

var (
	ytdlpVersion  = regexp.MustCompile(`^([0-9]{4}\.[0-9]{2}\.[0-9]{2}(?:[._+-][0-9A-Za-z]+){0,5})$`)
	ffmpegVersion = regexp.MustCompile(`^ffmpeg version ([0-9A-Za-z][0-9A-Za-z._+~:-]{0,95})(?:\s|$)`)
	nodeVersion   = regexp.MustCompile(`^(v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)$`)
)

// CheckTools decodes the same persisted protocol config and applies the same
// defaults as the media fetcher. No request data is part of the probes.
func CheckTools(ctx context.Context, stored any) (ToolStatus, error) {
	defaults := configDefaults()
	raw := defaults
	if err := util.MapToStruct(stored, &raw); err != nil {
		return ToolStatus{}, err
	}
	for _, path := range []string{raw.YtDlpPath, raw.FFmpegPath, raw.JsRuntimePath} {
		if len(path) > maxToolPath || strings.ContainsRune(path, 0) {
			return ToolStatus{}, errors.New("invalid media tool configuration")
		}
	}
	cfg := effectiveConfig(raw)
	defaults = effectiveConfig(defaults)
	var result ToolStatus
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		info := newToolStatus(raw.YtDlpPath, defaults.YtDlpPath)
		info.Path, info.State = executableToolPath(cfg.YtDlpPath)
		info.Enabled = info.State == toolReady
		if info.Enabled {
			info.Version, info.State = readToolVersion(ctx, toolTimeout, info.Path, ytdlpVersion, "--ignore-config", "--version")
		}
		result.YtDlp = info
	}()
	go func() {
		defer wg.Done()
		info := newToolStatus(raw.FFmpegPath, defaults.FFmpegPath)
		info.Fallback = info.Custom && raw.FFmpegPath != cfg.FFmpegPath
		info.Path, info.State = ffmpegToolPath(cfg.FFmpegPath)
		info.Enabled = info.State == toolReady
		if info.Enabled {
			info.Version, info.State = readToolVersion(ctx, toolTimeout, info.Path, ffmpegVersion, "-version")
		}
		result.FFmpeg = info
	}()
	go func() {
		defer wg.Done()
		info := newToolStatus(raw.JsRuntimePath, defaults.JsRuntimePath)
		switch {
		case cfg.JsRuntimePath == "":
			info.State = toolNotConfigured
		case !filepath.IsAbs(cfg.JsRuntimePath):
			info.State = toolInactive
		default:
			info.Path, info.State = executableToolPath(cfg.JsRuntimePath)
			info.Enabled = enabledNodePath(cfg) != ""
			if info.Enabled && info.State == toolReady {
				info.Version, info.State = readToolVersion(ctx, toolTimeout, info.Path, nodeVersion, "--version")
			}
		}
		result.Node = info
	}()
	go func() {
		defer wg.Done()
		result.Bridge = checkBridge(ctx, toolTimeout, cfg)
	}()
	wg.Wait()
	return result, ctx.Err()
}

func newToolStatus(setting, defaultPath string) toolStatus {
	return toolStatus{Setting: setting, DefaultPath: defaultPath, Custom: setting != "" && setting != defaultPath}
}

// Match the ffmpeg executable selected from yt-dlp's --ffmpeg-location.
// A relative value is a file/directory location, even if the name is on PATH.
func ffmpegToolPath(location string) (string, toolState) {
	info, err := os.Stat(location)
	if err != nil {
		path, _ := absoluteToolPath(location)
		return path, toolPathError(err)
	}
	switch {
	case info.IsDir():
		location += string(filepath.Separator) + "ffmpeg"
	case !strings.Contains(filepath.Base(location), "ffmpeg") && strings.Contains(filepath.Base(location), "ffprobe"):
		name := strings.ReplaceAll(filepath.Base(location), "ffprobe", "ffmpeg")
		sibling := filepath.Join(filepath.Dir(location), name)
		if _, err := os.Stat(sibling); err == nil {
			location = sibling
		} else if !os.IsNotExist(err) {
			return sibling, toolPathError(err)
		} else {
			location = filepath.Join(filepath.Dir(location), "ffmpeg")
		}
	}
	path, err := absoluteToolPath(location)
	if err != nil {
		return "", toolUnavailable
	}
	return executableToolPath(path)
}

func executableToolPath(value string) (string, toolState) {
	path, err := exec.LookPath(value)
	if err != nil {
		// For explicit paths retain the target and distinguish a missing file
		// from a file the OS located but refused to execute.
		if filepath.IsAbs(value) || filepath.VolumeName(value) != "" || filepath.Base(value) != value {
			path, absErr := absoluteToolPath(value)
			if absErr != nil {
				return "", toolUnavailable
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				return path, toolPathError(statErr)
			}
			if !info.Mode().IsRegular() || errors.Is(err, os.ErrPermission) || errors.Is(err, exec.ErrDot) {
				return path, toolNotRunnable
			}
			return path, toolUnavailable
		}
		if errors.Is(err, exec.ErrDot) {
			return "", toolNotRunnable
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", toolMissing
		}
		return "", toolPathError(err)
	}
	path, err = absoluteToolPath(path)
	if err != nil {
		return "", toolUnavailable
	}
	info, err := os.Stat(path)
	if err != nil {
		return path, toolPathError(err)
	}
	if !info.Mode().IsRegular() {
		return path, toolNotRunnable
	}
	return path, toolReady
}

// Keep the selected spelling: cleaning ".." before traversal can redirect an
// executable when the preceding component is a symlink.
func absoluteToolPath(value string) (string, error) {
	if filepath.IsAbs(value) {
		return value, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return dir + string(filepath.Separator) + value, nil
}

func toolPathError(err error) toolState {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return toolMissing
	case errors.Is(err, os.ErrPermission):
		return toolNotRunnable
	default:
		return toolUnavailable
	}
}

type boundedToolOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *boundedToolOutput) Write(p []byte) (int, error) {
	room := maxToolOutput - b.buffer.Len()
	b.overflow = b.overflow || len(p) > room
	_, _ = b.buffer.Write(p[:min(len(p), room)])
	return len(p), nil
}

func readToolVersion(ctx context.Context, timeout time.Duration, path string, version *regexp.Regexp, args ...string) (string, toolState) {
	return runToolProbe(ctx, timeout, path, version, nil, args...)
}

func runToolProbe(ctx context.Context, timeout time.Duration, path string, version *regexp.Regexp, extra []*os.File, args ...string) (string, toolState) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = cleanEnvironment()
	cmd.ExtraFiles = extra
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 100 * time.Millisecond
	// Discard stderr and never return tool output verbatim.
	var out boundedToolOutput
	cmd.Stdout = &out
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		_ = killProcessGroup(cmd)
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, exec.ErrWaitDelay):
		return "", toolTimedOut
	case ctx.Err() != nil:
		return "", toolUnavailable
	case err != nil:
		return "", toolNotRunnable
	case out.overflow:
		return "", toolUnrecognized
	}
	line, _, _ := strings.Cut(out.buffer.String(), "\n")
	match := version.FindStringSubmatch(strings.TrimSpace(line))
	if len(match) != 2 || len(match[1]) > 96 {
		return "", toolUnrecognized
	}
	return match[1], toolReady
}

func checkBridge(ctx context.Context, timeout time.Duration, cfg model.Config) bridgeStatus {
	_, state := executableToolPath(cfg.YtDlpPath)
	if state != toolReady {
		return bridgeStatus{State: state}
	}
	interpreter, err := mediaBridgeInterpreter(cfg)
	if err != nil {
		return bridgeStatus{State: toolUnavailable}
	}
	if interpreter == "" {
		return bridgeStatus{State: toolUnsupported}
	}
	result := bridgeStatus{State: toolUnavailable, Interpreter: interpreter}
	// Execute the same wrapper and its recovery installation without dispatching
	// yt-dlp.main or creating a task, target URL, HTTP lease or budget.
	program := strings.Replace(contextWrapper, "# KOWNLOAD_RECOVERY_BRIDGE", recoveryWrapper, 1)
	const invocation = "yt_dlp.main(context['args'] + ['--downloader', 'native'])"
	if strings.Count(program, invocation) != 1 {
		return result
	}
	program = strings.Replace(program, invocation, "from yt_dlp.version import __version__\nprint(__version__)", 1)
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return result
	}
	defer null.Close()
	input, err := os.CreateTemp("", "kownload-tool-check-*")
	if err != nil {
		return result
	}
	defer os.Remove(input.Name())
	_, err = input.WriteString(`{"args":[],"scopes":[],"probe":"","recovery":{"read":3,"write":4}}`)
	if closeErr := input.Close(); err != nil || closeErr != nil {
		return result
	}
	result.Version, result.State = runToolProbe(ctx, timeout, interpreter, ytdlpVersion, []*os.File{null, null}, "-B", "-c", program, input.Name())
	if result.State == toolNotRunnable || result.State == toolUnrecognized {
		result.State = toolIncompatible
	}
	return result
}
