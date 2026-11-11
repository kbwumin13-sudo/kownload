package download

import (
	"context"
	"time"
)

// MCPReadPublishedStats is a detached, memory-only observation. Known flags
// distinguish an observed zero from a protocol that has not published a value.
// These types do not add Task persistence, sampling, or a publishing API.
type MCPReadPublishedStats struct {
	ActiveStreamsKnown bool
	ActiveStreams      int64
	TargetStreamsKnown bool
	TargetStreams      int64
	PeersKnown         bool
	Peers              int64
}

// MCPReadPublishedError contains only the accepted current-run classification.
// The producer owns run/source guards, invalidation and retry policy. It must
// never put raw error text into Category or infer OccurredAtMillis from a task
// modification time. Known=false means no observed event, not error-free history.
type MCPReadPublishedError struct {
	Known            bool
	Category         string
	Retryable        bool
	OccurredAtMillis int64
}

type MCPReadPublishedSnapshot struct {
	Stats MCPReadPublishedStats
	Error MCPReadPublishedError
}

func mcpReadPublishedSnapshot(ctx context.Context, task *Task) (MCPReadPublishedSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return MCPReadPublishedSnapshot{}, err
	}
	if !task.dataLock.TryRLock() {
		return MCPReadPublishedSnapshot{}, ErrMCPReadBusy
	}
	value := mcpReadPublishedValueLocked(task)
	task.dataLock.RUnlock()
	if err := ctx.Err(); err != nil {
		return MCPReadPublishedSnapshot{}, err
	}
	return value, nil
}

// Caller holds Task.dataLock. The optional producer method may only copy owned
// scalars; it must not relock, acquire main/status/owner locks, or call fetchers.
// Until that producer is implemented, this deliberately returns no observations.
func mcpReadPublishedValueLocked(source any) MCPReadPublishedSnapshot {
	if published, ok := source.(interface {
		mcpReadPublishedSnapshotLocked() MCPReadPublishedSnapshot
	}); ok {
		return published.mcpReadPublishedSnapshotLocked()
	}
	return MCPReadPublishedSnapshot{}
}

// Conversion happens after releasing the task lock. Pointers in the wire DTO
// refer to these copied values, never to Task or a protocol-owned object.
func mcpReadStatsDTO(value MCPReadPublishedStats) (MCPReadStats, error) {
	for _, counter := range []struct {
		known bool
		value int64
	}{
		{value.ActiveStreamsKnown, value.ActiveStreams},
		{value.TargetStreamsKnown, value.TargetStreams},
		{value.PeersKnown, value.Peers},
	} {
		if counter.known && (counter.value < 0 || counter.value > mcpReadMaxInteger) {
			return MCPReadStats{}, ErrMCPReadUnavailable
		}
	}
	result := MCPReadStats{Available: value.ActiveStreamsKnown || value.TargetStreamsKnown || value.PeersKnown}
	if value.ActiveStreamsKnown {
		result.ActiveStreams = &value.ActiveStreams
	}
	if value.TargetStreamsKnown {
		result.TargetStreams = &value.TargetStreams
	}
	if value.PeersKnown {
		result.Peers = &value.Peers
	}
	return result, nil
}

func mcpReadErrorsDTO(value MCPReadPublishedError) (MCPReadErrors, error) {
	if !value.Known {
		return MCPReadErrors{}, nil
	}
	switch value.Category {
	case "network", "authentication", "integrity", "storage", "cancelled", "unknown":
	default:
		return MCPReadErrors{}, ErrMCPReadUnavailable
	}
	// Last millisecond of RFC3339 year 9999; also below JS's safe integer bound.
	if value.OccurredAtMillis < 1 || value.OccurredAtMillis > 253402300799999 {
		return MCPReadErrors{}, ErrMCPReadUnavailable
	}
	return MCPReadErrors{
		Available: true, Category: value.Category, Retryable: &value.Retryable,
		OccurredAt: time.UnixMilli(value.OccurredAtMillis).UTC().Format(time.RFC3339Nano),
	}, nil
}
