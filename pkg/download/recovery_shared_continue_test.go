package download

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// Both inputs are produced by the actual writer and reopened normally. The
// first Task has a durable deferral, the second is independently runnable;
// neither a schema version nor a RAM permit is injected by this fixture.
func TestSharedRecoveryColdContinueCollectionsQueueCooldown(t *testing.T) {
	for _, mode := range []string{"all", "batch"} {
		t.Run(mode, func(t *testing.T) {
			payload := bytes.Repeat([]byte("collection-peer"), 4096)
			var coldRequests, peerRequests atomic.Int32
			var allowPeer atomic.Bool
			cold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				coldRequests.Add(1)
				if r.Header.Get("Range") == "" {
					w.Header().Set("Accept-Ranges", "bytes")
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					return
				}
				w.Header().Set("Retry-After", "10")
				w.WriteHeader(503)
			}))
			defer cold.Close()
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				peerRequests.Add(1)
				w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				if !allowPeer.Load() {
					<-r.Context().Done()
					return
				}
				_, _ = w.Write(payload)
			}))
			defer peer.Close()
			dir := t.TempDir()
			d := coverageRuntimeDownloader(t, true, dir)
			defer d.Close()
			d.lock.Lock()
			d.cfg.MaxRunning = 1
			d.lock.Unlock()
			request := operationRequest(cold.URL+"/cold", dir)
			request.OperationID = "collection-cold-" + mode
			request.Reqs[0].Opts.Name = "cold.bin"
			request.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 1}
			created, err := d.CreateBatchOperation(context.Background(), request)
			if err != nil || len(created.TaskIDs) != 1 {
				t.Fatalf("create cold: %+v %v", created, err)
			}
			coldID := created.TaskIDs[0]
			before := coverageRuntimeRecord(t, d, coldID, func(r recoveryRecord) bool { return r.DeferralUsed == 1 })
			peerID, err := d.CreateDirect(&base.Request{URL: peer.URL + "/ready"}, &base.Options{
				Path: dir, Name: "peer.bin", Extra: &httpModel.OptsExtra{Connections: 1},
			})
			if err != nil {
				t.Fatal(err)
			}
			// A queued peer makes the original long-wait policy yield its slot.
			waitStatus(t, d, coldID, base.DownloadStatusWait, 3*time.Second)
			if err := d.Pause(nil); err != nil {
				t.Fatal(err)
			}
			d.Close() // Includes real pause/Store ACK and all original workers.
			allowPeer.Store(true)
			d2 := operationDownloader(t, NewBoltStorage(filepath.Join(dir, "db")), dir)
			defer d2.Close()
			before = coverageRuntimeRecord(t, d2, coldID, func(r recoveryRecord) bool { return r.DeferralUsed == 1 })
			loaded := d2.taskList()
			if len(loaded) != 2 || loaded[0].ID != coldID || loaded[1].ID != peerID || loaded[0].getStatus() != base.DownloadStatusPause {
				t.Fatal("cold startup ordering/state did not establish the collection precondition")
			}
			until, err := parseRecoveryUntil(before.NotBefore)
			if err != nil || time.Until(until) < 3*time.Second {
				t.Fatalf("cooldown fixture already expired: %v %v", until, err)
			}
			coldBaseline, peerBaseline := coldRequests.Load(), peerRequests.Load()
			if mode == "all" {
				err = d2.Continue(nil)
			} else {
				err = d2.ContinueBatch(&TaskFilter{IDs: []string{coldID, peerID}})
			}
			if err != nil {
				t.Fatalf("collection rejected a legitimate cooldown instead of queuing it: %v; cold=%s peer=%s", err, d2.GetTask(coldID).Status, d2.GetTask(peerID).Status)
			}
			waitStatus(t, d2, peerID, base.DownloadStatusDone, 2*time.Second)
			coldTask := d2.getTask(coldID)
			coldTask.lock.Lock()
			unrestored := coldTask.fetcher == nil && coldTask.recoveryOwner.Load() == nil
			coldTask.lock.Unlock()
			if !time.Now().Before(until) || coldTask.getStatus() != base.DownloadStatusWait || !unrestored || coldRequests.Load() != coldBaseline || peerRequests.Load() <= peerBaseline {
				t.Fatal("cooldown collection restored/ran early or skipped its runnable peer")
			}
			after := coverageRuntimeRecord(t, d2, coldID, func(r recoveryRecord) bool { return r.DeferralUsed == 1 })
			if !reflect.DeepEqual(before, after) {
				t.Fatal("queuing a cold task changed its budget, identity or Until")
			}
			actual, err := os.ReadFile(d2.GetTask(peerID).Meta.SingleFilepath())
			if err != nil || !bytes.Equal(actual, payload) {
				t.Fatal("runnable collection peer did not preserve full bytes", err)
			}
		})
	}
}
