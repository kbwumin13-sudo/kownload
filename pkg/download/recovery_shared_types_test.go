package download

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// These are declared schema fixtures, not actual v4 upgrade/writer evidence.
func sharedTypeFixture(t *testing.T) (*Task, recoveryRecord) {
	t.Helper()
	store := NewMemStorage()
	t.Cleanup(func() { _ = store.Close() })
	task, owner := recoveryFixtureStore(t, store, t.TempDir())
	task.Recovery = &TaskRecoveryRef{Version: 4}
	record := owner.record
	record.Version, record.Generation = 4, 1
	record.InitialAttempt = recoveryInitialAttempt{State: "issued", Generation: 1}
	return task, record
}
func sharedTypeCoverage(t *testing.T, record *recoveryRecord) {
	t.Helper()
	coverage, err := transfer.NewHTTPCoverageState(strings.Repeat("a", 64), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	coverage.DurableRanges = []transfer.HTTPCoverageRange{{Begin: 0, End: 2048}}
	coverage.SettledRanges = []transfer.HTTPCoverageRange{{Begin: 0, End: 1024}}
	coverage.CheckpointHash = strings.Repeat("b", 64)
	coverage.SettlementSequence = 7
	record.Coverage = &coverage
}
func sharedTypeActions(record *recoveryRecord, kinds ...recoveryActionKind) {
	record.Actions = make([]recoveryAction, len(kinds))
	record.TotalUsed, record.TransportUsed, record.RefreshUsed, record.DeferralUsed = len(kinds), 0, 0, 0
	record.NextAction = uint64(len(kinds))
	for i, kind := range kinds {
		record.Actions[i] = recoveryAction{ID: uint64(i + 1), Kind: string(kind), Generation: record.Generation, SourceRevision: record.SourceRevision, Members: []string{"work"}, Finished: []string{}, State: "charged"}
		switch kind {
		case recoveryTransportAction:
			record.TransportUsed++
		case recoveryRefreshAction:
			record.RefreshUsed++
		case recoveryDeferralAction:
			record.DeferralUsed++
			record.NotBefore = "2027-01-01T00:00:00Z"
		}
	}
}
func TestSharedRecoveryV4TripletVariants(t *testing.T) {
	for _, tc := range []struct {
		name, cap string
		change    func(*recoveryRecord)
		valid     bool
	}{
		{"plain", recoverySharedPlainCapability, nil, true},
		{"g0-ungranted", recoverySharedPlainCapability, func(r *recoveryRecord) {
			r.Generation = 0
			r.InitialAttempt = recoveryInitialAttempt{State: "ungranted"}
		}, true},
		{"g0-uncertain", recoverySharedPlainCapability, func(r *recoveryRecord) {
			r.Generation = 0
			r.InitialAttempt = recoveryInitialAttempt{State: "uncertain"}
		}, true},
		{"mixed-kinds", recoverySharedPlainCapability, func(r *recoveryRecord) {
			sharedTypeActions(r, recoveryTransportAction, recoveryRefreshAction, recoveryDeferralAction)
		}, true},
		{"coverage", recoverySharedCoverageCapability, func(r *recoveryRecord) { sharedTypeCoverage(t, r) }, true},
		{"unprovable-history", recoverySharedCoverageCapability, func(r *recoveryRecord) {
			sharedTypeCoverage(t, r)
			r.Coverage.Status = "unprovable"
			r.LastExplicitRetry = &recoveryExplicitRetry{Version: 1, ActionID: "history", FromEpisode: "old", ToEpisode: "historical", SourceRevision: 1}
		}, true},
		{"unknown-cap", "unknown", nil, false},
		{"plain-with-coverage", recoverySharedPlainCapability, func(r *recoveryRecord) { sharedTypeCoverage(t, r) }, false},
		{"coverage-missing", recoverySharedCoverageCapability, nil, false},
		{"coverage-nil-range", recoverySharedCoverageCapability, func(r *recoveryRecord) { sharedTypeCoverage(t, r); r.Coverage.SettledRanges = nil }, false},
		{"coverage-g0", recoverySharedCoverageCapability, func(r *recoveryRecord) {
			sharedTypeCoverage(t, r)
			r.Generation = 0
			r.InitialAttempt = recoveryInitialAttempt{State: "ungranted"}
		}, false},
		{"g0-until", recoverySharedPlainCapability, func(r *recoveryRecord) {
			r.Generation = 0
			r.InitialAttempt = recoveryInitialAttempt{State: "uncertain"}
			r.NotBefore = "2027-01-01T00:00:00Z"
		}, false},
		{"g0-proof", recoverySharedPlainCapability, func(r *recoveryRecord) {
			r.Generation = 0
			r.InitialAttempt = recoveryInitialAttempt{State: "ungranted", CheckpointHash: strings.Repeat("c", 64)}
		}, false},
		{"noncanonical-until", recoverySharedPlainCapability, func(r *recoveryRecord) { r.NotBefore = "2027-01-01T00:00:00+00:00" }, false},
		{"zero-until", recoverySharedPlainCapability, func(r *recoveryRecord) { r.NotBefore = "0001-01-01T00:00:00Z" }, false},
		{"unknown-kind", recoverySharedPlainCapability, func(r *recoveryRecord) { sharedTypeActions(r, recoveryTransportAction); r.Actions[0].Kind = "unknown" }, false},
		{"sum-mismatch", recoverySharedPlainCapability, func(r *recoveryRecord) { sharedTypeActions(r, recoveryRefreshAction); r.TransportUsed = 1 }, false},
		{"refresh-limit", recoverySharedPlainCapability, func(r *recoveryRecord) {
			sharedTypeActions(r, recoveryRefreshAction, recoveryRefreshAction, recoveryRefreshAction, recoveryRefreshAction)
		}, false},
		{"deferral-limit", recoverySharedPlainCapability, func(r *recoveryRecord) {
			kinds := make([]recoveryActionKind, 11)
			for i := range kinds {
				kinds[i] = recoveryDeferralAction
			}
			sharedTypeActions(r, kinds...)
		}, false},
		{"total-limit", recoverySharedPlainCapability, func(r *recoveryRecord) {
			kinds := []recoveryActionKind{}
			for i := 0; i < 6; i++ {
				kinds = append(kinds, recoveryTransportAction)
			}
			for i := 0; i < 3; i++ {
				kinds = append(kinds, recoveryRefreshAction)
			}
			for i := 0; i < 8; i++ {
				kinds = append(kinds, recoveryDeferralAction)
			}
			sharedTypeActions(r, kinds...)
		}, false},
		{"deferral-until-missing", recoverySharedPlainCapability, func(r *recoveryRecord) { sharedTypeActions(r, recoveryDeferralAction); r.NotBefore = "" }, false},
		{"deferral-partial-finished", recoverySharedPlainCapability, func(r *recoveryRecord) {
			sharedTypeActions(r, recoveryDeferralAction)
			r.Actions[0].Members = []string{"a", "b"}
			r.Actions[0].Finished = []string{"a"}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, record := sharedTypeFixture(t)
			if tc.change != nil {
				tc.change(&record)
			}
			before := coverageRetryCopy(t, record)
			required := recoveryRequirement{Version: 4, Capability: tc.cap}
			err := validateRecoveryTriplet(task, record, required)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if !tc.valid && !errors.Is(err, transfer.ErrRecoveryHeld) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, coverageRetryCopy(t, record)) {
				t.Fatal("validation mutated record")
			}
			if tc.valid {
				got, err := recoveryRequirementFor(record)
				if err != nil || got != required {
					t.Fatal("requirement variant mismatch", got, err)
				}
				task.Recovery.Version = 3
				if validateRecoveryTriplet(task, record, required) == nil {
					t.Fatal("mixed Task version accepted")
				}
			}
		})
	}
}
func TestSharedRecoveryV4G0ReadKeepsCurrentCheckpointWithoutPermission(t *testing.T) {
	store := NewMemStorage()
	defer store.Close()
	task, owner := recoveryFixtureStore(t, store, t.TempDir())
	checkpoint := map[string]any{"offset": json.Number("9007199254740993")}
	if err := store.Put(bucketSave, task.ID, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := InitializeDatasetIdentity(store); err != nil {
		t.Fatal(err)
	}
	identity, err := (&Downloader{storage: store}).DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	readCheckpoint := func() json.RawMessage {
		t.Helper()
		var raw json.RawMessage
		var present bool
		err := withDatasetReadView(store, identity.DatasetID, func(reader datasetReader) error {
			var err error
			present, err = reader.Get(bucketSave, task.ID, &raw)
			return err
		})
		if err != nil || !present {
			t.Fatal("checkpoint missing", err)
		}
		return raw
	}
	before := readCheckpoint()
	record := owner.record
	record.Version = 4
	task.Recovery = &TaskRecoveryRef{Version: 4}
	required, err := recoveryRequirementFor(record)
	if err != nil || validateRecoveryTriplet(task, record, required) != nil {
		t.Fatal("legal G0 declaration rejected", err)
	}
	digest, err := checkpointDigest(before)
	if err != nil {
		t.Fatal(err)
	}
	expected := recoverySharedExpected{Token: transfer.RecoveryToken{TaskID: task.ID, Episode: record.Episode, Generation: 0, SourceRevision: record.SourceRevision}, Record: record, Required: required, CheckpointPresent: true, CheckpointHash: digest}
	if expected.Token.Generation != 0 || !expected.CheckpointPresent || expected.CheckpointHash == "" {
		t.Fatal("lost G0/checkpoint coordinate")
	}
	// Handwritten rows cover only strict read semantics, never installation or IO.
	// Actual G0 upgrade/begin with original/no RAM is tested through real new M.
	if err = store.UpdateAtomic(func(tx StorageTransaction) error {
		if e := tx.Put(bucketTask, task.ID, task); e != nil {
			return e
		}
		if e := tx.Put(bucketTaskRecovery, task.ID, &record); e != nil {
			return e
		}
		return tx.Put(bucketRecoveryRequired, task.ID, &required)
	}); err != nil {
		t.Fatal(err)
	}
	priorActive, priorInitial, priorContinuation := owner.active.Load(), owner.sharedInitialEligible.Load(), owner.sharedContinuation.Load()
	if owned, read, readErr := readRecovery(store, task.ID); readErr != nil || owned.Recovery == nil || owned.Recovery.Version != 4 || !reflect.DeepEqual(read, record) {
		t.Fatal("strict v4 read lost legal G0", readErr)
	}
	if owner.active.Load() != priorActive || owner.sharedContinuation.Load() != priorContinuation || owner.sharedInitialEligible.Load() != priorInitial {
		t.Fatal("reading a declaration changed existing process qualification")
	}
	after := readCheckpoint()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("checkpoint changed")
	}
}
func TestSharedRecoveryV4LegacyVariantsUnchanged(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		task, record := sharedTypeFixture(t)
		record.Version = version
		task.Recovery.Version = version
		if version >= 2 {
			record.LastExplicitRetry = &recoveryExplicitRetry{Version: 1, ActionID: "legacy", FromEpisode: "before", ToEpisode: record.Episode, SourceRevision: record.SourceRevision}
		}
		if version == 3 {
			sharedTypeCoverage(t, &record)
		}
		required, err := recoveryRequirementFor(record)
		if err != nil || required != (recoveryRequirement{version, recoveryCapability}) || validateRecoveryTriplet(task, record, required) != nil {
			t.Fatal("legacy variant changed", version, err)
		}
	}
}
