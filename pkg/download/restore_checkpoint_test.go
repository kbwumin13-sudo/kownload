package download

import (
	"testing"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestRestoreFetcherKeepsDurableCheckpointBeforeStart(t *testing.T) {
	dir := t.TempDir()
	d := NewDownloader(&DownloaderConfig{Storage: NewBoltStorage(dir), StorageDir: dir,
		FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task := NewTask()
	task.Protocol = "http"
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "http://127.0.0.1/unused"},
		Opts: &base.Options{Path: dir, Name: "partial.bin"},
		Res:  &base.Resource{Size: 1024, Files: []*base.FileInfo{{Name: "partial.bin", Size: 1024}}}}
	d.assignFetcherManager(task)
	checkpoint := map[string]any{"Connections": []any{map[string]any{
		"ID": 0, "Downloaded": 512, "Chunk": map[string]any{"Begin": 0, "End": 0, "Downloaded": 512}}}}
	if err := d.storage.Put(bucketSave, task.ID, checkpoint); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		task.fetcher = nil // Simulate another startup after crashing before Start/saveTask.
		if err := d.restoreFetcher(task); err != nil {
			t.Fatal(err)
		}
		if task.fetcher.Progress().TotalDownloaded() != 512 {
			t.Fatal("restored prefix was lost")
		}
		var saved map[string]any
		found, err := d.storage.Get(bucketSave, task.ID, &saved)
		if err != nil || !found {
			t.Fatal("restore consumed the durable checkpoint")
		}
		_ = task.fetcher.Close()
	}
}
