package download

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func extensionTestDownloader(t *testing.T) *Downloader {
	t.Helper()
	d := NewDownloader(&DownloaderConfig{StorageDir: t.TempDir(), FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestExtensionCompletionConcurrentClear(t *testing.T) {
	d := extensionTestDownloader(t)
	task := snapshotTestTask()
	task.updateStatus(base.DownloadStatusDone)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			d.triggerOnDone(task)
		}
	}()
	for i := 0; i < 1000; i++ {
		if err := d.Clear(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestExtensionCallbackCanClearRegistry(t *testing.T) {
	d := extensionTestDownloader(t)
	for i := 0; i < 2; i++ {
		dir := t.TempDir()
		manifest := fmt.Sprintf(`{"name":"callback-%d","title":"callback","version":"1","scripts":[{"event":"onDone","match":{"urls":["*://localhost/*"]},"entry":"index.js"}]}`, i)
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(`gopeed.events.onDone(function(ctx) {});`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := d.InstallExtensionByFolder(dir, true); err != nil {
			t.Fatal(err)
		}
	}
	result := make(chan error, 1)
	go func() {
		calls := 0
		var callbackErr error
		req := &base.Request{URL: "http://localhost/file"}
		err := doTrigger(d, EventOnDone, req, &OnResolveContext{Req: req}, func(_ *Extension, _ *Instance, _ *OnResolveContext) {
			calls++
			if clearErr := d.Clear(); clearErr != nil {
				callbackErr = clearErr
			}
		})
		if err == nil {
			err = callbackErr
		}
		if err == nil && calls != 2 {
			err = fmt.Errorf("snapshot callbacks = %d, want 2", calls)
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("callback held the registry lock while calling Clear")
	}
	if len(d.GetExtensions()) != 0 {
		t.Fatal("Clear left registered extensions")
	}
}
