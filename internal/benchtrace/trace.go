//go:build speedbench

// Package benchtrace records payload milestones for isolated speed trials.
package benchtrace

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

var Enabled = output != ""

type span struct{ start, end int64 }

type timeline struct {
	First     int64 `json:"firstNs,omitempty"`
	P90       int64 `json:"p90Ns,omitempty"`
	Last      int64 `json:"lastNs,omitempty"`
	Retries   int   `json:"retries"`
	Deferrals int   `json:"deferrals"`
	spans     []span
	bytes     int64
}

var output = os.Getenv("KOWNLOAD_SPEEDBENCH_TRACE")
var recorder = struct {
	sync.Mutex
	transfers map[string]*timeline
}{transfers: make(map[string]*timeline)}

// Payload counts unique offsets from accepted responses, including the prefix
// read by prefetch. Repeated offsets from recovery never advance the threshold.
func Payload(url string, size, offset int64, n int) {
	if output == "" || n == 0 {
		return
	}
	recorder.Lock()
	defer recorder.Unlock()
	now := time.Now().UnixNano()
	t := transferTimeline(url)
	if t.First == 0 {
		t.First = now
	}
	t.Last = now
	if t.P90 != 0 || size <= 0 {
		return
	}

	merged := span{offset, offset + int64(n)}
	first, last := 0, 0
	for first < len(t.spans) && t.spans[first].end < merged.start {
		first++
	}
	for last = first; last < len(t.spans) && t.spans[last].start <= merged.end; last++ {
		old := t.spans[last]
		t.bytes -= old.end - old.start
		merged.start = min(merged.start, old.start)
		merged.end = max(merged.end, old.end)
	}
	if first == last {
		t.spans = append(t.spans, span{})
		copy(t.spans[first+1:], t.spans[first:])
	} else {
		copy(t.spans[first+1:], t.spans[last:])
		t.spans = t.spans[:len(t.spans)-(last-first-1)]
	}
	t.spans[first] = merged
	t.bytes += merged.end - merged.start
	if t.bytes >= size-size/10 {
		t.P90 = now
		t.spans = nil
	}
}

func transferTimeline(raw string) *timeline {
	t := recorder.transfers[raw]
	if t == nil {
		t = &timeline{}
		recorder.transfers[raw] = t
	}
	return t
}

func Retry(raw string) {
	if !Enabled {
		return
	}
	recorder.Lock()
	defer recorder.Unlock()
	transferTimeline(raw).Retries++
}

func Deferral(raw string) {
	if !Enabled {
		return
	}
	recorder.Lock()
	defer recorder.Unlock()
	transferTimeline(raw).Deferrals++
}

// Budget records local fixture admission milestones in a sidecar trace. The
// proxy identity and source key never leave memory.
func Budget(source, kind string, at, until time.Time) {
	if !Enabled {
		return
	}
	origin, ok := strings.CutPrefix(source, "origin|")
	if !ok {
		return
	}
	origin, _, _ = strings.Cut(origin, "|")
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
		return
	}
	event := struct {
		Kind  string `json:"kind"`
		Port  string `json:"port"`
		At    int64  `json:"atNs"`
		Until int64  `json:"untilNs,omitempty"`
	}{Kind: kind, Port: parsed.Port(), At: at.UnixNano()}
	if !until.IsZero() {
		event.Until = until.UnixNano()
	}
	appendEvent(output+".budget", event)
}

// Terminal records the completion notification after core status publication.
// Only whitelisted synthetic fixture names are written to the trace.
func Terminal(raw, status, reason string) {
	if !Enabled {
		return
	}
	at := time.Now().UnixNano()
	recorder.Lock()
	defer recorder.Unlock()
	t := transferTimeline(raw)
	delete(recorder.transfers, raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() != "127.0.0.1" || parsed.User != nil {
		return
	}
	name, ok := strings.CutPrefix(parsed.Path, "/f/")
	if !ok || name == "" || strings.IndexFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || strings.ContainsRune("._-", r))
	}) >= 0 {
		return
	}
	appendEvent(output, struct {
		*timeline
		Name     string `json:"name"`
		Status   string `json:"status"`
		Reason   string `json:"reason,omitempty"`
		Terminal int64  `json:"terminalNs"`
	}{t, name, status, reason, at})
}

func appendEvent(path string, event any) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(event)
}
