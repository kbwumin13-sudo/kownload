package download

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type mcpReadForbiddenStorage struct{ Storage }
type mcpReadForbiddenManager struct{ fetcher.FetcherManager }
type mcpReadForbiddenFetcher struct{ fetcher.Fetcher }
type mcpReadPrivateValue struct{}

func (mcpReadPrivateValue) MarshalJSON() ([]byte, error) { panic("private metadata serialized") }

func mcpReadFixture(ids ...string) *Downloader {
	d := &Downloader{lock: &sync.Mutex{}, storage: mcpReadForbiddenStorage{}}
	for _, id := range ids {
		task := &Task{
			ID: id, Protocol: "http", Status: base.DownloadStatusPause,
			CreatedAt: time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 11, 2, 0, 0, 0, time.UTC),
			Progress:       &Progress{Downloaded: 42, Speed: 7, Uploaded: 3, UploadSpeed: 1, MediaPhase: "downloading", MultiPartBaseName: "PRIVATE_SENTINEL"},
			Meta:           &fetcher.FetcherMeta{Req: &base.Request{URL: "https://PRIVATE_SENTINEL", Extra: mcpReadPrivateValue{}}, Res: &base.Resource{Size: 999}, Opts: &base.Options{Path: "/PRIVATE_SENTINEL"}},
			fetcherManager: mcpReadForbiddenManager{}, statusLock: &sync.Mutex{},
		}
		d.tasks = append(d.tasks, task)
	}
	return d
}
func TestMCPReadNoProtocolStorageOrPrivateMetadata(t *testing.T) {
	ctx := context.Background()
	for _, loaded := range []bool{false, true} {
		d := mcpReadFixture("task_1")
		task := d.tasks[0]
		if loaded {
			task.fetcher = mcpReadForbiddenFetcher{}
		}
		before := *task.Progress
		meta := task.Meta
		originalFetcher := task.fetcher
		page, err := d.MCPReadList(ctx, 10, "")
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.MCPReadGetTask(ctx, "task_1")
		if err != nil {
			t.Fatal(err)
		}
		progress, err := d.MCPReadGetProgress(ctx, "task_1")
		if err != nil {
			t.Fatal(err)
		}
		stats, err := d.MCPReadGetStats(ctx, "task_1")
		if err != nil || stats.Available {
			t.Fatalf("stats: %v %v", stats, err)
		}
		last, err := d.MCPReadGetErrors(ctx, "task_1")
		if err != nil || last.Available {
			t.Fatalf("errors: %v %v", last, err)
		}
		if got.Progress.TotalBytes != nil || progress.TotalBytes != nil {
			t.Fatal("read protocol-owned total")
		}
		if got.Progress.DownloadedBytes != 42 || got.Status != "pause" {
			t.Fatalf("bad snapshot: %+v", got)
		}
		data, err := json.Marshal([]any{page, got, progress, stats, last})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "PRIVATE_SENTINEL") {
			t.Fatal("private data escaped")
		}
		if !reflect.DeepEqual(before, *task.Progress) || task.Meta != meta || task.fetcher != originalFetcher || task.Status != base.DownloadStatusPause {
			t.Fatal("read mutated task")
		}
	}
}
func TestMCPReadStableKeysetPagination(t *testing.T) {
	ctx := context.Background()
	d := mcpReadFixture("c", "a", "b")
	first, err := d.MCPReadList(ctx, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Tasks) != 2 || first.Tasks[0].ID != "a" || first.Tasks[1].ID != "b" || first.NextCursor == "" {
		t.Fatalf("first=%+v", first)
	}
	// Reorder, remove the cursor task, insert before and after the cursor.
	other := mcpReadFixture("aa", "d")
	d.tasks = []*Task{other.tasks[1], d.tasks[0], other.tasks[0]}
	second, err := d.MCPReadList(ctx, 2, first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Tasks) != 2 || second.Tasks[0].ID != "c" || second.Tasks[1].ID != "d" || second.NextCursor != "" {
		t.Fatalf("second=%+v", second)
	}
	empty, err := mcpReadFixture().MCPReadList(ctx, 1, "")
	if err != nil || empty.Tasks == nil {
		t.Fatal("empty list must be []")
	}
	for _, cursor := range []string{"v2_YQ", "v1_", "v1_YQ=", "v1_YQ\n", "v1_Li4v", strings.Repeat("a", 129)} {
		if _, err := d.MCPReadList(ctx, 1, cursor); !errors.Is(err, ErrMCPReadInvalid) {
			t.Errorf("accepted cursor %q", cursor)
		}
	}
	for _, limit := range []int{0, 101, -1} {
		if _, err := d.MCPReadList(ctx, limit, ""); !errors.Is(err, ErrMCPReadInvalid) {
			t.Fatal("accepted limit")
		}
	}
	if _, err := mcpReadFixture("dup", "dup").MCPReadList(ctx, 1, ""); !errors.Is(err, ErrMCPReadUnavailable) {
		t.Fatal("duplicate ID accepted")
	}
}
func TestMCPReadNonblockingLockOrderAndCancel(t *testing.T) {
	d := mcpReadFixture("task_1")
	ctx := context.Background()
	d.lock.Lock()
	if _, err := d.MCPReadList(ctx, 1, ""); !errors.Is(err, ErrMCPReadBusy) {
		t.Fatal(err)
	}
	if _, err := d.MCPReadGetTask(ctx, "task_1"); !errors.Is(err, ErrMCPReadBusy) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := d.MCPReadGetTask(cancelled, "task_1"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	d.lock.Unlock()
	task := d.tasks[0]
	task.dataLock.Lock()
	if _, err := d.MCPReadGetTask(ctx, "task_1"); !errors.Is(err, ErrMCPReadBusy) {
		t.Fatal(err)
	}
	if !d.lock.TryLock() {
		t.Fatal("main lock retained while reading task")
	}
	d.lock.Unlock()
	// No protocol stats means no need to acquire even the task data lock.
	if _, err := d.MCPReadGetStats(ctx, "task_1"); err != nil {
		t.Fatal(err)
	}
	task.dataLock.Unlock()
	task.statusLock.Lock()
	if _, err := d.MCPReadGetTask(ctx, "task_1"); err != nil {
		t.Fatal(err)
	}
	task.statusLock.Unlock()
}
func TestMCPReadClosedEnumsAndUnsafeScalars(t *testing.T) {
	d := mcpReadFixture("task_1")
	task := d.tasks[0]
	task.Protocol = "PRIVATE_SENTINEL"
	task.Progress.MediaPhase = "PRIVATE_SENTINEL"
	got, err := d.MCPReadGetTask(context.Background(), "task_1")
	if err != nil || got.Protocol != "unknown" || got.Progress.Phase != "unknown" {
		t.Fatalf("enum: %+v %v", got, err)
	}
	task.Status = base.DownloadStatusDone
	got, err = d.MCPReadGetTask(context.Background(), "task_1")
	if err != nil || got.Progress.Phase != "done" {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Task){
		func(t *Task) { t.Status = "PRIVATE_SENTINEL" },
		func(t *Task) { t.Progress.Downloaded = -1 },
		func(t *Task) { t.Progress.Speed = mcpReadMaxInteger + 1 },
		func(t *Task) { t.Progress = nil },
		func(t *Task) { t.CreatedAt = time.Time{} },
		func(t *Task) { t.UpdatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		d := mcpReadFixture("task_1")
		mutate(d.tasks[0])
		if _, err := d.MCPReadGetTask(context.Background(), "task_1"); !errors.Is(err, ErrMCPReadUnavailable) {
			t.Fatal("unsafe scalar accepted", err)
		}
	}
	for _, id := range []string{"", "..", "x/y", "x?token", strings.Repeat("a", 65)} {
		if _, err := d.MCPReadGetTask(context.Background(), id); !errors.Is(err, ErrMCPReadInvalid) {
			t.Fatal("invalid ID accepted")
		}
	}
	if _, err := d.MCPReadGetStats(context.Background(), "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatal(err)
	}
	var absent *Downloader
	if _, err := absent.MCPReadList(context.Background(), 1, ""); !errors.Is(err, ErrMCPReadUnavailable) {
		t.Fatal(err)
	}
	d.closed.Store(true)
	if _, err := d.MCPReadGetErrors(context.Background(), "task_1"); !errors.Is(err, ErrMCPReadUnavailable) {
		t.Fatal(err)
	}
}
func TestMCPReadConcurrentCachedSnapshots(t *testing.T) {
	d := mcpReadFixture("task_1", "task_2")
	task := d.tasks[0]
	task.Progress.Speed = task.Progress.Downloaded // Establish the invariant before publishing to readers.
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Wait()
	go func() {
		defer wg.Done()
		for i := int64(0); i < 1000; i++ {
			task.dataLock.Lock()
			task.Progress.Downloaded = i
			task.Progress.Speed = i
			task.UpdatedAt = task.CreatedAt.Add(time.Duration(i) * time.Second)
			task.dataLock.Unlock()
			// Represents a mutable protocol-owned resource: readers must never touch it.
			task.Meta.Res.Size = i
			d.lock.Lock()
			d.tasks[0], d.tasks[1] = d.tasks[1], d.tasks[0]
			d.lock.Unlock()
		}
	}()
	for i := 0; i < 1000; i++ {
		page, err := d.MCPReadList(context.Background(), 2, "")
		if err != nil && !errors.Is(err, ErrMCPReadBusy) {
			t.Fatal(err)
		}
		if err == nil {
			for _, item := range page.Tasks {
				if item.ID == "task_1" && item.Progress.DownloadedBytes != item.Progress.SpeedBytesPerSecond {
					t.Fatal("torn snapshot")
				}
			}
		}
	}
	wg.Wait()
}
