package download

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"go.etcd.io/bbolt"
)

type unsupportedCompletionManager struct {
	*httpProtocol.FetcherManager
	restores, starts atomic.Int32
}

func (m *unsupportedCompletionManager) Store(f fetcher.Fetcher) (any, error) {
	if wrapped, ok := f.(*unsupportedCompletionFetcher); ok {
		f = wrapped.Fetcher
	}
	return m.FetcherManager.Store(f)
}
func (m *unsupportedCompletionManager) Restore() (any, func(*fetcher.FetcherMeta, any) fetcher.Fetcher) {
	m.restores.Add(1)
	v, restore := m.FetcherManager.Restore()
	return v, func(meta *fetcher.FetcherMeta, v any) fetcher.Fetcher {
		return &unsupportedCompletionFetcher{Fetcher: restore(meta, v), starts: &m.starts}
	}
}

type unsupportedCompletionFetcher struct {
	fetcher.Fetcher
	starts *atomic.Int32
}

func (f *unsupportedCompletionFetcher) Start() error { f.starts.Add(1); return f.Fetcher.Start() }

func unsupportedCompletionSnapshot(t *testing.T, s Storage) string {
	t.Helper()
	const record = "task_completion_v1"
	const required = "task_completion_required_v1"
	raw := map[string]map[string]json.RawMessage{}
	switch s := s.(type) {
	case *MemStorage:
		s.lock.RLock()
		defer s.lock.RUnlock()
		for _, b := range []string{bucketTask, record, required} {
			if values, exists := s.data[b]; exists {
				raw[b] = map[string]json.RawMessage{}
				for k, v := range values {
					data, err := json.Marshal(v)
					if err != nil {
						t.Fatal(err)
					}
					raw[b][k] = data
				}
			}
		}
	case *BoltStorage:
		if err := s.db.View(func(tx *bbolt.Tx) error {
			for _, name := range []string{bucketTask, record, required} {
				if b := tx.Bucket([]byte(name)); b != nil {
					raw[name] = map[string]json.RawMessage{}
					if err := b.ForEach(func(k, v []byte) error { raw[name][string(k)] = append([]byte(nil), v...); return nil }); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestUnsupportedCompletionPresenceNeverExecutes(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			// Every nonempty subset of ref/record/required, plus malformed/unknown
			// values, is held. Only the all-absent legacy case can execute.
			for _, mode := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10} {
				names := map[int]string{0: "legacy-none", 1: "ref", 2: "record", 3: "ref-record", 4: "required", 5: "ref-required", 6: "record-required", 7: "committed-threeway", 8: "unknown-ref", 9: "bad-record", 10: "bad-required"}
				t.Run(fmt.Sprintf("%02d-%s", mode, names[mode]), func(t *testing.T) {
					dir := t.TempDir()
					var s Storage = NewMemStorage()
					if backend == "bolt" {
						s = NewBoltStorage(filepath.Join(dir, "db"))
					}
					// Schema initialization may create empty buckets; freeze that shape
					// before comparing every stored Task/record/required value.
					if err := s.Setup([]string{bucketTask, bucketTaskCompletion, bucketCompletionRequired}); err != nil {
						t.Fatal(err)
					}
					var hits, done atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = w.Write([]byte("legacy-body")) }))
					defer server.Close()
					task := startupStoredTask("unsupported-completion", dir, base.DownloadStatusPause)
					task.Meta.Req.URL = server.URL
					mask := mode
					if mode > 7 {
						mask = 7
					}
					if mask&1 != 0 {
						task.Completion = &TaskCompletionRef{Version: 1, CompletionID: "completion-1"}
					}
					if mode == 8 {
						task.Completion.Version = 99
					}
					if mode == 7 {
						task.Status = base.DownloadStatusDone
						task.Uploading = true
					}
					if err := s.Put(bucketTask, task.ID, task); err != nil {
						t.Fatal(err)
					}
					if mask&2 != 0 {
						if err := s.Setup([]string{"task_completion_v1"}); err != nil {
							t.Fatal(err)
						}
						record := any(map[string]any{
							"version": 1, "completionId": "completion-1", "phase": "committed",
							"directoryIdentity": map[string]any{"device": 1, "inode": 2},
							"stagingIdentity":   map[string]any{"device": 1, "inode": 3},
							"stagingName":       "stage.bin", "finalName": "final.bin", "size": 11,
							"expectedSHA256": strings.Repeat("a", 64), "actualSHA256": strings.Repeat("a", 64),
						})
						if mode == 9 {
							record = map[string]any{"version": "bad", "completionId": false}
						}
						if err := s.Put("task_completion_v1", task.ID, record); err != nil {
							t.Fatal(err)
						}
					}
					if mask&4 != 0 {
						if err := s.Setup([]string{"task_completion_required_v1"}); err != nil {
							t.Fatal(err)
						}
						required := any(map[string]any{"version": 1, "capability": "http-sha256-completion/v1"})
						if mode == 10 {
							required = "malformed-required"
						}
						if err := s.Put("task_completion_required_v1", task.ID, required); err != nil {
							t.Fatal(err)
						}
					}
					before := unsupportedCompletionSnapshot(t, s)
					manager := &unsupportedCompletionManager{FetcherManager: &httpProtocol.FetcherManager{}}
					d := NewDownloader(&DownloaderConfig{Storage: s, StorageDir: dir, FetchManagers: []fetcher.FetcherManager{manager}})
					if err := d.Setup(); err != nil {
						t.Fatal(err)
					}
					defer d.Close()
					d.Listener(func(e *Event) {
						if e.Key == EventKeyDone {
							done.Add(1)
						}
					})
					if mode == 0 {
						if err := d.Continue(&TaskFilter{IDs: []string{task.ID}}); err != nil {
							t.Fatal(err)
						}
						waitStartupDone(t, d, task.ID)
						body, err := os.ReadFile(filepath.Join(dir, task.ID+".bin"))
						if err != nil || string(body) != "legacy-body" || hits.Load() == 0 || manager.restores.Load() == 0 || manager.starts.Load() == 0 {
							t.Fatalf("legacy execution lost: %q %v", body, err)
						}
						return
					}
					for name, run := range map[string]func() error{
						"continue":       func() error { return d.Continue(&TaskFilter{IDs: []string{task.ID}}) },
						"continue-all":   func() error { return d.Continue(nil) },
						"continue-batch": func() error { return d.ContinueBatch(&TaskFilter{IDs: []string{task.ID}}) },
						"stats":          func() error { _, err := d.Stats(task.ID); return err },
						"patch":          func() error { return d.Patch(task.ID, &base.Request{URL: server.URL + "/replacement"}, nil) },
					} {
						if err := run(); !errors.Is(err, transfer.ErrRecoveryHeld) {
							t.Fatalf("%s did not hold: %v", name, err)
						}
					}
					if got := unsupportedCompletionSnapshot(t, s); got != before {
						t.Fatal("unsupported state was rewritten")
					}
					if manager.restores.Load() != 0 || manager.starts.Load() != 0 || hits.Load() != 0 || done.Load() != 0 {
						t.Fatalf("held task executed: restore=%d start=%d requests=%d done=%d", manager.restores.Load(), manager.starts.Load(), hits.Load(), done.Load())
					}
					if mode == 7 && d.GetTask(task.ID).Status != base.DownloadStatusDone {
						t.Fatal("historical committed task was altered")
					}
					if _, err := os.Stat(filepath.Join(dir, task.ID+".bin")); !os.IsNotExist(err) {
						t.Fatalf("held output touched: %v", err)
					}
				})
			}
		})
	}
}
