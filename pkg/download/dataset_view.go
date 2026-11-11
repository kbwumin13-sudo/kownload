package download

import (
	"encoding/json"
	"errors"
	"sync"

	"go.etcd.io/bbolt"
)

var (
	ErrDatasetMismatch   = errors.New("dataset_mismatch")
	ErrDatasetViewClosed = errors.New("dataset_view_closed")
)

// datasetReader is also satisfied by StorageTransaction. A read-only view
// never exposes Put, the backing transaction or its page slices to callers.
type datasetReader interface {
	Get(bucket, key string, out any) (bool, error)
}

type datasetSnapshotReader struct {
	lock   sync.Mutex
	active bool
	raw    func(bucket, key string) ([]byte, error)
}

func (r *datasetSnapshotReader) Get(bucket, key string, out any) (bool, error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if !r.active {
		return false, ErrDatasetViewClosed
	}
	raw, err := r.raw(bucket, key)
	if err != nil || raw == nil {
		return false, err
	}
	// Both RawMessage and ordinary decoded values must own their bytes after
	// the callback returns. Keep the operation decoder's exact-number policy.
	return true, decodeStoredJSON(append([]byte(nil), raw...), out)
}

func (r *datasetSnapshotReader) close() {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.active, r.raw = false, nil
}

// requireDataset must run inside the same view/write transaction as the
// operation it guards. It does not initialize or repair identity metadata.
func requireDataset(reader datasetReader, expectedID string) error {
	if !datasetIDPattern.MatchString(expectedID) {
		return ErrDatasetInvalid
	}
	var raw json.RawMessage
	exists, err := reader.Get(bucketConfig, datasetIdentityKey, &raw)
	if err != nil {
		return ErrDatasetInvalid
	}
	if !exists {
		return ErrDatasetMissing
	}
	identity, err := decodeDataset(raw, false)
	if err != nil {
		return err
	}
	if identity.DatasetID != expectedID {
		return ErrDatasetMismatch
	}
	return nil
}

// withDatasetReadView checks the domain before invoking read, and keeps all
// callback Gets in one real Bolt View or Mem read lock. Callbacks must not
// acquire another storage view/lock, write, attach Q or retain the reader.
// No optional/unknown Storage is silently treated as snapshot-capable.
func withDatasetReadView(store Storage, expectedID string, read func(datasetReader) error) error {
	if read == nil {
		return ErrDatasetInvalid
	}
	visit := func(raw func(string, string) ([]byte, error)) error {
		reader := &datasetSnapshotReader{active: true, raw: raw}
		defer reader.close()
		if err := requireDataset(reader, expectedID); err != nil {
			return err
		}
		return read(reader)
	}
	switch s := store.(type) {
	case *BoltStorage:
		return s.db.View(func(tx *bbolt.Tx) error {
			return visit(func(bucket, key string) ([]byte, error) {
				b := tx.Bucket([]byte(bucket))
				if b == nil {
					return nil, nil
				}
				return b.Get([]byte(key)), nil
			})
		})
	case *MemStorage:
		s.lock.RLock()
		defer s.lock.RUnlock()
		return visit(func(bucket, key string) ([]byte, error) {
			value, exists := s.data[bucket][key]
			if !exists {
				return nil, nil
			}
			return json.Marshal(value)
		})
	default:
		return ErrDatasetUnsupported
	}
}
