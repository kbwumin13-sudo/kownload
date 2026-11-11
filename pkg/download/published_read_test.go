package download

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

func TestPublishedReadStatsReplaceAndReject(t *testing.T) {
	task := mcpReadFixture("task_1").tasks[0]
	event := MCPReadPublishedError{Known: true, Category: "network", Retryable: true, OccurredAtMillis: 1}
	task.dataLock.Lock()
	defer task.dataLock.Unlock()
	if err := task.publishReadErrorLocked(event); err != nil {
		t.Fatal(err)
	}
	for mask := 0; mask < 8; mask++ {
		v := MCPReadPublishedStats{ActiveStreamsKnown: mask&1 != 0, ActiveStreams: 0, TargetStreamsKnown: mask&2 != 0, TargetStreams: 4, PeersKnown: mask&4 != 0, Peers: mcpReadMaxInteger}
		if err := task.publishReadStatsLocked(v); err != nil {
			t.Fatal(err)
		}
		got := task.mcpReadPublishedSnapshotLocked()
		want := v
		if !want.TargetStreamsKnown {
			want.TargetStreams = 0
		}
		if !want.PeersKnown {
			want.Peers = 0
		}
		if got.Stats != want || got.Error != event {
			t.Fatalf("partial replacement %d: %+v", mask, got)
		}
		v.ActiveStreams = 999
		got.Stats.TargetStreams = 999
		if task.mcpReadPublishedSnapshotLocked().Stats != want {
			t.Fatal("published values alias argument or snapshot")
		}
	}
	valid := MCPReadPublishedStats{ActiveStreamsKnown: true, ActiveStreams: 2}
	for _, bad := range []int64{-1, mcpReadMaxInteger + 1} {
		for _, v := range []MCPReadPublishedStats{
			{ActiveStreamsKnown: true, ActiveStreams: bad},
			{TargetStreamsKnown: true, TargetStreams: bad},
			{PeersKnown: true, Peers: bad},
		} {
			if err := task.publishReadStatsLocked(valid); err != nil {
				t.Fatal(err)
			}
			if err := task.publishReadStatsLocked(v); err != ErrMCPReadUnavailable {
				t.Fatalf("non-fixed rejection: %v", err)
			}
			got := task.mcpReadPublishedSnapshotLocked()
			if got.Stats != (MCPReadPublishedStats{}) || got.Error != event {
				t.Fatal("invalid stats retained or other cache cleared")
			}
		}
	}
	if err := task.publishReadStatsLocked(MCPReadPublishedStats{ActiveStreams: -1, TargetStreams: mcpReadMaxInteger + 1}); err != nil {
		t.Fatal(err)
	}
	if got := task.mcpReadPublishedSnapshotLocked(); got.Stats != (MCPReadPublishedStats{}) || got.Error != event {
		t.Fatal("unknown payload retained")
	}
}

func TestPublishedReadErrorsReplaceAndReject(t *testing.T) {
	task := mcpReadFixture("task_1").tasks[0]
	stats := MCPReadPublishedStats{ActiveStreamsKnown: true, ActiveStreams: 0, TargetStreamsKnown: true, TargetStreams: 2}
	task.dataLock.Lock()
	defer task.dataLock.Unlock()
	if err := task.publishReadStatsLocked(stats); err != nil {
		t.Fatal(err)
	}
	for _, category := range []string{"network", "authentication", "integrity", "storage", "cancelled", "unknown"} {
		for _, retryable := range []bool{false, true} {
			for _, millis := range []int64{1, 1794432000123, 253402300799999} {
				v := MCPReadPublishedError{Known: true, Category: category, Retryable: retryable, OccurredAtMillis: millis}
				if err := task.publishReadErrorLocked(v); err != nil {
					t.Fatal(err)
				}
				got := task.mcpReadPublishedSnapshotLocked()
				if got.Error != v || got.Stats != stats {
					t.Fatalf("event changed: %+v", got)
				}
				v.Category = "PRIVATE_SENTINEL"
				got.Error.OccurredAtMillis = 0
				if task.mcpReadPublishedSnapshotLocked().Error.Category != category || task.mcpReadPublishedSnapshotLocked().Error.OccurredAtMillis != millis {
					t.Fatal("event aliases argument or result")
				}
			}
		}
	}
	valid := MCPReadPublishedError{Known: true, Category: "network", OccurredAtMillis: 1}
	for _, v := range []MCPReadPublishedError{
		{Known: true, Category: "", OccurredAtMillis: 1},
		{Known: true, Category: "PRIVATE_SENTINEL", OccurredAtMillis: 1},
		{Known: true, Category: "timeout", OccurredAtMillis: 1},
		{Known: true, Category: "network", OccurredAtMillis: -1},
		{Known: true, Category: "network", OccurredAtMillis: 0},
		{Known: true, Category: "network", OccurredAtMillis: 253402300800000},
	} {
		if err := task.publishReadErrorLocked(valid); err != nil {
			t.Fatal(err)
		}
		if err := task.publishReadErrorLocked(v); err != ErrMCPReadUnavailable {
			t.Fatalf("non-fixed rejection: %v", err)
		}
		got := task.mcpReadPublishedSnapshotLocked()
		if got.Error != (MCPReadPublishedError{}) || got.Stats != stats {
			t.Fatal("invalid error retained or other cache cleared")
		}
	}
	if err := task.publishReadErrorLocked(MCPReadPublishedError{Category: "PRIVATE_SENTINEL", Retryable: true, OccurredAtMillis: -1}); err != nil {
		t.Fatal(err)
	}
	if got := task.mcpReadPublishedSnapshotLocked(); got.Error != (MCPReadPublishedError{}) || got.Stats != stats {
		t.Fatal("unobserved error retained")
	}
}

func TestPublishedReadMethodsAndGetterCopy(t *testing.T) {
	d := mcpReadFixture("task_1")
	task := d.tasks[0]
	task.fetcher = mcpReadForbiddenFetcher{}
	before := *task.Progress
	status := task.Status
	// Calling methods while these locks are held proves they do not relock them.
	d.lock.Lock()
	task.statusLock.Lock()
	task.dataLock.Lock()
	statsErr := task.publishReadStatsLocked(MCPReadPublishedStats{ActiveStreamsKnown: true, TargetStreamsKnown: true, TargetStreams: 4})
	errorErr := task.publishReadErrorLocked(MCPReadPublishedError{Known: true, Category: "network", OccurredAtMillis: 1})
	task.dataLock.Unlock()
	task.statusLock.Unlock()
	d.lock.Unlock()
	if statsErr != nil || errorErr != nil {
		t.Fatal(statsErr, errorErr)
	}
	stats, se := d.MCPReadGetStats(context.Background(), task.ID)
	last, ee := d.MCPReadGetErrors(context.Background(), task.ID)
	if se != nil || ee != nil {
		t.Fatal(se, ee)
	}
	statsJSON, _ := json.Marshal(stats)
	lastJSON, _ := json.Marshal(last)
	if string(statsJSON) != `{"available":true,"activeStreams":0,"targetStreams":4}` || string(lastJSON) != `{"available":true,"category":"network","retryable":false,"occurredAt":"1970-01-01T00:00:00.001Z"}` {
		t.Fatalf("wire values: %s / %s", statsJSON, lastJSON)
	}
	*stats.ActiveStreams = 99
	*last.Retryable = true
	task.dataLock.Lock()
	copied := task.mcpReadPublishedSnapshotLocked()
	task.clearReadPublishedLocked()
	task.clearReadPublishedLocked() // Explicit clear is idempotent.
	cleared := task.mcpReadPublishedSnapshotLocked()
	task.dataLock.Unlock()
	if copied.Stats.ActiveStreams != 0 || copied.Error.Retryable || cleared != (MCPReadPublishedSnapshot{}) {
		t.Fatal("alias or failed clear")
	}
	stats, se = d.MCPReadGetStats(context.Background(), task.ID)
	last, ee = d.MCPReadGetErrors(context.Background(), task.ID)
	if se != nil || ee != nil || stats != (MCPReadStats{}) || last != (MCPReadErrors{}) {
		t.Fatal("clear not visible to getters")
	}
	if *task.Progress != before || task.Status != status {
		t.Fatal("publisher modified lifecycle/progress")
	}
}

// Method-level concurrency only: no real HTTP producer, token lifecycle, timer,
// or network is involved. Those remain separate integration acceptance work.
func TestPublishedReadConcurrentMethodSnapshots(t *testing.T) {
	d := mcpReadFixture("task_1")
	task := d.tasks[0]
	task.fetcher = mcpReadForbiddenFetcher{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(0); i < 1000; i++ {
			task.dataLock.Lock()
			se := task.publishReadStatsLocked(MCPReadPublishedStats{ActiveStreamsKnown: true, ActiveStreams: i, TargetStreamsKnown: true, TargetStreams: i + 1})
			ee := task.publishReadErrorLocked(MCPReadPublishedError{Known: true, Category: "network", Retryable: i%2 == 0, OccurredAtMillis: i + 1})
			if i%3 == 0 {
				task.clearReadPublishedLocked()
			}
			task.dataLock.Unlock()
			if se != nil || ee != nil {
				t.Error(se, ee)
			}
		}
	}()
	for i := 0; i < 1000; i++ {
		stats, se := d.MCPReadGetStats(context.Background(), task.ID)
		last, ee := d.MCPReadGetErrors(context.Background(), task.ID)
		if se != nil && !errors.Is(se, ErrMCPReadBusy) {
			t.Error(se)
		}
		if ee != nil && !errors.Is(ee, ErrMCPReadBusy) {
			t.Error(ee)
		}
		if stats.Available && (stats.ActiveStreams == nil || stats.TargetStreams == nil || *stats.TargetStreams != *stats.ActiveStreams+1 || stats.Peers != nil) {
			t.Error("torn statistics snapshot")
		}
		if last.Available && (last.Category != "network" || last.Retryable == nil || last.OccurredAt == "") {
			t.Error("torn error snapshot")
		}
	}
	wg.Wait()
}
