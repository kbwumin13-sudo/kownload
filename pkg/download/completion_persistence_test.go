package download

import (
	"bytes"
	"errors"
	"github.com/GopeedLab/gopeed/pkg/base"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type completionPutGate struct {
	AtomicStorage
	entered chan struct{}
	release chan struct{}
	fired   atomic.Bool
	once    sync.Once
	mode    string
}

func (s *completionPutGate) unblock() { s.once.Do(func() { close(s.release) }) }
func (s *completionPutGate) Put(bucket, key string, value any) error {
	if task, ok := value.(*Task); ok && bucket == bucketTask && task.Status == "done" && s.fired.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
		if s.mode != "" {
			if s.mode == "committed-ack-lost" {
				if err := s.AtomicStorage.Put(bucket, key, value); err != nil {
					return err
				}
			}
			return errors.New("synthetic completion persistence acknowledgement failure")
		}
	}
	return s.AtomicStorage.Put(bucket, key, value)
}
func TestCompletionPutCannotResurrectDeletedTask(t *testing.T) {
	dir := t.TempDir()
	dbDir := filepath.Join(dir, "db")
	store := &completionPutGate{AtomicStorage: NewBoltStorage(dbDir), entered: make(chan struct{}), release: make(chan struct{})}
	url, _ := localOperationSource(t)
	d := operationDownloader(t, store, dir)
	defer func() { store.unblock(); _ = d.Close() }()
	var success atomic.Int32
	d.Listener(func(event *Event) {
		if event.Key == EventKeyDone {
			success.Add(1)
		}
	})
	r := operationRequest(url, dir)
	result := requireOperation(t, d, r)
	id := result.TaskIDs[0]
	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("completion Put was not reached")
	}
	published := d.GetTask(id).getStatus() == "done"
	deleted := make(chan error, 1)
	go func() { deleted <- d.Delete(&TaskFilter{IDs: []string{id}}, false) }()
	deadline := time.Now().Add(3 * time.Second)
	for d.GetTask(id) != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if d.GetTask(id) != nil {
		store.unblock()
		t.Fatal("Delete did not remove live task")
	}
	store.unblock()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Delete blocked after completion Put released")
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.watchedTasks.Load(id); !ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	var durable Task
	present, err := store.Get(bucketTask, id, &durable)
	if err != nil || present {
		t.Errorf("late completion Put resurrected deleted Task: present=%t err=%v", present, err)
	}
	receipt, err := d.QueryBatchOperation(r.OperationID)
	if err != nil || len(receipt.TaskIDs) != 1 || receipt.Members[0].TaskPresent == nil || *receipt.Members[0].TaskPresent {
		t.Errorf("historical association changed: %+v %v", receipt, err)
	}
	if published {
		t.Error("done was visible before storage acknowledgement")
	}
	if success.Load() != 0 {
		t.Error("deleted/uncertain completion emitted success")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := NewBoltStorage(dbDir)
	defer reopened.Close()
	present, err = reopened.Get(bucketTask, id, &durable)
	if err != nil || present {
		t.Errorf("deleted Task survived real Bolt reopen: present=%t err=%v", present, err)
	}
}

func TestCompletionAcknowledgementGatesSuccess(t *testing.T) {
	for _, mode := range []string{"success", "failed-before-commit", "committed-ack-lost"} {
		t.Run(mode, func(t *testing.T) {
			store := &completionPutGate{AtomicStorage: NewMemStorage(), entered: make(chan struct{}), release: make(chan struct{})}
			if mode != "success" {
				store.mode = mode
			}
			url, _ := localOperationSource(t)
			dir := t.TempDir()
			d := operationDownloader(t, store, dir)
			defer func() { store.unblock(); _ = d.Close() }()
			var successes atomic.Int32
			finished := make(chan *Event, 1)
			d.Listener(func(event *Event) {
				if event.Key == EventKeyDone {
					successes.Add(1)
				}
				if event.Key == EventKeyFinally {
					select {
					case finished <- event:
					default:
					}
				}
			})
			r := operationRequest(url, dir)
			result := requireOperation(t, d, r)
			id := result.TaskIDs[0]
			select {
			case <-store.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("no completion storage attempt")
			}
			if d.GetTask(id).getStatus() == base.DownloadStatusDone || successes.Load() != 0 {
				store.unblock()
				t.Fatal("success escaped before storage ACK")
			}
			store.unblock()
			select {
			case event := <-finished:
				if mode == "success" {
					if event.Err != nil || event.Task.getStatus() != base.DownloadStatusDone || successes.Load() != 1 {
						t.Fatal("acknowledged completion did not publish once")
					}
				} else if event.Err == nil || event.Task.getStatus() != base.DownloadStatusError || successes.Load() != 0 {
					t.Fatal("unknown/failed completion published success")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("completion did not settle")
			}
			data, err := os.ReadFile(d.GetTask(id).Meta.SingleFilepath())
			if err != nil || !bytes.Equal(data, []byte("durable-batch-payload")) {
				t.Fatal("completion failure removed/changed complete file", err)
			}
			receipt, err := d.QueryBatchOperation(r.OperationID)
			if err != nil || len(receipt.TaskIDs) != 1 || receipt.TaskIDs[0] != id || receipt.Members[0].TaskPresent == nil || !*receipt.Members[0].TaskPresent {
				t.Fatal("completion changed operation association", err)
			}
		})
	}
}

func TestCompletionListenerCanReenterClose(t *testing.T) {
	url, _ := localOperationSource(t)
	dir := t.TempDir()
	d := operationDownloader(t, NewMemStorage(), dir)
	defer d.Close()
	result := make(chan error, 1)
	d.Listener(func(event *Event) {
		if event.Key == EventKeyDone {
			result <- d.Close()
		}
	})
	requireOperation(t, d, operationRequest(url, dir))
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("completion callback deadlocked in Close")
	}
}
