package rest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"go.etcd.io/bbolt"
)

type datasetReply struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func datasetServer(t *testing.T, kind model.Storage, token string) (*http.Server, string, *download.Downloader, string) {
	t.Helper()
	previousCore, previousServer, previousAES, previousPort := Downloader, srv, aesKey, runningPort
	// BuildServer publishes legacy globals even when this fixture serves its
	// private listener directly. Restore them after closing only our instances.
	t.Cleanup(func() { Downloader, srv, aesKey, runningPort = previousCore, previousServer, previousAES, previousPort })
	dir := t.TempDir()
	server, listener, err := BuildServer(&model.StartConfig{Storage: kind, StorageDir: dir, ApiToken: token})
	if err != nil {
		t.Fatal(err)
	}
	core := Downloader
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close(); _ = core.Close() })
	return server, "http://" + listener.Addr().String(), core, dir
}

func datasetGET(t *testing.T, endpoint, token string) (int, datasetReply) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("X-Api-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result datasetReply
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, result
}

func TestDatasetCatalogRESTAuthReadOnlyAndExistingOperation(t *testing.T) {
	for _, kind := range []model.Storage{model.StorageMem, model.StorageBolt} {
		t.Run(string(kind), func(t *testing.T) {
			server, url, core, dir := datasetServer(t, kind, "synthetic-dataset-token")
			for _, path := range []string{"/api/v1/dataset/identity", "/api/v1/task-batches/operations"} {
				for _, token := range []string{"", "wrong-synthetic-token"} {
					if status, _ := datasetGET(t, url+path, token); status != http.StatusUnauthorized {
						t.Fatal("auth bypass", status)
					}
				}
			}
			status, reply := datasetGET(t, url+"/api/v1/dataset/identity", "synthetic-dataset-token")
			var identity download.DatasetIdentity
			if err := json.Unmarshal(reply.Data, &identity); err != nil {
				t.Fatal(err)
			}
			if status != 200 || reply.Code != 0 || identity.Version != 1 || identity.Persistent != (kind == model.StorageBolt) {
				t.Fatalf("identity: %+v", reply)
			}
			var hits atomic.Int32
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = io.WriteString(w, "must-not-fetch") }))
			defer source.Close()
			// The existing S2 cancelled-request path durably reserves a manifest
			// without a Task or network resolution. No new write API is introduced.
			body := fmt.Sprintf(`{"version":1,"operationId":"catalog-pending","reqs":[{"req":{"url":%q},"opts":{"path":%q,"extra":{"Cookie":"synthetic-private-secret"}}}]}`, source.URL, t.TempDir())
			request := httptest.NewRequest("POST", "/api/v1/task-batches/operations", strings.NewReader(body))
			request.Header.Set("X-Api-Token", "synthetic-dataset-token")
			ctx, cancel := context.WithCancel(request.Context())
			cancel()
			request = request.WithContext(ctx)
			recorder := httptest.NewRecorder()
			server.Handler.ServeHTTP(recorder, request)
			if recorder.Code != 200 || len(core.GetTasks()) != 0 || hits.Load() != 0 {
				t.Fatalf("fixture unexpectedly executed: %d %d", recorder.Code, hits.Load())
			}
			_, beforeConfig := datasetGET(t, url+"/api/v1/config", "synthetic-dataset-token")
			var dbBefore []byte
			if kind == model.StorageBolt {
				dbBefore, _ = os.ReadFile(filepath.Join(dir, "gopeed.db"))
			}
			for i := 0; i < 3; i++ {
				status, result := datasetGET(t, url+"/api/v1/task-batches/operations?filter=pending&limit=1", "synthetic-dataset-token")
				var page download.BatchCatalogPage
				if err := json.Unmarshal(result.Data, &page); err != nil {
					t.Fatal(err)
				}
				if status != 200 || result.Code != 0 || page.DatasetID != identity.DatasetID || len(page.Items) != 1 || page.Items[0].OperationID != "catalog-pending" || page.Items[0].State != "pending" {
					t.Fatalf("catalog: %+v %s", page, result.Data)
				}
				if bytes.Contains(result.Data, []byte("synthetic-private-secret")) || bytes.Contains(result.Data, []byte(source.URL)) {
					t.Fatal("catalog leaked request")
				}
				if status, result := datasetGET(t, url+"/api/v1/task-batches/operations/catalog-pending", "synthetic-dataset-token"); status != 200 || !bytes.Contains(result.Data, []byte(`"state":"pending"`)) {
					t.Fatalf("single operation query changed: %d %s", status, result.Data)
				}
				if status, result := datasetGET(t, url+"/api/v1/dataset/identity", "synthetic-dataset-token"); status != 200 || !bytes.Contains(result.Data, []byte(identity.DatasetID)) {
					t.Fatal("identity changed")
				}
			}
			_, afterConfig := datasetGET(t, url+"/api/v1/config", "synthetic-dataset-token")
			if !bytes.Equal(beforeConfig.Data, afterConfig.Data) || hits.Load() != 0 || len(core.GetTasks()) != 0 {
				t.Fatal("GET performed task/protocol/config work")
			}
			if kind == model.StorageBolt {
				after, _ := os.ReadFile(filepath.Join(dir, "gopeed.db"))
				if sha256.Sum256(dbBefore) != sha256.Sum256(after) {
					t.Fatal("REST reads wrote database")
				}
			}
			for _, query := range []string{"?limit=0", "?limit=101", "?limit=x", "?limit=1&limit=2", "?filter=other", "?filter=", "?cursor=!", "?cursor=a&cursor=b"} {
				if status, _ := datasetGET(t, url+"/api/v1/task-batches/operations"+query, "synthetic-dataset-token"); status != 400 {
					t.Fatal("invalid query accepted", query, status)
				}
			}
		})
	}
}

func TestDatasetIdentityRESTInvalidStartupPreservesRunningInstance(t *testing.T) {
	_, url, existing, _ := datasetServer(t, model.StorageBolt, "")
	_, first := datasetGET(t, url+"/api/v1/dataset/identity", "")
	dir := t.TempDir()
	db, err := bbolt.Open(filepath.Join(dir, "gopeed.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"version":2,"datasetId":"07070707070707070707070707070707"}`)
	if err := db.Update(func(tx *bbolt.Tx) error {
		b, e := tx.CreateBucketIfNotExists([]byte("config"))
		if e != nil {
			return e
		}
		return b.Put([]byte("dataset_identity_v1"), raw)
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	server, listener, err := BuildServer(&model.StartConfig{Storage: model.StorageBolt, StorageDir: dir})
	if err == nil || server != nil || listener != nil || Downloader != existing {
		t.Fatal("bad identity reached/published Setup", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
		t.Fatal("Downloader/logger initialized after identity rejection")
	}
	status, after := datasetGET(t, url+"/api/v1/dataset/identity", "")
	if status != 200 || !bytes.Equal(first.Data, after.Data) {
		t.Fatal("failed startup closed or replaced running storage")
	}
	// Reopening proves the failed candidate released only its own DB lock.
	db, err = bbolt.Open(filepath.Join(dir, "gopeed.db"), 0600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *bbolt.Tx) error {
		if !bytes.Equal(tx.Bucket([]byte("config")).Get([]byte("dataset_identity_v1")), raw) {
			t.Fatal("future record changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDatasetCatalogRESTRemainsBoundToItsCore(t *testing.T) {
	_, firstURL, _, _ := datasetServer(t, model.StorageMem, "")
	_, first := datasetGET(t, firstURL+"/api/v1/dataset/identity", "")
	_, secondURL, _, _ := datasetServer(t, model.StorageMem, "")
	_, second := datasetGET(t, secondURL+"/api/v1/dataset/identity", "")
	_, repeat := datasetGET(t, firstURL+"/api/v1/dataset/identity", "")
	if bytes.Equal(first.Data, second.Data) || !bytes.Equal(first.Data, repeat.Data) {
		t.Fatal("GET label followed another server's global Downloader")
	}
}

func TestDatasetFixtureRestoresLegacyStart(t *testing.T) {
	previousCore, previousServer, previousAES, previousPort := Downloader, srv, aesKey, runningPort
	Downloader, srv, aesKey, runningPort = nil, nil, nil, 0
	t.Cleanup(func() { Downloader, srv, aesKey, runningPort = previousCore, previousServer, previousAES, previousPort })
	t.Run("private-server", func(t *testing.T) {
		_, url, _, _ := datasetServer(t, model.StorageMem, "")
		if status, _ := datasetGET(t, url+"/api/v1/dataset/identity", ""); status != 200 {
			t.Fatal("private server not ready")
		}
	})
	t.Cleanup(Stop)
	port, err := Start(&model.StartConfig{Storage: model.StorageMem, StorageDir: t.TempDir()})
	if err != nil || port == 0 {
		t.Fatalf("legacy Start after fixture cleanup: port=%d err=%v", port, err)
	}
	if status, response := datasetGET(t, fmt.Sprintf("http://127.0.0.1:%d/api/v1/info", port), ""); status != 200 || response.Code != 0 {
		t.Fatal("legacy Start did not publish a usable listener")
	}
}
