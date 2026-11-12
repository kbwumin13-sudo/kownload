package download

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// readSharedRecovery accepts declared, complete v1-v4 triplets in an owned
// same-dataset view/transaction. It neither enrolls legacy Tasks nor supplies
// runtime authority. The existing runtime reader still rejects v4.
func readSharedRecovery(reader datasetReader, id string) (*Task, recoveryRecord, recoveryRequirement, error) {
	if reader == nil || id == "" {
		return nil, recoveryRecord{}, recoveryRequirement{}, transfer.ErrRecoveryHeld
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
			return nil, recoveryRecord{}, recoveryRequirement{}, transfer.ErrRecoveryUnavailable
		}
		if !present || decodeStoredJSON(raw, part.value) != nil {
			return nil, recoveryRecord{}, recoveryRequirement{}, transfer.ErrRecoveryHeld
		}
	}
	if task.ID != id || validateRecoveryTriplet(task, record, required) != nil {
		return nil, recoveryRecord{}, recoveryRequirement{}, transfer.ErrRecoveryHeld
	}
	return task, record, required, nil
}

func sharedRecoveryStoreError(err error) error {
	if err == nil {
		return nil
	}
	for _, fixed := range []error{transfer.ErrRecoveryHeld, transfer.ErrRecoveryStale,
		transfer.ErrRecoveryWork, transfer.ErrRecoveryExhausted,
		ErrDatasetInvalid, ErrDatasetMissing, ErrDatasetMismatch} {
		if errors.Is(err, fixed) {
			return fixed
		}
	}
	return transfer.ErrRecoveryUnavailable
}

// This is durable CAS only, not execution Token.Valid. Legal plain G0 keeps
// its generation and may have a CURRENT checkpoint; neither fact grants IO.
// The caller owns the lifecycle Fence. No callback takes live/owner/main locks
// or performs protocol/file IO while the storage transaction is held.
func sharedRecoveryCAS(tx StorageTransaction, dataset string, expected recoverySharedExpected) (*Task, recoveryRecord, recoveryRequirement, error) {
	if err := requireDataset(tx, dataset); err != nil {
		return nil, recoveryRecord{}, recoveryRequirement{}, err
	}
	task, record, required, err := readSharedRecovery(tx, expected.Token.TaskID)
	if err != nil {
		return nil, recoveryRecord{}, recoveryRequirement{}, err
	}
	token := expected.Token
	if token.Episode != record.Episode || token.Generation != record.Generation || token.SourceRevision != record.SourceRevision ||
		!reflect.DeepEqual(record, expected.Record) || required != expected.Required {
		return nil, recoveryRecord{}, recoveryRequirement{}, transfer.ErrRecoveryStale
	}
	if expected.CheckpointPresent && !validRecoveryHash(expected.CheckpointHash) ||
		!expected.CheckpointPresent && expected.CheckpointHash != "" {
		return nil, recoveryRecord{}, recoveryRequirement{}, transfer.ErrRecoveryWork
	}
	var raw json.RawMessage
	present, err := tx.Get(bucketSave, token.TaskID, &raw)
	if err != nil {
		return nil, recoveryRecord{}, recoveryRequirement{}, transfer.ErrRecoveryUnavailable
	}
	if present != expected.CheckpointPresent {
		return nil, recoveryRecord{}, recoveryRequirement{}, transfer.ErrRecoveryStale
	}
	if present {
		digest, err := checkpointDigest(raw)
		if err != nil || digest != expected.CheckpointHash {
			return nil, recoveryRecord{}, recoveryRequirement{}, transfer.ErrRecoveryStale
		}
	}
	return task, record, required, nil
}

// upgradeSharedRecovery changes declarations only, preserving the CURRENT
// Task, record, checkpoint and all existing evidence. Core must join old workers
// outside its locks and hold the safe inactive Fence before calling. No initial,
// earned, continuation or wave authorization is installed or returned here.
func upgradeSharedRecovery(store AtomicStorage, dataset string, expected recoverySharedExpected) (recoveryRecord, error) {
	if store == nil {
		return recoveryRecord{}, transfer.ErrRecoveryUnavailable
	}
	expected, err := cloneOperationJSON(expected)
	if err != nil {
		return recoveryRecord{}, transfer.ErrRecoveryHeld
	}
	var result recoveryRecord
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		task, record, _, err := sharedRecoveryCAS(tx, dataset, expected)
		if err != nil {
			return err
		}
		if record.Version == recoverySharedVersion {
			result = record // Confirmed same-variant read; no writes or grants.
			return nil
		}
		record.Version, task.Recovery.Version = recoverySharedVersion, recoverySharedVersion
		required, err := recoveryRequirementFor(record)
		if err != nil || validateRecoveryTriplet(task, record, required) != nil {
			return transfer.ErrRecoveryHeld
		}
		if err := tx.Put(bucketTask, task.ID, task); err != nil {
			return err
		}
		if err := tx.Put(bucketTaskRecovery, task.ID, &record); err != nil {
			return err
		}
		if err := tx.Put(bucketRecoveryRequired, task.ID, &required); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return recoveryRecord{}, sharedRecoveryStoreError(err)
	}
	return result, nil
}

// reserveSharedRecoveryAction returns ACK-confirmed storage facts only. fresh
// means this call committed one new logical debit, NEVER permission to send.
// A runtime owner must separately bind its one-shot wave to D/E/G/S/action and
// frozen workset, and enforce Until at actual admission. Unknown ACK returns
// zero values even if the database committed; a later query cannot mint a wave.
func reserveSharedRecoveryAction(store AtomicStorage, dataset string, expected recoverySharedExpected, action recoverySharedAction) (record recoveryRecord, actionID uint64, fresh bool, err error) {
	if store == nil {
		return recoveryRecord{}, 0, false, transfer.ErrRecoveryUnavailable
	}
	expected, err = cloneOperationJSON(expected)
	if err != nil {
		return recoveryRecord{}, 0, false, transfer.ErrRecoveryHeld
	}
	action, err = cloneOperationJSON(action)
	if err != nil {
		return recoveryRecord{}, 0, false, transfer.ErrRecoveryWork
	}
	err = store.UpdateAtomic(func(tx StorageTransaction) error {
		task, current, required, err := sharedRecoveryCAS(tx, dataset, expected)
		if err != nil {
			return err
		}
		if current.Version != recoverySharedVersion {
			return transfer.ErrRecoveryHeld
		}
		members, err := recoveryMembers(action.Members)
		if err != nil || !hasRecoveryMember(members, action.Work) {
			return transfer.ErrRecoveryWork
		}
		switch action.Kind {
		case recoveryTransportAction, recoveryRefreshAction:
			if action.Until != "" || action.ExtendActionID != 0 {
				return transfer.ErrRecoveryWork
			}
		case recoveryDeferralAction:
			if action.Until == "" {
				return transfer.ErrRecoveryWork
			}
		default:
			return transfer.ErrRecoveryWork
		}
		until, err := parseRecoveryUntil(action.Until)
		if err != nil {
			return transfer.ErrRecoveryWork
		}
		previousUntil, err := parseRecoveryUntil(current.NotBefore)
		if err != nil {
			return transfer.ErrRecoveryHeld
		}
		if action.Seen > current.NextAction {
			return transfer.ErrRecoveryStale
		}
		if action.ExtendActionID != 0 {
			id := action.ExtendActionID
			if id != current.NextAction || action.Seen != id-1 {
				return transfer.ErrRecoveryStale
			}
			last := current.Actions[id-1]
			if recoveryActionKind(last.Kind) != recoveryDeferralAction || last.State != "charged" || len(last.Finished) != 0 ||
				last.SourceRevision != current.SourceRevision || last.Generation == 0 || last.Generation > current.Generation ||
				!sameRecoveryMembers(last.Members, members) || current.NotBefore == "" {
				return transfer.ErrRecoveryStale
			}
			if until.After(previousUntil) {
				current.NotBefore = action.Until
				if validateRecoveryTriplet(task, current, required) != nil {
					return transfer.ErrRecoveryHeld
				}
				if err := tx.Put(bucketTaskRecovery, task.ID, &current); err != nil {
					return err
				}
			}
			record, actionID = current, id
			return nil // No new debit and no fresh wave, even after an extension ACK.
		}
		if action.Seen < current.NextAction {
			last := current.Actions[current.NextAction-1]
			if last.ID != action.Seen+1 || recoveryActionKind(last.Kind) != action.Kind ||
				last.Generation != current.Generation || last.SourceRevision != current.SourceRevision || !sameRecoveryMembers(last.Members, members) {
				return transfer.ErrRecoveryStale
			}
			if action.Kind == recoveryDeferralAction && until.After(previousUntil) {
				return transfer.ErrRecoveryWork // Must explicitly extend; never discard stronger Retry-After.
			}
			record, actionID = current, last.ID
			return nil
		}
		if current.Generation == 0 {
			return transfer.ErrRecoveryHeld // G0 is valid storage, not a funded execution.
		}
		if current.TotalUsed >= recoveryMaxTotal ||
			action.Kind == recoveryTransportAction && current.TransportUsed >= recoveryMaxTransport ||
			action.Kind == recoveryRefreshAction && current.RefreshUsed >= recoveryMaxRefresh ||
			action.Kind == recoveryDeferralAction && current.DeferralUsed >= recoveryMaxDeferral {
			return transfer.ErrRecoveryExhausted
		}
		current.TotalUsed++
		current.NextAction++
		switch action.Kind {
		case recoveryTransportAction:
			current.TransportUsed++
		case recoveryRefreshAction:
			current.RefreshUsed++
		case recoveryDeferralAction:
			current.DeferralUsed++
			if until.After(previousUntil) {
				current.NotBefore = action.Until
			}
		}
		current.Actions = append(current.Actions, recoveryAction{ID: current.NextAction, Kind: string(action.Kind),
			Generation: current.Generation, SourceRevision: current.SourceRevision, Members: members, Finished: []string{}, State: "charged"})
		if validateRecoveryTriplet(task, current, required) != nil {
			return transfer.ErrRecoveryHeld
		}
		if err := tx.Put(bucketTaskRecovery, task.ID, &current); err != nil {
			return err
		}
		record, actionID, fresh = current, current.NextAction, true
		return nil
	})
	if err != nil {
		return recoveryRecord{}, 0, false, sharedRecoveryStoreError(err)
	}
	return record, actionID, fresh, nil
}
