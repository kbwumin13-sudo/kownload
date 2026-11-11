package download

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func verifiedFenceFixture(t *testing.T, expected ...string) (*Downloader, *Task, *taskRecoveryOwner, *transfer.RecoveryLease) {
	t.Helper()
	return verifiedFenceFixtureStore(t, false, expected...)
}

func verifiedFenceFixtureStore(t *testing.T, bolt bool, expected ...string) (*Downloader, *Task, *taskRecoveryOwner, *transfer.RecoveryLease) {
	t.Helper()
	d, store, dir := scopedOperationFixture(t, bolt)
	request := operationRequest("https://fixture.invalid/verified", dir)
	request.integrity = &VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", strings.Repeat("a", 64)}
	if len(expected) > 0 {
		request.integrity.ExpectedSHA256 = expected[0]
	}
	items, hash, err := canonicalBatch(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.reserveOperationInDataset(store, request, items, hash, scopedDatasetA); err != nil {
		t.Fatal(err)
	}
	created, err := d.createOperationMemberInDataset(store, request.OperationID, hash, 0, items[0], scopedDatasetA)
	if err != nil || !created {
		t.Fatal(err)
	}
	r, err := d.loadOperationInDataset(scopedDatasetA, request.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := d.readVerifiedTask(scopedDatasetA, r.Members[0].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	initTask(task)
	task.recoveryInitialEligible.Store(true)
	owner, err := newTaskRecoveryOwner(store, task)
	if err != nil {
		t.Fatal(err)
	}
	task.Recovery = &TaskRecoveryRef{Version: 1}
	task.recoveryOwner.Store(owner)
	task.Status = base.DownloadStatusRunning
	task.runGeneration = 1
	lease, err := owner.begin(1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.persistSnapshot(task.clone(), nil, false); err != nil {
		t.Fatal(err)
	}
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	return d, task, owner, lease
}

func TestVerifiedFenceSerializesDeleteAndRejectsLateAdmission(t *testing.T) {
	d, task, owner, lease := verifiedFenceFixture(t)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	go func() {
		finished <- d.verifiedFence(context.Background(), scopedDatasetA, task, lease.Token, func(*Task, transfer.VerifiedCompletionRecord) error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("publication fence did not enter")
	}
	attempted, deleted := make(chan struct{}), make(chan error, 1)
	go func() { close(attempted); deleted <- d.Delete(&TaskFilter{IDs: []string{task.ID}}, false) }()
	<-attempted
	select {
	case err := <-deleted:
		t.Fatal("Delete crossed live publication admission", err)
	default:
	}
	if !owner.active.Load() {
		t.Fatal("Delete invalidated inside publication admission")
	}
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("publication did not return")
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Delete did not finish")
	}
	called := false
	if err := d.verifiedFence(context.Background(), scopedDatasetA, task, lease.Token, func(*Task, transfer.VerifiedCompletionRecord) error { called = true; return nil }); err == nil || called {
		t.Fatal("late deleted task entered publication")
	}
}

func TestVerifiedFenceRejectsSourceChangeAndClose(t *testing.T) {
	d, task, owner, lease := verifiedFenceFixture(t)
	task.lock.Lock()
	snapshot := task.clone()
	snapshot.Meta.Req.URL = "https://fixture.invalid/changed"
	err := owner.persistSnapshot(snapshot, nil, false)
	task.dataLock.Lock()
	task.Meta.Req = snapshot.Meta.Req
	task.dataLock.Unlock()
	task.lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err = d.verifiedFence(context.Background(), scopedDatasetA, task, lease.Token, func(*Task, transfer.VerifiedCompletionRecord) error { called = true; return nil }); err == nil || called {
		t.Fatal("obsolete source entered publication", err)
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	if err = d.verifiedFence(context.Background(), scopedDatasetA, task, lease.Token, func(*Task, transfer.VerifiedCompletionRecord) error { called = true; return nil }); err == nil || called {
		t.Fatal("closed instance entered publication", err)
	}
}
