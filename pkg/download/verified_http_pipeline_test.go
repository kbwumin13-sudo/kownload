package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestVerifiedHTTPRealCoreMatchAndMismatch(t *testing.T) {
	for _, match := range []bool{true, false} {
		t.Run(map[bool]string{true: "match", false: "mismatch"}[match], func(t *testing.T) {
			payload := bytes.Repeat([]byte("verified HTTP artifact\n"), 4096)
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"verified-fixed-resource"`)
				http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(payload))
			}))
			defer source.Close()
			dir := t.TempDir()
			store := NewBoltStorage(filepath.Join(dir, "db"))
			d := operationDownloader(t, store, dir)
			defer d.Close()
			if err := InitializeDatasetIdentity(store); err != nil {
				t.Fatal(err)
			}
			identity, err := d.DatasetIdentity()
			if err != nil {
				t.Fatal(err)
			}
			events, release, err := d.SubscribeVerifiedHTTPDone(context.Background(), identity.DatasetID)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			var successes, starts atomic.Int32
			var completionBeforeStart atomic.Bool
			d.Listener(func(e *Event) {
				if e.Key == EventKeyStart {
					starts.Add(1)
				}
				if e.Key == EventKeyDone {
					completionBeforeStart.Store(starts.Load() != 1)
					successes.Add(1)
				}
			})
			digest := sha256.Sum256(payload)
			expected := hex.EncodeToString(digest[:])
			if !match {
				expected = strings.Repeat("a", 64)
			}
			request := operationRequest(source.URL+"/payload", dir)
			request.Reqs[0].Opts.Name = "verified.bin"
			request.Reqs[0].Opts.Extra = map[string]any{"connections": 1, "autoTorrent": false, "autoExtract": false}
			request.integrity = &VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", expected}
			result, err := d.createBatchOperationInDataset(context.Background(), request, identity.DatasetID)
			if err != nil || len(result.TaskIDs) != 1 {
				t.Fatal("create", err, result)
			}
			id := result.TaskIDs[0]
			want := base.DownloadStatusDone
			if !match {
				want = base.DownloadStatusError
			}
			waitStatus(t, d, id, want, 8*time.Second)
			owned, record, err := d.readVerifiedTask(identity.DatasetID, id)
			if err != nil {
				t.Fatal("authoritative completion", err)
			}
			stage, err := os.ReadFile(filepath.Join(dir, record.StagingName))
			if err != nil || !bytes.Equal(stage, payload) {
				t.Fatal("stage not preserved", err)
			}
			if match {
				if record.Phase != transfer.CompletionCommitted || record.ActualSHA256 != expected || owned.Status != base.DownloadStatusDone {
					t.Fatal("unconfirmed done", record.Phase)
				}
				final, err := os.ReadFile(filepath.Join(dir, "verified.bin"))
				if err != nil || !bytes.Equal(final, payload) {
					t.Fatal("final body", err)
				}
				select {
				case event := <-events:
					if event.TaskID != id || event.CompletionID != record.ID {
						t.Fatal(event)
					}
				case <-time.After(time.Second):
					t.Fatal("fresh C had no live event")
				}
				deadline := time.Now().Add(time.Second)
				for successes.Load() != 1 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if successes.Load() != 1 || starts.Load() != 1 || completionBeforeStart.Load() {
					t.Fatal("Start must precede the single success exit", starts.Load(), successes.Load(), completionBeforeStart.Load())
				}
			} else {
				if record.Phase == transfer.CompletionCommitted || record.ActualSHA256 != "" || owned.Status == base.DownloadStatusDone || successes.Load() != 0 {
					t.Fatal("mismatch succeeded", record)
				}
				if _, err := os.Stat(filepath.Join(dir, "verified.bin")); !os.IsNotExist(err) {
					t.Fatal("mismatch published final", err)
				}
				select {
				case event := <-events:
					t.Fatal("mismatch emitted success", event)
				default:
				}
			}
		})
	}
}
