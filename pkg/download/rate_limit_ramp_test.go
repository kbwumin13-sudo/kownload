package download

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func TestTokenLimitedBatchRecoveryPreservesFilesAndPeerSlot(t *testing.T) {
	payload := rateLimitPayload(2 * 1024 * 1024)
	var mu sync.Mutex
	var firstRefusal time.Time
	var lastAccepted time.Time
	var starts []time.Time
	var requestCount int
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		mu.Lock()
		requestCount++
		refuse := requestCount <= 2 || now.Sub(lastAccepted) < 200*time.Millisecond
		if refuse && firstRefusal.IsZero() {
			firstRefusal = now
		}
		if !refuse {
			starts = append(starts, now)
			lastAccepted = now
		}
		status := http.StatusTooManyRequests
		if requestCount%2 == 0 {
			status = http.StatusServiceUnavailable
		}
		mu.Unlock()
		if refuse {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(status)
			return
		}
		http.ServeContent(w, r, "payload.bin", time.Unix(1700000000, 0), bytes.NewReader(payload))
	}))
	defer source.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload[:32768])
	}))
	defer peer.Close()

	d := newRateLimitDownloader(t, 2)
	defer d.Close()
	dir := t.TempDir()
	create := func(url, name string) string {
		t.Helper()
		id, err := d.CreateDirect(&base.Request{URL: url}, &base.Options{
			Path: dir, Name: name, Extra: &httpModel.OptsExtra{Connections: 4},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	x := create(source.URL+"/x", "x.bin")
	y := create(source.URL+"/y", "y.bin")
	peerID := create(peer.URL+"/peer", "peer.bin")
	waitStatus(t, d, peerID, base.DownloadStatusDone, 3*time.Second)
	mu.Lock()
	acceptedWhenPeerFinished := len(starts)
	mu.Unlock()
	if acceptedWhenPeerFinished != 0 {
		t.Fatal("independent peer did not take a slot during the initial cooldown")
	}

	waitStatus(t, d, x, base.DownloadStatusDone, 20*time.Second)
	waitStatus(t, d, y, base.DownloadStatusDone, 20*time.Second)
	for name, want := range map[string][]byte{"x.bin": payload, "y.bin": payload, "peer.bin": payload[:32768]} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || sha256.Sum256(data) != sha256.Sum256(want) {
			t.Fatalf("%s payload mismatch: %v", name, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) == 0 || starts[0].Sub(firstRefusal) < 2*time.Second {
		t.Fatalf("first accepted request bypassed the server's wait: %v", starts)
	}
}
