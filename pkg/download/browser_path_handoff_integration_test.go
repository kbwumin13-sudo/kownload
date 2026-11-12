package download

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestBrowserPathHandoffSHAActualCore(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "output")
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("browser typed path sha\n"), 4096)
	digest := sha256.Sum256(payload)
	var requests atomic.Int32
	var taskID atomic.Value
	var checked atomic.Bool
	var d *Downloader
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"browser-path-bytes"`)
		if r.Header.Get("Range") != "" {
			id, _ := taskID.Load().(string)
			identity, e := d.DatasetIdentity()
			if e == nil {
				e = withDatasetReadView(d.storage, identity.DatasetID, func(reader datasetReader) error {
					_, state, err := ReadPathFreeze(reader, id)
					if err != nil {
						return err
					}
					_, completion, err := readTaskCompletion(reader, id)
					if err != nil {
						return err
					}
					if state.Phase != PathFreezeFrozen || completion.StagingIdentity.Inode == 0 || completion.DirectoryIdentity.Inode == 0 {
						return ErrPathFreezeHeld
					}
					return nil
				})
			}
			if e != nil {
				t.Errorf("payload before F/staging ACK: %v", e)
				w.WriteHeader(500)
				return
			}
			checked.Store(true)
		}
		http.ServeContent(w, r, "handoff.bin", time.Time{}, bytes.NewReader(payload))
	}))
	defer server.Close()
	d = operationDownloader(t, NewBoltStorage(filepath.Join(dir, "db")), dir)
	defer d.Close()
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	r := handoffRequest(server.URL+"/file", root)
	r.OperationID = "browser-d6-sha-actual"
	r.DatasetID = identity.DatasetID
	caps := []string{HTTPPathRequiredCapability, verifiedHTTPCapability}
	r.PathCreate = &HTTPPathCreateRequest{Version: 1, OperationID: r.OperationID, RequiredCapabilities: caps, Request: &base.CreateTaskBatchItem{Req: r.Req, Opts: r.Opts}, PathIntent: PathIntent{Version: 1, Path: root, Origin: pathpolicy.UserSelected}, Integrity: &VerifiedHTTPIntegrityIntent{Version: 1, RequiredCapability: verifiedHTTPCapability, Algorithm: "sha256", ExpectedSHA256: hex.EncodeToString(digest[:])}}
	prepared, err := d.BrowserHandoff(r)
	if err != nil || prepared.State != "prepared" || prepared.PathCreate == nil || !reflect.DeepEqual(prepared.PathCreate.RequiredCapabilities, caps) || len(prepared.TaskIDs) != 1 {
		t.Fatalf("prepare: %+v %v", prepared, err)
	}
	taskID.Store(prepared.TaskID)
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 || requests.Load() != 0 {
		t.Fatalf("prepared performed IO: entries=%d requests=%d err=%v", len(entries), requests.Load(), err)
	}
	if d.GetTask(prepared.TaskID).Status != base.DownloadStatusPause {
		t.Fatal("prepared was queued")
	}
	query := r
	query.Action, query.PathCreate, query.DatasetID = "query", nil, ""
	history, err := d.BrowserHandoff(query)
	if err != nil || history.TaskID != prepared.TaskID || history.PathCreate == nil || history.PathCreate.DatasetID != identity.DatasetID || requests.Load() != 0 {
		t.Fatalf("read-only prepared query: %+v %v", history, err)
	}
	commit := commitRequest(r, prepared.TaskID)
	commit.PathCreate = nil
	accepted, err := d.BrowserHandoff(commit)
	if err != nil || accepted.State != "committed" {
		t.Fatalf("commit: %+v %v", accepted, err)
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		task := d.GetTask(prepared.TaskID)
		if task.Status == base.DownloadStatusDone {
			break
		}
		if task.Status == base.DownloadStatusError {
			t.Fatalf("actual SHA/F failed: %+v", task)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if task := d.GetTask(prepared.TaskID); task.Status != base.DownloadStatusDone {
		t.Fatalf("no Done: %+v", task)
	}
	actual, err := os.ReadFile(filepath.Join(root, "handoff.bin"))
	if err != nil || sha256.Sum256(actual) != digest || !checked.Load() {
		t.Fatalf("full published SHA: len=%d checked=%v err=%v", len(actual), checked.Load(), err)
	}
	_, completion, err := d.readVerifiedTask(identity.DatasetID, prepared.TaskID)
	if err != nil || completion.Phase != transfer.CompletionCommitted {
		t.Fatalf("C: %+v %v", completion, err)
	}
	before := requests.Load()
	replayed, err := d.BrowserHandoff(r)
	if err != nil || replayed.TaskID != prepared.TaskID || requests.Load() != before {
		t.Fatalf("same-op replay: %+v %v", replayed, err)
	}
}
