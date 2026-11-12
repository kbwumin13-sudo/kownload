package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestMCPCompletedCheckpointRestoresAndSyncsBoundFD(t *testing.T) {
	payload := rangeProbePayload(32 * 1024)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(payload))
	}))
	defer server.Close()
	stage := filepath.Join(t.TempDir(), "staging")
	first, err := os.OpenFile(stage, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f := buildFetcher()
	f.RequireMCPOutput()
	if err = f.SetVerifiedOutput(first); err != nil {
		first.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Pause() })
	if err = f.Resolve(&base.Request{URL: server.URL}, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	if err = f.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-f.doneCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial real HTTP download did not complete")
	}
	if err = f.JoinVerifiedWriters(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = f.MCPOutputSyncResult(); err != nil {
		t.Fatal(err)
	}
	if err = f.Pause(); err != nil {
		t.Fatal(err)
	}
	manager := &FetcherManager{}
	checkpoint, err := manager.Store(f)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	metaBytes, err := json.Marshal(f.Meta())
	if err != nil {
		t.Fatal(err)
	}
	var meta fetcher.FetcherMeta
	if err = json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatal(err)
	}
	saved, restore := manager.Restore()
	if err = json.Unmarshal(encoded, saved); err != nil {
		t.Fatal(err)
	}
	resumed := restore(&meta, saved).(*Fetcher)
	resumed.Setup(buildFetcher().ctl)
	resumed.RequireMCPOutput()
	t.Cleanup(func() { _ = resumed.Pause() })
	reopened, err := os.OpenFile(stage, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = resumed.SetVerifiedOutput(reopened); err != nil {
		reopened.Close()
		t.Fatal(err)
	}
	if err = resumed.MCPOutputSyncResult(); !errors.Is(err, ErrMCPOutputSync) {
		t.Fatal("new FD inherited old Sync", err)
	}
	// Replacing the pathname must not redirect local completion or alter a
	// foreign file; the reopened descriptor remains pinned to the full staging.
	owned := stage + ".owned"
	if err = os.Rename(stage, owned); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("foreign path must remain unchanged")
	if err = os.WriteFile(stage, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	before := requests.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wait := resumed.BindRunWait(ctx)
	if err = resumed.Start(); err != nil {
		t.Fatal(err)
	}
	if err = wait(); err != nil {
		t.Fatal(err)
	}
	if err = resumed.JoinVerifiedWriters(ctx); err != nil {
		t.Fatal(err)
	}
	if err = resumed.MCPOutputSyncResult(); err != nil {
		t.Fatal("complete restored staging was not synced", err)
	}
	if resumed.getState() != stateDone || requests.Load() != before {
		t.Fatal("local completion performed network or failed to finish")
	}
	if _, err = reopened.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("completed binding was not closed", err)
	}
	for name, want := range map[string][]byte{owned: payload, stage: foreign} {
		got, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("output changed: %s: %v", filepath.Base(name), err)
		}
	}
}
