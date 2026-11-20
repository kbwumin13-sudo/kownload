package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type statsSnapshot struct {
	EpochUnixNs        int64            `json:"epochUnixNs"`
	Inflight           int64            `json:"inflight"`
	Requests           int64            `json:"requests"`
	BytesSent          int64            `json:"bytesSent"`
	DuplicateBytesSent int64            `json:"duplicateBytesSent"`
	StatusCounts       map[int]int64    `json:"statusCounts"`
	RetryAfterCounts   map[string]int64 `json:"retryAfterCounts"`
	RequestTimeline    []requestEvent   `json:"requestTimeline"`
}

func newTestFixture() *server {
	return &server{groups: map[string]*group{}, c: &counters{}, epoch: time.Now()}
}

func readStats(t *testing.T, s *server, suffix string) (statsSnapshot, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	s.stats(w, httptest.NewRequest(http.MethodGet, "/stats"+suffix, nil))
	var result statsSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result, w.Body.Bytes()
}

type heldWrite struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *heldWrite) Write(buf []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.ResponseRecorder.Write(buf)
}

func TestFixtureDrainAndAtomicReset(t *testing.T) {
	s := newTestFixture()
	w := &heldWrite{httptest.NewRecorder(), make(chan struct{}), make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.serveFile(w, httptest.NewRequest(http.MethodGet, "/f/file.bin?size=16", nil))
	}()
	<-w.entered
	defer func() {
		close(w.release)
		<-done
	}()
	live, _ := readStats(t, s, "?summary=1")
	if live.EpochUnixNs <= 0 || live.Inflight != 1 || live.Requests != 1 {
		t.Fatalf("expected one active request: %+v", live)
	}
	fixtureReset := httptest.NewRecorder()
	s.reset(fixtureReset, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if fixtureReset.Code != http.StatusConflict {
		t.Fatalf("active reset returned %d", fixtureReset.Code)
	}
	after, _ := readStats(t, s, "")
	if after.EpochUnixNs != live.EpochUnixNs || after.Inflight != 1 || after.Requests != 1 {
		t.Fatalf("rejected reset changed the trial: %+v", after)
	}
}

func TestFixtureCountsWrittenRangesAndRetryAfterWithoutCredentials(t *testing.T) {
	s := newTestFixture()
	tests := []struct {
		path, requestedRange string
		status               int
	}{
		{"/f/sample.bin?size=16&group=sample", "bytes=0-9", http.StatusPartialContent},
		{"/f/sample.bin?size=16&group=sample", "bytes=5-14", http.StatusPartialContent},
		{"/f/other.bin?size=16&group=sample", "bytes=5-14", http.StatusPartialContent},
		{"/f/sample.bin?size=16&group=seconds&limit=1&retry=2", "bytes=1-4", http.StatusTooManyRequests},
		{"/f/sample.bin?size=16&group=date&limit=1&status=503&retry=date:3", "bytes=1-4", http.StatusServiceUnavailable},
		{"/f/sample.bin?size=16&group=missing&limit=1&retry=none", "bytes=1-4", http.StatusTooManyRequests},
		{"/f/sample.bin?size=16&group=invalid&limit=1&retry=unsafe-retry-header", "bytes=1-4", http.StatusTooManyRequests},
		{"/f/tokens.bin?size=10&group=tokens&rps=0.05", "", http.StatusOK},
		{"/f/tokens.bin?size=10&group=tokens&rps=0.05", "bytes=1-4", http.StatusTooManyRequests},
	}
	for i, tc := range tests {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "http://private-user:private-password@localhost"+tc.path+"&token=private-url-token", nil)
		r.Header.Set("Range", tc.requestedRange)
		r.Header.Set("Cookie", "private-cookie")
		r.Header.Set("Authorization", "Bearer private-authorization")
		s.serveFile(w, r)
		if w.Code != tc.status {
			t.Fatalf("request %d returned %d, want %d", i+1, w.Code, tc.status)
		}
	}
	snapshot, raw := readStats(t, s, "")
	if bytes.Contains(raw, []byte("private-")) || bytes.Contains(raw, []byte("unsafe-retry-header")) {
		t.Fatal("trace contains the raw URL or header values")
	}
	if snapshot.Inflight != 0 || snapshot.Requests != int64(len(tests)) || snapshot.BytesSent != 40 || snapshot.DuplicateBytesSent != 5 {
		t.Fatalf("wrong finalized counters: %+v", snapshot)
	}
	if snapshot.StatusCounts[429] != 4 || snapshot.StatusCounts[503] != 1 ||
		snapshot.RetryAfterCounts["seconds"] != 2 || snapshot.RetryAfterCounts["date"] != 1 ||
		snapshot.RetryAfterCounts["missing"] != 1 || snapshot.RetryAfterCounts["invalid"] != 1 {
		t.Fatalf("wrong status or Retry-After counters: %+v", snapshot)
	}
	if len(snapshot.RequestTimeline) != len(tests) {
		t.Fatalf("expected %d request rows, got %d", len(tests), len(snapshot.RequestTimeline))
	}
	first, overlap, refused := snapshot.RequestTimeline[0], snapshot.RequestTimeline[1], snapshot.RequestTimeline[3]
	if first.ID != 1 || first.BodyStart == nil || *first.BodyStart != 0 ||
		overlap.RangeStart == nil || *overlap.RangeStart != 5 ||
		overlap.RangeEndExclusive == nil || *overlap.RangeEndExclusive != 15 || overlap.BytesSent != 10 || overlap.Task != "sample.bin" ||
		refused.RefusalSource != "finiteRange" || refused.RetryAfterWaitNs == nil ||
		*refused.RetryAfterWaitNs != int64(2*time.Second) ||
		snapshot.RequestTimeline[len(tests)-1].RefusalSource != "tokenBucket" {
		t.Fatalf("wrong per-request evidence: first=%+v overlap=%+v refused=%+v", first, overlap, refused)
	}
	for _, event := range snapshot.RequestTimeline {
		if event.StartNs > event.ResponseNs || event.ResponseNs > event.EndNs {
			t.Fatalf("request timeline out of order: %+v", event)
		}
	}
	w := httptest.NewRecorder()
	s.reset(w, httptest.NewRequest(http.MethodPost, "/reset", nil))
	reset, _ := readStats(t, s, "")
	if w.Code != http.StatusNoContent || reset.EpochUnixNs <= snapshot.EpochUnixNs ||
		reset.Inflight != 0 || reset.Requests != 0 || len(reset.RequestTimeline) != 0 {
		t.Fatalf("expected an empty new trial, reset status %d, stats %+v", w.Code, reset)
	}
}
