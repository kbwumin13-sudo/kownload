package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

func TestPythonBrowserContextIsTransportOnly(t *testing.T) {
	req := &base.Request{URL: "https://www.bilibili.com/video/BV1t8411b75p"}
	extra := &model.ReqExtra{
		SourcePage: req.URL,
		Header: map[string]string{
			"Referer": req.URL, "User-Agent": "Browser fixture/1.0",
			"Authorization": "fixture", "X-Scope": "fixture", "Cookie": "fixture=raw",
		},
		ScopedHeaders: []model.HeaderScope{{Origin: "https://api.x.com", Headers: map[string]string{
			"Authorization": "fixture", "x-csrf-token": "fixture", "x-guest-token": "fixture",
		}}},
	}
	cfg := model.Config{FFmpegPath: "ffmpeg"}
	plain, cleanup, err := requestArgs(req, &model.ReqExtra{}, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	withBrowser, cleanup, err := requestArgs(req, extra, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !reflect.DeepEqual(plain, withBrowser) {
		t.Fatal("browser context must not become global yt-dlp options")
	}
	scopes, err := contextScopes(req, extra)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 3 || scopes[1].Headers["Referer"] != extra.SourcePage || scopes[1].Headers["User-Agent"] != extra.Header["User-Agent"] {
		t.Fatal("browser navigation must remain in the transport scope")
	}
}

func TestExtractorNavigationScopes(t *testing.T) {
	python := os.Getenv("GOPEED_MEDIA_PYTHON")
	if python == "" {
		python = filepath.Join("..", "..", "..", "..", "toolchains", "media-2026.08.19", "bin", "python")
	}
	if _, err := os.Stat(python); err != nil {
		t.Skip("configured yt-dlp interpreter unavailable; set GOPEED_MEDIA_PYTHON")
	}
	// Keep the venv entrypoint: resolving its symlink loses the installed modules.
	python, err := filepath.Abs(python)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-B", "navigation_test.py", "-v")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("extractor navigation scopes: %v\n%s", err, out)
	} else {
		t.Logf("%s", out)
	}
}
