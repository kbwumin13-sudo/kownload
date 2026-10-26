package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func legacyPrefixFetcher(t *testing.T, url string, data []byte, prefix int64, modified time.Time) *Fetcher {
	t.Helper()
	opts := rangeProbeOptions(t)
	meta := &fetcher.FetcherMeta{Req: &base.Request{URL: url}, Opts: opts,
		Res: &base.Resource{Size: int64(len(data)), Files: []*base.FileInfo{{Name: "payload.bin", Size: int64(len(data)), Ctime: &modified}}}}
	if err := os.WriteFile(meta.SingleFilepath(), data[:prefix], 0600); err != nil {
		t.Fatal(err)
	}
	fd := &fetcherData{Connections: []*connection{{ID: 0, Role: roleResolve, State: connDownloading,
		Downloaded: prefix, Chunk: &chunk{Begin: 0, End: 0, Downloaded: prefix}}}}
	encoded, _ := json.Marshal(fd)
	fm := &FetcherManager{}
	value, restore := fm.Restore()
	if err := json.Unmarshal(encoded, value); err != nil {
		t.Fatal(err)
	}
	f := restore(meta, value).(*Fetcher)
	f.Setup(buildFetcher().ctl)
	t.Cleanup(func() { _ = f.Pause() })
	return f
}

func TestLegacyRangeResumePreservesPrefixAndExpands(t *testing.T) {
	data := rangeProbePayload(8 * 1024 * 1024)
	prefix := int64(1024 * 1024)
	modified := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	var mu sync.Mutex
	var ranges []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		if r.Header.Get("If-Range") != modified.Format(http.TimeFormat) {
			t.Error("missing saved validator")
		}
		http.ServeContent(&hiddenRangeWriter{ResponseWriter: w, delay: 3 * time.Millisecond}, r,
			"payload.bin", modified, bytes.NewReader(data))
	}))
	defer server.Close()
	f := legacyPrefixFetcher(t, server.URL, data, prefix, modified)
	// Real OnStart extensions round-trip request extras through JavaScript/JSON
	// after the fetcher manager has already restored the typed request.
	f.meta.Req.Extra = map[string]any{"method": "GET", "header": map[string]any{"X-Test": "extension"}}
	checkRangeProbeDownload(t, f, data)
	if !f.meta.Res.Range || len(f.connections) < 2 || f.Progress().TotalDownloaded() != int64(len(data)) {
		t.Fatalf("legacy prefix was not upgraded correctly: range=%v connections=%d", f.meta.Res.Range, len(f.connections))
	}
	mu.Lock()
	defer mu.Unlock()
	wantProbe := fmt.Sprintf("bytes=%d-%d", prefix-64*1024, prefix-1)
	if len(ranges) < 3 || ranges[0] != wantProbe {
		t.Fatalf("missing overlap probe/expansion: %v", ranges)
	}
	for _, value := range ranges[1:] {
		var begin, end int64
		if _, err := fmt.Sscanf(value, "bytes=%d-%d", &begin, &end); err != nil || begin < prefix {
			t.Fatalf("saved prefix was downloaded again: %q", value)
		}
	}
	stored, err := (&FetcherManager{}).Store(f)
	if err != nil || stored.(*fetcherData).RangeResumeValidator != modified.Format(http.TimeFormat) {
		t.Fatal("resume validator was not persisted")
	}
	encoded, _ := json.Marshal(stored)
	v, restore := (&FetcherManager{}).Restore()
	_ = json.Unmarshal(encoded, v)
	if restore(f.meta, v).(*Fetcher).rangeResumeValidator != f.rangeResumeValidator {
		t.Fatal("resume validator was not restored")
	}
}

func TestLegacyRangeResumeFailurePreservesPartialFile(t *testing.T) {
	data := rangeProbePayload(2 * 1024 * 1024)
	prefix := int64(128 * 1024)
	modified := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, mode := range []string{"ignored", "changed_date", "changed_overlap", "wrong_size", "missing_file"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "ignored" {
					w.WriteHeader(http.StatusOK)
					return
				}
				date := modified
				body := data
				if mode == "changed_date" {
					date = date.Add(time.Hour)
				}
				if mode == "changed_overlap" {
					body = append([]byte(nil), data...)
					body[prefix-1] ^= 1
				}
				if mode == "wrong_size" {
					body = body[:len(body)-1]
				}
				http.ServeContent(w, r, "payload.bin", date, bytes.NewReader(body))
			}))
			defer server.Close()
			f := legacyPrefixFetcher(t, server.URL, data, prefix, modified)
			if mode == "missing_file" {
				_ = os.Remove(f.meta.SingleFilepath())
			}
			if err := f.Start(); err == nil {
				t.Fatal("unverified legacy prefix was resumed")
			}
			if f.meta.Res.Range || f.connections[0].Downloaded != prefix {
				t.Fatal("failed verification changed saved state")
			}
			if mode != "missing_file" {
				got, err := os.ReadFile(f.meta.SingleFilepath())
				if err != nil || !bytes.Equal(got, data[:prefix]) {
					t.Fatal("failed verification changed partial file")
				}
			}
		})
	}
}

func TestLegacyRangeResumePauseStoreRestore(t *testing.T) {
	data := rangeProbePayload(12 * 1024 * 1024)
	prefix := int64(1024 * 1024)
	modified := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Range") != modified.Format(http.TimeFormat) {
			t.Error("restored worker lost validator")
		}
		http.ServeContent(&hiddenRangeWriter{ResponseWriter: w, delay: 8 * time.Millisecond}, r,
			"payload.bin", modified, bytes.NewReader(data))
	}))
	defer server.Close()
	f := legacyPrefixFetcher(t, server.URL, data, prefix, modified)
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for f.Progress().TotalDownloaded() < prefix+256*1024 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := f.Pause(); err != nil {
		t.Fatal(err)
	}
	paused := f.Progress().TotalDownloaded()
	if paused <= prefix || paused >= int64(len(data)) {
		t.Fatalf("not a partial resumed transfer: %d", paused)
	}
	fm := &FetcherManager{}
	snapshot, err := fm.Store(f)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(snapshot)
	metaBytes, _ := json.Marshal(f.meta)
	var meta fetcher.FetcherMeta
	_ = json.Unmarshal(metaBytes, &meta)
	v, restore := fm.Restore()
	_ = json.Unmarshal(encoded, v)
	resumed := restore(&meta, v).(*Fetcher)
	resumed.Setup(buildFetcher().ctl)
	defer resumed.Pause()
	if resumed.Progress().TotalDownloaded() != paused {
		t.Fatal("restore lost partial progress")
	}
	checkRangeProbeDownload(t, resumed, data)
}
