package download

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func datasetViewStore(t *testing.T, bolt bool) (Storage, string) {
	t.Helper()
	s := datasetStore(t, bolt)
	if err := s.Setup([]string{bucketConfig, bucketTask}); err != nil {
		t.Fatal(err)
	}
	if err := InitializeDatasetIdentity(s); err != nil {
		t.Fatal(err)
	}
	id, err := (&Downloader{storage: s}).DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return s, id.DatasetID
}

func TestDatasetViewOwnedNumbersAndReadOnlyBytes(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt], func(t *testing.T) {
			s, id := datasetViewStore(t, bolt)
			raw := json.RawMessage(`{"large":900719925474099312345,"nested":{"keep":true}}`)
			if err := s.Put(bucketTask, "entry", raw); err != nil {
				t.Fatal(err)
			}
			var before []byte
			if b, ok := s.(*BoltStorage); ok {
				before, _ = os.ReadFile(b.path)
			}
			var decoded map[string]any
			var owned json.RawMessage
			var escaped datasetReader
			err := withDatasetReadView(s, id, func(reader datasetReader) error {
				escaped = reader
				if _, writable := reader.(StorageTransaction); writable {
					t.Fatal("view exposed a write interface")
				}
				if ok, err := reader.Get(bucketTask, "entry", &decoded); err != nil || !ok {
					t.Fatalf("read: %v %v", ok, err)
				}
				if decoded["large"] != json.Number("900719925474099312345") {
					t.Fatal("number lost precision")
				}
				if ok, err := reader.Get(bucketTask, "entry", &owned); err != nil || !ok {
					t.Fatal(err)
				}
				var missing any
				if ok, err := reader.Get("absent-bucket", "absent", &missing); ok || err != nil {
					t.Fatalf("missing bucket: %v %v", ok, err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := escaped.Get(bucketTask, "entry", &decoded); !errors.Is(err, ErrDatasetViewClosed) {
				t.Fatalf("escaped view: %v", err)
			}
			decoded["nested"].(map[string]any)["keep"] = false
			owned[0] = '['
			if err := withDatasetReadView(s, id, func(reader datasetReader) error {
				var got json.RawMessage
				_, err := reader.Get(bucketTask, "entry", &got)
				if err == nil && !bytes.Equal(got, raw) {
					t.Fatal("caller changed stored bytes through decoded values")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if b, ok := s.(*BoltStorage); ok {
				after, _ := os.ReadFile(b.path)
				if !bytes.Equal(before, after) {
					t.Fatal("read-only views wrote database bytes")
				}
			}
		})
	}
}

func TestDatasetViewRejectsMissingBadAndOtherDomain(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		for _, raw := range []string{"", `null`, `{`, `{"version":2,"datasetId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, `{"version":1,"datasetId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","extra":true}`, `{"version":1,"datasetId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`} {
			t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt]+"/"+raw, func(t *testing.T) {
				s := datasetStore(t, bolt)
				if raw != "" {
					putDatasetRaw(t, s, []byte(raw))
				}
				before := storedDatasetBytes(t, s, bucketConfig, datasetIdentityKey)
				called := false
				err := withDatasetReadView(s, strings.Repeat("b", 32), func(datasetReader) error { called = true; return nil })
				if err == nil || called {
					t.Fatalf("unsafe callback: %v called=%v", err, called)
				}
				if raw == `{"version":1,"datasetId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}` && !errors.Is(err, ErrDatasetMismatch) {
					t.Fatalf("wrong mismatch error: %v", err)
				}
				if !bytes.Equal(before, storedDatasetBytes(t, s, bucketConfig, datasetIdentityKey)) {
					t.Fatal("rejected read rewrote identity")
				}
			})
		}
	}
}

func TestDatasetViewTransactionGuardRollsBackBeforeWrites(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt], func(t *testing.T) {
			s, id := datasetViewStore(t, bolt)
			store := s.(AtomicStorage)
			for _, expected := range []string{strings.Repeat("f", 32), "bad-id", id} {
				err := store.UpdateAtomic(func(tx StorageTransaction) error {
					if err := requireDataset(tx, expected); err != nil {
						return err
					}
					return tx.Put(bucketTask, "new", map[string]any{"owned": true})
				})
				if (err == nil) != (expected == id) {
					t.Fatalf("guard %q: %v", expected, err)
				}
				if expected != id && len(storedDatasetBytes(t, s, bucketTask, "new")) != 0 {
					t.Fatal("rejected transaction wrote task")
				}
			}
		})
	}
}

func TestDatasetViewSingleSnapshotDuringConcurrentWrite(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt], func(t *testing.T) {
			s, id := datasetViewStore(t, bolt)
			if err := s.Put(bucketTask, "pair", map[string]int{"epoch": 1}); err != nil {
				t.Fatal(err)
			}
			attempted, prepared, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			err := withDatasetReadView(s, id, func(reader datasetReader) error {
				var first, second map[string]int
				if _, err := reader.Get(bucketTask, "pair", &first); err != nil {
					return err
				}
				go func() {
					close(attempted)
					finished <- s.(AtomicStorage).UpdateAtomic(func(tx StorageTransaction) error {
						err := tx.Put(bucketTask, "pair", map[string]int{"epoch": 2})
						close(prepared)
						return err
					})
				}()
				<-attempted
				if bolt {
					<-prepared // Bolt writer has changed its private pages, not this View.
				}
				if _, err := reader.Get(bucketTask, "pair", &second); err != nil {
					return err
				}
				if first["epoch"] != 1 || second["epoch"] != 1 {
					t.Fatal("one read view mixed writer generations")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			if err := withDatasetReadView(s, id, func(reader datasetReader) error {
				var pair map[string]int
				_, err := reader.Get(bucketTask, "pair", &pair)
				if err == nil && pair["epoch"] != 2 {
					t.Fatal("next view failed to observe committed write")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type unknownDatasetViewStorage struct{ Storage }

func TestDatasetViewUnavailableAndCallbackError(t *testing.T) {
	s, id := datasetViewStore(t, true)
	if err := withDatasetReadView(unknownDatasetViewStorage{s}, id, func(datasetReader) error {
		t.Fatal("unknown store callback")
		return nil
	}); !errors.Is(err, ErrDatasetUnsupported) {
		t.Fatal(err)
	}
	want := errors.New("callback fixture")
	if err := withDatasetReadView(s, id, func(datasetReader) error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := withDatasetReadView(s, id, func(datasetReader) error { t.Fatal("closed store callback"); return nil }); err == nil {
		t.Fatal("closed database accepted")
	}
}
