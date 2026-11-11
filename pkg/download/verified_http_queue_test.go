package download

import (
	"bytes"
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

// The third public Check is inside VerifyHTTPCompletion, after acquiring the
// hash slot and outside every lifecycle Fence. Delegation retains the real
// owner token; this barrier never grants IO or substitutes a successful check.
type gatedVerifiedCheck struct {
	transfer.RecoveryController
	checks  atomic.Int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedVerifiedCheck) CheckRecovery(token transfer.RecoveryToken) error {
	if g.checks.Add(1) == 3 {
		close(g.entered)
		<-g.release
	}
	return g.RecoveryController.CheckRecovery(token)
}
func (g *gatedVerifiedCheck) unblock() { g.once.Do(func() { close(g.release) }) }
func gateVerifiedLease(lease *transfer.RecoveryLease) (*transfer.RecoveryLease, *gatedVerifiedCheck) {
	g := &gatedVerifiedCheck{RecoveryController: lease.Owner, entered: make(chan struct{}), release: make(chan struct{})}
	copy := *lease
	copy.Owner = g
	return &copy, g
}
func waitVerifiedGate(t *testing.T, g *gatedVerifiedCheck) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("did not reach real hash-slot Check outside Fence")
	}
}

func TestVerifiedHTTPHashDoesNotOccupyNetworkQueue(t *testing.T) {
	body := bytes.Repeat([]byte("sealed hash slot\n"), 100)
	d, task, _, lease := verifiedWrittenFixture(t, body)
	d.cfg.MaxRunning = 1
	var requests atomic.Int32
	ordinary := []byte("ordinary download ran while hashing")
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.ServeContent(w, r, "ordinary.bin", time.Time{}, bytes.NewReader(ordinary))
	}))
	defer source.Close()
	id, err := d.CreateDirect(&base.Request{URL: source.URL}, &base.Options{Path: t.TempDir(), Name: "ordinary.bin"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, id, base.DownloadStatusWait, time.Second)
	gated, gate := gateVerifiedLease(lease)
	defer gate.unblock()
	finished := make(chan error, 1)
	go func() {
		fresh, err := d.completeVerifiedHTTP(task, quiescedVerifiedWriter{}, lease.Token.Generation, gated)
		if err == nil && !fresh {
			err = transfer.ErrRecoveryHeld
		}
		finished <- err
	}()
	waitVerifiedGate(t, gate)
	waitStatus(t, d, id, base.DownloadStatusDone, 3*time.Second)
	if requests.Load() == 0 {
		t.Fatal("ordinary task did not perform IO")
	}
	if task.getStatus() != base.DownloadStatusRunning || !task.verificationOnly.Load() {
		t.Fatal("hashing changed task into fake done")
	}
	final := d.getTask(id).Meta.SingleFilepath()
	got, err := os.ReadFile(final)
	if err != nil || !bytes.Equal(got, ordinary) {
		t.Fatal("ordinary output changed", err)
	}
	gate.unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hash did not finish")
	}
}

func TestVerifiedHTTPLateHashCannotPublishAfterPauseOrDelete(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause-reverify", true: "delete"}[remove], func(t *testing.T) {
			d, task, _, lease := verifiedWrittenFixture(t, bytes.Repeat([]byte("late hash\n"), 100))
			beforeTask, beforeBudget, err := readRecovery(d.storage, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			gated, gate := gateVerifiedLease(lease)
			defer gate.unblock()
			finished := make(chan error, 1)
			go func() {
				fresh, err := d.completeVerifiedHTTP(task, quiescedVerifiedWriter{}, lease.Token.Generation, gated)
				if fresh {
					err = transfer.ErrRecoveryHeld
				}
				finished <- err
			}()
			waitVerifiedGate(t, gate)
			_, record, err := d.readVerifiedTask(scopedDatasetA, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if remove {
				err = d.Delete(&TaskFilter{IDs: []string{task.ID}}, false)
			} else {
				err = d.Pause(&TaskFilter{IDs: []string{task.ID}})
			}
			if err != nil {
				t.Fatal(err)
			}
			gate.unblock()
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("late hash accepted")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("late hash did not stop")
			}
			if _, err := os.Stat(filepath.Join(beforeTask.Meta.Opts.Path, record.FinalName)); !os.IsNotExist(err) {
				t.Fatal("late result published final", err)
			}
			if task.verifiedSuccess.Load() != nil {
				t.Fatal("late result granted fresh effects")
			}
			if remove {
				var saved *Task
				found, err := d.storage.Get(bucketTask, task.ID, &saved)
				if err != nil || found {
					t.Fatal("late hash resurrected deleted task", err)
				}
				return
			}
			task.statusLock.Lock()
			pauseDone := task.pauseDone
			task.statusLock.Unlock()
			select {
			case <-pauseDone:
			case <-time.After(time.Second):
				t.Fatal("pause persistence did not finish")
			}
			d.cfg.MaxRunning = 0 // No network admission is available to this sealed continuation.
			if err = d.Continue(&TaskFilter{IDs: []string{task.ID}}); err != nil {
				t.Fatal(err)
			}
			waitStatus(t, d, task.ID, base.DownloadStatusDone, 3*time.Second)
			_, afterBudget, err := readRecovery(d.storage, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if afterBudget.Episode != beforeBudget.Episode || afterBudget.TransportUsed != beforeBudget.TransportUsed || afterBudget.TotalUsed != beforeBudget.TotalUsed {
				t.Fatal("sealed reverify reset budget")
			}
			if task.fetcher != nil {
				t.Fatal("sealed continuation restored network fetcher")
			}
			if lease.Check() == nil {
				t.Fatal("old lease reactivated")
			}
		})
	}
}
