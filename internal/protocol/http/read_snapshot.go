package http

import (
	"context"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// PublishedReadStats is an in-memory observation from the existing progress
// sample. It contains no URL, headers, file path or protocol/client pointer.
type PublishedReadStats struct {
	Context            context.Context
	Token              transfer.RecoveryToken
	Managed            bool
	Observed           bool
	ActiveStreams      int64
	TargetStreamsKnown bool
	TargetStreams      int64
}

func (f *Fetcher) publishReadObservation(active int64) {
	value := &PublishedReadStats{Context: f.boundRunContext(), Observed: true, ActiveStreams: active}
	if run := f.recovery.Load(); run != nil && run.lease != nil {
		value.Managed = true
		value.Token = run.lease.Token
	}
	if member := f.member.Load(); member != nil {
		snapshot := member.Snapshot()
		value.TargetStreamsKnown = true
		value.TargetStreams = int64(max(1, min(snapshot.Limit, snapshot.SourceCapacity-(snapshot.SourceActive-snapshot.Active))))
	}
	f.readPublished.Store(value)
}

// Pure copy: MCP never invokes this method directly, and this method never
// restores a protocol, samples Stats or performs IO.
func (f *Fetcher) PublishedReadSnapshot() PublishedReadStats {
	if value := f.readPublished.Load(); value != nil {
		return *value
	}
	return PublishedReadStats{}
}
