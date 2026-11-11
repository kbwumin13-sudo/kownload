package download

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func TestExplicitRecoveryRetryRealHTTPPreservesTaskAndFile(t *testing.T) {
	payload := bytes.Repeat([]byte("explicit retry payload"), 1<<15)
	var unavailable atomic.Bool
	unavailable.Store(true)
	var ranges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"explicit-retry-same-resource"`)
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			_, _ = w.Write(payload[:4096])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		ranges.Add(1)
		if unavailable.Load() {
			w.WriteHeader(500)
			return
		}
		http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(payload))
	}))
	defer server.Close()
	dir := t.TempDir()
	d := operationDownloader(t, NewBoltStorage(filepath.Join(dir, "db")), dir)
	defer d.Close()
	if err := InitializeDatasetIdentity(d.storage); err != nil {
		t.Fatal(err)
	}
	var dataset datasetRecord
	if _, err := d.storage.Get(bucketConfig, datasetIdentityKey, &dataset); err != nil {
		t.Fatal(err)
	}
	id, err := d.CreateDirect(&base.Request{URL: server.URL + "/payload"}, &base.Options{Path: dir, Name: "same-file.bin", Extra: &httpModel.OptsExtra{Connections: 1}})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, id, base.DownloadStatusError, 15*time.Second)
	before, err := d.QueryRecoveryForDataset(dataset.DatasetID, id)
	if err != nil || before.TransportUsed != recoveryMaxTransport {
		t.Fatal("original episode not exhausted", err, before)
	}
	// Public error can precede the failed-run persistence ACK. Retry is
	// allowed only by the authoritative stored state, never that transient UI.
	for end := time.Now().Add(2 * time.Second); !before.RetryAllowed && time.Now().Before(end); {
		time.Sleep(time.Millisecond)
		before, err = d.QueryRecoveryForDataset(dataset.DatasetID, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !before.RetryAllowed {
		t.Fatal("error was not durably retryable", before)
	}
	oldTask := d.GetTask(id)
	unavailable.Store(false)
	request := RecoveryRetryRequest{1, "user-retry-real", before.Episode, before.SourceRevision, "retry"}
	accepted, err := d.RetryRecoveryForDataset(context.Background(), dataset.DatasetID, id, request)
	if err != nil || accepted.Episode == before.Episode {
		t.Fatal("explicit retry rejected", err, accepted)
	}
	waitStatus(t, d, id, base.DownloadStatusDone, 10*time.Second)
	got, err := os.ReadFile(filepath.Join(dir, "same-file.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("same path lost integrity", err)
	}
	newTask := d.GetTask(id)
	if newTask.ID != oldTask.ID || newTask.Meta.Opts.Path != oldTask.Meta.Opts.Path || newTask.Meta.Opts.Name != oldTask.Meta.Opts.Name {
		t.Fatal("retry replaced task or output path")
	}
	state, err := d.QueryRecoveryForDataset(dataset.DatasetID, id)
	if err != nil || state.TotalUsed != 0 || state.Episode != accepted.Episode {
		t.Fatal("fresh attempt charged or rebound", err, state)
	}
	calls := ranges.Load()
	if _, err = d.RetryRecoveryForDataset(context.Background(), dataset.DatasetID, id, request); err != nil {
		t.Fatal(err)
	}
	if ranges.Load() != calls {
		t.Fatal("receipt replay started network IO")
	}
}
