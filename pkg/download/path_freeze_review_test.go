package download

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func TestPathFreezeHTTPResponseMIMEAndReservedNames(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fallback, videos := filepath.Join(dir, "fallback"), filepath.Join(dir, "videos")
	for _, root := range []string{fallback, videos} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	payload := bytes.Repeat([]byte("same-name actual HTTP bytes\n"), 32768)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4; charset=binary")
		w.Header().Set("ETag", `"same-object"`)
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		http.ServeContent(w, r, "payload.bin", time.Unix(1700000000, 0), bytes.NewReader(payload))
	}))
	defer server.Close()
	d := operationDownloader(t, NewBoltStorage(filepath.Join(dir, "db")), dir)
	defer d.Close()
	cfg, _ := d.GetConfig()
	cfg.MaxRunning = 2
	cfg.Extra, err = pathpolicy.WithConfig(cfg.Extra, pathpolicy.Config{Enabled: true, Rules: []pathpolicy.Rule{{ID: "actual-video", Kind: pathpolicy.MIMERule, Match: pathpolicy.Match{MIME: "video/*"}, Root: videos}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PutConfig(cfg); err != nil {
		t.Fatal(err)
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	requests := make([]HTTPPathCreateRequest, 2)
	ids := make([]string, 2)
	for i, op := range []string{"mime-name-first", "mime-name-second"} {
		requests[i] = HTTPPathCreateRequest{Version: 1, OperationID: op, RequiredCapabilities: []string{HTTPPathRequiredCapability}, PathIntent: PathIntent{Version: 1, Path: fallback, Origin: pathpolicy.Default}, Request: &base.CreateTaskBatchItem{Req: &base.Request{URL: server.URL + "/payload.bin"}, Opts: &base.Options{Path: fallback, Name: "payload.bin", Extra: &httpModel.OptsExtra{Connections: 1}}}}
		result, err := d.CreatePathOperationForDataset(context.Background(), identity.DatasetID, HTTPPathCreateKind, requests[i])
		if err != nil || len(result.Operation.TaskIDs) != 1 {
			t.Fatalf("create %d: %+v %v", i, result, err)
		}
		ids[i] = result.Operation.TaskIDs[0]
	}
	frozen := make([]TaskPathFreeze, 2)
	for i, id := range ids {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			task := d.GetTask(id)
			if task.Status == base.DownloadStatusError {
				t.Fatalf("execution: %+v", task)
			}
			if task.PathFreeze != nil && task.PathFreeze.Phase == PathFreezeFrozen && task.PathFreeze.Output != nil && task.PathFreeze.Output.Phase == MCPWriteOutputStaging {
				frozen[i] = *task.PathFreeze
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if frozen[i].Decision == nil || frozen[i].Decision.RuleID != "actual-video" || frozen[i].Decision.Root != videos {
			t.Fatalf("actual response MIME not frozen: %+v", frozen[i])
		}
	}
	if frozen[0].Layout.Members[0].RelativePath == frozen[1].Layout.Members[0].RelativePath {
		t.Fatal("two incomplete operations reserved the same final name")
	}
	for _, state := range frozen {
		if _, err := os.Stat(filepath.Join(videos, state.Layout.Members[0].RelativePath)); !os.IsNotExist(err) {
			t.Fatalf("final existed before source released: %v", err)
		}
	}
	cfg.Extra, err = pathpolicy.WithConfig(cfg.Extra, pathpolicy.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PutConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for i, request := range requests {
		result, err := d.CreatePathOperationForDataset(context.Background(), identity.DatasetID, HTTPPathCreateKind, request)
		if err != nil || len(result.Operation.TaskIDs) != 1 || result.Operation.TaskIDs[0] != ids[i] || !reflect.DeepEqual(d.GetTask(ids[i]).PathFreeze, &frozen[i]) {
			t.Fatal("replay re-evaluated MIME or renamed frozen target", err)
		}
	}
	close(release)
	for i, id := range ids {
		waitStatus(t, d, id, base.DownloadStatusDone, 8*time.Second)
		body, err := os.ReadFile(filepath.Join(videos, frozen[i].Layout.Members[0].RelativePath))
		if err != nil || !bytes.Equal(body, payload) {
			t.Fatal("independent published bytes", err)
		}
	}
}
