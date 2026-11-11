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
func TestMCPReadRecordedFinalTotal(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      base.Status
		downloaded  int64
		missing     bool
		badSpeed    bool
		unavailable bool
	}{
		{name: "done", status: base.DownloadStatusDone, downloaded: 42},
		{name: "done_zero", status: base.DownloadStatusDone},
		{name: "done_max_safe", status: base.DownloadStatusDone, downloaded: mcpReadMaxInteger},
		{name: "done_missing", status: base.DownloadStatusDone, missing: true, unavailable: true},
		{name: "done_negative", status: base.DownloadStatusDone, downloaded: -1, unavailable: true},
		{name: "done_unsafe", status: base.DownloadStatusDone, downloaded: mcpReadMaxInteger + 1, unavailable: true},
		{name: "done_other_invalid_scalar", status: base.DownloadStatusDone, badSpeed: true, unavailable: true},
		{name: "ready", status: base.DownloadStatusReady, downloaded: 42},
		{name: "wait", status: base.DownloadStatusWait, downloaded: 42},
		{name: "running", status: base.DownloadStatusRunning, downloaded: 42},
		{name: "pause", status: base.DownloadStatusPause, downloaded: 42},
		{name: "error", status: base.DownloadStatusError, downloaded: 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := mcpReadFixture("task_1")
			task := d.tasks[0]
			task.Status = tc.status
			task.Progress.Downloaded = tc.downloaded
			if tc.missing {
				task.Progress = nil
			} else if tc.badSpeed {
				task.Progress.Speed = -1
			}
			// Any call to the embedded nil fetcher/storage/manager panics.
			// Resource size deliberately disagrees; the total must not use it.
			task.fetcher = mcpReadForbiddenFetcher{}
			task.Meta.Res.Size = -99
			ctx := context.Background()
			got, getErr := d.MCPReadGetTask(ctx, task.ID)
			progress, progressErr := d.MCPReadGetProgress(ctx, task.ID)
			page, listErr := d.MCPReadList(ctx, 1, "")
			if tc.unavailable {
				for _, err := range []error{getErr, progressErr, listErr} {
					if !errors.Is(err, ErrMCPReadUnavailable) {
						t.Fatalf("invalid snapshot: %v", err)
					}
				}
				if got.Progress.TotalBytes != nil || progress.TotalBytes != nil || len(page.Tasks) != 0 {
					t.Fatal("unavailable snapshot supplied data")
				}
				return
			}
			if getErr != nil || progressErr != nil || listErr != nil || len(page.Tasks) != 1 {
				t.Fatalf("read errors: %v %v %v", getErr, progressErr, listErr)
			}
			for _, p := range []MCPReadProgress{got.Progress, progress, page.Tasks[0].Progress} {
				if tc.status == base.DownloadStatusDone {
					if p.TotalBytes == nil || *p.TotalBytes != tc.downloaded {
						t.Fatalf("recorded total: %+v", p)
					}
				} else if p.TotalBytes != nil {
					t.Fatal("non-done snapshot supplied a total")
				}
			}
			stats, statsErr := d.MCPReadGetStats(ctx, task.ID)
			last, errorsErr := d.MCPReadGetErrors(ctx, task.ID)
			if statsErr != nil || errorsErr != nil || stats.Available || last.Available {
				t.Fatal("stats/errors availability changed")
			}
			data, err := json.Marshal([]any{got, progress, page, stats, last})
			if err != nil || strings.Contains(string(data), "PRIVATE_SENTINEL") {
				t.Fatal("private metadata escaped", err)
			}
			if got.Progress.TotalBytes != nil {
				*got.Progress.TotalBytes = 123 // Returned pointer must not alias Task or other reads.
				if task.Progress.Downloaded != tc.downloaded || *progress.TotalBytes != tc.downloaded || got.Progress.DownloadedBytes != tc.downloaded {
					t.Fatal("returned total aliases another scalar")
				}
			}
		})
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
	if _, err := d.MCPReadGetStats(ctx, "task_1"); !errors.Is(err, ErrMCPReadBusy) {
		t.Fatal(err)
	}
	if _, err := d.MCPReadGetErrors(ctx, "task_1"); !errors.Is(err, ErrMCPReadBusy) {
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
			task.Status = base.DownloadStatusRunning
			if i%2 == 0 {
				task.Status = base.DownloadStatusDone
			}
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
				if item.ID == "task_1" {
					if item.Progress.DownloadedBytes != item.Progress.SpeedBytesPerSecond {
						t.Fatal("torn snapshot")
					}
					if item.Status == "done" {
						if item.Progress.TotalBytes == nil || *item.Progress.TotalBytes != item.Progress.DownloadedBytes || item.Progress.DownloadedBytes%2 != 0 {
							t.Fatal("torn final total")
						}
					} else if item.Progress.TotalBytes != nil {
						t.Fatal("non-done total under concurrent writes")
					}
				}
			}
		}
	}
	wg.Wait()
}

// This fixture exercises only the agreed value-copy seam; it is deliberately
// not a Task method and supplies no production publishing implementation.
type mcpReadPublishedFixture struct{ value MCPReadPublishedSnapshot }

func (f *mcpReadPublishedFixture) mcpReadPublishedSnapshotLocked() MCPReadPublishedSnapshot {
	return f.value
}

func TestMCPReadPublishedStatsValues(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		value := MCPReadPublishedStats{ActiveStreamsKnown: mask&1 != 0, ActiveStreams: 0, TargetStreamsKnown: mask&2 != 0, TargetStreams: 4, PeersKnown: mask&4 != 0, Peers: mcpReadMaxInteger}
		got, err := mcpReadStatsDTO(value)
		if err != nil || got.Available != (mask != 0) {
			t.Fatalf("mask %d: %+v %v", mask, got, err)
		}
		for _, field := range []struct {
			known bool
			got   *int64
			want  int64
		}{
			{value.ActiveStreamsKnown, got.ActiveStreams, value.ActiveStreams},
			{value.TargetStreamsKnown, got.TargetStreams, value.TargetStreams},
			{value.PeersKnown, got.Peers, value.Peers},
		} {
			if field.known != (field.got != nil) || (field.got != nil && *field.got != field.want) {
				t.Fatalf("mask %d lost observation", mask)
			}
			if field.got != nil {
				*field.got = 123
			}
		}
		if value.ActiveStreams != 0 || value.TargetStreams != 4 || value.Peers != mcpReadMaxInteger {
			t.Fatal("DTO aliases source")
		}
	}
	for _, bad := range []int64{-1, mcpReadMaxInteger + 1} {
		for _, value := range []MCPReadPublishedStats{
			{ActiveStreamsKnown: true, ActiveStreams: bad, TargetStreamsKnown: true, TargetStreams: 4},
			{TargetStreamsKnown: true, TargetStreams: bad},
			{PeersKnown: true, Peers: bad},
		} {
			got, err := mcpReadStatsDTO(value)
			if !errors.Is(err, ErrMCPReadUnavailable) || got != (MCPReadStats{}) {
				t.Fatalf("invalid counter accepted: %+v %v", got, err)
			}
		}
	}
	// Values whose Known bit is false are never observations and never leak.
	got, err := mcpReadStatsDTO(MCPReadPublishedStats{ActiveStreams: -1, TargetStreams: mcpReadMaxInteger + 1})
	data, marshalErr := json.Marshal(got)
	if err != nil || marshalErr != nil || string(data) != `{"available":false}` {
		t.Fatalf("unknown counters escaped: %s %v", data, err)
	}
}

func TestMCPReadPublishedErrorValues(t *testing.T) {
	for _, category := range []string{"network", "authentication", "integrity", "storage", "cancelled", "unknown"} {
		for _, retryable := range []bool{false, true} {
			for _, millis := range []int64{1, 1794432000123, 253402300799999} {
				value := MCPReadPublishedError{Known: true, Category: category, Retryable: retryable, OccurredAtMillis: millis}
				got, err := mcpReadErrorsDTO(value)
				if err != nil || !got.Available || got.Category != category || got.Retryable == nil || *got.Retryable != retryable {
					t.Fatalf("event changed: %+v %v", got, err)
				}
				parsed, err := time.Parse(time.RFC3339Nano, got.OccurredAt)
				if err != nil || parsed.UnixMilli() != millis || !strings.HasSuffix(got.OccurredAt, "Z") {
					t.Fatalf("timestamp changed: %q %v", got.OccurredAt, err)
				}
				*got.Retryable = !retryable
				if value.Retryable != retryable {
					t.Fatal("retryable aliases source")
				}
			}
		}
	}
	for _, value := range []MCPReadPublishedError{
		{Known: true, Category: "network", OccurredAtMillis: 0},
		{Known: true, Category: "network", OccurredAtMillis: -1},
		{Known: true, Category: "network", OccurredAtMillis: 253402300800000},
		{Known: true, Category: "network", OccurredAtMillis: mcpReadMaxInteger},
		{Known: true, Category: "", OccurredAtMillis: 1},
		{Known: true, Category: "PRIVATE_SENTINEL", OccurredAtMillis: 1},
		// Producer must map its typed classification; the reader never guesses.
		{Known: true, Category: "timeout", OccurredAtMillis: 1},
	} {
		got, err := mcpReadErrorsDTO(value)
		if !errors.Is(err, ErrMCPReadUnavailable) || got != (MCPReadErrors{}) {
			t.Fatalf("invalid event accepted: %+v %v", got, err)
		}
	}
	got, err := mcpReadErrorsDTO(MCPReadPublishedError{Category: "PRIVATE_SENTINEL", Retryable: true, OccurredAtMillis: -1})
	data, marshalErr := json.Marshal(got)
	if err != nil || marshalErr != nil || string(data) != `{"available":false}` {
		t.Fatalf("unobserved event escaped: %s %v", data, err)
	}
}

func TestMCPReadPublishedOptionalCopy(t *testing.T) {
	if got := mcpReadPublishedValueLocked(struct{}{}); got != (MCPReadPublishedSnapshot{}) {
		t.Fatal("missing producer supplied values")
	}
	source := &mcpReadPublishedFixture{value: MCPReadPublishedSnapshot{
		Stats: MCPReadPublishedStats{ActiveStreamsKnown: true, ActiveStreams: 0},
		Error: MCPReadPublishedError{Known: true, Category: "network", Retryable: true, OccurredAtMillis: 1},
	}}
	got := mcpReadPublishedValueLocked(source)
	if got != source.value {
		t.Fatal("copy changed published fields")
	}
	got.Stats.ActiveStreams = 99
	got.Error.Category = "storage"
	if source.value.Stats.ActiveStreams != 0 || source.value.Error.Category != "network" {
		t.Fatal("snapshot aliases producer")
	}
}

func TestMCPReadPublishedNoProducerLockAndCancellation(t *testing.T) {
	d := mcpReadFixture("task_1")
	task := d.tasks[0]
	task.fetcher = mcpReadForbiddenFetcher{}
	// A status transition must not block a read on an additional lifecycle lock.
	task.statusLock.Lock()
	defer task.statusLock.Unlock()
	readers := []func(context.Context) error{
		func(ctx context.Context) error { _, err := d.MCPReadGetStats(ctx, task.ID); return err },
		func(ctx context.Context) error { _, err := d.MCPReadGetErrors(ctx, task.ID); return err },
	}
	for _, read := range readers {
		if err := read(context.Background()); err != nil {
			t.Fatal(err)
		}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := read(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		expired, cancelDeadline := context.WithDeadline(context.Background(), time.Unix(1, 0))
		if err := read(expired); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline: %v", err)
		}
		cancelDeadline()
		d.lock.Lock()
		err := read(context.Background())
		d.lock.Unlock()
		if !errors.Is(err, ErrMCPReadBusy) {
			t.Fatalf("main busy: %v", err)
		}
		task.dataLock.Lock()
		err = read(context.Background())
		task.dataLock.Unlock()
		if !errors.Is(err, ErrMCPReadBusy) {
			t.Fatalf("data busy: %v", err)
		}
		if !d.lock.TryLock() {
			t.Fatal("main lock retained")
		}
		d.lock.Unlock()
		if !task.dataLock.TryLock() {
			t.Fatal("data lock retained")
		}
		task.dataLock.Unlock()
	}
	// Real getters with no publisher stay unavailable while unrelated progress
	// updates contend on dataLock; this is not a simulated live statistics cache.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			task.dataLock.Lock()
			task.Progress.Downloaded = int64(i)
			task.dataLock.Unlock()
		}
	}()
	for i := 0; i < 1000; i++ {
		stats, se := d.MCPReadGetStats(context.Background(), task.ID)
		last, ee := d.MCPReadGetErrors(context.Background(), task.ID)
		if (se != nil && !errors.Is(se, ErrMCPReadBusy)) || (ee != nil && !errors.Is(ee, ErrMCPReadBusy)) {
			t.Error("unexpected read failure", se, ee)
		}
		if stats != (MCPReadStats{}) || last != (MCPReadErrors{}) {
			t.Error("missing publisher fabricated observations")
		}
	}
	wg.Wait()
}
