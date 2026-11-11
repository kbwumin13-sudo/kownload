package rest

import (
	"encoding/json"
	"errors"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"os"
	"path/filepath"
	"testing"
)

func TestStartupPolicyRESTForwardingAndEarlyRejection(t *testing.T) {
	dir := t.TempDir()
	var cfg model.StartConfig
	if err := json.Unmarshal([]byte(`{"startupTaskPolicy":"imported-review","storage":"mem"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.StorageDir = dir
	_, ln, err := BuildServer(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	defer Downloader.Close()
	if err = Downloader.Continue(nil); !errors.Is(err, download.ErrStartupTaskReview) {
		t.Fatalf("REST start policy not forwarded: %v", err)
	}
	invalidDir := filepath.Join(dir, "must-not-create")
	_, invalid, err := BuildServer(&model.StartConfig{StorageDir: invalidDir, StartupTaskPolicy: "unknown-future"})
	if !errors.Is(err, download.ErrStartupTaskPolicy) || invalid != nil {
		t.Fatalf("unknown startup policy accepted: %v", err)
	}
	if _, err = os.Stat(invalidDir); !os.IsNotExist(err) {
		t.Fatal("invalid startup policy opened database")
	}
}
