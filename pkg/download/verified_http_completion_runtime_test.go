package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// The writer is explicitly quiesced in this component test. Real HTTP writer
// join and body integrity are exercised by TestVerifiedHTTPRealCoreMatchAndMismatch.
type quiescedVerifiedWriter struct{ fetcher.Fetcher }

func (quiescedVerifiedWriter) JoinVerifiedWriters(ctx context.Context) error { return ctx.Err() }

func verifiedWrittenFixture(t *testing.T, body []byte) (*Downloader, *Task, *taskRecoveryOwner, *transfer.RecoveryLease) {
	t.Helper()
	sum := sha256.Sum256(body)
	d, task, owner, _ := verifiedFenceFixtureStore(t, true, hex.EncodeToString(sum[:]))
	task.timer.Start()
	task.lock.Lock()
	task.dataLock.Lock()
	task.Meta.Res = &base.Resource{Size: int64(len(body)), Files: []*base.FileInfo{{Name: "one.bin", Size: int64(len(body))}}}
	task.IsCreated = true
	task.dataLock.Unlock()
	err := owner.persistSnapshot(task.clone(), nil, false)
	task.lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	lease, err := owner.currentLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	file, err := d.prepareVerifiedOutput(context.Background(), scopedDatasetA, task, lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(body); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Sync(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	return d, task, owner, lease
}

type unconfirmedCompletionStore struct {
	AtomicStorage
	id    string
	armed atomic.Bool
}

func (s *unconfirmedCompletionStore) UpdateAtomic(fn func(StorageTransaction) error) error {
	committed := false
	err := s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error {
		if err := fn(tx); err != nil {
			return err
		}
		var record transfer.VerifiedCompletionRecord
		present, err := tx.Get(bucketTaskCompletion, s.id, &record)
		if err != nil {
			return err
		}
		committed = present && record.Phase == transfer.CompletionCommitted
		return nil
	})
	if err == nil && committed && s.armed.CompareAndSwap(true, false) {
		return errors.New("synthetic lost storage C acknowledgement")
	}
	return err
}

func TestVerifiedHTTPUnconfirmedCRebuildsDoneWithoutFresh(t *testing.T) {
	body := bytes.Repeat([]byte("durable C is not a fresh event\n"), 1024)
	d, task, _, lease := verifiedWrittenFixture(t, body)
	events, release, err := d.SubscribeVerifiedHTTPDone(context.Background(), scopedDatasetA)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	store := &unconfirmedCompletionStore{AtomicStorage: d.storage.(AtomicStorage), id: task.ID}
	store.armed.Store(true)
	fresh, err := d.completeVerifiedHTTPStored(task, quiescedVerifiedWriter{}, lease.Token.Generation, lease, store)
	if err != nil || fresh || task.getStatus() != base.DownloadStatusDone {
		t.Fatal("unknown C ACK recreated fresh or lost authoritative done", fresh, err)
	}
	owned, record, err := d.readVerifiedTask(scopedDatasetA, task.ID)
	if err != nil || record.Phase != transfer.CompletionCommitted || owned.Status != base.DownloadStatusDone {
		t.Fatal("C not committed", err)
	}
	actual, err := os.ReadFile(filepath.Join(task.Meta.Opts.Path, record.FinalName))
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatal("C output changed", err)
	}
	select {
	case event := <-events:
		t.Fatal("unknown ACK emitted fresh", event)
	default:
	}
	views, err := d.TaskCompletionViewsForDataset(scopedDatasetA, []string{task.ID})
	if err != nil || !views[task.ID].Available || views[task.ID].Phase != string(transfer.CompletionCommitted) {
		t.Fatal("API-confirmed C projection missing", err, views)
	}
}
