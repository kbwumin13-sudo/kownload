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
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
)

type verifiedGatedReader struct {
	*bytes.Reader
	ctx     context.Context
	entered chan struct{}
	release <-chan struct{}
	once    *sync.Once
}

func (r *verifiedGatedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.release:
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
	return r.Reader.Read(p)
}

func TestVerifiedHTTPFrozenServerTimeAndForeignFinalPreservation(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "time-policy", true: "foreign-final"}[foreign], func(t *testing.T) {
			body := bytes.Repeat([]byte("pinned output policy\n"), 4096)
			sum := sha256.Sum256(body)
			expected := hex.EncodeToString(sum[:])
			serverTime := time.Unix(1600000100, 0).UTC()
			entered, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"verified-policy-source"`)
				if r.Header.Get("Range") != "" {
					http.ServeContent(w, r, "policy.bin", serverTime, &verifiedGatedReader{bytes.NewReader(body), r.Context(), entered, release, &once})
					return
				}
				http.ServeContent(w, r, "policy.bin", serverTime, bytes.NewReader(body))
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
			d.configSnapshotMu.Lock()
			d.cfg.ProtocolConfig["http"] = map[string]any{"connections": 1, "useServerCtime": true}
			d.configSnapshotMu.Unlock()
			request := operationRequest(source.URL+"/payload", dir)
			request.Reqs[0].Opts.Name = "policy.bin"
			request.Reqs[0].Opts.Extra = map[string]any{"connections": 1, "autoTorrent": false, "autoExtract": false, "verifiedUseServerCtime": false}
			request.integrity = &VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", expected}
			items, hash, err := canonicalBatch(request)
			if err != nil {
				t.Fatal(err)
			}
			if err = d.reserveOperationInDataset(store, request, items, hash, identity.DatasetID); err != nil {
				t.Fatal(err)
			}
			// Same raw R must retain the effective original true policy even after
			// global false; a caller Extra key cannot override the frozen bool.
			d.configSnapshotMu.Lock()
			d.cfg.ProtocolConfig["http"] = map[string]any{"connections": 1, "useServerCtime": false}
			d.configSnapshotMu.Unlock()
			events, unsubscribe, err := d.SubscribeVerifiedHTTPDone(context.Background(), identity.DatasetID)
			if err != nil {
				t.Fatal(err)
			}
			defer unsubscribe()
			result, err := d.createBatchOperationInDataset(context.Background(), request, identity.DatasetID)
			if err != nil || len(result.TaskIDs) != 1 {
				t.Fatal(err, result)
			}
			id := result.TaskIDs[0]
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("range did not reach pinned writer gate")
			}
			_, record, err := d.readVerifiedTask(identity.DatasetID, id)
			if err != nil || record.StagingIdentity.Inode == 0 {
				t.Fatal("IO preceded anchor ACK", err)
			}
			var before os.FileInfo
			if foreign {
				if err = os.WriteFile(filepath.Join(dir, "policy.bin"), []byte("foreign complete output"), 0600); err != nil {
					t.Fatal(err)
				}
				before, err = os.Stat(filepath.Join(dir, "policy.bin"))
				if err != nil {
					t.Fatal(err)
				}
			}
			unblock()
			want := base.DownloadStatusDone
			if foreign {
				want = base.DownloadStatusError
			}
			waitStatus(t, d, id, want, 5*time.Second)
			data, err := os.ReadFile(filepath.Join(dir, "policy.bin"))
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(dir, "policy.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if foreign {
				if string(data) != "foreign complete output" || !os.SameFile(before, info) || !before.ModTime().Equal(info.ModTime()) {
					t.Fatal("foreign target bytes/inode/time changed")
				}
				select {
				case e := <-events:
					t.Fatal("conflict emitted success", e)
				default:
				}
			} else {
				if !bytes.Equal(data, body) || !info.ModTime().Equal(serverTime) {
					t.Fatal("R-frozen fd time or complete bytes lost", info.ModTime())
				}
				select {
				case <-events:
				case <-time.After(time.Second):
					t.Fatal("fresh C event missing")
				}
			}
		})
	}
}
