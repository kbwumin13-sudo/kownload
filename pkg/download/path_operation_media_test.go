package download

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	mediaProtocol "github.com/GopeedLab/gopeed/internal/protocol/media"
	"github.com/GopeedLab/gopeed/pkg/base"
	mediaModel "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

func TestPathMediaStandaloneRejectedBeforeReservation(t *testing.T) {
	root := t.TempDir()
	tool := filepath.Join(root, "standalone-tool")
	invoked := filepath.Join(root, "invoked")
	// Interpreter discovery may inspect this fixture, but must never execute it.
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho invoked > \""+invoked+"\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	d := NewDownloader(&DownloaderConfig{Storage: NewMemStorage(), StorageDir: root, FetchManagers: []fetcher.FetcherManager{&mediaProtocol.FetcherManager{}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := InitializeDatasetIdentity(d.storage); err != nil {
		t.Fatal(err)
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	d.configSnapshotMu.Lock()
	d.cfg.DownloadDir = root
	d.cfg.ProtocolConfig["media"] = &mediaModel.Config{YtDlpPath: tool}
	d.configSnapshotMu.Unlock()
	item := &base.CreateTaskBatchItem{Req: &base.Request{Protocol: "media", URL: "https://fixture.invalid/video"}, Opts: &base.Options{Path: root, Name: "video.mp4"}}
	candidate := &Task{Protocol: "media", Meta: &fetcher.FetcherMeta{Req: item.Req, Opts: item.Opts}}
	snapshot, supported, err := d.captureParentRecoveryContext(candidate)
	if err != nil || !supported || snapshot == nil {
		t.Fatal("actual parent context", err)
	}
	f, err := d.buildFetcher(item.Req)
	if err != nil {
		t.Fatal(err)
	}
	bridge, ok := f.(*mediaProtocol.Fetcher)
	if !ok {
		t.Fatal("actual media fetcher absent")
	}
	supported, err = bridge.SupportsParentRecovery(snapshot)
	if err != nil || supported {
		t.Fatal("fixture is not standalone", err)
	}
	if _, err = d.capturePathDefaults(candidate); !errors.Is(err, ErrPathFreezeHeld) {
		t.Fatalf("standalone enrolled: %v", err)
	}
	request := HTTPPathCreateRequest{Version: 1, OperationID: "unsupported-media", RequiredCapabilities: []string{"media-path-freeze/v1"}, Request: item, PathIntent: PathIntent{Version: 1, Path: root, Origin: pathpolicy.APIExplicit}}
	if _, err = d.CreatePathOperationForDataset(context.Background(), identity.DatasetID, "media-path-create/v1", request); !errors.Is(err, ErrPathFreezeHeld) {
		t.Fatalf("create did not reject: %v", err)
	}
	result, err := d.QueryPathOperationForDataset(identity.DatasetID, "media-path-create/v1", request.OperationID)
	if err != nil || result.Operation.State != "unknown" || len(d.GetTasks()) != 0 {
		t.Fatal("rejected context reserved R/Task", err)
	}
	if _, err = os.Stat(invoked); !os.IsNotExist(err) {
		t.Fatal("tool executed", err)
	}
}
