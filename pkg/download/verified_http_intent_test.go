package download

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

func TestVerifiedHTTPIntentPreservesLegacyHash(t *testing.T) {
	request := operationRequest("https://fixture.invalid/file", t.TempDir())
	items, legacy, err := canonicalBatch(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(struct {
		Version   int    `json:"version"`
		Namespace string `json:"namespace"`
		Items     any    `json:"items"`
	}{1, batchOperationNamespace, items})
	sum := sha256.Sum256(raw)
	if legacy != hex.EncodeToString(sum[:]) {
		t.Fatal("legacy fingerprint changed")
	}
	request.integrity = &VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", strings.Repeat("a", 64)}
	_, first, err := canonicalBatch(request)
	if err != nil || first == legacy {
		t.Fatal("integrity omitted from permanent intent", err)
	}
	request.integrity.ExpectedSHA256 = strings.Repeat("b", 64)
	_, second, err := canonicalBatch(request)
	if err != nil || second == first {
		t.Fatal("different expectation shares fingerprint", err)
	}
}

func TestVerifiedHTTPReservationFreezesPolicyAndAtomicCompletion(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt], func(t *testing.T) {
			d, store, dir := scopedOperationFixture(t, bolt)
			request := operationRequest("https://fixture.invalid/file", dir)
			request.integrity = &VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", strings.Repeat("a", 64)}
			items, hash, err := canonicalBatch(request)
			if err != nil {
				t.Fatal(err)
			}
			if err = d.reserveOperationInDataset(store, request, items, hash, scopedDatasetA); err != nil {
				t.Fatal(err)
			}
			initial, err := d.loadOperationInDataset(scopedDatasetA, request.OperationID)
			if err != nil || initial.Integrity == nil || initial.Members[0].CompletionID == "" {
				t.Fatal("R omitted permanent intent", err)
			}
			d.configSnapshotMu.Lock()
			d.cfg.AutoTorrent.Enable = true
			d.cfg.Archive.AutoExtract = true
			d.configSnapshotMu.Unlock()
			if err = d.reserveOperationInDataset(store, request, items, hash, scopedDatasetA); err != nil {
				t.Fatal("later defaults changed old R receipt", err)
			}
			changed := request
			changed.OperationID = "new-global-true"
			_, changedHash, _ := canonicalBatch(changed)
			if err = d.reserveOperationInDataset(store, changed, items, changedHash, scopedDatasetA); !errors.Is(err, ErrVerifiedHTTPUnsupportedOption) {
				t.Fatal("new true option silently disabled", err)
			}
			absent, err := d.loadOperationInDataset(scopedDatasetA, changed.OperationID)
			if err != nil || absent != nil {
				t.Fatal("unsupported option wrote R", err)
			}
			created, err := d.createOperationMemberInDataset(store, request.OperationID, hash, 0, items[0], scopedDatasetA)
			if err != nil || !created {
				t.Fatal("M failed", err)
			}
			if err = withDatasetReadView(store, scopedDatasetA, func(reader datasetReader) error {
				stored, err := loadOperationFrom(reader, request.OperationID)
				if err != nil {
					return err
				}
				task, record, err := readTaskCompletion(reader, stored.Members[0].TaskID)
				if err != nil {
					return err
				}
				if record.Phase != transfer.CompletionPrepared || record.Size != -1 || record.StagingIdentity.Inode != 0 || record.FinalName != "" || task.Completion.CompletionID != record.ID || record.ID != stored.Members[0].CompletionID {
					t.Fatal("M did not preserve same completion binding")
				}
				if err = checkCreationAssociationFrom(reader, task); err != nil {
					return err
				}
				raw, _ := json.Marshal(task.Meta.Opts.Extra)
				var extra map[string]any
				json.Unmarshal(raw, &extra)
				if extra["autoTorrent"] != false || extra["autoExtract"] != false {
					t.Fatal("effective false was not frozen")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// No Q or HTTP execution is invoked by this transaction-level check.
			created, err = d.createOperationMemberInDataset(store, request.OperationID, hash, 0, items[0], scopedDatasetA)
			if err != nil || created {
				t.Fatal("replay obtained new M authority", err)
			}
			legacy := request
			legacy.integrity = nil
			_, legacyHash, _ := canonicalBatch(legacy)
			if err = d.reserveOperationInDataset(store, legacy, items, legacyHash, scopedDatasetA); !errors.Is(err, ErrOperationConflict) {
				t.Fatal("legacy payload replaced SHA intention", err)
			}
		})
	}
}
