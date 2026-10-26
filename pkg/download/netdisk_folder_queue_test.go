package download

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	netfetch "github.com/GopeedLab/gopeed/internal/protocol/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

type queuedFolderProvider struct {
	cloud.Provider
	calls   chan string
	release chan struct{}
}

func (*queuedFolderProvider) Info() model.ProviderInfo {
	return model.ProviderInfo{ID: "quark", Hosts: []string{"pan.quark.cn"}}
}
func (*queuedFolderProvider) List(context.Context, *cloud.APIClient, *cloud.Session, model.Source, string) (*model.Listing, error) {
	l := &model.Listing{}
	for i := 0; i < 906; i++ {
		l.Files = append(l.Files, model.File{ID: fmt.Sprint(i), Name: fmt.Sprintf("%d.bin", i), Size: 1})
	}
	return l, nil
}
func (p *queuedFolderProvider) Resolve(ctx context.Context, _ *cloud.APIClient, _ *cloud.Session, _ model.Source, f model.File) (*cloud.Download, error) {
	p.calls <- f.ID
	select {
	case <-p.release:
		return nil, errors.New("one file unavailable")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestNetdiskLargeFolderQueuesWithoutFetchingWaitingLinks(t *testing.T) {
	p := &queuedFolderProvider{calls: make(chan string, 1000), release: make(chan struct{}, 1)}
	s := cloud.NewService(p)
	cap, err := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/fixture", InstanceID: "browser", ContextID: "tab", AccountID: "account"})
	if err != nil {
		t.Fatal(err)
	}
	d := NewDownloader(&DownloaderConfig{Storage: NewMemStorage(), StorageDir: t.TempDir(), FetchManagers: []fetcher.FetcherManager{&netfetch.FetcherManager{Service: s}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	d.cfg.MaxRunning = 3
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	prepared, err := s.Prepare(ctx, controller.NewController(), &base.Request{URL: cap.Source.URL}, cap.Source, cap.SessionID, []model.File{{ID: "folder", Name: "Folder", IsDir: true}}, &base.Options{Path: t.TempDir()})
	if err != nil {
		t.Fatalf("folder did not prepare independently of download links: %v", err)
	}
	created := make(chan error, 1)
	go func() {
		ids, err := d.CreateDirectBatch(&base.CreateTaskBatch{Reqs: prepared.Reqs})
		if err == nil && len(ids) != 906 {
			err = fmt.Errorf("only %d tasks created", len(ids))
		}
		created <- err
	}()
	select {
	case err := <-created:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("batch creation blocked on resolving queued-file links")
	}
	for i := 0; i < 3; i++ {
		select {
		case <-p.calls:
		case <-time.After(time.Second):
			t.Fatal("download slots did not resolve concurrently")
		}
	}
	select {
	case id := <-p.calls:
		t.Fatalf("waiting task %s fetched its short-lived link early", id)
	case <-time.After(30 * time.Millisecond):
	}
	p.release <- struct{}{}
	select {
	case <-p.calls: // A failed file releases a slot for the next file.
	case <-time.After(3 * time.Second):
		t.Fatal("one failed file blocked the rest of the folder")
	}
	ids := []string{}
	for _, task := range d.GetTasks() {
		ids = append(ids, task.ID)
	}
	if err := d.Pause(&TaskFilter{IDs: ids}); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-p.calls:
		t.Fatalf("paused folder task %s restarted from the stale wait queue", id)
	case <-time.After(50 * time.Millisecond):
	}
}
