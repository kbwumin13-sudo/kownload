package download

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type completionStateFixture struct {
	store     AtomicStorage
	dataset   string
	candidate *Task
	initial   transfer.VerifiedCompletionRecord
	token     transfer.RecoveryToken
}

func newCompletionStateFixture(t *testing.T, backend string, prepare bool) *completionStateFixture {
	t.Helper()
	var store AtomicStorage = NewMemStorage()
	if backend == "bolt" {
		store = NewBoltStorage(t.TempDir())
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Setup([]string{bucketConfig, bucketTask, bucketTaskCompletion, bucketCompletionRequired,
		bucketTaskRecovery, bucketRecoveryRequired}); err != nil {
		t.Fatal(err)
	}
	if err := InitializeDatasetIdentity(store); err != nil {
		t.Fatal(err)
	}
	d := &Downloader{storage: store}
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	candidate := NewTask()
	candidate.ID, candidate.Protocol = "completion-state-task", "http"
	candidate.Recovery = &TaskRecoveryRef{Version: 1}
	candidate.Progress = &Progress{Used: 7}
	candidate.Meta = &fetcher.FetcherMeta{
		Req: &base.Request{URL: "https://fixture.invalid/file"},
		Opts: &base.Options{Name: "original.bin", Path: filepath.Join(t.TempDir(), "original-target"),
			Extra: map[string]any{"exact": json.Number("9007199254740993"), "nested": map[string]any{"keep": "original"}}},
	}
	candidate.CreationOperation = &CreationOperation{Version: 1, Namespace: batchOperationNamespace, OperationID: "stable-operation", Index: 2}
	candidate.Handoff = new(BrowserHandoff)
	f := &completionStateFixture{store: store, dataset: identity.DatasetID, candidate: candidate,
		initial: transfer.VerifiedCompletionRecord{Version: 1, ID: "stable-completion", Phase: transfer.CompletionPrepared,
			Size: -1, ExpectedSHA256: strings.Repeat("a", 64)},
		token: transfer.RecoveryToken{TaskID: candidate.ID, Episode: "fixture-episode", Generation: 1, SourceRevision: 1}}
	if prepare {
		f.prepare(t, store)
	}
	return f
}

func (f *completionStateFixture) prepare(t *testing.T, store AtomicStorage) {
	t.Helper()
	defaults, err := transfer.HTTPExecutionDefaultsHash(f.candidate.Meta.Req, "", "direct")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := recoverySourceHash(f.candidate.Meta.Req, defaults)
	if err != nil {
		t.Fatal(err)
	}
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := prepareTaskCompletion(tx, f.dataset, f.candidate, f.initial); err != nil {
			return err
		}
		r := recoveryRecord{Version: 1, TaskID: f.token.TaskID, Episode: f.token.Episode,
			Generation: f.token.Generation, SourceRevision: f.token.SourceRevision, SourceHash: hash,
			ExecutionDefaultsHash: defaults, InitialAttempt: recoveryInitialAttempt{State: "issued", Generation: 1}}
		if err := tx.Put(bucketTaskRecovery, f.token.TaskID, r); err != nil {
			return err
		}
		return tx.Put(bucketRecoveryRequired, f.token.TaskID, recoveryRequirement{Version: 1, Capability: recoveryCapability})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *completionStateFixture) read() (*Task, transfer.VerifiedCompletionRecord, error) {
	var task *Task
	var record transfer.VerifiedCompletionRecord
	err := withDatasetReadView(f.store, f.dataset, func(reader datasetReader) error {
		var err error
		task, record, err = readTaskCompletion(reader, f.token.TaskID)
		return err
	})
	return task, record, err
}

func (f *completionStateFixture) save(t *testing.T, before, after transfer.VerifiedCompletionRecord) {
	t.Helper()
	if err := saveTaskCompletionState(f.store, f.dataset, f.token, before, after); err != nil {
		t.Fatal(err)
	}
}

func (f *completionStateFixture) published(t *testing.T) transfer.VerifiedCompletionRecord {
	t.Helper()
	return f.publishedSize(t, 123)
}

func (f *completionStateFixture) publishedSize(t *testing.T, size int64) transfer.VerifiedCompletionRecord {
	t.Helper()
	r := f.initial
	bound := r
	bound.StagingName, bound.FinalName = ".owned-stage", "final.bin"
	bound.DirectoryIdentity = transfer.CompletionFileIdentity{Device: 1, Inode: 11}
	bound.StagingIdentity = transfer.CompletionFileIdentity{Device: 1, Inode: 22}
	f.save(t, r, bound)
	r = bound
	next := r
	next.Phase, next.Size = transfer.CompletionWaiting, size
	f.save(t, r, next)
	r = next
	next.Phase = transfer.CompletionVerifying
	f.save(t, r, next)
	r = next
	next.Phase, next.ActualSHA256 = transfer.CompletionVerified, next.ExpectedSHA256
	f.save(t, r, next)
	r = next
	next.Phase = transfer.CompletionPublished
	f.save(t, r, next)
	return next
}

func requireCompletionStateKind(t *testing.T, err error, kind TaskCompletionStateErrorKind) {
	t.Helper()
	var classified *TaskCompletionStateError
	if !errors.As(err, &classified) || classified.Kind != kind {
		t.Fatalf("want fixed category %s, got %v", kind, err)
	}
}

func TestCompletionStateReadOwnedAndCommitPreservation(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := newCompletionStateFixture(t, backend, true)
			first, r, err := f.read()
			if err != nil || first == nil || first.Completion.CompletionID != r.ID || r != f.initial {
				t.Fatal("missing prepared tuple", err)
			}
			if first.Meta.Opts.Extra.(map[string]any)["exact"] != json.Number("9007199254740993") {
				t.Fatal("owned state read rounded an opaque integer")
			}
			first.Meta.Opts.Path = "changed-outside-view"
			first.Meta.Opts.Extra.(map[string]any)["nested"].(map[string]any)["keep"] = "changed"
			second, _, err := f.read()
			if err != nil || second.Meta.Opts.Path != f.candidate.Meta.Opts.Path ||
				second.Meta.Opts.Extra.(map[string]any)["nested"].(map[string]any)["keep"] != "original" {
				t.Fatal("view exposed stored Task/Meta aliases", err)
			}
			published := f.published(t)
			// An unrelated metadata update after an earlier caller snapshot must
			// survive C. Do not change req/source: S3 must reject that separately.
			if err := f.store.UpdateAtomic(func(tx StorageTransaction) error {
				current, _, err := readTaskCompletion(tx, f.token.TaskID)
				if err != nil {
					return err
				}
				current.Meta.Opts.Extra.(map[string]any)["later"] = "preserve-current-DB-value"
				return tx.Put(bucketTask, f.token.TaskID, current)
			}); err != nil {
				t.Fatal(err)
			}
			before, _, err := f.read()
			if err != nil {
				t.Fatal(err)
			}
			committed := published
			committed.Phase = transfer.CompletionCommitted
			progress := &Progress{Used: 3, Downloaded: committed.Size}
			fresh, err := commitTaskCompletionState(f.store, f.dataset, f.token, published, committed, progress)
			if err != nil || !fresh {
				t.Fatal("C did not acknowledge one winner", err)
			}
			after, stored, err := f.read()
			if err != nil || stored != committed || after.Status != base.DownloadStatusDone ||
				after.Progress.Downloaded != committed.Size || after.Progress.Used != 7 {
				t.Fatal("C did not publish a consistent durable tuple", err)
			}
			if !reflect.DeepEqual(before.Meta, after.Meta) || !reflect.DeepEqual(before.Handoff, after.Handoff) ||
				!reflect.DeepEqual(before.CreationOperation, after.CreationOperation) ||
				!reflect.DeepEqual(before.Recovery, after.Recovery) || !reflect.DeepEqual(before.Completion, after.Completion) ||
				before.CreatedAt != after.CreatedAt {
				t.Fatal("C overwrote current DB metadata/ref/history")
			}
			if progress.Used != 3 || f.candidate.Status != base.DownloadStatusReady || f.candidate.Progress.Downloaded != 0 {
				t.Fatal("C mutated caller progress or the original Task")
			}
			fresh, err = commitTaskCompletionState(f.store, f.dataset, f.token, published, committed, progress)
			if err != nil || fresh {
				t.Fatal("repeated same C granted a second success", err)
			}
		})
	}
}

func TestCompletionStateMissingCorruptAndMixedTuple(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, bad := range []string{"task_missing", "record_missing", "required_missing", "ref_missing", "ref_version", "ref_empty_cid", "ref_other_cid",
			"record_other_cid", "record_version", "required_version", "required_capability", "task_wrong_id", "protocol", "scheme", "unknown_phase", "digest", "done_without_C", "C_without_done"} {
			t.Run(backend+"/"+bad, func(t *testing.T) {
				f := newCompletionStateFixture(t, backend, true)
				task, record, err := f.read()
				if err != nil {
					t.Fatal(err)
				}
				required := completionRequirement{Version: 1, Capability: transfer.VerifiedCompletionCapability}
				switch bad {
				case "task_missing":
					err = f.store.Delete(bucketTask, f.token.TaskID)
				case "record_missing":
					err = f.store.Delete(bucketTaskCompletion, f.token.TaskID)
				case "required_missing":
					err = f.store.Delete(bucketCompletionRequired, f.token.TaskID)
				case "ref_missing":
					task.Completion = nil
				case "ref_version":
					task.Completion.Version = 2
				case "ref_empty_cid":
					task.Completion.CompletionID = ""
				case "ref_other_cid":
					task.Completion.CompletionID = "foreign-completion"
				case "record_other_cid":
					record.ID = "foreign-completion"
				case "record_version":
					record.Version = 2
				case "required_version":
					required.Version = 2
				case "required_capability":
					required.Capability = "foreign-capability"
				case "task_wrong_id":
					task.ID = "foreign-task"
				case "protocol":
					task.Protocol = "netdisk"
				case "scheme":
					task.Meta.Req.URL = "ftp://fixture.invalid/file"
				case "unknown_phase":
					record.Phase = "future-phase"
				case "digest":
					record.ExpectedSHA256 = "bad"
				case "done_without_C":
					task.Status = base.DownloadStatusDone
				case "C_without_done":
					record = f.published(t)
					record.Phase = transfer.CompletionCommitted
				}
				if err != nil {
					t.Fatal(err)
				}
				if bad != "task_missing" && bad != "record_missing" && bad != "required_missing" {
					if err := f.store.UpdateAtomic(func(tx StorageTransaction) error {
						if err := tx.Put(bucketTask, f.token.TaskID, task); err != nil {
							return err
						}
						if err := tx.Put(bucketTaskCompletion, f.token.TaskID, record); err != nil {
							return err
						}
						return tx.Put(bucketCompletionRequired, f.token.TaskID, required)
					}); err != nil {
						t.Fatal(err)
					}
				}
				returned, returnedRecord, err := f.read()
				requireCompletionStateKind(t, err, TaskCompletionHeld)
				if returned != nil || returnedRecord != (transfer.VerifiedCompletionRecord{}) {
					t.Fatal("failed tuple read leaked a partial snapshot")
				}
			})
		}
	}
}

func TestCompletionStateLegacyAndOrphanAreDifferent(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := newCompletionStateFixture(t, backend, false)
			if err := f.store.Put(bucketTask, f.token.TaskID, f.candidate); err != nil {
				t.Fatal(err)
			}
			_, _, err := f.read()
			requireCompletionStateKind(t, err, TaskCompletionNotRequired)
			if err := f.store.Put(bucketTaskCompletion, f.token.TaskID, f.initial); err != nil {
				t.Fatal(err)
			}
			_, _, err = f.read()
			requireCompletionStateKind(t, err, TaskCompletionHeld)
		})
	}
}

func TestCompletionStateSaveBindingTokenAndCAS(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := newCompletionStateFixture(t, backend, true)
			published := f.published(t)
			for _, mutate := range []func(*transfer.VerifiedCompletionRecord){
				func(r *transfer.VerifiedCompletionRecord) { r.ID = "another-completion" },
				func(r *transfer.VerifiedCompletionRecord) {
					r.ExpectedSHA256 = strings.Repeat("b", 64)
					r.ActualSHA256 = r.ExpectedSHA256
				},
				func(r *transfer.VerifiedCompletionRecord) { r.StagingName = "other-stage" },
				func(r *transfer.VerifiedCompletionRecord) { r.FinalName = "other-final" },
				func(r *transfer.VerifiedCompletionRecord) { r.StagingIdentity.Inode++ },
				func(r *transfer.VerifiedCompletionRecord) { r.DirectoryIdentity.Inode++ },
				func(r *transfer.VerifiedCompletionRecord) { r.Size++ },
				func(r *transfer.VerifiedCompletionRecord) { r.Phase = transfer.CompletionCommitted },
				func(r *transfer.VerifiedCompletionRecord) { r.Phase = transfer.CompletionWaiting; r.ActualSHA256 = "" },
			} {
				next := published
				mutate(&next)
				if err := saveTaskCompletionState(f.store, f.dataset, f.token, published, next); err == nil {
					t.Fatal("bound fields/phase were replaced")
				}
			}
			for _, mutate := range []func(*transfer.RecoveryToken){
				func(v *transfer.RecoveryToken) { v.Episode = "old-episode" },
				func(v *transfer.RecoveryToken) { v.Generation++ },
				func(v *transfer.RecoveryToken) { v.SourceRevision++ },
			} {
				token := f.token
				mutate(&token)
				err := saveTaskCompletionState(f.store, f.dataset, token, published, published)
				requireCompletionStateKind(t, err, TaskCompletionRunChanged)
			}
			if err := saveTaskCompletionState(f.store, strings.Repeat("0", 32), f.token, published, published); !errors.Is(err, ErrDatasetMismatch) {
				t.Fatal("wrong dataset did not fail closed", err)
			}
			if err := saveTaskCompletionState(f.store, f.dataset, f.token, f.initial, published); err == nil {
				t.Fatal("stale full-record expected value accepted")
			}
			reverify := published
			reverify.Phase, reverify.ActualSHA256 = transfer.CompletionVerifying, ""
			f.save(t, published, reverify)
			_, current, err := f.read()
			if err != nil || current != reverify {
				t.Fatal("uncommitted recovery did not clear old actual digest", err)
			}
		})
	}
}

type completionStateFault struct {
	AtomicStorage
	failPut int
	ackLost bool
}
type completionStateFaultTx struct {
	StorageTransaction
	failAt int
	puts   int
}

var errCompletionFixtureFault = errors.New("private fixture backend error must not appear in public error")

func (s completionStateFault) UpdateAtomic(body func(StorageTransaction) error) error {
	err := s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error {
		return body(&completionStateFaultTx{StorageTransaction: tx, failAt: s.failPut})
	})
	if err == nil && s.ackLost {
		return errCompletionFixtureFault
	}
	return err
}
func (tx *completionStateFaultTx) Put(bucket, key string, value any) error {
	if err := tx.StorageTransaction.Put(bucket, key, value); err != nil {
		return err
	}
	tx.puts++
	if tx.puts == tx.failAt {
		return errCompletionFixtureFault
	}
	return nil
}

func TestCompletionStateAtomicRollbackAndUnknownACK(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, at := range []int{1, 2, 3} {
			t.Run(backend+"/M/"+string(rune('0'+at)), func(t *testing.T) {
				f := newCompletionStateFixture(t, backend, false)
				err := completionStateFault{AtomicStorage: f.store, failPut: at}.UpdateAtomic(func(tx StorageTransaction) error {
					return prepareTaskCompletion(tx, f.dataset, f.candidate, f.initial)
				})
				if err == nil {
					t.Fatal("M write fault swallowed")
				}
				for _, bucket := range []string{bucketTask, bucketTaskCompletion, bucketCompletionRequired} {
					var raw json.RawMessage
					found, err := f.store.Get(bucket, f.token.TaskID, &raw)
					if err != nil || found {
						t.Fatal("partial M tuple survived", bucket, err)
					}
				}
			})
			t.Run(backend+"/C/"+string(rune('0'+at)), func(t *testing.T) {
				f := newCompletionStateFixture(t, backend, true)
				published := f.published(t)
				committed := published
				committed.Phase = transfer.CompletionCommitted
				fresh, err := commitTaskCompletionState(completionStateFault{AtomicStorage: f.store, failPut: at},
					f.dataset, f.token, published, committed, &Progress{Downloaded: committed.Size})
				if err == nil || fresh || strings.Contains(err.Error(), errCompletionFixtureFault.Error()) {
					t.Fatal("C fault admitted success or exposed backend text", err)
				}
				task, record, err := f.read()
				if err != nil || task.Status == base.DownloadStatusDone || record != published {
					t.Fatal("C rollback left a partial tuple", err)
				}
			})
		}
		t.Run(backend+"/unknown-C-ACK", func(t *testing.T) {
			f := newCompletionStateFixture(t, backend, true)
			published := f.published(t)
			committed := published
			committed.Phase = transfer.CompletionCommitted
			progress := &Progress{Downloaded: committed.Size}
			fresh, err := commitTaskCompletionState(completionStateFault{AtomicStorage: f.store, ackLost: true},
				f.dataset, f.token, published, committed, progress)
			if err == nil || fresh {
				t.Fatal("unknown ACK granted success", err)
			}
			// Query the same tuple first. Bolt closes/reopens the actual database.
			if bolt, ok := f.store.(*BoltStorage); ok {
				dir := filepath.Dir(bolt.path)
				if err := bolt.Close(); err != nil {
					t.Fatal(err)
				}
				f.store = NewBoltStorage(dir)
				t.Cleanup(func() { _ = f.store.Close() })
			}
			task, r, err := f.read()
			if err != nil || task.Status != base.DownloadStatusDone || r != committed {
				t.Fatal("unknown ACK query lost durable C", err)
			}
			fresh, err = commitTaskCompletionState(f.store, f.dataset, f.token, published, committed, progress)
			if err != nil || fresh {
				t.Fatal("confirmed same C replayed success", err)
			}
		})
	}
}

func TestCompletionStateCommitConcurrentWinnerAndProgressWhitelist(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := newCompletionStateFixture(t, backend, true)
			published := f.published(t)
			committed := published
			committed.Phase = transfer.CompletionCommitted
			for _, p := range []*Progress{nil, {Downloaded: committed.Size - 1}, {Downloaded: committed.Size, Used: -1},
				{Downloaded: committed.Size, Speed: 1}, {Downloaded: committed.Size, Uploaded: 1},
				{Downloaded: committed.Size, MediaPhase: "merging"}, {Downloaded: committed.Size, ExtractStatus: ExtractStatusDone},
				{Downloaded: committed.Size, MultiPartBaseName: "other"}} {
				fresh, err := commitTaskCompletionState(f.store, f.dataset, f.token, published, committed, p)
				if err == nil || fresh {
					t.Fatal("non-whitelisted progress committed")
				}
			}
			var wg sync.WaitGroup
			results := make(chan bool, 6)
			failures := make(chan error, 6)
			for i := 0; i < 6; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					fresh, err := commitTaskCompletionState(f.store, f.dataset, f.token, published, committed, &Progress{Downloaded: committed.Size})
					results <- fresh
					failures <- err
				}()
			}
			wg.Wait()
			close(results)
			close(failures)
			count := 0
			for fresh := range results {
				if fresh {
					count++
				}
			}
			for err := range failures {
				if err != nil {
					t.Fatal(err)
				}
			}
			if count != 1 {
				t.Fatal("C did not choose exactly one fresh local winner", count)
			}
			if err := saveTaskCompletionState(f.store, f.dataset, f.token, committed, published); err == nil {
				t.Fatal("committed state regressed")
			}
		})
	}
}

func TestCompletionStateSaveRollbackUnknownACKAndConcurrentCAS(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := newCompletionStateFixture(t, backend, true)
			next := f.initial
			next.StagingName = ".first-stage"
			for _, at := range []int{1, 2} {
				err := saveTaskCompletionState(completionStateFault{AtomicStorage: f.store, failPut: at}, f.dataset, f.token, f.initial, next)
				if err == nil {
					t.Fatal("save fault swallowed")
				}
				_, r, err := f.read()
				if err != nil || r != f.initial {
					t.Fatal("partial save survived", err)
				}
			}
			err := saveTaskCompletionState(completionStateFault{AtomicStorage: f.store, ackLost: true}, f.dataset, f.token, f.initial, next)
			if err == nil {
				t.Fatal("save unknown ACK swallowed")
			}
			_, r, err := f.read()
			if err != nil || r != next {
				t.Fatal("same-ID query did not resolve save ACK", err)
			}
			if err := saveTaskCompletionState(f.store, f.dataset, f.token, f.initial, next); err == nil {
				t.Fatal("stale CAS after unknown ACK accepted")
			}
			left, right := r, r
			left.FinalName, right.FinalName = "left.bin", "right.bin"
			out := make(chan error, 2)
			go func() { out <- saveTaskCompletionState(f.store, f.dataset, f.token, r, left) }()
			go func() { out <- saveTaskCompletionState(f.store, f.dataset, f.token, r, right) }()
			wins := 0
			for i := 0; i < 2; i++ {
				if <-out == nil {
					wins++
				}
			}
			if wins != 1 {
				t.Fatal("full-record CAS did not have one winner", wins)
			}
		})
	}
}

func TestCompletionStateEmptyFileAndDeletedTask(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := newCompletionStateFixture(t, backend, true)
			pub := f.publishedSize(t, 0)
			done := pub
			done.Phase = transfer.CompletionCommitted
			fresh, err := commitTaskCompletionState(f.store, f.dataset, f.token, pub, done, &Progress{})
			if err != nil || !fresh {
				t.Fatal("known empty file could not commit", err)
			}
			if err := f.store.Delete(bucketTask, f.token.TaskID); err != nil {
				t.Fatal(err)
			}
			fresh, err = commitTaskCompletionState(f.store, f.dataset, f.token, pub, done, &Progress{})
			if err == nil || fresh {
				t.Fatal("deleted Task was recreated", err)
			}
			var task Task
			if exists, err := f.store.Get(bucketTask, f.token.TaskID, &task); err != nil || exists {
				t.Fatal("Task resurrected", err)
			}
			_, _, err = f.read()
			requireCompletionStateKind(t, err, TaskCompletionHeld)
		})
	}
}

func TestCompletionStateDoesNotLockPublishedTaskSnapshot(t *testing.T) {
	f := newCompletionStateFixture(t, "mem", true)
	pub := f.published(t)
	done := pub
	done.Phase = transfer.CompletionCommitted
	var live []*Task
	if err := f.store.List(bucketTask, &live); err != nil || len(live) != 1 {
		t.Fatal("List fixture", err)
	}
	live[0].dataLock.Lock()
	locked := true
	defer func() {
		if locked {
			live[0].dataLock.Unlock()
		}
	}()
	result := make(chan error, 1)
	go func() {
		fresh, err := commitTaskCompletionState(f.store, f.dataset, f.token, pub, done, &Progress{Downloaded: done.Size})
		if err == nil && !fresh {
			err = errors.New("no fresh winner")
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		live[0].dataLock.Unlock()
		locked = false
		<-result // Release and join the waiter before failing the fixture.
		t.Fatal("state transaction tried to lock a published/live Task snapshot")
	}
}

type completionStateCrashStore struct {
	AtomicStorage
	at int
}
type completionStateCrashTx struct {
	StorageTransaction
	at, puts int
}

func (s completionStateCrashStore) UpdateAtomic(fn func(StorageTransaction) error) error {
	err := s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error {
		return fn(&completionStateCrashTx{StorageTransaction: tx, at: s.at})
	})
	if err == nil && s.at == 4 {
		os.Exit(97)
	}
	return err
}
func (tx *completionStateCrashTx) Put(bucket, key string, v any) error {
	if err := tx.StorageTransaction.Put(bucket, key, v); err != nil {
		return err
	}
	tx.puts++
	if tx.puts == tx.at {
		os.Exit(97)
	}
	return nil
}

func TestCompletionStateCrashWorker(t *testing.T) {
	dir := os.Getenv("K12_STATE_CRASH_DIR")
	if dir == "" {
		return
	}
	at := int(os.Getenv("K12_STATE_CRASH_AT")[0] - '0')
	store := NewBoltStorage(dir)
	defer store.Close()
	d := &Downloader{storage: store}
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	var r transfer.VerifiedCompletionRecord
	var recovery recoveryRecord
	err = withDatasetReadView(store, identity.DatasetID, func(reader datasetReader) error {
		var e error
		_, r, e = readTaskCompletion(reader, "completion-state-task")
		if e != nil {
			return e
		}
		_, e = reader.Get(bucketTaskRecovery, "completion-state-task", &recovery)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	token := transfer.RecoveryToken{TaskID: "completion-state-task", Episode: recovery.Episode, Generation: recovery.Generation, SourceRevision: recovery.SourceRevision}
	done := r
	done.Phase = transfer.CompletionCommitted
	_, err = commitTaskCompletionState(completionStateCrashStore{AtomicStorage: store, at: at}, identity.DatasetID, token, r, done, &Progress{Downloaded: r.Size})
	t.Fatal("crash point was not reached", err)
}

func TestCompletionStateRealBoltCrashAtomicC(t *testing.T) {
	for _, at := range []int{1, 2, 3, 4} {
		t.Run(string(rune('0'+at)), func(t *testing.T) {
			f := newCompletionStateFixture(t, "bolt", true)
			pub := f.published(t)
			dir := filepath.Dir(f.store.(*BoltStorage).path)
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestCompletionStateCrashWorker$", "-test.timeout=15s")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "K12_STATE_CRASH_DIR="+dir, "K12_STATE_CRASH_AT="+string(rune('0'+at)))
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 97 {
				t.Fatalf("child failed before crash seam: %v %s", err, output)
			}
			f.store = NewBoltStorage(dir)
			t.Cleanup(func() { _ = f.store.Close() })
			task, r, err := f.read()
			if err != nil {
				t.Fatal("three-way tuple tore across real process exit", err)
			}
			if at < 4 {
				if r != pub || task.Status == base.DownloadStatusDone {
					t.Fatal("uncommitted partial C became visible")
				}
			} else {
				if r.Phase != transfer.CompletionCommitted || task.Status != base.DownloadStatusDone {
					t.Fatal("durable C lost after ACK-window crash")
				}
			}
		})
	}
}

func TestCompletionStateRejectsNullAndUnknownGuardFields(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		for _, bad := range []string{"null-ref-orphan", "ref-extra", "required-extra", "record-extra"} {
			t.Run(backend+"/"+bad, func(t *testing.T) {
				f := newCompletionStateFixture(t, backend, true)
				var raw json.RawMessage
				bucket := bucketTask
				if bad == "required-extra" {
					bucket = bucketCompletionRequired
				}
				if bad == "record-extra" {
					bucket = bucketTaskCompletion
				}
				if err := withDatasetReadView(f.store, f.dataset, func(reader datasetReader) error {
					_, err := reader.Get(bucket, f.token.TaskID, &raw)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				var object map[string]any
				if err := decodeStoredJSON(raw, &object); err != nil {
					t.Fatal(err)
				}
				if bad == "null-ref-orphan" {
					object["completion"] = nil
					if err := f.store.Delete(bucketTaskCompletion, f.token.TaskID); err != nil {
						t.Fatal(err)
					}
					if err := f.store.Delete(bucketCompletionRequired, f.token.TaskID); err != nil {
						t.Fatal(err)
					}
				} else if bad == "ref-extra" {
					object["completion"].(map[string]any)["futureField"] = true
				} else {
					object["futureField"] = true
				}
				// Deliberate corruption of a private fixture, never a production
				// storage representation or substitute Task serializer.
				if err := f.store.Put(bucket, f.token.TaskID, object); err != nil {
					t.Fatal(err)
				}
				_, _, err := f.read()
				requireCompletionStateKind(t, err, TaskCompletionHeld)
			})
		}
	}
}

func TestCompletionStateDoesNotEraseIncompatibleDBProgress(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := newCompletionStateFixture(t, backend, true)
			published := f.published(t)
			committed := published
			committed.Phase = transfer.CompletionCommitted
			if err := f.store.UpdateAtomic(func(tx StorageTransaction) error {
				task, _, err := readTaskCompletion(tx, f.token.TaskID)
				if err != nil {
					return err
				}
				task.Progress.MediaPhase = "foreign-phase"
				return tx.Put(bucketTask, f.token.TaskID, task)
			}); err != nil {
				t.Fatal(err)
			}
			fresh, err := commitTaskCompletionState(f.store, f.dataset, f.token, published, committed, &Progress{Downloaded: committed.Size})
			if err == nil || fresh {
				t.Fatal("incompatible DB progress was silently erased", err)
			}
			task, record, err := f.read()
			if err != nil || task.Progress.MediaPhase != "foreign-phase" || record != published {
				t.Fatal("incompatible DB state was changed", err)
			}
		})
	}
}

func TestCompletionStateDuplicateCDoesNotRewriteLaterProgress(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := newCompletionStateFixture(t, backend, true)
			pub := f.published(t)
			done := pub
			done.Phase = transfer.CompletionCommitted
			if fresh, err := commitTaskCompletionState(f.store, f.dataset, f.token, pub, done, &Progress{Downloaded: done.Size}); err != nil || !fresh {
				t.Fatal(err)
			}
			if err := f.store.UpdateAtomic(func(tx StorageTransaction) error {
				task, _, err := readTaskCompletion(tx, f.token.TaskID)
				if err != nil {
					return err
				}
				task.Progress.ExtractStatus = ExtractStatusDone
				return tx.Put(bucketTask, f.token.TaskID, task)
			}); err != nil {
				t.Fatal(err)
			}
			fresh, err := commitTaskCompletionState(f.store, f.dataset, f.token, pub, done, nil)
			if err != nil || fresh {
				t.Fatal("confirmed C was retried as a new commit", err)
			}
			task, _, err := f.read()
			if err != nil || task.Progress.ExtractStatus != ExtractStatusDone {
				t.Fatal("post-C progress was erased", err)
			}
		})
	}
}
