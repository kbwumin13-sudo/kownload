package download

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

type authPatchStorage struct {
	*MemStorage
	fail atomic.Bool
}

func (s *authPatchStorage) Put(bucket, key string, v any) error {
	if bucket == bucketTask && s.fail.Load() {
		return errors.New("synthetic patch storage failure")
	}
	return s.MemStorage.Put(bucket, key, v)
}
func TestAuthPatchStorageFailureKeepsDurableRequestAndNeverStarts(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = w.Write([]byte("B")) }))
	defer server.Close()
	dir := t.TempDir()
	store := &authPatchStorage{MemStorage: NewMemStorage()}
	d := handoffDownloader(t, store, dir)
	defer d.Close()
	task := startupStoredTask("auth-patch-storage", dir, base.DownloadStatusPause)
	task.Meta.Req = &base.Request{URL: "https://source.example/file", Extra: &httpModel.ReqExtra{Header: map[string]string{"Authorization": "Bearer A", "Cookie": "A=one", "X-Private": "A"}}}
	if e := d.assignFetcherManager(task); e != nil {
		t.Fatal(e)
	}
	initTask(task)
	if e := store.Put(bucketTask, task.ID, task.clone()); e != nil {
		t.Fatal(e)
	}
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	store.fail.Store(true)
	e := d.Patch(task.ID, &base.Request{URL: server.URL + "/file"}, nil)
	store.fail.Store(false)
	if e == nil {
		t.Fatal("storage failure was hidden")
	}
	if hits.Load() != 0 || task.getStatus() != base.DownloadStatusPause {
		t.Fatal("failed persistence started request")
	}
	var durable Task
	if ok, e := store.Get(bucketTask, task.ID, &durable); e != nil || !ok {
		t.Fatal("lost durable task", e)
	}
	if durable.Meta.Req.URL != "https://source.example/file" {
		t.Fatal("failed save changed durable request")
	}
	if task.Meta.Req.URL != server.URL+"/file" {
		t.Fatal("memory boundary changed")
	}
	if e := base.ParseReqExtra[httpModel.ReqExtra](task.Meta.Req); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"Authorization", "Cookie", "X-Private"} {
		if task.Meta.Req.Extra.(*httpModel.ReqExtra).Header[name] != "" {
			t.Fatal("candidate B inherited A")
		}
	}
}
