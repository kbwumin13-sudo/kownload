package download

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestSharedRecoveryDefaultSetupInitializesDomainBeforeExecution(t *testing.T) {
	payload := []byte("default Core API retains ordinary HTTP downloads")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
	defer server.Close()
	d := NewDownloader(nil) // Exercise the original public default-Mem constructor.
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	identity, err := d.DatasetIdentity()
	if err != nil || identity.Persistent || !datasetIDPattern.MatchString(identity.DatasetID) {
		t.Fatalf("default Setup domain: %+v %v", identity, err)
	}
	id, err := d.CreateDirect(&base.Request{URL: server.URL}, &base.Options{Path: t.TempDir(), Name: "default.bin"})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, id, base.DownloadStatusDone, 3*time.Second)
	content, err := os.ReadFile(d.GetTask(id).Meta.SingleFilepath())
	if err != nil || !bytes.Equal(content, payload) {
		t.Fatal("default public execution lost its complete output", err)
	}
	r := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Version == 4 })
	if r.TotalUsed != 0 || r.Generation != 1 {
		t.Fatal("initial default execution changed its allowance")
	}
}

func TestSharedRecoverySetupRetainsIdentityAndRejectsInvalid(t *testing.T) {
	t.Run("bolt-reopen", func(t *testing.T) {
		dir := t.TempDir()
		store := NewBoltStorage(filepath.Join(dir, "db"))
		if err := initializeDatasetIdentity(store, bytes.NewReader(bytes.Repeat([]byte{31}, 16))); err != nil {
			t.Fatal(err)
		}
		before := storedDatasetBytes(t, store, bucketConfig, datasetIdentityKey)
		d := operationDownloader(t, store, dir)
		first, err := d.DatasetIdentity()
		if err != nil {
			t.Fatal(err)
		}
		d.Close()
		store2 := NewBoltStorage(filepath.Join(dir, "db"))
		d2 := operationDownloader(t, store2, dir)
		defer d2.Close()
		second, err := d2.DatasetIdentity()
		if err != nil || first != second || !bytes.Equal(before, storedDatasetBytes(t, store2, bucketConfig, datasetIdentityKey)) {
			t.Fatal("Setup re-minted or rewrote an existing domain", err)
		}
	})
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "bad-mem", true: "bad-bolt"}[bolt], func(t *testing.T) {
			store := datasetStore(t, bolt)
			putDatasetRaw(t, store, []byte(`{"version":99,"datasetId":"01234567890123456789012345678901"}`))
			before := storedDatasetBytes(t, store, bucketConfig, datasetIdentityKey)
			d := NewDownloader(&DownloaderConfig{Storage: store, StorageDir: t.TempDir()})
			defer d.Close()
			if err := d.Setup(); !errors.Is(err, ErrDatasetInvalid) {
				t.Fatalf("invalid domain was accepted/repaired: %v", err)
			}
			if !bytes.Equal(before, storedDatasetBytes(t, store, bucketConfig, datasetIdentityKey)) {
				t.Fatal("failed Setup rewrote invalid metadata")
			}
		})
	}
}

type sharedLegacyDomainStore struct{ AtomicStorage }

func TestSharedRecoveryCustomStorageRetainsManagedVersionAndCharges(t *testing.T) {
	for _, declaredFour := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-managed-charged", true: "existing-four-held"}[declaredFour], func(t *testing.T) {
			payload := []byte("same custom-storage Task after explicit Continue")
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) == 1 {
					w.WriteHeader(500)
					return
				}
				_, _ = w.Write(payload)
			}))
			defer server.Close()
			dir := t.TempDir()
			backing := NewMemStorage()
			custom := &sharedLegacyDomainStore{AtomicStorage: backing}
			var store Storage = custom
			if declaredFour {
				store = backing
			}
			d := operationDownloader(t, store, dir)
			defer d.Close()
			id, err := d.CreateDirect(&base.Request{URL: server.URL}, &base.Options{Path: dir, Name: "custom.bin"})
			if err != nil {
				t.Fatal(err)
			}
			waitStatus(t, d, id, base.DownloadStatusError, 3*time.Second)
			before := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Generation == 1 })
			if declaredFour {
				if before.Version != 4 {
					t.Fatal("actual concrete writer failed to produce v4")
				}
				d.Close()
				d = operationDownloader(t, custom, dir)
				defer d.Close()
				if err := d.Continue(&TaskFilter{IDs: []string{id}}); !errors.Is(err, transfer.ErrRecoveryHeld) {
					t.Fatalf("custom storage downgraded an existing v4 Task: %v", err)
				}
				after := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Version == 4 })
				if hits.Load() != 1 || !reflect.DeepEqual(before, after) {
					t.Fatal("held custom v4 state was rewritten or sent IO")
				}
				return
			}
			if before.Version != 1 || before.TotalUsed != 0 {
				t.Fatalf("unsupported domain storage was implicitly upgraded: %+v", before)
			}
			if err := d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
				t.Fatal(err)
			}
			waitStatus(t, d, id, base.DownloadStatusDone, 3*time.Second)
			after := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.TransportUsed == 1 })
			if after.Version != 1 || after.Episode != before.Episode || after.TotalUsed != 1 || after.TransportUsed != 1 || hits.Load() != 2 {
				t.Fatalf("custom managed retry became free or changed its episode: %+v", after)
			}
			content, err := os.ReadFile(d.GetTask(id).Meta.SingleFilepath())
			if err != nil || !bytes.Equal(content, payload) {
				t.Fatal("custom managed continuation lost full bytes", err)
			}
		})
	}
}
