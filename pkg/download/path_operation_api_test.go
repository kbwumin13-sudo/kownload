package download

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
)

// These are receiver/read boundaries, not evidence for the protocol F/writer
// pipeline. The stored M below uses the existing component fixture explicitly.
func TestPathOperationFacadeK12ReadPreviewReplay(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt], func(t *testing.T) {
			d, store, root := scopedOperationFixture(t, bolt)
			f := &mcpK12Fixture{store: store, d: scopedDatasetA}
			request, _ := pathK12Intent(f, "http", "facade-read")
			request.Request.Opts.Path = root
			request.PathIntent.Path = root
			request.PathIntent.Origin = pathpolicy.APIExplicit
			result, err := d.QueryPathOperationForDataset(f.d, HTTPPathCreateKind, request.OperationID)
			if err != nil || result.Operation.State != "unknown" || result.RequiredCapabilities == nil || len(result.RequiredCapabilities) != 0 || len(result.Operation.TaskIDs) != 0 {
				t.Fatalf("absent receipt: %v %+v", err, result)
			}
			if _, err = d.QueryPathOperationForDataset(scopedDatasetB, HTTPPathCreateKind, request.OperationID); !errors.Is(err, ErrDatasetMismatch) {
				t.Fatalf("wrong dataset: %v", err)
			}
			preview, err := d.PreviewPathForDataset(context.Background(), f.d, HTTPPathCreateKind, request)
			if err != nil || preview.Name != request.Request.Opts.Name || preview.Decision.Origin != pathpolicy.APIExplicit || !filepath.IsAbs(preview.Decision.Root) {
				t.Fatalf("preview: %v %+v", err, preview)
			}
			// Preview performs no reservation; it is not an M, F or output permission.
			result, err = d.QueryPathOperationForDataset(f.d, HTTPPathCreateKind, request.OperationID)
			if err != nil || result.Operation.State != "unknown" || len(d.GetTasks()) != 0 {
				t.Fatal("preview mutated authority", err)
			}
			snapshot, opts, err := d.preparePathReception(request)
			if err != nil {
				t.Fatal(err)
			}
			r, _, err := reservePathTreeOperation(store, f.d, request, snapshot, opts)
			if err != nil {
				t.Fatal(err)
			}
			d.configSnapshotMu.Lock()
			d.cfg.DownloadDir = "/not-the-frozen-root"
			d.cfg.Extra = map[string]any{"pathPolicyV1": map[string]any{"version": 99}}
			d.configSnapshotMu.Unlock()
			replay, err := d.CreatePathOperationForDataset(context.Background(), f.d, HTTPPathCreateKind, request)
			if err != nil || replay.Operation.State != "pending" || len(d.GetTasks()) != 0 {
				t.Fatal("pending replay reinterpreted cfg or completed M", err, replay)
			}
			changed, _ := cloneOperationJSON(request)
			changed.Request.Opts.Name = "different.bin"
			if _, err = d.CreatePathOperationForDataset(context.Background(), f.d, HTTPPathCreateKind, changed); !errors.Is(err, ErrPathFreezeConflict) {
				t.Fatalf("changed raw accepted: %v", err)
			}
			task, initial := pathK12Candidate(t, r, 0)
			if _, _, err = createPathOperationTree(store, f.d, r, []*Task{task}, []recoveryRecord{initial}); err != nil {
				t.Fatal(err)
			}
			replay, err = d.CreatePathOperationForDataset(context.Background(), f.d, HTTPPathCreateKind, request)
			if err != nil || replay.Operation.CreatedCount != 1 || replay.Operation.TaskIDs[0] != task.ID || len(d.GetTasks()) != 0 {
				t.Fatal("stored M replay attached/ran", err, replay)
			}
			if _, err = d.QueryPathOperationForDataset(f.d, "media-path-create/v1", request.OperationID); !errors.Is(err, ErrPathFreezeHeld) {
				t.Fatalf("mixed variant accepted: %v", err)
			}
			history, err := d.QueryBatchOperationForDataset(f.d, request.OperationID)
			if err != nil || history.Version != 1 || history.CreatedCount != 1 {
				t.Fatal("cold legacy journal query", err, history)
			}
		})
	}
}
