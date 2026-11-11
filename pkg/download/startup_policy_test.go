package download

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func startupStoredTask(id, dir string, status base.Status) *Task {
	t := NewTask()
	t.ID = id
	t.Protocol = "http"
	t.Status = status
	t.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "http://127.0.0.1:1/never"}, Opts: &base.Options{Path: dir, Name: id + ".bin"}}
	t.Progress = &Progress{}
	return t
}

func TestStartupPolicyRejectsUnknownBeforeStorage(t *testing.T) {
	s := NewMemStorage()
	d := NewDownloader(&DownloaderConfig{Storage: s, StorageDir: t.TempDir(), StartupTaskPolicy: "future-policy"})
	if err := d.Setup(); !errors.Is(err, ErrStartupTaskPolicy) {
		t.Fatalf("unknown policy accepted: %v", err)
	}
	if len(s.data) != 0 {
		t.Fatal("invalid startup policy touched storage")
	}
	_ = d.Close()
}

func TestStartupPolicyImportedReviewBlocksBulkButAllowsExplicit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("reviewed")) }))
	defer server.Close()
	dir := t.TempDir()
	s := NewMemStorage()
	if err := s.Setup([]string{bucketTask}); err != nil {
		t.Fatal(err)
	}
	task := startupStoredTask("imported", dir, base.DownloadStatusPause)
	task.Meta.Req.URL = server.URL
	if err := s.Put(bucketTask, task.ID, task); err != nil {
		t.Fatal(err)
	}
	d := NewDownloader(&DownloaderConfig{Storage: s, StorageDir: dir, StartupTaskPolicy: StartupTaskPolicyImportedReview, FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, filter := range []*TaskFilter{nil, {}, {Statuses: []base.Status{base.DownloadStatusPause}}} {
		if err := d.Continue(filter); !errors.Is(err, ErrStartupTaskReview) {
			t.Fatalf("Continue bypass: %v", err)
		}
		if err := d.ContinueBatch(filter); !errors.Is(err, ErrStartupTaskReview) {
			t.Fatalf("ContinueBatch bypass: %v", err)
		}
	}
	if d.GetTask(task.ID).Status != base.DownloadStatusPause {
		t.Fatal("review startup started imported task")
	}
	if _, err := os.Stat(filepath.Join(dir, "imported.bin")); !os.IsNotExist(err) {
		t.Fatalf("bulk review touched output: %v", err)
	}
	held := handoffRequest(server.URL, dir)
	v := requirePrepared(t, d, held)
	if err := d.Continue(&TaskFilter{IDs: []string{v.TaskID}}); !errors.Is(err, ErrHandoffHeld) {
		t.Fatalf("review bypassed handoff: %v", err)
	}
	if err := d.Continue(&TaskFilter{IDs: []string{task.ID}}); err != nil {
		t.Fatal(err)
	}
	waitStartupDone(t, d, task.ID)
	id, err := d.CreateDirect(&base.Request{URL: server.URL}, &base.Options{Path: dir, Name: "new.bin"})
	if err != nil {
		t.Fatal(err)
	}
	waitStartupDone(t, d, id)
	for _, name := range []string{"imported.bin", "new.bin"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != "reviewed" {
			t.Fatalf("explicit result %s %q %v", name, b, err)
		}
	}
}

func waitStartupDone(t *testing.T, d *Downloader, id string) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond * 5) {
		if d.GetTask(id).Status == base.DownloadStatusDone {
			return
		}
	}
	t.Fatalf("explicit task did not finish: %+v", d.GetTask(id))
}

type startupUploadManager struct {
	*httpProtocol.FetcherManager
	uploads chan struct{}
}

func (m *startupUploadManager) Restore() (any, func(*fetcher.FetcherMeta, any) fetcher.Fetcher) {
	v, restore := m.FetcherManager.Restore()
	return v, func(meta *fetcher.FetcherMeta, v any) fetcher.Fetcher {
		return &startupUploadFetcher{Fetcher: restore(meta, v), uploads: m.uploads}
	}
}

type startupUploadFetcher struct {
	fetcher.Fetcher
	uploads chan struct{}
}

func (f *startupUploadFetcher) Upload() error        { f.uploads <- struct{}{}; return nil }
func (f *startupUploadFetcher) UploadedBytes() int64 { return 0 }
func (f *startupUploadFetcher) WaitUpload() error    { return nil }

func TestStartupPolicyUploadRecoveryAndNonPersistence(t *testing.T) {
	for _, policy := range []StartupTaskPolicy{"", StartupTaskPolicyNormal, StartupTaskPolicyImportedReview} {
		t.Run(string(policy), func(t *testing.T) {
			dir := t.TempDir()
			s := NewMemStorage()
			_ = s.Setup([]string{bucketTask, bucketConfig})
			task := startupStoredTask("upload", dir, base.DownloadStatusDone)
			task.Uploading = true
			_ = s.Put(bucketTask, task.ID, task)
			config := (&base.DownloaderStoreConfig{DownloadDir: dir}).Init()
			_ = s.Put(bucketConfig, "config", config)
			m := &startupUploadManager{FetcherManager: &httpProtocol.FetcherManager{}, uploads: make(chan struct{}, 1)}
			d := NewDownloader(&DownloaderConfig{Storage: s, StorageDir: dir, StartupTaskPolicy: policy, FetchManagers: []fetcher.FetcherManager{m}})
			if err := d.Setup(); err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			select {
			case <-m.uploads:
				if policy == StartupTaskPolicyImportedReview {
					t.Fatal("imported upload started")
				}
			case <-time.After(150 * time.Millisecond):
				if policy != StartupTaskPolicyImportedReview {
					t.Fatal("ordinary upload no longer restored")
				}
			}
			persisted := &base.DownloaderStoreConfig{}
			if _, err := s.Get(bucketConfig, "config", persisted); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(config)
			after, _ := json.Marshal(persisted)
			if string(before) != string(after) || strings.Contains(string(after), "startupTaskPolicy") {
				t.Fatal("startup policy changed persistent user config")
			}
		})
	}
}

func TestStartupPolicyReviewDoesNotAutoDeleteMissingImportedFile(t *testing.T) {
	dir := t.TempDir()
	s := NewMemStorage()
	_ = s.Setup([]string{bucketTask, bucketConfig})
	task := startupStoredTask("missing-import", dir, base.DownloadStatusDone)
	task.Meta.Res = &base.Resource{Files: []*base.FileInfo{{Name: "missing.bin"}}}
	_ = s.Put(bucketTask, task.ID, task)
	_ = s.Put(bucketConfig, "config", (&base.DownloaderStoreConfig{AutoDeleteMissingFileTasks: true}).Init())
	d := NewDownloader(&DownloaderConfig{Storage: s, StorageDir: dir, StartupTaskPolicy: StartupTaskPolicyImportedReview})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.GetTask(task.ID) == nil {
		t.Fatal("review startup deleted imported task")
	}
}
