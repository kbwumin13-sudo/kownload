package download

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func recoveryFixtureTask(dir string) *Task {
	task := NewTask()
	task.ID = "recovery-fixture-task"
	task.Protocol = "http"
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "https://fixture.invalid/file"}, Opts: &base.Options{Path: dir}}
	task.Progress = &Progress{}
	initTask(task)
	return task
}
func recoveryFixtureStore(t *testing.T, store AtomicStorage, dir string) (*Task, *taskRecoveryOwner) {
	t.Helper()
	if err := store.Setup([]string{bucketTask, bucketSave, bucketTaskRecovery, bucketRecoveryRequired}); err != nil {
		t.Fatal(err)
	}
	task := recoveryFixtureTask(dir)
	task.recoveryInitialEligible.Store(true)
	if err := store.Put(bucketTask, task.ID, task); err != nil {
		t.Fatal(err)
	}
	owner, err := newTaskRecoveryOwner(store, task)
	if err != nil {
		t.Fatal(err)
	}
	return task, owner
}
func TestRecoveryInitialIssueCrashWorker(t *testing.T) {
	dir := os.Getenv("K15_RECOVERY_CRASH_DIR")
	if dir == "" {
		return
	}
	store := NewBoltStorage(dir)
	_, owner := recoveryFixtureStore(t, store, dir)
	lease, err := owner.begin(1, "")
	if err != nil || lease == nil {
		t.Fatal(err)
	}
	// No protocol IO is performed. The durable grant itself is unresolved.
	os.Exit(73)
}
func TestRecoveryInitialGrantIsNotFreeAgainAfterProcessCrash(t *testing.T) {
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(binary, "-test.run=^TestRecoveryInitialIssueCrashWorker$")
	child.Dir = dir
	child.Env = append(os.Environ(), "K15_RECOVERY_CRASH_DIR="+dir)
	raw, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 73 {
		t.Fatalf("crash worker: %v %s", err, raw)
	}
	store := NewBoltStorage(dir)
	defer store.Close()
	var task Task
	if ok, err := store.Get(bucketTask, "recovery-fixture-task", &task); err != nil || !ok {
		t.Fatal("lost task", err)
	}
	owner, err := newTaskRecoveryOwner(store, &task)
	if err != nil {
		t.Fatal(err)
	}
	oldEpisode := owner.record.Episode
	if owner.record.TotalUsed != 0 || owner.record.InitialAttempt.State != "issued" {
		t.Fatal("initial grant not durable")
	}
	lease, err := owner.begin(2, "")
	if err != nil || lease == nil || owner.record.TotalUsed != 1 || owner.record.TransportUsed != 1 || owner.record.Episode != oldEpisode {
		t.Fatalf("restart granted a new free attempt: %v %+v", err, owner.record)
	}
}
func TestRecoveryWaveChargesOnceAndMembersCannotChurn(t *testing.T) {
	_, owner := recoveryFixtureStore(t, NewMemStorage(), t.TempDir())
	lease, err := owner.begin(1, "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	permits := make(chan *transfer.RecoveryPermit, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := lease.Retry(context.Background(), lease.Round, "a", []string{"b", "a"})
			if e != nil {
				t.Error(e)
				return
			}
			permits <- p
		}()
	}
	wg.Wait()
	close(permits)
	var first *transfer.RecoveryPermit
	for p := range permits {
		if first == nil {
			first = p
		}
		if p != first {
			t.Fatal("repeat query manufactured a new permit")
		}
	}
	if first == nil || owner.record.TotalUsed != 1 {
		t.Fatal("same wave charged more than once")
	}
	if err := first.Consume(); err != nil {
		t.Fatal(err)
	}
	if err := first.Consume(); !errors.Is(err, transfer.ErrRecoveryPermitUsed) {
		t.Fatal("same logical work could repeat IO")
	}
	for _, members := range [][]string{{"a-copy", "b"}, {"a", "b", "c"}} {
		if _, e := lease.Retry(context.Background(), 0, members[0], members); e != transfer.ErrRecoveryWork {
			t.Fatal("work churn entered existing wave", e)
		}
	}
	other, err := lease.Retry(context.Background(), 0, "b", []string{"a", "b"})
	if err != nil || other.Consume() != nil {
		t.Fatal("one member starved the other", err)
	}
	if err := first.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := other.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if owner.record.Actions[0].State != "finished" || owner.record.TotalUsed != 1 {
		t.Fatal("wave did not settle exactly once")
	}
	round := first.Round
	for i := 1; i < recoveryMaxTransport; i++ {
		p, e := lease.Retry(context.Background(), round, "a", []string{"a", "b"})
		if e != nil {
			t.Fatal(e)
		}
		if e = p.Consume(); e != nil {
			t.Fatal(e)
		}
		round = p.Round
	}
	if _, e := lease.Retry(context.Background(), round, "a", []string{"a", "b"}); e != transfer.ErrRecoveryExhausted {
		t.Fatal("transport budget was not bounded", e)
	}
}

type recoveryAckFault struct {
	AtomicStorage
	armed atomic.Bool
}

func (s *recoveryAckFault) UpdateAtomic(fn func(StorageTransaction) error) error {
	err := s.AtomicStorage.UpdateAtomic(fn)
	if err == nil && s.armed.Swap(false) {
		return errors.New("synthetic recovery ACK lost")
	}
	return err
}
func TestRecoveryUnknownDebitReturnsNoPermitAndDoesNotReset(t *testing.T) {
	store := &recoveryAckFault{AtomicStorage: NewMemStorage()}
	task, owner := recoveryFixtureStore(t, store, t.TempDir())
	lease, err := owner.begin(1, "")
	if err != nil {
		t.Fatal(err)
	}
	store.armed.Store(true)
	permit, err := lease.Retry(context.Background(), 0, "single", []string{"single"})
	if permit != nil || err != transfer.ErrRecoveryUnavailable {
		t.Fatal("unknown commit authorized IO", err)
	}
	if _, e := lease.Retry(context.Background(), 0, "single", []string{"single"}); e != transfer.ErrRecoveryStale {
		t.Fatal("unknown action replayed", e)
	}
	restored, err := newTaskRecoveryOwner(store, task)
	if err != nil {
		t.Fatal(err)
	}
	if restored.record.TotalUsed != 1 {
		t.Fatal("unknown committed debit disappeared")
	}
	if _, err = restored.begin(2, ""); err != nil || restored.record.TotalUsed != 2 {
		t.Fatal("restart reset unknown debit", err)
	}
}
func TestRecoveryRequiredStateCannotFallBackToLegacy(t *testing.T) {
	for _, mode := range []string{"missing-ref", "unknown-required", "unknown-record", "missing-task", "wrong-protocol"} {
		t.Run(mode, func(t *testing.T) {
			store := NewMemStorage()
			task, owner := recoveryFixtureStore(t, store, filepath.Join(t.TempDir(), mode))
			var saved Task
			_, _ = store.Get(bucketTask, task.ID, &saved)
			switch mode {
			case "missing-ref":
				saved.Recovery = nil
				_ = store.Put(bucketTask, task.ID, &saved)
			case "unknown-required":
				_ = store.Put(bucketRecoveryRequired, task.ID, &recoveryRequirement{2, "unknown"})
			case "unknown-record":
				owner.record.Version = 2
				_ = store.Put(bucketTaskRecovery, task.ID, &owner.record)
			case "missing-task":
				_ = store.Delete(bucketTask, task.ID)
			case "wrong-protocol":
				saved.Protocol = "bt"
				_ = store.Put(bucketTask, task.ID, &saved)
			}
			if o, e := newTaskRecoveryOwner(store, task); e == nil || o != nil {
				t.Fatal("required state downgraded to legacy")
			}
		})
	}
}

func TestRecoveryStartupAndExplicitContinueFailClosed(t *testing.T) {
	for _, mode := range []string{"missing-ref", "missing-ledger", "missing-required", "future-ref", "future-capability"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			store := NewBoltStorage(dir)
			task, _ := recoveryFixtureStore(t, store, dir)
			var saved Task
			if _, err := store.Get(bucketTask, task.ID, &saved); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing-ref":
				saved.Recovery = nil
				if err := store.Put(bucketTask, task.ID, &saved); err != nil {
					t.Fatal(err)
				}
			case "missing-ledger":
				if err := store.Delete(bucketTaskRecovery, task.ID); err != nil {
					t.Fatal(err)
				}
			case "missing-required":
				if err := store.Delete(bucketRecoveryRequired, task.ID); err != nil {
					t.Fatal(err)
				}
			case "future-ref":
				saved.Recovery = &TaskRecoveryRef{2}
				if err := store.Put(bucketTask, task.ID, &saved); err != nil {
					t.Fatal(err)
				}
			case "future-capability":
				if err := store.Put(bucketRecoveryRequired, task.ID, &recoveryRequirement{1, "future/v2"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			d := operationDownloader(t, NewBoltStorage(dir), dir)
			defer d.Close()
			loaded := d.getTask(task.ID)
			if loaded == nil || !loaded.recoveryHeld() || loaded.fetcher != nil {
				t.Fatal("startup restored incompatible task")
			}
			for _, run := range []func() error{func() error { return d.Continue(&TaskFilter{IDs: []string{task.ID}}) }, func() error { return d.ContinueBatch(&TaskFilter{IDs: []string{task.ID}}) }, func() error { return d.Patch(task.ID, &base.Request{URL: "https://fixture.invalid/new"}, nil) }} {
				if e := run(); !errors.Is(e, transfer.ErrRecoveryHeld) {
					t.Fatalf("explicit path bypassed required state: %v", e)
				}
			}
			if loaded.fetcher != nil {
				t.Fatal("guard performed protocol restore")
			}
		})
	}
}

func TestRecoveryGuardReadDoesNotEnrollLegacy(t *testing.T) {
	dir := t.TempDir()
	d := operationDownloader(t, NewMemStorage(), dir)
	defer d.Close()
	task := recoveryFixtureTask(dir)
	if err := d.storage.Put(bucketTask, task.ID, task); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := d.checkTaskRecovery(task); err != nil {
			t.Fatal(err)
		}
	}
	for _, bucket := range []string{bucketTaskRecovery, bucketRecoveryRequired} {
		var v any
		if found, err := d.storage.Get(bucket, task.ID, &v); err != nil || found {
			t.Fatalf("read enrolled legacy: %v %v", found, err)
		}
	}
	if task.Recovery != nil {
		t.Fatal("read modified Task")
	}
}

func TestRecoveryCleanPauseCertificateAndUncertainRestart(t *testing.T) {
	for _, mode := range []string{"clean", "changed-checkpoint", "unfinished-wave", "unknown-pause"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			store := NewBoltStorage(dir)
			task, owner := recoveryFixtureStore(t, store, dir)
			if _, err := owner.begin(1, ""); err != nil {
				t.Fatal(err)
			}
			// An earlier uncertain run is conservatively charged, not forgotten.
			owner.invalidate()
			lease, err := owner.begin(2, "")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unfinished-wave" {
				p, e := lease.Retry(context.Background(), lease.Round, "range/0/1024", []string{"range/0/1024"})
				if e != nil || p.Consume() != nil {
					t.Fatal(e)
				}
			}
			if err = store.Put(bucketSave, task.ID, map[string]any{"exact": json.Number("9007199254740993")}); err != nil {
				t.Fatal(err)
			}
			owner.invalidate()
			if mode != "unknown-pause" {
				if err = owner.markCleanPaused(); err != nil {
					t.Fatal(err)
				}
			}
			before := owner.record
			if mode == "clean" && before.InitialAttempt.State != "clean-paused" {
				t.Fatal("joined clean pause did not certify checkpoint")
			}
			if mode == "changed-checkpoint" {
				if err = store.Put(bucketSave, task.ID, map[string]any{"exact": json.Number("9007199254740992")}); err != nil {
					t.Fatal(err)
				}
			}
			if err = store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := NewBoltStorage(dir)
			defer reopened.Close()
			var saved Task
			if _, err = reopened.Get(bucketTask, task.ID, &saved); err != nil {
				t.Fatal(err)
			}
			next, err := newTaskRecoveryOwner(reopened, &saved)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = next.begin(3, ""); err != nil {
				t.Fatal(err)
			}
			expected := before.TotalUsed + 1
			if mode == "clean" {
				expected = before.TotalUsed
			}
			if next.record.Episode != before.Episode || next.record.TotalUsed != expected || next.record.TransportUsed != expected || next.record.InitialAttempt.State != "issued" {
				t.Fatalf("pause/restart replenished or mischarged episode: before=%+v after=%+v", before, next.record)
			}
		})
	}
}

func TestRecoveryDefaultsRebindingPreservesIssuedEvidenceAfterReopen(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherited", true: "explicit"}[explicit], func(t *testing.T) {
			dir := t.TempDir()
			store := NewBoltStorage(dir)
			if err := store.Setup([]string{bucketTask, bucketSave, bucketTaskRecovery, bucketRecoveryRequired}); err != nil {
				t.Fatal(err)
			}
			task := recoveryFixtureTask(dir)
			task.recoveryInitialEligible.Store(true)
			if explicit {
				task.Meta.Req.Extra = map[string]any{"header": map[string]string{"user-agent": "explicit-UA"}}
				task.Meta.Req.Proxy = &base.RequestProxy{Mode: base.RequestProxyModeNone}
			}
			if err := store.Put(bucketTask, task.ID, task); err != nil {
				t.Fatal(err)
			}
			oldD, _ := transfer.HTTPExecutionDefaultsHash(task.Meta.Req, "UA-A", "proxy-A")
			owner, err := newTaskRecoveryOwner(store, task, oldD)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = owner.begin(1, ""); err != nil {
				t.Fatal(err)
			}
			old := owner.record
			if err = store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := NewBoltStorage(dir)
			defer reopened.Close()
			var saved Task
			if _, err = reopened.Get(bucketTask, task.ID, &saved); err != nil {
				t.Fatal(err)
			}
			next, err := newTaskRecoveryOwner(reopened, &saved, oldD)
			if err != nil {
				t.Fatal(err)
			}
			newD, _ := transfer.HTTPExecutionDefaultsHash(saved.Meta.Req, "UA-B", "proxy-B")
			if err = next.persistSnapshot(&saved, nil, false, newD); err != nil {
				t.Fatal(err)
			}
			rev := old.SourceRevision
			if !explicit {
				rev++
			}
			if next.record.SourceRevision != rev || next.record.Episode != old.Episode || next.record.TotalUsed != old.TotalUsed || next.record.InitialAttempt != old.InitialAttempt {
				t.Fatal("default rebind lost durable issued evidence")
			}
			if _, err = next.begin(2, ""); err != nil {
				t.Fatal(err)
			}
			if next.record.TotalUsed != 1 || next.record.Episode != old.Episode {
				t.Fatal("config change granted free restart")
			}
		})
	}
}

func TestRecoveryMissingOrBadDefaultsCannotBeReconstructed(t *testing.T) {
	for _, value := range []string{"", "not-a-digest"} {
		store := NewBoltStorage(t.TempDir())
		task, owner := recoveryFixtureStore(t, store, t.TempDir())
		owner.record.ExecutionDefaultsHash = value
		if err := store.Put(bucketTaskRecovery, task.ID, &owner.record); err != nil {
			t.Fatal(err)
		}
		if _, err := newTaskRecoveryOwner(store, task); !errors.Is(err, transfer.ErrRecoveryHeld) {
			t.Fatal("bad defaults were rebound", err)
		}
		store.Close()
	}
}

func TestRecoveryHistoricalEnrollmentNeverGrantsFreeInitialAttempt(t *testing.T) {
	store := NewMemStorage()
	if err := store.Setup([]string{bucketTask, bucketSave, bucketTaskRecovery, bucketRecoveryRequired}); err != nil {
		t.Fatal(err)
	}
	task := recoveryFixtureTask(t.TempDir())
	if err := store.Put(bucketTask, task.ID, task); err != nil {
		t.Fatal(err)
	}
	owner, err := newTaskRecoveryOwner(store, task)
	if err != nil {
		t.Fatal(err)
	}
	if owner.record.InitialAttempt.State != "uncertain" {
		t.Fatal("historical task acquired first-use proof")
	}
	if _, err = owner.begin(1, ""); err != nil {
		t.Fatal(err)
	}
	if owner.record.TotalUsed != 1 {
		t.Fatal("historical enrollment created a free IO attempt")
	}
}

func TestRecoveryCanceledPreparationInvalidatesAlreadyIssuedPermit(t *testing.T) {
	_, owner := recoveryFixtureStore(t, NewMemStorage(), t.TempDir())
	if _, err := owner.begin(1, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	lease, err := owner.currentLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	permit, err := lease.Retry(ctx, 0, "a", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	before := owner.record
	cancel()
	if !errors.Is(permit.Consume(), transfer.ErrRecoveryStale) {
		t.Fatal("canceled generation authorized wire IO")
	}
	if _, err = lease.Retry(context.Background(), 1, "a", []string{"a"}); !errors.Is(err, transfer.ErrRecoveryStale) {
		t.Fatal("cancel bypassed by unrelated context", err)
	}
	if err = owner.FinishRecovery(context.Background(), lease.Token, permit.Round, "a"); !errors.Is(err, transfer.ErrRecoveryStale) {
		t.Fatal("late result wrote action", err)
	}
	if owner.record.TotalUsed != before.TotalUsed || owner.record.Actions[0].State != "charged" {
		t.Fatal("canceled generation changed durable budget")
	}
}

func TestRecoveryStaleOwnerCannotReplaceSourceOrDebitAnotherEpisode(t *testing.T) {
	for _, mode := range []string{"source-revision", "episode"} {
		t.Run(mode, func(t *testing.T) {
			store := NewMemStorage()
			task, owner := recoveryFixtureStore(t, store, t.TempDir())
			lease, err := owner.begin(1, "")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "source-revision" {
				var saved Task
				if _, err = store.Get(bucketTask, task.ID, &saved); err != nil {
					t.Fatal(err)
				}
				other, err := newTaskRecoveryOwner(store, &saved)
				if err != nil {
					t.Fatal(err)
				}
				fresh := saved.clone()
				fresh.Meta.Req.URL = "https://new-source.invalid/file"
				if err = other.persistSnapshot(fresh, nil, false); err != nil {
					t.Fatal(err)
				}
				if err = owner.persistSnapshot(&saved, nil, false); !errors.Is(err, transfer.ErrRecoveryStale) {
					t.Fatal("stale owner overwrote a new source", err)
				}
				if err = owner.persistSnapshot(&saved, nil, false); !errors.Is(err, transfer.ErrRecoveryStale) {
					t.Fatal("conflict readback authorized replay of old source", err)
				}
				var after Task
				if _, err = store.Get(bucketTask, task.ID, &after); err != nil || after.Meta.Req.URL != fresh.Meta.Req.URL {
					t.Fatal("source rolled back", err)
				}
			} else {
				foreign := owner.record
				foreign.Episode = "different-episode"
				if err = store.Put(bucketTaskRecovery, task.ID, &foreign); err != nil {
					t.Fatal(err)
				}
				if permit, e := lease.Retry(context.Background(), 0, "a", []string{"a"}); permit != nil || !errors.Is(e, transfer.ErrRecoveryStale) {
					t.Fatal("foreign episode granted IO", e)
				}
				var after recoveryRecord
				if _, err = store.Get(bucketTaskRecovery, task.ID, &after); err != nil || after.Episode != foreign.Episode || after.TotalUsed != 0 {
					t.Fatal("foreign episode was debited", err)
				}
			}
		})
	}
}
