package http

type config struct {
	UserAgent      string `json:"userAgent"`
	Connections    int    `json:"connections"`
	UseServerCtime bool   `json:"useServerCtime"`
	// Tuning holds internal switches for controlled comparisons. Absent fields
	// keep the defaults below; the settings UI does not expose them.
	Tuning *tuningConfig `json:"tuning,omitempty"`
}

type tuningConfig struct {
	SharedTransport *bool `json:"sharedTransport,omitempty"`
	HTTP2           *bool `json:"http2,omitempty"`
	Adaptive        *bool `json:"adaptive,omitempty"`
	Trace           *bool `json:"trace,omitempty"`
	ReadBuffer      int   `json:"readBuffer,omitempty"`
}

type tuning struct {
	sharedTransport bool // reuse connections through the downloader's pool
	http2           bool // negotiate HTTP/2 on pooled stream connections
	adaptive        bool // validate stream counts with source throughput
	trace           bool // collect connection timings for Stats
	readBuffer      int  // bytes per body read
}

// defaultTuning is what downloads use unless a comparison overrides it. The
// values follow the 2026-10-26 controlled comparisons recorded in
// artifacts/speed-optimization-2026-10-26.
var defaultTuning = tuning{
	sharedTransport: true,
	http2:           false,
	adaptive:        false,
	trace:           true,
	readBuffer:      64 << 10,
}

func (c *config) tuning() tuning {
	t := defaultTuning
	if c == nil || c.Tuning == nil {
		return t
	}
	set := func(dst *bool, src *bool) {
		if src != nil {
			*dst = *src
		}
	}
	set(&t.sharedTransport, c.Tuning.SharedTransport)
	set(&t.http2, c.Tuning.HTTP2)
	set(&t.adaptive, c.Tuning.Adaptive)
	set(&t.trace, c.Tuning.Trace)
	// Work stealing leaves at least 256 KiB to the victim; a read in flight
	// must stay below half of that so the split never overlaps it.
	if c.Tuning.ReadBuffer >= 4096 && c.Tuning.ReadBuffer <= 128<<10 {
		t.readBuffer = c.Tuning.ReadBuffer
	}
	return t
}
