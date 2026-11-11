package download

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

const scopedDatasetA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const scopedDatasetB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func scopedOperationFixture(t *testing.T, bolt bool) (*Downloader, AtomicStorage, string) {
	t.Helper()
	dir := t.TempDir()
	var store AtomicStorage = NewMemStorage()
	if bolt {
		store = NewBoltStorage(filepath.Join(dir, "db"))
	}
	d := operationDownloader(t, store, dir)
	t.Cleanup(func() { _ = d.Close() })
	if err := InitializeDatasetIdentity(store); err != nil {
		t.Fatal(err)
	}
	setScopedDataset(t, store, scopedDatasetA)
	return d, store, dir
}

func setScopedDataset(t *testing.T, store Storage, id string) {
	t.Helper()
	if err := store.Put(bucketConfig, datasetIdentityKey, datasetRecord{Version: 1, DatasetID: id}); err != nil {
		t.Fatal(err)
	}
}
func requireNoScopedReceipt(t *testing.T, result BatchOperationResult) {
	t.Helper()
	if result.State != "" || len(result.TaskIDs) != 0 || len(result.Members) != 0 {
		t.Fatalf("unverified domain exposed a receipt: %+v", result)
	}
}
func scopedReservation(t *testing.T, d *Downloader, store AtomicStorage, r BatchOperationRequest) ([]*base.CreateTaskBatchItem, string) {
	t.Helper()
	items, hash, err := canonicalBatch(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.reserveOperationInDataset(store, r, items, hash, scopedDatasetA); err != nil {
		t.Fatal(err)
	}
	return items, hash
}

type scopedBeforeAtomic struct {
	AtomicStorage
	before func()
}

func (s scopedBeforeAtomic) UpdateAtomic(fn func(StorageTransaction) error) error {
	s.before()
	return s.AtomicStorage.UpdateAtomic(fn)
}

func TestScopedBatchRejectsWrongDomainAndPreservesLegacy(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt], func(t *testing.T) {
			d, store, dir := scopedOperationFixture(t, bolt)
			url, hits := localOperationSource(t)
			r := operationRequest(url, dir)
			result, err := d.CreateBatchOperationForDataset(context.Background(), scopedDatasetB, r)
			if !errors.Is(err, ErrDatasetMismatch) {
				t.Fatal(err)
			}
			requireNoScopedReceipt(t, result)
			unknown, err := d.QueryBatchOperation(r.OperationID)
			if err != nil || unknown.State != "unknown" || hits.Load() != 0 {
				t.Fatalf("wrong domain wrote or executed: %+v %v hits=%d", unknown, err, hits.Load())
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			pending, err := d.CreateBatchOperationForDataset(ctx, scopedDatasetA, r)
			if err != nil || pending.PendingCount != 1 {
				t.Fatalf("matching reservation: %+v %v", pending, err)
			}
			setScopedDataset(t, store, scopedDatasetB)
			// Existing R must not bypass the domain check on the fast path.
			result, err = d.CreateBatchOperationForDataset(ctx, scopedDatasetA, r)
			if !errors.Is(err, ErrDatasetMismatch) {
				t.Fatal(err)
			}
			requireNoScopedReceipt(t, result)
			result, err = d.CancelBatchOperationForDataset(scopedDatasetA, r.OperationID)
			if !errors.Is(err, ErrDatasetMismatch) {
				t.Fatal(err)
			}
			requireNoScopedReceipt(t, result)
			result, err = d.QueryBatchOperationForDataset(scopedDatasetA, r.OperationID)
			if !errors.Is(err, ErrDatasetMismatch) {
				t.Fatal(err)
			}
			requireNoScopedReceipt(t, result)
			legacy, err := d.QueryBatchOperation(r.OperationID)
			if err != nil || !reflect.DeepEqual(legacy, pending) {
				t.Fatalf("wrong-domain action changed pending record: %+v %v", legacy, err)
			}
			// Old namespace and API remain unchanged; cancel only ends pending.
			ended, err := d.CancelBatchOperation(r.OperationID)
			if err != nil || ended.FailedCount != 1 || hits.Load() != 0 {
				t.Fatalf("legacy cancel changed: %+v %v", ended, err)
			}
		})
	}
}

func TestScopedBatchGuardsEachAtomicBoundary(t *testing.T) {
	for _, phase := range []string{"reserve", "member", "cancel"} {
		t.Run(phase, func(t *testing.T) {
			d, store, dir := scopedOperationFixture(t, true)
			url, hits := localOperationSource(t)
			r := operationRequest(url, dir)
			items, hash, err := canonicalBatch(r)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "reserve" {
				// Change identity after R's same-view precheck, before its write tx.
				gate := scopedBeforeAtomic{store, func() { setScopedDataset(t, store, scopedDatasetB) }}
				err = d.reserveOperationInDataset(gate, r, items, hash, scopedDatasetA)
			} else {
				scopedReservation(t, d, store, r)
				setScopedDataset(t, store, scopedDatasetB)
				if phase == "member" {
					var created bool
					created, err = d.createOperationMemberInDataset(store, r.OperationID, hash, 0, items[0], scopedDatasetA)
					if created {
						t.Fatal("mismatched M reported a new member")
					}
				} else {
					var result BatchOperationResult
					result, err = d.CancelBatchOperationForDataset(scopedDatasetA, r.OperationID)
					requireNoScopedReceipt(t, result)
				}
			}
			if !errors.Is(err, ErrDatasetMismatch) {
				t.Fatal(err)
			}
			result, err := d.QueryBatchOperation(r.OperationID)
			if err != nil || len(result.TaskIDs) != 0 || len(d.GetTasks()) != 0 || hits.Load() != 0 {
				t.Fatalf("mismatch wrote M/Q: %+v %v hits=%d", result, err, hits.Load())
			}
			if (phase == "reserve" && result.State != "unknown") || (phase != "reserve" && result.PendingCount != 1) {
				t.Fatalf("mismatch removed/changed reservation: %+v", result)
			}
			readback, err := d.readAfterOperationErrorInDataset(scopedDatasetA, r.OperationID, ErrOperationUnavailable)
			if !errors.Is(err, ErrDatasetMismatch) {
				t.Fatal(err)
			}
			requireNoScopedReceipt(t, readback)
		})
	}
}

func TestScopedBatchReplayConflictAndDeletedHistory(t *testing.T) {
	d, _, dir := scopedOperationFixture(t, true)
	url, _ := localOperationSource(t)
	r := operationRequest(url, dir)
	created, err := d.CreateBatchOperationForDataset(context.Background(), scopedDatasetA, r)
	if err != nil || len(created.TaskIDs) != 1 {
		t.Fatalf("create: %+v %v", created, err)
	}
	id := created.TaskIDs[0]
	waitStartupDone(t, d, id)
	r.Reqs[0].Opts.Name = "different.bin"
	conflict, err := d.CreateBatchOperationForDataset(context.Background(), scopedDatasetA, r)
	if !errors.Is(err, ErrOperationConflict) || len(conflict.TaskIDs) != 1 || conflict.TaskIDs[0] != id {
		t.Fatalf("different request was not rejected with original receipt: %+v %v", conflict, err)
	}
	r.Reqs[0].Opts.Name = "one.bin"
	if err = d.Delete(&TaskFilter{IDs: []string{id}}, false); err != nil {
		t.Fatal(err)
	}
	result, err := d.CreateBatchOperationForDataset(context.Background(), scopedDatasetA, r)
	if err != nil || len(result.Members) != 1 || result.Members[0].TaskPresent == nil || *result.Members[0].TaskPresent || result.TaskIDs[0] != id || d.GetTask(id) != nil {
		t.Fatalf("deleted historical task was recreated: %+v %v", result, err)
	}
	// Close/reopen keeps logical identity and historical member IDs.
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := operationDownloader(t, NewBoltStorage(filepath.Join(dir, "db")), dir)
	defer reopened.Close()
	result, err = reopened.QueryBatchOperationForDataset(scopedDatasetA, r.OperationID)
	if err != nil || result.Members[0].TaskPresent == nil || *result.Members[0].TaskPresent || result.TaskIDs[0] != id {
		t.Fatalf("history changed after reopen: %+v %v", result, err)
	}
}

func TestScopedBatchPostFirstExecutionAndUnknownAck(t *testing.T) {
	for _, ackLost := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-M", true: "unknown-M-ACK"}[ackLost], func(t *testing.T) {
			d, store, dir := scopedOperationFixture(t, true)
			var posts atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("redirect target did not receive GET")
				}
				_, _ = w.Write([]byte("scoped first POST payload"))
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				if r.Method != "POST" {
					t.Error("initial source method changed")
				}
				http.Redirect(w, r, target.URL, http.StatusSeeOther)
			}))
			defer source.Close()
			r := operationRequest(source.URL, dir)
			r.Reqs[0].Req.Extra = &httpModel.ReqExtra{Method: "POST", Body: "synthetic-form"}
			var result BatchOperationResult
			var err error
			if ackLost {
				items, hash := scopedReservation(t, d, store, r)
				fault := &operationFaultStorage{AtomicStorage: store, phase: "ack-member"}
				createdNow, e := d.createOperationMemberInDataset(fault, r.OperationID, hash, 0, items[0], scopedDatasetA)
				if !errors.Is(e, ErrOperationUnavailable) || createdNow {
					t.Fatalf("unknown M granted first execution: new=%v err=%v", createdNow, e)
				}
				// Exercise the same post-M publication routine with the uncertain ACK.
				cause := e
				if e = d.publishOperationMemberInDataset(scopedDatasetA, r.OperationID, hash, 0, createdNow); e != nil {
					t.Fatal(e)
				}
				result, err = d.readAfterOperationErrorInDataset(scopedDatasetA, r.OperationID, cause)
			} else {
				result, err = d.CreateBatchOperationForDataset(context.Background(), scopedDatasetA, r)
			}
			if (!ackLost && err != nil) || (ackLost && !errors.Is(err, ErrOperationUnavailable)) || len(result.TaskIDs) != 1 {
				t.Fatalf("same-domain receipt/ACK classification missing: %+v %v", result, err)
			}
			id := result.TaskIDs[0]
			want := base.DownloadStatusDone
			if ackLost {
				want = base.DownloadStatusError
			}
			waitStatus(t, d, id, want, 3*time.Second)
			if !ackLost {
				body, e := os.ReadFile(d.GetTask(id).Meta.SingleFilepath())
				if e != nil || string(body) != "scoped first POST payload" {
					t.Fatalf("download body differs: %q %v", body, e)
				}
			}
			if _, err = d.CreateBatchOperationForDataset(context.Background(), scopedDatasetA, r); err != nil {
				t.Fatal(err)
			}
			wantPosts := int32(1)
			if ackLost {
				wantPosts = 0
			}
			if posts.Load() != wantPosts || len(d.GetTasks()) != 1 {
				t.Fatalf("same-op replay/ACK changed sends/tasks: posts=%d tasks=%d", posts.Load(), len(d.GetTasks()))
			}
		})
	}
}

type scopedFailReader struct {
	datasetReader
	bucket string
}

func (r scopedFailReader) Get(bucket, key string, out any) (bool, error) {
	if bucket == r.bucket {
		return false, errors.New("synthetic view read failure")
	}
	return r.datasetReader.Get(bucket, key, out)
}

func TestScopedBatchPresenceFailureAndAssociationFailClosed(t *testing.T) {
	d, store, dir := scopedOperationFixture(t, false)
	url, hits := localOperationSource(t)
	r := operationRequest(url, dir)
	items, hash := scopedReservation(t, d, store, r)
	if _, err := d.createOperationMemberInDataset(store, r.OperationID, hash, 0, items[0], scopedDatasetA); err != nil {
		t.Fatal(err)
	}
	var partial scopedOperationSnapshot
	err := withDatasetReadView(store, scopedDatasetA, func(reader datasetReader) error {
		var err error
		partial, err = readOperationSnapshotFrom(scopedFailReader{reader, bucketTask}, r.OperationID)
		return err
	})
	if !errors.Is(err, ErrOperationUnavailable) || partial.result.State != "settled" || len(partial.result.TaskIDs) != 1 || partial.result.Members[0].TaskPresent != nil {
		t.Fatalf("presence failure invented absence/lost receipt: %+v %v", partial.result, err)
	}
	id := partial.result.TaskIDs[0]
	if err = store.Put(bucketOperationTasks, id, &operationTaskLink{TaskID: id, Ref: CreationOperation{1, batchOperationNamespace, r.OperationID, 99}}); err != nil {
		t.Fatal(err)
	}
	result, err := d.CreateBatchOperationForDataset(context.Background(), scopedDatasetA, r)
	if !errors.Is(err, ErrOperationConflict) || result.Members[0].Error != "association_conflict" || len(d.GetTasks()) != 0 || hits.Load() != 0 {
		t.Fatalf("bad association reached Q: %+v %v", result, err)
	}
}

func TestScopedBatchUnknownDomainAndStorageHaveNoReceipt(t *testing.T) {
	for _, mode := range []string{"missing", "invalid", "unknown-storage"} {
		t.Run(mode, func(t *testing.T) {
			d, store, dir := scopedOperationFixture(t, false)
			r := operationRequest("http://127.0.0.1:1/no-send", dir)
			scopedReservation(t, d, store, r)
			switch mode {
			case "missing":
				if err := store.Delete(bucketConfig, datasetIdentityKey); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := store.Put(bucketConfig, datasetIdentityKey, datasetRecord{Version: 99, DatasetID: scopedDatasetA}); err != nil {
					t.Fatal(err)
				}
			case "unknown-storage":
				d.storage = &operationFaultStorage{AtomicStorage: store}
			}
			result, err := d.QueryBatchOperationForDataset(scopedDatasetA, r.OperationID)
			if err == nil {
				t.Fatal("unknown domain/storage accepted")
			}
			requireNoScopedReceipt(t, result)
			result, err = d.readAfterOperationErrorInDataset(scopedDatasetA, r.OperationID, ErrOperationUnavailable)
			if err == nil {
				t.Fatal("failure readback fell back to unscoped receipt")
			}
			requireNoScopedReceipt(t, result)
		})
	}
}

type scopedGateReader struct {
	datasetReader
	afterOperation func()
	fired          bool
}

func (r *scopedGateReader) Get(bucket, key string, out any) (bool, error) {
	ok, err := r.datasetReader.Get(bucket, key, out)
	if err == nil && bucket == bucketTaskOperations && !r.fired {
		r.fired = true
		r.afterOperation()
	}
	return ok, err
}

func TestScopedBatchReceiptUsesOneReadView(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt], func(t *testing.T) {
			d, store, dir := scopedOperationFixture(t, bolt)
			r := operationRequest("http://127.0.0.1:1/no-send", dir)
			items, hash := scopedReservation(t, d, store, r)
			if _, err := d.createOperationMemberInDataset(store, r.OperationID, hash, 0, items[0], scopedDatasetA); err != nil {
				t.Fatal(err)
			}
			record, err := d.loadOperation(r.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			id := record.Members[0].TaskID
			if bolt {
				// Leave mapped free pages before pinning the read view. Otherwise
				// this tiny fixture may make bbolt's writer wait for mmap growth,
				// which is not the snapshot interleaving under test.
				if err := store.Put(bucketConfig, "scoped-view-padding", strings.Repeat("p", 1<<20)); err != nil {
					t.Fatal(err)
				}
				if err := store.Delete(bucketConfig, "scoped-view-padding"); err != nil {
					t.Fatal(err)
				}
			}
			started, changed := make(chan struct{}), make(chan error, 1)
			var snapshot scopedOperationSnapshot
			err = withDatasetReadView(store, scopedDatasetA, func(reader datasetReader) error {
				gate := &scopedGateReader{datasetReader: reader, afterOperation: func() {
					go func() {
						close(started)
						changed <- store.UpdateAtomic(func(tx StorageTransaction) error {
							if err := tx.Put(bucketConfig, datasetIdentityKey, datasetRecord{1, scopedDatasetB}); err != nil {
								return err
							}
							var task Task
							if _, err := tx.Get(bucketTask, id, &task); err != nil {
								return err
							}
							task.CreationOperation = nil
							return tx.Put(bucketTask, id, &task)
						})
					}()
					<-started
					if bolt {
						select {
						case e := <-changed:
							if e != nil {
								t.Error(e)
							}
							changed <- e
						case <-time.After(3 * time.Second):
							t.Error("writer could not commit alongside Bolt read view")
						}
					} else {
						select {
						case <-changed:
							t.Error("Mem writer crossed active RLock")
						default:
						}
					}
				}}
				var e error
				snapshot, e = readOperationSnapshotFrom(gate, r.OperationID)
				return e
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case e := <-changed:
				if e != nil {
					t.Fatal(e)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("writer did not finish after view")
			}
			if snapshot.result.State != "settled" || snapshot.result.Members[0].Error != "" || snapshot.result.Members[0].TaskPresent == nil || !*snapshot.result.Members[0].TaskPresent || snapshot.tasks[id].CreationOperation == nil {
				t.Fatalf("mixed dataset/task generations: %+v", snapshot.result)
			}
			result, err := d.QueryBatchOperationForDataset(scopedDatasetA, r.OperationID)
			if !errors.Is(err, ErrDatasetMismatch) {
				t.Fatal(err)
			}
			requireNoScopedReceipt(t, result)
		})
	}
}

func TestScopedBatchDomainChangeBeforeQAndQueryNeverAttach(t *testing.T) {
	d, store, dir := scopedOperationFixture(t, true)
	url, hits := localOperationSource(t)
	r := operationRequest(url, dir)
	items, hash := scopedReservation(t, d, store, r)
	createdNow, err := d.createOperationMemberInDataset(store, r.OperationID, hash, 0, items[0], scopedDatasetA)
	if err != nil || !createdNow {
		t.Fatalf("M not committed: %v %v", createdNow, err)
	}
	setScopedDataset(t, store, scopedDatasetB)
	if err = d.publishOperationMemberInDataset(scopedDatasetA, r.OperationID, hash, 0, createdNow); !errors.Is(err, ErrDatasetMismatch) {
		t.Fatal(err)
	}
	if len(d.GetTasks()) != 0 || hits.Load() != 0 {
		t.Fatal("mismatched Q attached or sent network request")
	}
	setScopedDataset(t, store, scopedDatasetA)
	result, err := d.QueryBatchOperationForDataset(scopedDatasetA, r.OperationID)
	if err != nil || len(result.TaskIDs) != 1 || result.Members[0].TaskPresent == nil || !*result.Members[0].TaskPresent {
		t.Fatalf("durable M lost: %+v %v", result, err)
	}
	if len(d.GetTasks()) != 0 || hits.Load() != 0 {
		t.Fatal("read-only query executed Q")
	}
}

func TestScopedBatchConcurrentSameOperation(t *testing.T) {
	d, _, dir := scopedOperationFixture(t, false)
	url, _ := localOperationSource(t)
	r := operationRequest(url, dir)
	var workers sync.WaitGroup
	type outcome struct {
		result BatchOperationResult
		err    error
	}
	results := make(chan outcome, 8)
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			result, err := d.CreateBatchOperationForDataset(context.Background(), scopedDatasetA, r)
			results <- outcome{result, err}
		}()
	}
	workers.Wait()
	close(results)
	id := ""
	for value := range results {
		if value.err != nil || len(value.result.TaskIDs) != 1 {
			t.Fatalf("concurrent result: %+v %v", value.result, value.err)
		}
		if id == "" {
			id = value.result.TaskIDs[0]
		}
		if id != value.result.TaskIDs[0] {
			t.Fatal("same op changed stable member ID")
		}
	}
	waitStartupDone(t, d, id)
	if len(d.GetTasks()) != 1 {
		t.Fatal("concurrent Q duplicated a Task")
	}
}

type scopedReadCost struct {
	datasetReader
	operations     int
	decodedMembers int
	tasks          int
	links          int
}

func (r *scopedReadCost) Get(bucket, key string, out any) (bool, error) {
	found, err := r.datasetReader.Get(bucket, key, out)
	if err != nil || !found {
		return found, err
	}
	switch bucket {
	case bucketTaskOperations:
		r.operations++
		if record, ok := out.(*batchOperationRecord); ok {
			r.decodedMembers += len(record.Members)
		}
	case bucketTask:
		r.tasks++
	case bucketOperationTasks:
		r.links++
	}
	return found, nil
}
func scopedReadCostFixture(t *testing.T, n int) (*Downloader, AtomicStorage, BatchOperationRequest, string) {
	t.Helper()
	d, store, dir := scopedOperationFixture(t, false)
	r := operationRequest("http://127.0.0.1:1/no-network", dir)
	for i := 1; i < n; i++ {
		r.Reqs = append(r.Reqs, &base.CreateTaskBatchItem{Req: &base.Request{URL: r.Reqs[0].Req.URL}, Opts: &base.Options{Path: dir, Name: fmt.Sprintf("item-%d.bin", i)}})
	}
	items, hash := scopedReservation(t, d, store, r)
	for i, item := range items {
		if _, err := d.createOperationMemberInDataset(store, r.OperationID, hash, i, item, scopedDatasetA); err != nil {
			t.Fatal(err)
		}
	}
	return d, store, r, hash
}
func TestScopedBatchSnapshotDecodesManifestOnce(t *testing.T) {
	for _, n := range []int{8, 16, 32} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			_, store, r, _ := scopedReadCostFixture(t, n)
			var cost *scopedReadCost
			err := withDatasetReadView(store, scopedDatasetA, func(reader datasetReader) error {
				cost = &scopedReadCost{datasetReader: reader}
				snapshot, err := readOperationSnapshotFrom(cost, r.OperationID)
				if err == nil && len(snapshot.tasks) != n {
					t.Error("full snapshot lost associated tasks")
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("members=%d manifestReads=%d decodedMembers=%d taskReads=%d linkReads=%d", n, cost.operations, cost.decodedMembers, cost.tasks, cost.links)
			if cost.operations != 1 || cost.decodedMembers != n || cost.tasks != n || cost.links != n {
				t.Fatal("full same-view snapshot repeatedly decoded its manifest")
			}
		})
	}
}

func TestScopedBatchMemberPublicationReadsOnlyTarget(t *testing.T) {
	for _, n := range []int{8, 16, 32} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			_, store, r, hash := scopedReadCostFixture(t, n)
			total := &scopedReadCost{}
			for index := 0; index < n; index++ {
				err := withDatasetReadView(store, scopedDatasetA, func(reader datasetReader) error {
					cost := &scopedReadCost{datasetReader: reader}
					task, err := readOperationMemberFrom(cost, r.OperationID, hash, index)
					if err == nil && (task == nil || task.CreationOperation.Index != index) {
						t.Error("target member missing or changed")
					}
					total.operations += cost.operations
					total.decodedMembers += cost.decodedMembers
					total.tasks += cost.tasks
					total.links += cost.links
					if cost.operations != 1 || cost.decodedMembers != n || cost.tasks != 1 || cost.links != 1 {
						t.Errorf("M->Q read unrelated members: %+v", cost)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("members=%d allQManifestReads=%d allQDecodedMembers=%d allQTaskReads=%d allQLinkReads=%d", n, total.operations, total.decodedMembers, total.tasks, total.links)
			if total.operations != n || total.decodedMembers != n*n || total.tasks != n || total.links != n {
				t.Fatal("avoidable cubic M->Q read amplification returned")
			}
		})
	}
}
