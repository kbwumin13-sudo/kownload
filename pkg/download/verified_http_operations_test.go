package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestVerifiedHTTPOperationReusesS2IntentAndHistory(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt], func(t *testing.T) {
			d, _, root := scopedOperationFixture(t, bolt)
			body := bytes.Repeat([]byte("typed receiver intent\n"), 200)
			sum := sha256.Sum256(body)
			var hits atomic.Int32
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("ETag", `"typed-source"`)
				http.ServeContent(w, r, "one.bin", time.Time{}, bytes.NewReader(body))
			}))
			defer source.Close()
			item := operationRequest(source.URL, root).Reqs[0]
			item.Req.Extra = map[string]any{"opaque": json.Number("9007199254740993")}
			item.Opts.Extra = map[string]any{"connections": 1, "autoTorrent": false, "autoExtract": false, "opaque": json.Number("9007199254740993")}
			request := VerifiedHTTPRequest{Version: 1, OperationID: "typed-op", Request: item, Integrity: VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", hex.EncodeToString(sum[:])}}
			result, err := d.createVerifiedOperation(context.Background(), scopedDatasetA, request)
			if err != nil || len(result.Operation.TaskIDs) != 1 {
				t.Fatal(err, result)
			}
			id := result.Operation.TaskIDs[0]
			waitStatus(t, d, id, base.DownloadStatusDone, 4*time.Second)
			result, err = d.queryVerifiedOperation(scopedDatasetA, request.OperationID)
			if err != nil || len(result.CompletionViews) != 1 || !result.CompletionViews[0].Available || result.CompletionViews[0].Phase != "committed" {
				t.Fatal("typed query lost authoritative view", result, err)
			}
			initialHits := hits.Load()
			d.configSnapshotMu.Lock()
			d.cfg.AutoTorrent.Enable = true
			d.cfg.Archive.AutoExtract = true
			d.configSnapshotMu.Unlock()
			again, err := d.createVerifiedOperation(context.Background(), scopedDatasetA, request)
			if err != nil || len(again.Operation.TaskIDs) != 1 || again.Operation.TaskIDs[0] != id || hits.Load() != initialHits {
				t.Fatal("same raw op changed frozen policy/task or repeated IO", err)
			}
			record, err := d.loadOperationInDataset(scopedDatasetA, request.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(record.Members[0].Options.Extra)
			if !bytes.Contains(raw, []byte("9007199254740993")) {
				t.Fatal("R frozen options rounded Extra")
			}
			saved := d.GetTask(id)
			if saved == nil {
				t.Fatal("created task missing")
			}
			b, err := os.ReadFile(saved.Meta.SingleFilepath())
			if err != nil || !bytes.Equal(b, body) {
				t.Fatal("receiver output changed", err)
			}
			changed := request
			changed.Integrity.ExpectedSHA256 = strings.Repeat("a", 64)
			if _, err = d.createVerifiedOperation(context.Background(), scopedDatasetA, changed); !errors.Is(err, ErrOperationConflict) {
				t.Fatal("same op changed digest", err)
			}
			if _, err = d.createVerifiedOperation(context.Background(), scopedDatasetB, request); !errors.Is(err, ErrDatasetMismatch) {
				t.Fatal("wrong domain created task", err)
			}
			if err = d.Delete(&TaskFilter{IDs: []string{id}}, false); err != nil {
				t.Fatal(err)
			}
			historical, err := d.queryVerifiedOperation(scopedDatasetA, request.OperationID)
			if err != nil || historical.Operation.CreatedCount != 1 || len(historical.Operation.Members) != 1 || historical.Operation.Members[0].TaskPresent == nil || *historical.Operation.Members[0].TaskPresent {
				t.Fatal("deleted historical association lost", err, historical)
			}
			if _, err = d.createVerifiedOperation(context.Background(), scopedDatasetA, request); err != nil {
				t.Fatal(err)
			}
			if len(d.GetTasks()) != 0 || hits.Load() != initialHits {
				t.Fatal("history replay recreated deleted task")
			}
		})
	}
}

func TestVerifiedHTTPUnknownMACKAndTypedCancelRemainNonExecuting(t *testing.T) {
	d, store, root := scopedOperationFixture(t, true)
	url, hits := localOperationSource(t)
	r := operationRequest(url, root)
	r.Reqs[0].Req.Extra = map[string]any{"opaque": json.Number("9007199254740993")}
	r.integrity = &VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", strings.Repeat("a", 64)}
	items, hash, err := canonicalBatch(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.reserveOperationInDataset(store, r, items, hash, scopedDatasetA); err != nil {
		t.Fatal(err)
	}
	fault := &operationFaultStorage{AtomicStorage: store, phase: "ack-member"}
	created, err := d.createOperationMemberInDataset(fault, r.OperationID, hash, 0, items[0], scopedDatasetA)
	if created || !errors.Is(err, ErrOperationUnavailable) {
		t.Fatal("unknown M granted first IO", created, err)
	}
	d.operationMu.Lock()
	err = d.publishOperationMemberInDataset(scopedDatasetA, r.OperationID, hash, 0, false)
	d.operationMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := d.queryVerifiedOperation(scopedDatasetA, r.OperationID)
	if err != nil || receipt.Operation.CreatedCount != 1 {
		t.Fatal(receipt, err)
	}
	id := receipt.Operation.TaskIDs[0]
	task := d.getTask(id)
	if task == nil || task.getStatus() != base.DownloadStatusPause || task.initialHTTPExecution.Load() || hits.Load() != 0 {
		t.Fatal("unknown ACK attached as executing")
	}
	rawReq, _ := json.Marshal(task.Meta.Req.Extra)
	if !bytes.Contains(rawReq, []byte("9007199254740993")) {
		t.Fatal("M precision lost before protocol typed interpretation")
	}

	request := VerifiedHTTPRequest{1, r.OperationID, items[0], *r.integrity}
	replay, err := d.createVerifiedOperation(context.Background(), scopedDatasetA, request)
	if err != nil || replay.Operation.TaskIDs[0] != id || hits.Load() != 0 || task.getStatus() != base.DownloadStatusPause {
		t.Fatal("replay regained IO", err)
	}
	// Pending typed cancellation reuses R and grants no Task/network permission.
	pending := r
	pending.OperationID = "pending-verified"
	pendingItems, pendingHash, err := canonicalBatch(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.reserveOperationInDataset(store, pending, pendingItems, pendingHash, scopedDatasetA); err != nil {
		t.Fatal(err)
	}
	canceled, err := d.cancelVerifiedOperation(scopedDatasetA, pending.OperationID)
	if err != nil || canceled.Operation.FailedCount != 1 || canceled.Operation.Members[0].Error != "cancelled" || hits.Load() != 0 {
		t.Fatal(canceled, err)
	}
	legacy := pending
	legacy.OperationID = "pending-legacy"
	legacy.integrity = nil
	legacyItems, legacyHash, err := canonicalBatch(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.reserveOperationInDataset(store, legacy, legacyItems, legacyHash, scopedDatasetA); err != nil {
		t.Fatal(err)
	}
	if _, err = d.cancelVerifiedOperation(scopedDatasetA, legacy.OperationID); !errors.Is(err, ErrOperationConflict) {
		t.Fatal("verified cancel altered legacy op", err)
	}
	untouched, err := d.QueryBatchOperationForDataset(scopedDatasetA, legacy.OperationID)
	if err != nil || untouched.PendingCount != 1 {
		t.Fatal("legacy operation changed", untouched, err)
	}
}
