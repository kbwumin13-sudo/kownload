package download

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestVerifiedHTTPForceDeleteRejectsWholeSelectionAndPreservesForeignOutput(t *testing.T) {
	body := bytes.Repeat([]byte("owned staging body\n"), 100)
	d, task, _, lease := verifiedWrittenFixture(t, body)
	beforeTask, record, err := d.readVerifiedTask(scopedDatasetA, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(beforeTask.Meta.Opts.Path, record.FinalName)
	if err = os.WriteFile(foreign, []byte("foreign output must survive force"), 0600); err != nil {
		t.Fatal(err)
	}
	foreignInfo, err := os.Stat(foreign)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := d.completeVerifiedHTTP(task, quiescedVerifiedWriter{}, lease.Token.Generation, lease)
	if err == nil || fresh {
		t.Fatal("actual postjoin publication did not conflict", fresh, err)
	}
	d.doOnError(task, err, lease.Token.Generation)
	if task.getStatus() != base.DownloadStatusError {
		t.Fatal("conflict not accepted as error")
	}
	// Add an ordinary queued Task ahead of the conflicting verified Task. A
	// mixed selection must not delete this first member before it rejects SHA.
	legacy := NewTask()
	legacy.Progress = &Progress{}
	initTask(legacy)
	legacy.Protocol = "http"
	legacy.Status = base.DownloadStatusWait
	legacy.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "https://fixture.invalid/queued"}, Opts: &base.Options{Path: t.TempDir(), Name: "queued.bin"}}
	if err = d.storage.Put(bucketTask, legacy.ID, legacy.clone()); err != nil {
		t.Fatal(err)
	}
	d.lock.Lock()
	d.cfg.MaxRunning = 0 // Hold this fixture-only queue; no synthetic Task has a network worker.
	d.tasks = append([]*Task{legacy}, d.tasks...)
	d.enqueueLocked(legacy)
	d.lock.Unlock()
	taskIDs := []string{legacy.ID, task.ID}
	d.lock.Lock()
	beforeQueue := append([]*Task(nil), d.waitTasks...)
	d.lock.Unlock()
	dbPath := d.storage.(*BoltStorage).db.Path()
	dbBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, record, err = d.readVerifiedTask(scopedDatasetA, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(task.Meta.Opts.Path, record.StagingName)
	stageBefore, err := os.ReadFile(stage)
	if err != nil {
		t.Fatal(err)
	}
	err = d.Delete(&TaskFilter{IDs: taskIDs}, true)
	if !errors.Is(err, ErrVerifiedHTTPUnsupportedOption) {
		t.Errorf("force did not reject completion before changes: %v", err)
	}
	after, readErr := os.ReadFile(foreign)
	if readErr != nil || string(after) != "foreign output must survive force" {
		t.Errorf("force removed unowned foreign output: %v", readErr)
	}
	afterInfo, statErr := os.Stat(foreign)
	if statErr != nil || !os.SameFile(foreignInfo, afterInfo) {
		t.Error("foreign inode changed")
	}
	stageAfter, stageErr := os.ReadFile(stage)
	if stageErr != nil || !bytes.Equal(stageBefore, stageAfter) {
		t.Error("force changed staging", stageErr)
	}
	if d.getTask(legacy.ID) != legacy || d.getTask(task.ID) != task {
		t.Error("force changed mixed Task membership")
	}
	d.lock.Lock()
	sameQueue := reflect.DeepEqual(beforeQueue, d.waitTasks)
	d.lock.Unlock()
	if !sameQueue {
		t.Error("force changed queued membership")
	}
	dbAfter, dbErr := os.ReadFile(dbPath)
	if dbErr != nil || sha256.Sum256(dbBefore) != sha256.Sum256(dbAfter) {
		t.Error("force preflight changed persistent DB", dbErr)
	}
	if t.Failed() {
		return
	}
	// Plain Task deletion still keeps both owned and foreign files and history.
	if err = d.Delete(&TaskFilter{IDs: []string{task.ID}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(stage); err != nil {
		t.Fatal("Task-only delete removed stage", err)
	}
	if _, err = os.Stat(foreign); err != nil {
		t.Fatal("Task-only delete removed foreign output", err)
	}
	d.lock.Lock()
	legacy.updateStatus(base.DownloadStatusPause)
	d.lock.Unlock()
}

func TestVerifiedHTTPForceDeleteAllAbsentLegacyRetainsFileRemoval(t *testing.T) {
	d, store, root := scopedOperationFixture(t, true)
	task := NewTask()
	task.Progress = &Progress{}
	initTask(task)
	task.Status = base.DownloadStatusDone
	task.Protocol = "http"
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "https://fixture.invalid/ordinary"}, Opts: &base.Options{Path: root}, Res: &base.Resource{Files: []*base.FileInfo{{Name: "ordinary.bin"}}}}
	path := task.Meta.SingleFilepath()
	if err := os.WriteFile(path, []byte("ordinary owned output"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(bucketTask, task.ID, task.clone()); err != nil {
		t.Fatal(err)
	}
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	if err := d.Delete(&TaskFilter{IDs: []string{task.ID}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("legacy force file removal changed", err)
	}
	if d.getTask(task.ID) != nil {
		t.Fatal("legacy force kept Task")
	}
}

func TestVerifiedHTTPForceDeleteRejectsEveryCompletionPresence(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		for mask := 1; mask < 8; mask++ {
			t.Run(fmt.Sprintf("bolt-%v/presence-%d", bolt, mask), func(t *testing.T) {
				d, store, root := scopedOperationFixture(t, bolt)
				task := NewTask()
				task.Protocol = "http"
				task.Status = base.DownloadStatusDone
				task.Progress = &Progress{}
				initTask(task)
				task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "https://fixture.invalid/presence"}, Opts: &base.Options{Path: root}, Res: &base.Resource{Files: []*base.FileInfo{{Name: "keep.bin"}}}}
				if mask&1 != 0 {
					task.Completion = &TaskCompletionRef{Version: 99, CompletionID: "unknown-version"}
				}
				if err := store.Put(bucketTask, task.ID, task.clone()); err != nil {
					t.Fatal(err)
				}
				if mask&2 != 0 {
					if err := store.Put(bucketTaskCompletion, task.ID, map[string]any{"version": 99}); err != nil {
						t.Fatal(err)
					}
				}
				if mask&4 != 0 {
					if err := store.Put(bucketCompletionRequired, task.ID, map[string]any{"version": 99, "capability": "unknown"}); err != nil {
						t.Fatal(err)
					}
				}
				path := task.Meta.SingleFilepath()
				if err := os.WriteFile(path, []byte("unproven output"), 0600); err != nil {
					t.Fatal(err)
				}
				d.lock.Lock()
				d.tasks = append(d.tasks, task)
				d.lock.Unlock()
				if err := d.Delete(&TaskFilter{IDs: []string{task.ID}}, true); !errors.Is(err, ErrVerifiedHTTPUnsupportedOption) {
					t.Fatal("presence accepted force", err)
				}
				if d.getTask(task.ID) != task {
					t.Fatal("presence changed Task membership")
				}
				b, err := os.ReadFile(path)
				if err != nil || string(b) != "unproven output" {
					t.Fatal("presence removed output", err)
				}
			})
		}
	}
}
