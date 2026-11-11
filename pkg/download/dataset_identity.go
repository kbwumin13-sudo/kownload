package download

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"

	"go.etcd.io/bbolt"
)

const datasetIdentityKey = "dataset_identity_v1"

var (
	ErrDatasetMissing     = errors.New("dataset_identity_uninitialized")
	ErrDatasetInvalid     = errors.New("dataset_identity_invalid")
	ErrDatasetUnsupported = errors.New("dataset_identity_storage_unsupported")
	datasetIDPattern      = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// DatasetIdentity is a logical lineage label, not authentication, a physical
// database identifier, or proof that a copied/restored database is current.
type DatasetIdentity struct {
	Version    int    `json:"version"`
	DatasetID  string `json:"datasetId"`
	Persistent bool   `json:"persistent"`
}

type datasetRecord struct {
	Version   int    `json:"version"`
	DatasetID string `json:"datasetId"`
}

func decodeDataset(raw []byte, persistent bool) (DatasetIdentity, error) {
	if raw == nil {
		return DatasetIdentity{}, ErrDatasetMissing
	}
	var record datasetRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return DatasetIdentity{}, ErrDatasetInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || record.Version != 1 || !datasetIDPattern.MatchString(record.DatasetID) {
		return DatasetIdentity{}, ErrDatasetInvalid
	}
	return DatasetIdentity{Version: 1, DatasetID: record.DatasetID, Persistent: persistent}, nil
}

func memDatasetRaw(bucket map[string]any) ([]byte, error) {
	v, ok := bucket[datasetIdentityKey]
	if !ok {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, ErrDatasetInvalid
	}
	return raw, nil
}

// InitializeDatasetIdentity is an explicit startup operation. Call it before
// Setup can restore tasks. Reads never call it or repair an existing record.
// Custom Storage implementations are not silently treated as durable domains.
func InitializeDatasetIdentity(storage Storage) error {
	return initializeDatasetIdentity(storage, rand.Reader)
}

func initializeDatasetIdentity(storage Storage, entropy io.Reader) error {
	create := func() (datasetRecord, error) {
		var random [16]byte
		if _, err := io.ReadFull(entropy, random[:]); err != nil {
			return datasetRecord{}, err
		}
		return datasetRecord{Version: 1, DatasetID: hex.EncodeToString(random[:])}, nil
	}
	switch s := storage.(type) {
	case *BoltStorage:
		return s.db.Update(func(tx *bbolt.Tx) error {
			bucket, err := tx.CreateBucketIfNotExists([]byte(bucketConfig))
			if err != nil {
				return err
			}
			if raw := bucket.Get([]byte(datasetIdentityKey)); raw != nil {
				_, err = decodeDataset(raw, true)
				return err
			}
			record, err := create()
			if err != nil {
				return err
			}
			raw, err := json.Marshal(record)
			if err != nil {
				return err
			}
			return bucket.Put([]byte(datasetIdentityKey), raw)
		})
	case *MemStorage:
		s.lock.Lock()
		defer s.lock.Unlock()
		bucket := s.data[bucketConfig]
		raw, err := memDatasetRaw(bucket)
		if err != nil {
			return err
		}
		if raw != nil {
			_, err = decodeDataset(raw, false)
			return err
		}
		record, err := create()
		if err != nil {
			return err
		}
		if bucket == nil {
			bucket = make(map[string]any)
			s.data[bucketConfig] = bucket
		}
		bucket[datasetIdentityKey] = record
		return nil
	default:
		return ErrDatasetUnsupported
	}
}

// DatasetIdentity reads existing metadata without starting tasks or writing.
func (d *Downloader) DatasetIdentity() (DatasetIdentity, error) {
	switch s := d.storage.(type) {
	case *BoltStorage:
		var result DatasetIdentity
		err := s.db.View(func(tx *bbolt.Tx) error {
			bucket := tx.Bucket([]byte(bucketConfig))
			if bucket == nil {
				return ErrDatasetMissing
			}
			var err error
			result, err = decodeDataset(bucket.Get([]byte(datasetIdentityKey)), true)
			return err
		})
		return result, err
	case *MemStorage:
		s.lock.RLock()
		defer s.lock.RUnlock()
		raw, err := memDatasetRaw(s.data[bucketConfig])
		if err != nil {
			return DatasetIdentity{}, err
		}
		return decodeDataset(raw, false)
	default:
		return DatasetIdentity{}, ErrDatasetUnsupported
	}
}
