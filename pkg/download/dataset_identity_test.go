package download

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.etcd.io/bbolt"
)

func datasetStore(t *testing.T, bolt bool) Storage {
	t.Helper()
	var s Storage = NewMemStorage()
	if bolt {
		s = NewBoltStorage(t.TempDir())
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func storedDatasetBytes(t *testing.T, s Storage, bucket, key string) []byte {
	t.Helper()
	if b, ok := s.(*BoltStorage); ok {
		var result []byte
		if err := b.db.View(func(tx *bbolt.Tx) error {
			if entry := tx.Bucket([]byte(bucket)); entry != nil {
				result = append([]byte(nil), entry.Get([]byte(key))...)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return result
	}
	m := s.(*MemStorage)
	m.lock.RLock()
	defer m.lock.RUnlock()
	v, ok := m.data[bucket][key]
	if !ok {
		return nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return append([]byte(nil), raw...)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func putDatasetRaw(t *testing.T, s Storage, raw []byte) {
	t.Helper()
	if err := s.Setup([]string{bucketConfig}); err != nil {
		t.Fatal(err)
	}
	if b, ok := s.(*BoltStorage); ok {
		if err := b.db.Update(func(tx *bbolt.Tx) error {
			return tx.Bucket([]byte(bucketConfig)).Put([]byte(datasetIdentityKey), raw)
		}); err != nil {
			t.Fatal(err)
		}
	} else if err := s.Put(bucketConfig, datasetIdentityKey, json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestDatasetIdentityMissingReadAndInitialization(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt], func(t *testing.T) {
			s := datasetStore(t, bolt)
			d := &Downloader{storage: s}
			if _, err := d.DatasetIdentity(); !errors.Is(err, ErrDatasetMissing) {
				t.Fatalf("missing read: %v", err)
			}
			if err := s.Setup([]string{bucketConfig, bucketTask}); err != nil {
				t.Fatal(err)
			}
			for _, bucket := range []string{bucketConfig, bucketTask} {
				if err := s.Put(bucket, "existing", map[string]any{"preserve": true}); err != nil {
					t.Fatal(err)
				}
			}
			legacy := storedDatasetBytes(t, s, bucketTask, "existing")
			var fileBefore []byte
			if b, ok := s.(*BoltStorage); ok {
				fileBefore, _ = os.ReadFile(b.path)
			}
			for i := 0; i < 3; i++ {
				if _, err := d.DatasetIdentity(); !errors.Is(err, ErrDatasetMissing) {
					t.Fatal(err)
				}
			}
			if b, ok := s.(*BoltStorage); ok {
				after, _ := os.ReadFile(b.path)
				if sha256.Sum256(fileBefore) != sha256.Sum256(after) {
					t.Fatal("GET changed the database")
				}
			}
			if err := initializeDatasetIdentity(s, bytes.NewReader(bytes.Repeat([]byte{7}, 16))); err != nil {
				t.Fatal(err)
			}
			id, err := d.DatasetIdentity()
			if err != nil || id.DatasetID != "07070707070707070707070707070707" || id.Version != 1 || id.Persistent != bolt {
				t.Fatalf("identity: %+v %v", id, err)
			}
			original := storedDatasetBytes(t, s, bucketConfig, datasetIdentityKey)
			if err := s.Setup([]string{bucketConfig, bucketTask}); err != nil {
				t.Fatal(err)
			}
			// Existing identities never consume new entropy or rewrite their value.
			if err := initializeDatasetIdentity(s, bytes.NewReader(nil)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(original, storedDatasetBytes(t, s, bucketConfig, datasetIdentityKey)) || !bytes.Equal(legacy, storedDatasetBytes(t, s, bucketTask, "existing")) {
				t.Fatal("setup/init changed existing metadata or task")
			}
		})
	}
}

func TestDatasetIdentityConcurrentInitialize(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		s := datasetStore(t, bolt)
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := InitializeDatasetIdentity(s); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		id, err := (&Downloader{storage: s}).DatasetIdentity()
		if err != nil || !datasetIDPattern.MatchString(id.DatasetID) {
			t.Fatalf("concurrent identity: %+v %v", id, err)
		}
	}
}

func TestDatasetIdentityReopenCopyAndReplacement(t *testing.T) {
	dir := t.TempDir()
	s := NewBoltStorage(dir)
	if err := InitializeDatasetIdentity(s); err != nil {
		t.Fatal(err)
	}
	first, _ := (&Downloader{storage: s}).DatasetIdentity()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	copyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(copyDir, dbFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{dir, copyDir} {
		reopened := NewBoltStorage(root)
		if err := InitializeDatasetIdentity(reopened); err != nil {
			t.Fatal(err)
		}
		got, err := (&Downloader{storage: reopened}).DatasetIdentity()
		if err != nil || got != first {
			t.Fatalf("reopen/copy: %+v %v", got, err)
		}
		_ = reopened.Close()
	}
	if err := os.Rename(filepath.Join(dir, dbFile), filepath.Join(dir, "previous.db")); err != nil {
		t.Fatal(err)
	}
	fresh := NewBoltStorage(dir)
	defer fresh.Close()
	if err := InitializeDatasetIdentity(fresh); err != nil {
		t.Fatal(err)
	}
	got, _ := (&Downloader{storage: fresh}).DatasetIdentity()
	if got.DatasetID == first.DatasetID {
		t.Fatal("replacement database reused old logical ID")
	}
}

func TestDatasetIdentityInvalidValuesPreserved(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		for _, raw := range []string{`null`, `{`, `{"version":2,"datasetId":"07070707070707070707070707070707"}`, `{"version":1,"datasetId":"BAD"}`, `{"version":1}`, `{"version":1,"datasetId":"07070707070707070707070707070707","extra":true}`} {
			s := datasetStore(t, bolt)
			putDatasetRaw(t, s, []byte(raw))
			if err := InitializeDatasetIdentity(s); !errors.Is(err, ErrDatasetInvalid) {
				t.Fatalf("accepted %s: %v", raw, err)
			}
			if _, err := (&Downloader{storage: s}).DatasetIdentity(); !errors.Is(err, ErrDatasetInvalid) {
				t.Fatalf("read accepted %s: %v", raw, err)
			}
			if string(storedDatasetBytes(t, s, bucketConfig, datasetIdentityKey)) != raw {
				t.Fatal("bad identity overwritten")
			}
		}
	}
}

type noDatasetStorage struct{ Storage }

func TestDatasetIdentityInitializationFailures(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		s := datasetStore(t, bolt)
		if err := initializeDatasetIdentity(s, bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
			t.Fatalf("entropy failure: %v", err)
		}
		if _, err := (&Downloader{storage: s}).DatasetIdentity(); !errors.Is(err, ErrDatasetMissing) {
			t.Fatal("failed initialization published identity", err)
		}
	}
	if err := InitializeDatasetIdentity(noDatasetStorage{NewMemStorage()}); !errors.Is(err, ErrDatasetUnsupported) {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := NewBoltStorage(dir)
	_ = s.Close()
	db, err := bbolt.Open(filepath.Join(dir, dbFile), 0600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = InitializeDatasetIdentity(&BoltStorage{db: db}); !errors.Is(err, bbolt.ErrDatabaseReadOnly) {
		t.Fatalf("read-only init: %v", err)
	}
}
