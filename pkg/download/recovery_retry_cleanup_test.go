package download

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type retryCleanupContext struct {
	context.Context
	committed        atomic.Bool
	entered, release chan struct{}
	once             sync.Once
}

func (c *retryCleanupContext) Err() error {
	if !c.committed.Load() {
		return c.Context.Err()
	}
	c.once.Do(func() { close(c.entered); <-c.release })
	return context.Canceled
}

type retryCleanupAckStore struct {
	AtomicStorage
	ctx   *retryCleanupContext
	armed atomic.Bool
}

func (s *retryCleanupAckStore) UpdateAtomic(fn func(StorageTransaction) error) error {
	err := s.AtomicStorage.UpdateAtomic(fn)
	if err == nil && s.armed.CompareAndSwap(true, false) {
		s.ctx.committed.Store(true)
	}
	return err
}

func TestExplicitRecoveryRetryOldRequestCleanupCannotCancelContinuedRun(t *testing.T) {
	var allow atomic.Bool
	var once sync.Once
	bodyEntered, bodyRelease := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseBody := func() { releaseOnce.Do(func() { close(bodyRelease) }) }
	defer releaseBody()
	payload := bytes.Repeat([]byte("still owned by continued run\n"), 4096)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allow.Load() {
			w.WriteHeader(401)
			return
		}
		// Keep the new run alive past the old request's delayed cleanup.
		once.Do(func() { close(bodyEntered) })
		select {
		case <-bodyRelease:
		case <-r.Context().Done():
			return
		}
		http.ServeContent(w, r, "same.bin", time.Time{}, bytes.NewReader(payload))
	}))
	defer source.Close()
	dir := t.TempDir()
	store := NewBoltStorage(filepath.Join(dir, "db"))
	d := operationDownloader(t, store, dir)
	defer d.Close()
	if err := InitializeDatasetIdentity(store); err != nil {
		t.Fatal(err)
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.CreateDirect(&base.Request{URL: source.URL + "/body"}, &base.Options{Path: dir, Name: "same.bin", Extra: map[string]any{"connections": 1}})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, id, base.DownloadStatusError, 3*time.Second)
	state, err := d.QueryRecoveryForDataset(identity.DatasetID, id)
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(time.Second); !state.RetryAllowed && time.Now().Before(end); {
		time.Sleep(time.Millisecond)
		state, err = d.QueryRecoveryForDataset(identity.DatasetID, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !state.RetryAllowed {
		t.Fatal("original error not durable")
	}
	task := d.getTask(id)
	owner := task.recoveryOwner.Load()
	if owner == nil {
		t.Fatal("missing owner")
	}
	ctx := &retryCleanupContext{Context: context.Background(), entered: make(chan struct{}), release: make(chan struct{})}
	var cleanupOnce sync.Once
	releaseCleanup := func() { cleanupOnce.Do(func() { close(ctx.release) }) }
	defer releaseCleanup()
	fault := &retryCleanupAckStore{AtomicStorage: store, ctx: ctx}
	fault.armed.Store(true)
	owner.mu.Lock()
	owner.store = fault
	owner.mu.Unlock()
	request := RecoveryRetryRequest{1, "user-cleanup-race", state.Episode, state.SourceRevision, "retry"}
	finished := make(chan error, 1)
	go func() { _, err := d.RetryRecoveryForDataset(ctx, identity.DatasetID, id, request); finished <- err }()
	select {
	case <-ctx.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("Retry did not reach post-commit cleanup boundary")
	}
	allow.Store(true)
	if err := d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bodyEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("continued run did not acquire IO")
	}
	owner.mu.Lock()
	token := owner.token
	owner.mu.Unlock()
	current := &transfer.RecoveryLease{Token: token, Owner: owner}
	if current.Token.Episode == state.Episode {
		t.Fatal("new run did not receive new episode")
	}
	if err = current.Check(); err != nil {
		t.Fatal(err)
	}
	releaseCleanup()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("old request did not cancel", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old request cleanup blocked")
	}
	if err = current.Check(); err != nil {
		t.Fatal("old request cleanup revoked new lease", err)
	}
	owner.mu.Lock()
	used := owner.record.TotalUsed
	owner.mu.Unlock()
	if used != 0 {
		t.Fatal("old cleanup changed current budget")
	}
	releaseBody()
	waitStatus(t, d, id, base.DownloadStatusDone, 5*time.Second)
	got, err := os.ReadFile(filepath.Join(dir, "same.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("continued download did not finish original file", err)
	}
	if !errors.Is(owner.CheckRecovery(transfer.RecoveryToken{}), transfer.ErrRecoveryStale) {
		t.Fatal("invalid token accepted")
	}
}

func TestExplicitRecoveryRetryCleanupKeepsLaterEpisodeAuthority(t *testing.T) {
	store := NewMemStorage()
	dataset, task, owner, request := explicitRetryFixture(t, store)
	initTask(task)
	task.runGeneration = 1
	task.recoveryOwner.Store(owner)
	first, fresh, err := owner.commitExplicitRetry(context.Background(), dataset, request, 1)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	oldState := recoveryTaskState(Task{ID: task.ID, Status: task.Status}, first)
	secondRequest := RecoveryRetryRequest{1, "next-explicit-user-action", first.Episode, first.SourceRevision, "retry"}
	second, fresh, err := owner.commitExplicitRetry(context.Background(), dataset, secondRequest, 1)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	invalidateExplicitRetry(task, 1, oldState)
	if authority := owner.retryAuthorization.Load(); authority == nil || authority.Episode != second.Episode {
		t.Fatal("old cleanup revoked later explicit action")
	}
	invalidateExplicitRetry(task, 1, recoveryTaskState(Task{ID: task.ID, Status: task.Status}, second))
	if owner.retryAuthorization.Load() != nil {
		t.Fatal("matching cancelled preparation retained authority")
	}
}
