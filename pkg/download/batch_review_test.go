package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestBatchDeleteCallbackCanReenterLifecycle(t *testing.T) {
	for _, all := range []bool{false, true} {
		for _, action := range []string{"close", "cancel", "delete"} {
			t.Run(fmt.Sprintf("all=%t/%s", all, action), func(t *testing.T) {
				dir := t.TempDir()
				d := operationDownloader(t, NewMemStorage(), dir)
				pending := operationRequest("http://127.0.0.1:1/pending", dir)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, e := d.CreateBatchOperation(ctx, pending); e != nil {
					t.Fatal(e)
				}
				task := startupStoredTask("delete-callback", dir, base.DownloadStatusPause)
				_ = d.assignFetcherManager(task)
				initTask(task)
				if e := d.storage.Put(bucketTask, task.ID, task.clone()); e != nil {
					t.Fatal(e)
				}
				d.lock.Lock()
				d.tasks = append(d.tasks, task)
				d.lock.Unlock()
				callback := make(chan error, 1)
				d.Listener(func(event *Event) {
					if event.Key != EventKeyDelete {
						return
					}
					var stored Task
					if ok, e := d.storage.Get(bucketTask, task.ID, &stored); e != nil || ok {
						callback <- errors.New("delete event preceded durable deletion")
						return
					}
					switch action {
					case "close":
						callback <- d.Close()
					case "cancel":
						_, e := d.CancelBatchOperation(pending.OperationID)
						callback <- e
					case "delete":
						callback <- d.Delete(&TaskFilter{IDs: []string{task.ID}}, false)
					}
				})
				completed := make(chan error, 1)
				go func() {
					filter := &TaskFilter{IDs: []string{task.ID}}
					if all {
						filter = nil
					}
					completed <- d.Delete(filter, false)
				}()
				select {
				case e := <-completed:
					if e != nil {
						t.Fatal(e)
					}
				case <-time.After(time.Second):
					t.Fatal("Delete callback deadlocked reentrant " + action)
				}
				select {
				case e := <-callback:
					if e != nil {
						t.Fatal(e)
					}
				case <-time.After(time.Second):
					t.Fatal("callback not delivered")
				}
				if action != "close" {
					_ = d.Close()
				}
			})
		}
	}
}

type precisionProbeManager struct {
	*httpProtocol.FetcherManager
	restored chan string
	resolved chan string
}

func (m *precisionProbeManager) Build() fetcher.Fetcher {
	return &precisionProbeFetcher{Fetcher: m.FetcherManager.Build(), manager: m}
}
func (m *precisionProbeManager) Restore() (any, func(*fetcher.FetcherMeta, any) fetcher.Fetcher) {
	v, restore := m.FetcherManager.Restore()
	return v, func(meta *fetcher.FetcherMeta, value any) fetcher.Fetcher {
		raw, _ := json.Marshal([]any{meta.Req.Extra, meta.Opts.Extra})
		m.restored <- string(raw)
		return &precisionProbeFetcher{Fetcher: restore(meta, value), manager: m}
	}
}
func (m *precisionProbeManager) Store(f fetcher.Fetcher) (any, error) {
	return m.FetcherManager.Store(f.(*precisionProbeFetcher).Fetcher)
}

type precisionProbeFetcher struct {
	fetcher.Fetcher
	manager *precisionProbeManager
}

func (f *precisionProbeFetcher) Resolve(req *base.Request, opts *base.Options) error {
	return f.ResolveContext(context.Background(), req, opts)
}
func (f *precisionProbeFetcher) ResolveTask(ctx context.Context, req *base.Request, opts *base.Options) error {
	return f.ResolveContext(ctx, req, opts)
}
func (f *precisionProbeFetcher) ResolveContext(ctx context.Context, req *base.Request, opts *base.Options) error {
	raw, _ := json.Marshal([]any{req.Extra, opts.Extra})
	f.manager.resolved <- string(raw)
	return f.Fetcher.(fetcher.ContextResolver).ResolveContext(ctx, req, opts)
}
func TestBatchPrecisionSurvivesFrozenOptionsReopenAndExecution(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("precise")) }))
	defer source.Close()
	dir := t.TempDir()
	d := operationDownloader(t, NewBoltStorage(dir), dir)
	r := operationRequest(source.URL, dir)
	r.Reqs[0].Req.Extra = map[string]any{"opaque": json.Number("9007199254740993")}
	r.Reqs[0].Opts.Extra = map[string]any{"opaque": json.Number("9007199254740993")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := d.CreateBatchOperation(ctx, r); e != nil {
		t.Fatal(e)
	}
	record, e := d.loadOperation(r.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(record.Members[0].Options.Extra)
	if !strings.Contains(string(raw), "9007199254740993") {
		_ = d.Close()
		t.Fatalf("frozen Options rounded: %s", raw)
	}
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	manager := &precisionProbeManager{FetcherManager: &httpProtocol.FetcherManager{}, restored: make(chan string, 2), resolved: make(chan string, 2)}
	d = NewDownloader(&DownloaderConfig{Storage: NewBoltStorage(dir), StorageDir: dir, FetchManagers: []fetcher.FetcherManager{manager}})
	if e = d.Setup(); e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	result, e := d.CreateBatchOperation(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	for label, ch := range map[string]<-chan string{"restore": manager.restored, "resolve": manager.resolved} {
		select {
		case received := <-ch:
			if strings.Count(received, "9007199254740993") != 2 || strings.Contains(received, "9007199254740992") {
				t.Fatal(fmt.Sprintf("%s rounded request/options: %s", label, received))
			}
		case <-time.After(3 * time.Second):
			t.Fatal("fetcher did not receive " + label)
		}
	}
	waitStartupDone(t, d, result.TaskIDs[0])
}
