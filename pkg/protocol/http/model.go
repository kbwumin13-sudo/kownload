package http

import "time"

type ReqExtra struct {
	Method string            `json:"method"`
	Header map[string]string `json:"header"`
	Body   string            `json:"body"`
}

type OptsExtra struct {
	Connections int `json:"connections"`
	// AutoTorrent when task download complete, and it is a .torrent file, it will be auto create a new task for the torrent file
	// nil means use global config, true/false means explicit setting
	AutoTorrent *bool `json:"autoTorrent"`
	// DeleteTorrentAfterDownload when true, deletes the .torrent file after creating BT task
	// nil means use global config, true/false means explicit setting
	DeleteTorrentAfterDownload *bool `json:"deleteTorrentAfterDownload"`
	// AutoExtract when task download complete, and it is an archive file, it will be auto extracted
	// nil means use global config, true/false means explicit setting
	AutoExtract *bool `json:"autoExtract"`
	// ArchivePassword is the password for extracting password-protected archives
	ArchivePassword string `json:"archivePassword"`
	// DeleteAfterExtract when true, deletes the archive file after successful extraction
	DeleteAfterExtract bool `json:"deleteAfterExtract"`
}

// Stats for download
type Stats struct {
	Connections []*StatsConnection `json:"connections"`

	// Diagnostics below were added in core 1.9.3-kownload.5; older cores omit
	// them, so readers must treat every field as optional.

	// Phase: resolving, waiting, connecting, transferring, cooldown, paused, done or error.
	Phase string `json:"phase,omitempty"`
	// RangeState: declared, probed, resumed, unsupported, probeFailed or notProbed.
	RangeState  string `json:"rangeState,omitempty"`
	RangeReason string `json:"rangeReason,omitempty"`
	// ActiveStreams are requests transferring or connecting now; ConfiguredStreams
	// is the task's limit; TargetStreams is the limit currently granted by the
	// source budget (equal to ConfiguredStreams unless rate limits or adaptive
	// validation lowered it). Each stream uses its own connection.
	ActiveStreams     int `json:"activeStreams"`
	ConfiguredStreams int `json:"configuredStreams,omitempty"`
	TargetStreams     int `json:"targetStreams,omitempty"`
	// Protocol is the HTTP version of the latest response, e.g. HTTP/1.1 or HTTP/2.0.
	Protocol          string `json:"protocol,omitempty"`
	NewConnections    int64  `json:"newConnections"`
	ReusedConnections int64  `json:"reusedConnections"`
	Requests          int64  `json:"requests"`
	RateLimits        int64  `json:"rateLimits,omitempty"`
	// CooldownUntil is set while the source asked clients to wait (429/503).
	CooldownUntil *time.Time     `json:"cooldownUntil,omitempty"`
	Adaptive      *StatsAdaptive `json:"adaptive,omitempty"`
	Timings       *StatsTimings  `json:"timings,omitempty"`
	// LastError is a short, credential-free description of the latest failure.
	LastError string `json:"lastError,omitempty"`
}

// StatsTimings splits a download's time into stages. Stages can overlap
// (several streams connect while others transfer), so they are not summed.
type StatsTimings struct {
	QueueMs     int64 `json:"queueMs"`     // waiting for a running slot
	ResolveMs   int64 `json:"resolveMs"`   // first request until response headers
	DNSMs       int64 `json:"dnsMs"`       // DNS lookups for new connections, summed
	ConnectMs   int64 `json:"connectMs"`   // TCP connects for new connections, summed
	TLSMs       int64 `json:"tlsMs"`       // TLS handshakes for new connections, summed
	FirstByteMs int64 `json:"firstByteMs"` // run start until the first payload byte
	TransferMs  int64 `json:"transferMs"`  // first to latest payload byte of the run
	CooldownMs  int64 `json:"cooldownMs"`  // waiting for server rate limits
}

// StatsAdaptive describes throughput validation of the stream count for the
// download's source.
type StatsAdaptive struct {
	Target      int     `json:"target"`
	Level       int     `json:"level"`
	Phase       string  `json:"phase"`
	Rate        float64 `json:"rate"`
	Experiments int     `json:"experiments"`
}

type StatsConnection struct {
	Downloaded int64 `json:"downloaded"`
	Completed  bool  `json:"completed"`
	Failed     bool  `json:"failed"`
	RetryTimes int   `json:"retryTimes"`
}
