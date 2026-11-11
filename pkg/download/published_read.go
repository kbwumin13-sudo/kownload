package download

// All four methods require the caller to hold Task.dataLock (read lock is
// sufficient for the snapshot). They never acquire another lock or perform IO.
// Run/source validity and observation timing belong to the calling producer.
func (t *Task) mcpReadPublishedSnapshotLocked() MCPReadPublishedSnapshot {
	return t.mcpPublished
}

func (t *Task) publishReadStatsLocked(v MCPReadPublishedStats) error {
	for _, counter := range []struct {
		known bool
		value int64
	}{
		{v.ActiveStreamsKnown, v.ActiveStreams},
		{v.TargetStreamsKnown, v.TargetStreams},
		{v.PeersKnown, v.Peers},
	} {
		if counter.known && (counter.value < 0 || counter.value > mcpReadMaxInteger) {
			t.mcpPublished.Stats = MCPReadPublishedStats{}
			return ErrMCPReadUnavailable
		}
	}
	// Replace the observation, including its Known flags. Never keep stale values
	// from an earlier sample or retain a payload for an unobserved field.
	if !v.ActiveStreamsKnown {
		v.ActiveStreams = 0
	}
	if !v.TargetStreamsKnown {
		v.TargetStreams = 0
	}
	if !v.PeersKnown {
		v.Peers = 0
	}
	t.mcpPublished.Stats = v
	return nil
}

func (t *Task) publishReadErrorLocked(v MCPReadPublishedError) error {
	if !v.Known {
		t.mcpPublished.Error = MCPReadPublishedError{}
		return nil
	}
	validCategory := false
	switch v.Category {
	case "network", "authentication", "integrity", "storage", "cancelled", "unknown":
		validCategory = true
	}
	if !validCategory || v.OccurredAtMillis < 1 || v.OccurredAtMillis > 253402300799999 {
		t.mcpPublished.Error = MCPReadPublishedError{}
		return ErrMCPReadUnavailable
	}
	// Retryable is already classified by the producer; no state-based inference.
	t.mcpPublished.Error = v
	return nil
}

func (t *Task) clearReadPublishedLocked() {
	t.mcpPublished = MCPReadPublishedSnapshot{}
}
