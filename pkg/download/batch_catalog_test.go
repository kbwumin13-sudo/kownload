package download

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/base"
	"go.etcd.io/bbolt"
)

func catalogStore(t *testing.T, bolt bool) (*Downloader, Storage) {
	t.Helper()
	s := datasetStore(t, bolt)
	if err := InitializeDatasetIdentity(s); err != nil {
		t.Fatal(err)
	}
	if err := s.Setup([]string{bucketTaskOperations, bucketOperationTasks, bucketTask}); err != nil {
		t.Fatal(err)
	}
	// No fetcher, config, queue, or task resolver is initialized for these reads.
	return &Downloader{storage: s}, s
}

func catalogRecord(id, state string) *batchOperationRecord {
	m := operationMember{Index: 0, TaskID: "task-" + id, State: state, Protocol: "http",
		Options: &base.Options{Path: "/private-secret-path", Extra: map[string]any{"Cookie": "private-secret-credential", "url": "https://private-secret.invalid"}}}
	if state == "failed" {
		m.Error = "cancelled"
	}
	return &batchOperationRecord{Version: 1, Namespace: batchOperationNamespace, OperationID: id,
		RequestHash: strings.Repeat("a", 64), Members: []operationMember{m}}
}

func seedCatalog(t *testing.T, s Storage, id, state string) {
	t.Helper()
	if err := s.Put(bucketTaskOperations, operationKey(id), catalogRecord(id, state)); err != nil {
		t.Fatal(err)
	}
}

func catalogRaw(t *testing.T, s Storage, key string, raw []byte) {
	t.Helper()
	if b, ok := s.(*BoltStorage); ok {
		if err := b.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte(bucketTaskOperations)).Put([]byte(key), raw) }); err != nil {
			t.Fatal(err)
		}
	} else if err := s.Put(bucketTaskOperations, key, json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestBatchCatalogPagesAndUncertaintyDoNotLeakOrWrite(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		d, s := catalogStore(t, bolt)
		seedCatalog(t, s, "a-settled", "created")
		seedCatalog(t, s, "b-pending", "pending")
		catalogRaw(t, s, operationKey("c-broken"), []byte(`{"url":"https://private-secret.invalid"`))
		future := catalogRecord("d-future", "pending")
		future.Version = 2
		if err := s.Put(bucketTaskOperations, operationKey("d-future"), future); err != nil {
			t.Fatal(err)
		}
		if err := s.Put(bucketTaskOperations, operationKey("e-mismatch"), catalogRecord("other-id", "pending")); err != nil {
			t.Fatal(err)
		}
		seedCatalog(t, s, "f-failed", "failed")
		catalogRaw(t, s, "foreign:must-not-read", []byte(`{"secret":"private-secret"}`))
		var before []byte
		if b, ok := s.(*BoltStorage); ok {
			before, _ = os.ReadFile(b.path)
		}
		q := BatchCatalogQuery{Limit: 1}
		var ids []string
		for i := 0; i < 10; i++ {
			page, err := d.ListBatchOperationCatalog(q)
			if err != nil {
				t.Fatal(err)
			}
			if page.Consistency != "live-keyset" || page.Namespace != batchOperationNamespace || page.Persistent != bolt {
				t.Fatalf("page identity: %+v", page)
			}
			if len(page.Items) != 1 {
				t.Fatalf("page: %+v", page)
			}
			entry := page.Items[0]
			ids = append(ids, entry.OperationID)
			if len(entry.KeyHash) != 64 {
				t.Fatal("missing stable placeholder identity")
			}
			raw, _ := json.Marshal(page)
			if bytes.Contains(raw, []byte("private-secret")) || bytes.Contains(raw, []byte("Cookie")) {
				t.Fatal("catalog leaked original options")
			}
			if entry.State == "unknown" && (entry.Error == "" || entry.Counts != nil || entry.RequestHash != "") {
				t.Fatalf("invented unknown counts: %+v", entry)
			}
			if !page.HasMore {
				if page.NextCursor != "" {
					t.Fatal("terminal cursor")
				}
				break
			}
			if page.NextCursor == "" || page.NextCursor == q.Cursor {
				t.Fatal("cursor did not advance")
			}
			q.Cursor = page.NextCursor
		}
		if strings.Join(ids, ",") != "a-settled,b-pending,c-broken,d-future,e-mismatch,f-failed" {
			t.Fatal(ids)
		}
		pending, err := d.ListBatchOperationCatalog(BatchCatalogQuery{Filter: "pending"})
		if err != nil || len(pending.Items) != 4 {
			t.Fatalf("pending lost uncertain records: %+v %v", pending, err)
		}
		if b, ok := s.(*BoltStorage); ok {
			after, _ := os.ReadFile(b.path)
			if sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatal("catalog wrote database")
			}
		}
	}
}

func TestBatchCatalogEmptyPendingPageAdvancesScanBudget(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		d, s := catalogStore(t, bolt)
		atomic := s.(AtomicStorage)
		if err := atomic.UpdateAtomic(func(tx StorageTransaction) error {
			for i := 0; i < catalogScanLimit; i++ {
				id := fmt.Sprintf("settled-%04d", i)
				if err := tx.Put(bucketTaskOperations, operationKey(id), catalogRecord(id, "created")); err != nil {
					return err
				}
			}
			return tx.Put(bucketTaskOperations, operationKey("zz-pending"), catalogRecord("zz-pending", "pending"))
		}); err != nil {
			t.Fatal(err)
		}
		page, err := d.ListBatchOperationCatalog(BatchCatalogQuery{Filter: "pending", Limit: 1})
		if err != nil || len(page.Items) != 0 || page.Scanned != 1000 || !page.HasMore || page.NextCursor == "" {
			t.Fatalf("false empty result: %+v %v", page, err)
		}
		next, err := d.ListBatchOperationCatalog(BatchCatalogQuery{Filter: "pending", Limit: 1, Cursor: page.NextCursor})
		if err != nil || len(next.Items) != 1 || next.Items[0].OperationID != "zz-pending" || next.HasMore {
			t.Fatalf("next: %+v %v", next, err)
		}
	}
}

func TestBatchCatalogCursorScopeAndMissingIdentity(t *testing.T) {
	d, s := catalogStore(t, true)
	seedCatalog(t, s, "a", "pending")
	seedCatalog(t, s, "b", "pending")
	id, _ := d.DatasetIdentity()
	base := catalogCursor{1, id.DatasetID, batchOperationNamespace, "all", []byte(operationKey("a"))}
	for _, change := range []func(*catalogCursor){
		func(c *catalogCursor) { c.Version = 2 }, func(c *catalogCursor) { c.Filter = "pending" },
		func(c *catalogCursor) { c.Namespace = "other" }, func(c *catalogCursor) { c.After = []byte("config:secret") },
		func(c *catalogCursor) { c.After = nil }, func(c *catalogCursor) { c.After = bytes.Repeat([]byte("x"), bbolt.MaxKeySize+1) },
	} {
		cursor := base
		change(&cursor)
		raw, _ := json.Marshal(cursor)
		_, err := d.ListBatchOperationCatalog(BatchCatalogQuery{Cursor: base64.RawURLEncoding.EncodeToString(raw)})
		if !errors.Is(err, ErrCatalogInvalid) {
			t.Fatalf("bad cursor accepted: %v", err)
		}
	}
	other, _ := catalogStore(t, true)
	_, err := other.ListBatchOperationCatalog(BatchCatalogQuery{Cursor: encodeCatalogCursor(id, "all", base.After)})
	if !errors.Is(err, ErrCatalogCursorDomain) {
		t.Fatal("other dataset accepted cursor", err)
	}
	for _, query := range []BatchCatalogQuery{{Filter: "other"}, {Limit: -1}, {Limit: 101}, {Cursor: "!"}, {Cursor: strings.Repeat("x", catalogCursorMax+1)}} {
		if _, err := d.ListBatchOperationCatalog(query); !errors.Is(err, ErrCatalogInvalid) {
			t.Fatal(query, err)
		}
	}
	for _, bolt := range []bool{false, true} {
		store := datasetStore(t, bolt)
		if _, err := (&Downloader{storage: store}).ListBatchOperationCatalog(BatchCatalogQuery{}); !errors.Is(err, ErrDatasetMissing) {
			t.Fatal(err)
		}
		if len(storedDatasetBytes(t, store, bucketConfig, datasetIdentityKey)) != 0 {
			t.Fatal("GET initialized domain")
		}
	}
}

func TestBatchCatalogLiveKeysetAndConcurrentWrites(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		d, s := catalogStore(t, bolt)
		for _, id := range []string{"a", "c", "e"} {
			seedCatalog(t, s, id, "pending")
		}
		page, err := d.ListBatchOperationCatalog(BatchCatalogQuery{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		seedCatalog(t, s, "0-before-cursor", "pending")
		seedCatalog(t, s, "b", "pending")
		if err := s.Delete(bucketTaskOperations, operationKey("c")); err != nil {
			t.Fatal(err)
		}
		next, err := d.ListBatchOperationCatalog(BatchCatalogQuery{Cursor: page.NextCursor})
		if err != nil || len(next.Items) != 2 || next.Items[0].OperationID != "b" || next.Items[1].OperationID != "e" {
			t.Fatalf("live page: %+v %v", next, err)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				id := fmt.Sprintf("volatile-%03d", i)
				if err := s.Put(bucketTaskOperations, operationKey(id), catalogRecord(id, "pending")); err != nil {
					t.Error(err)
				}
				if err := s.Delete(bucketTaskOperations, operationKey(id)); err != nil {
					t.Error(err)
				}
			}
		}()
		for i := 0; i < 50; i++ {
			if _, err := d.ListBatchOperationCatalog(BatchCatalogQuery{Limit: 2}); err != nil {
				t.Fatal(err)
			}
		}
		wg.Wait()
	}
}

func TestBatchCatalogBinaryBadKeysKeepPlaceholderAndProgress(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		d, s := catalogStore(t, bolt)
		catalogRaw(t, s, string(catalogPrefix)+"\x00\xff", []byte(`null`))
		seedCatalog(t, s, "normal", "pending")
		page, err := d.ListBatchOperationCatalog(BatchCatalogQuery{Limit: 1})
		if err != nil || len(page.Items) != 1 || page.Items[0].Error != "record_key_invalid" || page.Items[0].OperationID != "" || !page.HasMore {
			t.Fatalf("bad key: %+v %v", page, err)
		}
		next, err := d.ListBatchOperationCatalog(BatchCatalogQuery{Cursor: page.NextCursor})
		if err != nil || len(next.Items) != 1 || next.Items[0].OperationID != "normal" {
			t.Fatalf("binary cursor: %+v %v", next, err)
		}
	}
}
