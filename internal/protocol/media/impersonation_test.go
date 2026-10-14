package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Use the release's isolated interpreter; never install dependencies in a test.
func TestScopedCurlTransport(t *testing.T) {
	python := os.Getenv("GOPEED_MEDIA_PYTHON")
	if python == "" {
		python = filepath.Join("..", "..", "..", "..", "toolchains", "media-2026.08.19", "bin", "python")
	}
	if _, err := os.Stat(python); err != nil {
		t.Skip("optional curl-cffi interpreter unavailable; set GOPEED_MEDIA_PYTHON")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "impersonation_test.py", "-v")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scoped curl transport: %v\n%s", err, out)
	} else {
		t.Logf("%s", out)
	}
}
