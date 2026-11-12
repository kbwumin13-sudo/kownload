package download

import (
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

const (
	recoverySharedVersion            = 4
	recoverySharedPlainCapability    = "http-shared-recovery/v1"
	recoverySharedCoverageCapability = "http-shared-coverage-recovery/v1"
	recoveryMaxRefresh               = 3
	recoveryMaxDeferral              = 10
)

type recoveryActionKind string

const (
	recoveryTransportAction recoveryActionKind = "transport"
	recoveryRefreshAction   recoveryActionKind = "refresh"
	recoveryDeferralAction  recoveryActionKind = "deferral"
)

// Expected is an owned storage-CAS coordinate, never an execution token.
// A legal G0 coordinate does not require the current checkpoint to be absent.
type recoverySharedExpected struct {
	Token             transfer.RecoveryToken
	Record            recoveryRecord
	Required          recoveryRequirement
	CheckpointPresent bool
	CheckpointHash    string
}

// These fields describe a command, not additional persistent action fields.
// A stronger Until on replay requires explicit ExtendActionID, never a new grant.
type recoverySharedAction struct {
	Kind           recoveryActionKind
	Seen           uint64
	Work           string
	Members        []string
	Until          string
	ExtendActionID uint64
}

// parseRecoveryUntil validates representation only. Time elapsing grants no IO.
func parseRecoveryUntil(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	until, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || until.IsZero() || until.UTC().Format(time.RFC3339Nano) != raw {
		return time.Time{}, transfer.ErrRecoveryHeld
	}
	return until, nil
}

// validShared validates only the declared v4 shape. The existing runtime reader
// deliberately continues to reject v4 until its Until/action gates are wired.
func (r *recoveryRecord) validShared(id string) bool {
	if r == nil || r.Version != recoverySharedVersion || id == "" || r.TaskID != id ||
		r.Episode == "" || r.SourceRevision == 0 || !validRecoveryHash(r.SourceHash) || !validRecoveryHash(r.ExecutionDefaultsHash) ||
		r.TotalUsed < 0 || r.TotalUsed > recoveryMaxTotal || r.TransportUsed < 0 || r.TransportUsed > recoveryMaxTransport ||
		r.RefreshUsed < 0 || r.RefreshUsed > recoveryMaxRefresh || r.DeferralUsed < 0 || r.DeferralUsed > recoveryMaxDeferral ||
		len(r.Actions) != r.TotalUsed || r.NextAction != uint64(len(r.Actions)) ||
		r.TransportUsed+r.RefreshUsed+r.DeferralUsed != r.TotalUsed {
		return false
	}
	if _, err := parseRecoveryUntil(r.NotBefore); err != nil || r.DeferralUsed > 0 && r.NotBefore == "" {
		return false
	}
	if r.Coverage != nil && (r.Generation == 0 || transfer.ValidateHTTPCoverageState(*r.Coverage, recoveryCoverageRangeLimit) != nil) {
		return false
	}
	if r.Generation == 0 && (r.TotalUsed != 0 || r.LastExplicitRetry != nil || r.Coverage != nil || r.NotBefore != "") {
		return false
	}
	switch r.InitialAttempt.State {
	case "ungranted", "uncertain":
		if r.Generation != 0 || r.InitialAttempt.Generation != 0 || r.InitialAttempt.CheckpointHash != "" {
			return false
		}
	case "issued":
		if r.Generation == 0 || r.InitialAttempt.Generation != r.Generation || r.InitialAttempt.CheckpointHash != "" {
			return false
		}
	case "clean-paused":
		if r.Generation == 0 || r.InitialAttempt.Generation != r.Generation || !validRecoveryHash(r.InitialAttempt.CheckpointHash) {
			return false
		}
	default:
		return false
	}
	if last := r.LastExplicitRetry; last != nil {
		if last.Version != 1 || !operationIDPattern.MatchString(last.ActionID) || last.FromEpisode == "" || last.ToEpisode == "" ||
			last.FromEpisode == last.ToEpisode || last.SourceRevision == 0 || last.SourceRevision > r.SourceRevision {
			return false
		}
	}
	counts := map[recoveryActionKind]int{}
	for i, action := range r.Actions {
		kind := recoveryActionKind(action.Kind)
		switch kind {
		case recoveryTransportAction, recoveryRefreshAction, recoveryDeferralAction:
		default:
			return false
		}
		counts[kind]++
		if kind == recoveryDeferralAction && action.State == "charged" && len(action.Finished) != 0 {
			return false // next-wave admission closes this whole wait atomically
		}
		if action.ID != uint64(i+1) || action.Generation == 0 || action.Generation > r.Generation || action.SourceRevision == 0 ||
			action.SourceRevision > r.SourceRevision || (action.State != "charged" && action.State != "finished") {
			return false
		}
		members, err := recoveryMembers(action.Members)
		if err != nil || !sameRecoveryMembers(members, action.Members) {
			return false
		}
		seen := map[string]bool{}
		for _, work := range action.Finished {
			if seen[work] || !hasRecoveryMember(action.Members, work) {
				return false
			}
			seen[work] = true
		}
		if action.State == "finished" && len(seen) != len(action.Members) {
			return false
		}
	}
	return counts[recoveryTransportAction] == r.TransportUsed && counts[recoveryRefreshAction] == r.RefreshUsed && counts[recoveryDeferralAction] == r.DeferralUsed
}

func recoveryRequirementFor(record recoveryRecord) (recoveryRequirement, error) {
	if record.Version == recoverySharedVersion {
		if !record.validShared(record.TaskID) {
			return recoveryRequirement{}, transfer.ErrRecoveryHeld
		}
		cap := recoverySharedPlainCapability
		if record.Coverage != nil {
			cap = recoverySharedCoverageCapability
		}
		return recoveryRequirement{Version: recoverySharedVersion, Capability: cap}, nil
	}
	if !record.valid(record.TaskID) {
		return recoveryRequirement{}, transfer.ErrRecoveryHeld
	}
	return recoveryRequirement{Version: record.Version, Capability: recoveryCapability}, nil
}

// This declaration validator performs no reads, enrollment, migration or grants.
func validateRecoveryTriplet(task *Task, record recoveryRecord, required recoveryRequirement) error {
	expected, err := recoveryRequirementFor(record)
	if err != nil || task == nil || task.ID != record.TaskID || task.Protocol != "http" || task.Meta == nil || task.Meta.Req == nil ||
		task.Recovery == nil || task.Recovery.Version != record.Version || required != expected {
		return transfer.ErrRecoveryHeld
	}
	hash, err := recoverySourceHash(task.Meta.Req, record.ExecutionDefaultsHash)
	if err != nil || hash != record.SourceHash {
		return transfer.ErrRecoveryHeld
	}
	return nil
}
