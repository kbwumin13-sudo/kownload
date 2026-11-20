//go:build speedbench

// Package benchtrace records payload milestones for isolated speed trials.
package benchtrace

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

var Enabled = output != ""

type span struct{ start, end int64 }

type timeline struct {
	First int64 `json:"firstNs,omitempty"`
	P90   int64 `json:"p90Ns,omitempty"`
	Last  int64 `json:"lastNs,omitempty"`
	spans []span
	bytes int64
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
	t := recorder.transfers[url]
	if t == nil {
		t = &timeline{First: now}
		recorder.transfers[url] = t
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

// Terminal records the completion notification after core status publication.
// The URL stays in memory. The runner correlates rows using the unique fixture name.
func Terminal(url, status string) {
	if output == "" || url == "" {
		return
	}
	now := time.Now().UnixNano()
	recorder.Lock()
	defer recorder.Unlock()
	t := recorder.transfers[url]
	delete(recorder.transfers, url)
	if t == nil {
		t = &timeline{}
	}
	name := url
	for i, c := range name {
		if c == '?' {
			name = name[:i]
			break
		}
	}
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			name = name[i+1:]
			break
		}
	}
	f, err := os.OpenFile(output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(struct {
		*timeline
		Name     string `json:"name"`
		Status   string `json:"status"`
		Terminal int64  `json:"terminalNs"`
	}{t, name, status, now})
}
