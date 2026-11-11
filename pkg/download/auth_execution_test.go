package download

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

func TestAuthBatchFirstPostRedirectDownloads(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusSeeOther} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			payload := bytes.Repeat([]byte("batch-first-post"), 1<<17)
			var posts, ranges atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != "GET" || len(body) != 0 {
					t.Error("target received source method/body")
				}
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("ETag", `"first-post"`)
				if r.Header.Get("Range") == "" {
					w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
					_, _ = w.Write(payload[:4096])
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				ranges.Add(1)
				http.ServeContent(w, r, "batch.bin", time.Time{}, bytes.NewReader(payload))
			}))
			defer target.Close()
			defer target.CloseClientConnections()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != "POST" || string(body) != "first-form" {
					t.Error("initial request changed")
				}
				posts.Add(1)
				http.Redirect(w, r, target.URL, status)
			}))
			defer source.Close()
			dir := t.TempDir()
			d := operationDownloader(t, NewBoltStorage(filepath.Join(dir, "db")), dir)
			defer d.Close()
			request := operationRequest(source.URL, dir)
			request.Reqs[0].Req.Extra = &httpModel.ReqExtra{Method: "POST", Body: "first-form"}
			request.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections: 2}
			result := requireOperation(t, d, request)
			if len(result.TaskIDs) != 1 {
				t.Fatal("missing same-operation task")
			}
			id := result.TaskIDs[0]
			waitStatus(t, d, id, base.DownloadStatusDone, 3*time.Second)
			content, err := os.ReadFile(d.GetTask(id).Meta.SingleFilepath())
			if err != nil || !bytes.Equal(content, payload) || posts.Load() != 1 || ranges.Load() == 0 {
				t.Fatalf("incorrect full download: err=%v posts=%d ranges=%d", err, posts.Load(), ranges.Load())
			}
		})
	}
}

func TestAuthUnresolvedStoredBatchDoesNotReplayBody(t *testing.T) {
	for _, method := range []string{"POST", "GET"} {
		t.Run(method, func(t *testing.T) {
			var sends atomic.Int32
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				_, _ = w.Write([]byte("must not be fetched"))
			}))
			defer source.Close()
			dir := t.TempDir()
			dbDir := filepath.Join(dir, "db")
			store := NewBoltStorage(dbDir)
			d := operationDownloader(t, store, dir)
			r := operationRequest(source.URL, dir)
			r.Reqs[0].Req.Extra = &httpModel.ReqExtra{Method: method, Body: "unproved-form"}
			items, hash, err := canonicalBatch(r)
			if err != nil {
				t.Fatal(err)
			}
			if err = d.reserveOperation(store, r, items, hash); err != nil {
				t.Fatal(err)
			}
			// Persist a real M transaction, then stop before Q/first execution.
			if _, err = d.createOperationMember(store, r.OperationID, hash, 0, items[0]); err != nil {
				t.Fatal(err)
			}
			record, err := d.loadOperation(r.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			id := record.Members[0].TaskID
			if err = d.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := operationDownloader(t, NewBoltStorage(dbDir), dir)
			defer reopened.Close()
			if reopened.GetTask(id).Meta.Res != nil {
				t.Fatal("fixture already resolved")
			}
			if err = reopened.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
				t.Fatal(err)
			}
			waitStatus(t, reopened, id, base.DownloadStatusError, 3*time.Second)
			if sends.Load() != 0 {
				t.Fatalf("historical unresolved request was sent %d times", sends.Load())
			}
			// An explicit same-op POST reconciles metadata, not first-I/O authority.
			if _, err = reopened.CreateBatchOperation(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			if sends.Load() != 0 {
				t.Fatal("operation replay restored first-I/O authority")
			}
		})
	}
}

func TestAuthBatchUnknownCommitDoesNotAuthorizePost(t *testing.T) {
	var sends atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		_, _ = w.Write([]byte("must not be fetched"))
	}))
	defer source.Close()
	dir := t.TempDir()
	store := &operationFaultStorage{AtomicStorage: NewMemStorage(), phase: "ack-member"}
	d := operationDownloader(t, store, dir)
	defer d.Close()
	r := operationRequest(source.URL, dir)
	r.Reqs[0].Req.Extra = &httpModel.ReqExtra{Method: "POST", Body: "unproved-form"}
	result, err := d.CreateBatchOperation(context.Background(), r)
	if err != ErrOperationUnavailable || len(result.TaskIDs) != 1 {
		t.Fatalf("unknown ACK lost durable receipt: %v %+v", err, result)
	}
	waitStatus(t, d, result.TaskIDs[0], base.DownloadStatusError, 3*time.Second)
	if _, err = d.CreateBatchOperation(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 0 {
		t.Fatal("unknown M ACK or replay granted source POST authority")
	}
}
