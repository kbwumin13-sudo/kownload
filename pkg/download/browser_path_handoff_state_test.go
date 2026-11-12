package download

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/base"
)

// Storage-only evidence; browser cancellation, first execution RAM and output
// IO are exercised by the existing caller sample, not manufactured here.
func TestBrowserPathHandoffK12AtomicPrepareAndCommit(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := mcpK12New(t, backend)
			create := func(op string) (*Task, BrowserHandoff) {
				request, snapshot := pathK12Intent(f, "http", op)
				sum := sha256.Sum256([]byte(request.Request.Req.URL))
				h := BrowserHandoff{Version: 1, OperationID: op, ClientID: "fixture-client", Browser: "chrome", Download: BrowserDownloadBinding{ID: 7, StartTime: "2026-11-12T01:00:00Z", URLHash: hex.EncodeToString(sum[:])}, Phase: "prepared", RequestHash: browserPathRequestHash(request)}
				r, fresh, err := reserveBrowserPathOperation(f.store, f.d, request, snapshot, request.Request.Opts, h)
				if err != nil || !fresh {
					t.Fatal("R", err, fresh)
				}
				changed := h
				changed.ClientID = "another-client"
				if _, _, err = reserveBrowserPathOperation(f.store, f.d, request, snapshot, request.Request.Opts, changed); !errors.Is(err, ErrPathFreezeConflict) {
					t.Fatalf("identity replacement: %v", err)
				}
				task, initial := pathK12Candidate(t, r, 0)
				if _, fresh, err = createPathOperationTask(f.store, f.d, r, task, initial); err == nil || fresh {
					t.Fatal("M omitted handoff")
				}
				task.Handoff = &h
				task, fresh, err = createPathOperationTask(f.store, f.d, r, task, initial)
				if err != nil || !fresh || task.Status != base.DownloadStatusPause || task.Meta.Res != nil || task.PathFreeze.Phase != PathFreezePending {
					t.Fatal("M", err)
				}
				saved, _ := pathK12Read(t, f, task.ID)
				if saved.Handoff == nil || *saved.Handoff != h {
					t.Fatal("atomic binding absent")
				}
				return saved, h
			}
			raw := func(bucket, key string) []byte {
				var data json.RawMessage
				err := withDatasetReadView(f.store, f.d, func(reader datasetReader) error { _, err := reader.Get(bucket, key, &data); return err })
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			task, h := create("browser-path-commit")
			original, err := cloneOperationJSON(task)
			if err != nil {
				t.Fatal(err)
			}
			recordBefore := raw(bucketTaskOperations, operationKey(h.OperationID))
			recoveryBefore := raw(bucketTaskRecovery, task.ID)
			requiredBefore := raw(bucketRecoveryRequired, task.ID)
			wrong := h
			wrong.Download.ID++
			if _, fresh, err := transitionBrowserPathHandoff(f.store, f.d, wrong, "committed", ""); err == nil || fresh {
				t.Fatal("wrong browser binding committed")
			}
			committed, fresh, err := transitionBrowserPathHandoff(f.store, f.d, h, "committed", "")
			if err != nil || !fresh || committed == nil || committed.Handoff.Phase != "committed" {
				t.Fatal("commit", err)
			}
			original.Handoff.Phase = "committed"
			want, _ := json.Marshal(original)
			got, _ := json.Marshal(committed)
			if !bytes.Equal(want, got) {
				t.Fatal("commit changed unrelated Task fields")
			}
			if !bytes.Equal(recordBefore, raw(bucketTaskOperations, operationKey(h.OperationID))) || !bytes.Equal(recoveryBefore, raw(bucketTaskRecovery, task.ID)) || !bytes.Equal(requiredBefore, raw(bucketRecoveryRequired, task.ID)) {
				t.Fatal("commit changed immutable R/recovery")
			}
			replay, fresh, err := transitionBrowserPathHandoff(f.store, f.d, h, "committed", "")
			if err != nil || fresh || replay == nil || !reflect.DeepEqual(replay.Handoff, committed.Handoff) {
				t.Fatal("replay recreated fresh grant", err)
			}
			if _, fresh, err = transitionBrowserPathHandoff(f.store, f.d, h, "aborted", "user_cancelled"); err == nil || fresh {
				t.Fatal("committed rewound")
			}
			// Real commit followed by lost acknowledgement must return no fresh value;
			// the subsequent authoritative read exposes history without a new permit.
			unknown, second := create("browser-path-unknown")
			owned, fresh, err := transitionBrowserPathHandoff(mcpK12Fault{f.store, true}, f.d, second, "committed", "")
			if err == nil || fresh || owned != nil {
				t.Fatal("unknown ACK granted")
			}
			after, _ := pathK12Read(t, f, unknown.ID)
			if after.Handoff.Phase != "committed" || after.Status != base.DownloadStatusPause || after.Meta.Res != nil {
				t.Fatal("unknown authoritative history")
			}
			if owned, fresh, err = transitionBrowserPathHandoff(f.store, f.d, second, "committed", ""); err != nil || fresh || owned == nil {
				t.Fatal("readback recreated grant", err)
			}
		})
	}
}
