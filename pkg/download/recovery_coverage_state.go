package download

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// These values are caller-owned snapshots, not run/IO authorization. In
// particular, the checkpoint CAS describes the CURRENT bucketSave value, not
// Coverage.CheckpointHash: a normal periodic Store may have advanced it.
type recoveryCoverageExpected struct {
	Token             transfer.RecoveryToken
	Record            recoveryRecord
	CheckpointPresent bool
	CheckpointHash    string
}

type recoveryCoverageUpdate struct {
	Snapshot   *Task
	Checkpoint json.RawMessage
	Plan       transfer.HTTPCoveragePlan
	NewEpisode string // empty: snapshot only; otherwise: earned settlement
}

// readRecoveryCoverage runs inside the caller's owned dataset view/transaction.
// It never enrolls a legacy Task or attaches its decoded Task to the live map.
func readRecoveryCoverage(reader datasetReader, id string) (*Task, recoveryRecord, error) {
	task, record, err := readRecoveryCoverageParts(reader, id)
	if err != nil || !hasRecoveryCoverage(record) {
		if err == nil {
			err = transfer.ErrRecoveryHeld
		}
		return nil, recoveryRecord{}, err
	}
	return task, record, nil
}

// The triplet must already be validated. A plain v4 declaration never supplies
// an implicit empty baseline, even though the shared CAS reader accepts it.
func hasRecoveryCoverage(record recoveryRecord) bool {
	return (record.Version == 3 || record.Version == recoverySharedVersion) && record.Coverage != nil
}

func readRecoveryCoverageParts(reader datasetReader, id string) (*Task, recoveryRecord, error) {
	if reader == nil || id == "" {
		return nil, recoveryRecord{}, transfer.ErrRecoveryHeld
	}
	task := new(Task)
	var record recoveryRecord
	var required recoveryRequirement
	for _, part := range []struct {
		bucket string
		value  any
	}{{bucketTask, task}, {bucketTaskRecovery, &record}, {bucketRecoveryRequired, &required}} {
		var raw json.RawMessage
		present, err := reader.Get(part.bucket, id, &raw)
		if err != nil {
			return nil, recoveryRecord{}, transfer.ErrRecoveryUnavailable
		}
		if !present {
			return nil, recoveryRecord{}, transfer.ErrRecoveryHeld
		}
		if err := decodeStoredJSON(raw, part.value); err != nil {
			return nil, recoveryRecord{}, transfer.ErrRecoveryHeld
		}
	}
	if task.ID != id || task.Meta == nil || task.Meta.Opts == nil || validateRecoveryTriplet(task, record, required) != nil {
		return nil, recoveryRecord{}, transfer.ErrRecoveryHeld
	}
	return task, record, nil
}

func recoveryCoverageStoreError(err error) error {
	if err == nil {
		return nil
	}
	for _, fixed := range []error{transfer.ErrRecoveryHeld, transfer.ErrRecoveryStale,
		ErrDatasetInvalid, ErrDatasetMissing, ErrDatasetMismatch} {
		if errors.Is(err, fixed) {
			return fixed
		}
	}
	return transfer.ErrRecoveryUnavailable
}

// DB callbacks take no owner/main/live Task locks and perform no protocol or
// file IO. The caller must fence membership/cancellation/source changes and
// prove writer join, same-fd Sync, response identity and small-file completion.
func recoveryCoverageCAS(tx StorageTransaction, dataset string, expected recoveryCoverageExpected) (*Task, recoveryRecord, error) {
	if err := requireDataset(tx, dataset); err != nil {
		return nil, recoveryRecord{}, err
	}
	task, record, err := readRecoveryCoverageParts(tx, expected.Token.TaskID)
	if err != nil {
		return nil, recoveryRecord{}, err
	}
	token := expected.Token
	if token.Episode == "" || token.Generation == 0 || token.SourceRevision == 0 ||
		token.Episode != record.Episode || token.Generation != record.Generation || token.SourceRevision != record.SourceRevision ||
		!reflect.DeepEqual(record, expected.Record) {
		return nil, recoveryRecord{}, transfer.ErrRecoveryStale
	}
	if expected.CheckpointPresent && !validRecoveryHash(expected.CheckpointHash) ||
		!expected.CheckpointPresent && expected.CheckpointHash != "" {
		return nil, recoveryRecord{}, transfer.ErrRecoveryHeld
	}
	var checkpoint json.RawMessage
	present, err := tx.Get(bucketSave, token.TaskID, &checkpoint)
	if err != nil {
		return nil, recoveryRecord{}, transfer.ErrRecoveryUnavailable
	}
	if present != expected.CheckpointPresent {
		return nil, recoveryRecord{}, transfer.ErrRecoveryStale
	}
	if present {
		hash, err := checkpointDigest(checkpoint)
		if err != nil || hash != expected.CheckpointHash {
			return nil, recoveryRecord{}, transfer.ErrRecoveryStale
		}
	}
	if task.Handoff != nil && (task.Handoff.Version != 1 || task.Handoff.Phase != "committed") {
		return nil, recoveryRecord{}, transfer.ErrRecoveryHeld
	}
	if err := checkCreationAssociationFrom(tx, task); err != nil {
		return nil, recoveryRecord{}, transfer.ErrRecoveryHeld
	}
	if err := guardCompletionSnapshot(tx, task); err != nil {
		return nil, recoveryRecord{}, transfer.ErrRecoveryHeld
	}
	return task, record, nil
}

// installRecoveryCoverage cannot prove the one-shot installation authority.
// Core must consume its private, ACK-confirmed new-M slot before calling, after
// response identity is known and before the first target byte. Failure burns
// that slot. A durable ungranted string/query/restart cannot recreate it.
func installRecoveryCoverage(store AtomicStorage, dataset string, expected recoveryCoverageExpected, initial transfer.HTTPCoverageState) (recoveryRecord, error) {
	if store == nil {
		return recoveryRecord{}, transfer.ErrRecoveryUnavailable
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return recoveryRecord{}, transfer.ErrRecoveryHeld
	}
	initial, err = cloneOperationJSON(initial)
	if err != nil {
		return recoveryRecord{}, transfer.ErrRecoveryHeld
	}
	var committed recoveryRecord
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		task, record, err := recoveryCoverageCAS(tx, dataset, expected)
		if err != nil {
			return err
		}
		legacy := record.Version == 1
		plainShared := record.Version == recoverySharedVersion && record.Coverage == nil
		if (!legacy && !plainShared) || record.InitialAttempt.State != "issued" || record.Generation == 0 || task.Status == base.DownloadStatusDone {
			return transfer.ErrRecoveryHeld
		}
		empty, err := transfer.NewHTTPCoverageState(initial.IdentityHash, initial.Size)
		if err != nil || !reflect.DeepEqual(empty, initial) {
			return transfer.ErrRecoveryHeld
		}
		if legacy {
			task.Recovery.Version, record.Version = 3, 3
		}
		record.Coverage = &initial
		required, err := recoveryRequirementFor(record)
		if err != nil || validateRecoveryTriplet(task, record, required) != nil {
			return transfer.ErrRecoveryHeld
		}
		if legacy {
			if err := tx.Put(bucketTask, task.ID, task); err != nil {
				return err
			}
		}
		if err := tx.Put(bucketTaskRecovery, task.ID, &record); err != nil {
			return err
		}
		if err := tx.Put(bucketRecoveryRequired, task.ID, &required); err != nil {
			return err
		}
		committed = record
		return nil
	})
	if err != nil {
		return recoveryRecord{}, recoveryCoverageStoreError(err)
	}
	return committed, nil
}

// saveRecoveryCoverageCheckpoint handles existing v3 or strict v4 coverage. The
// version/ref/required capability and Until are preserved. A successful bool is
// an earned-transaction ACK winner, not an IO permit. The runtime owner alone
// can install a private one-shot settlementAuthorization after the same Fence.
// Any error (including commit-with-lost-ACK) discards the closure's result.
func saveRecoveryCoverageCheckpoint(store AtomicStorage, dataset string, expected recoveryCoverageExpected, next recoveryCoverageUpdate) (recoveryRecord, bool, error) {
	if store == nil {
		return recoveryRecord{}, false, transfer.ErrRecoveryUnavailable
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return recoveryRecord{}, false, transfer.ErrRecoveryHeld
	}
	next, err = cloneOperationJSON(next)
	if err != nil {
		return recoveryRecord{}, false, transfer.ErrRecoveryHeld
	}
	var committed recoveryRecord
	earned := false
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		task, record, err := recoveryCoverageCAS(tx, dataset, expected)
		if err != nil {
			return err
		}
		if !hasRecoveryCoverage(record) || !recoveryCoverageSnapshotMatches(task, next.Snapshot) {
			return transfer.ErrRecoveryHeld
		}
		if err := guardCompletionSnapshot(tx, next.Snapshot); err != nil {
			return transfer.ErrRecoveryHeld
		}
		if !json.Valid(next.Checkpoint) || bytes.Equal(bytes.TrimSpace(next.Checkpoint), []byte("null")) {
			return transfer.ErrRecoveryHeld
		}
		digest, err := checkpointDigest(next.Checkpoint)
		if err != nil {
			return err
		}
		plan, err := transfer.PlanHTTPCoverageCheckpoint(*record.Coverage, next.Plan.Snapshot.DurableRanges, digest, recoveryCoverageRangeLimit)
		if err != nil || !reflect.DeepEqual(plan, next.Plan) {
			return transfer.ErrRecoveryHeld
		}
		coverage := plan.Snapshot
		if next.NewEpisode != "" {
			if next.NewEpisode == record.Episode || plan.Settlement == nil || plan.NetNewBytes <= 0 {
				return transfer.ErrRecoveryHeld
			}
			coverage = *plan.Settlement
			record.Episode = next.NewEpisode
			record.TotalUsed, record.TransportUsed, record.RefreshUsed, record.DeferralUsed = 0, 0, 0, 0
			record.Actions, record.NextAction = []recoveryAction{}, 0
			record.InitialAttempt = recoveryInitialAttempt{State: "issued", Generation: record.Generation}
			earned = true
		}
		record.Coverage = &coverage
		required, err := recoveryRequirementFor(record)
		if err != nil || validateRecoveryTriplet(task, record, required) != nil {
			return transfer.ErrRecoveryHeld
		}
		if err := tx.Put(bucketSave, task.ID, next.Checkpoint); err != nil {
			return err
		}
		if err := tx.Put(bucketTask, task.ID, next.Snapshot); err != nil {
			return err
		}
		if err := tx.Put(bucketTaskRecovery, task.ID, &record); err != nil {
			return err
		}
		committed = record
		return nil
	})
	if err != nil {
		return recoveryRecord{}, false, recoveryCoverageStoreError(err)
	}
	return committed, earned, nil
}

func recoveryCoverageSnapshotMatches(current, next *Task) bool {
	if next == nil || current.Status == base.DownloadStatusDone || next.Status == base.DownloadStatusDone {
		return false
	}
	switch next.Status {
	case base.DownloadStatusReady, base.DownloadStatusWait, base.DownloadStatusRunning, base.DownloadStatusPause, base.DownloadStatusError:
	default:
		return false
	}
	// Only status/progress/update-time may differ. In particular, the caller
	// cannot replace raw execution options, resolved output names, S2/handoff,
	// completion intent, or immutable Task metadata using an old snapshot.
	owned, err := cloneOperationJSON(next)
	if err != nil {
		return false
	}
	owned.Status, owned.Progress, owned.UpdatedAt = current.Status, current.Progress, current.UpdatedAt
	a, errA := json.Marshal(current)
	b, errB := json.Marshal(owned)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}
