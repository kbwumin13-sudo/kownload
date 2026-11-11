package download

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"

	"go.etcd.io/bbolt"
)

const (
	catalogScanLimit = 1000
	catalogCursorMax = 65536
)

var (
	ErrCatalogInvalid      = errors.New("invalid_catalog_request")
	ErrCatalogCursorDomain = errors.New("dataset_cursor_mismatch")
	catalogPrefix          = []byte(batchOperationNamespace + ":")
)

type BatchCatalogQuery struct {
	Filter string
	Limit  int
	Cursor string
}

type BatchCatalogCounts struct {
	Members int `json:"members"`
	Created int `json:"created"`
	Failed  int `json:"failed"`
	Pending int `json:"pending"`
}

// Only creation metadata is projected. Task presence/execution state belongs
// to the existing individual operation query, not this directory.
type BatchCatalogEntry struct {
	KeyHash     string              `json:"keyHash"`
	OperationID string              `json:"operationId,omitempty"`
	State       string              `json:"state"`
	RequestHash string              `json:"requestHash,omitempty"`
	Counts      *BatchCatalogCounts `json:"counts,omitempty"`
	Error       string              `json:"error,omitempty"`
}

type BatchCatalogPage struct {
	Version     int                 `json:"version"`
	Namespace   string              `json:"namespace"`
	DatasetID   string              `json:"datasetId"`
	Persistent  bool                `json:"persistent"`
	Consistency string              `json:"consistency"`
	Filter      string              `json:"filter"`
	Items       []BatchCatalogEntry `json:"items"`
	NextCursor  string              `json:"nextCursor,omitempty"`
	HasMore     bool                `json:"hasMore"`
	Scanned     int                 `json:"scanned"`
}

type catalogCursor struct {
	Version   int    `json:"version"`
	DatasetID string `json:"datasetId"`
	Namespace string `json:"namespace"`
	Filter    string `json:"filter"`
	After     []byte `json:"after"`
}

func catalogAfter(q BatchCatalogQuery, identity DatasetIdentity) ([]byte, error) {
	if q.Cursor == "" {
		return nil, nil
	}
	if len(q.Cursor) > catalogCursorMax {
		return nil, ErrCatalogInvalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(q.Cursor)
	if err != nil {
		return nil, ErrCatalogInvalid
	}
	var cursor catalogCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cursor) != nil {
		return nil, ErrCatalogInvalid
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || cursor.Version != 1 || cursor.Namespace != batchOperationNamespace ||
		cursor.Filter != q.Filter || !datasetIDPattern.MatchString(cursor.DatasetID) ||
		!bytes.HasPrefix(cursor.After, catalogPrefix) || len(cursor.After) > bbolt.MaxKeySize {
		return nil, ErrCatalogInvalid
	}
	if cursor.DatasetID != identity.DatasetID {
		return nil, ErrCatalogCursorDomain
	}
	return cursor.After, nil
}

func encodeCatalogCursor(identity DatasetIdentity, filter string, after []byte) string {
	raw, _ := json.Marshal(catalogCursor{1, identity.DatasetID, batchOperationNamespace, filter, after})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func catalogEntry(key, raw []byte, readErr error) BatchCatalogEntry {
	hash := sha256.Sum256(key)
	entry := BatchCatalogEntry{KeyHash: hex.EncodeToString(hash[:]), State: "unknown"}
	id := string(bytes.TrimPrefix(key, catalogPrefix))
	if !operationIDPattern.MatchString(id) {
		entry.Error = "record_key_invalid"
		return entry
	}
	entry.OperationID = id
	if readErr != nil {
		entry.Error = "record_unreadable"
		return entry
	}
	var record batchOperationRecord
	if decodeStoredJSON(raw, &record) != nil {
		entry.Error = "record_invalid"
		return entry
	}
	if record.Version != 1 || record.Namespace != batchOperationNamespace {
		entry.Error = "record_unsupported"
		return entry
	}
	if record.OperationID != id {
		entry.Error = "record_key_mismatch"
		return entry
	}
	if !record.valid() {
		entry.Error = "record_invalid"
		return entry
	}
	counts := &BatchCatalogCounts{Members: len(record.Members)}
	for _, member := range record.Members {
		switch member.State {
		case "created":
			counts.Created++
		case "failed":
			counts.Failed++
		case "pending":
			counts.Pending++
		}
	}
	entry.State = "settled"
	if counts.Pending != 0 {
		entry.State = "pending"
	}
	entry.RequestHash, entry.Counts = record.RequestHash, counts
	return entry
}

// seek runs while the caller holds one read transaction/lock. The returned
// iterator is confined to that snapshot; no transaction-backed bytes escape.
func catalogPage(identity DatasetIdentity, q BatchCatalogQuery, seek func([]byte) func() ([]byte, []byte, error)) (BatchCatalogPage, error) {
	page := BatchCatalogPage{Version: 1, Namespace: batchOperationNamespace, DatasetID: identity.DatasetID,
		Persistent: identity.Persistent, Consistency: "live-keyset", Filter: q.Filter, Items: []BatchCatalogEntry{}}
	after, err := catalogAfter(q, identity)
	if err != nil {
		return BatchCatalogPage{}, err
	}
	next := seek(after)
	var last []byte
	for page.Scanned < catalogScanLimit && len(page.Items) < q.Limit {
		key, raw, readErr := next()
		if !bytes.HasPrefix(key, catalogPrefix) {
			return page, nil
		}
		last = append(last[:0], key...)
		page.Scanned++
		entry := catalogEntry(key, raw, readErr)
		if q.Filter == "all" || entry.State != "settled" {
			page.Items = append(page.Items, entry)
		}
	}
	key, _, _ := next()
	page.HasMore = bytes.HasPrefix(key, catalogPrefix)
	if page.HasMore {
		page.NextCursor = encodeCatalogCursor(identity, q.Filter, last)
	}
	return page, nil
}

// ListBatchOperationCatalog never calls Q/resolve/restore or rewrites a record.
// Pages are live keysets, not a global snapshot or proof of absent operations.
func (d *Downloader) ListBatchOperationCatalog(q BatchCatalogQuery) (BatchCatalogPage, error) {
	if q.Filter == "" {
		q.Filter = "all"
	}
	if q.Limit == 0 {
		q.Limit = 50
	}
	if (q.Filter != "all" && q.Filter != "pending") || q.Limit < 1 || q.Limit > 100 {
		return BatchCatalogPage{}, ErrCatalogInvalid
	}
	var page BatchCatalogPage
	switch s := d.storage.(type) {
	case *BoltStorage:
		err := s.db.View(func(tx *bbolt.Tx) error {
			config := tx.Bucket([]byte(bucketConfig))
			if config == nil {
				return ErrDatasetMissing
			}
			identity, err := decodeDataset(config.Get([]byte(datasetIdentityKey)), true)
			if err != nil {
				return err
			}
			page, err = catalogPage(identity, q, func(after []byte) func() ([]byte, []byte, error) {
				bucket := tx.Bucket([]byte(bucketTaskOperations))
				if bucket == nil {
					return func() ([]byte, []byte, error) { return nil, nil, nil }
				}
				cursor := bucket.Cursor()
				key, value := cursor.Seek(catalogPrefix)
				if after != nil {
					key, value = cursor.Seek(after)
					if bytes.Equal(key, after) {
						key, value = cursor.Next()
					}
				}
				first := true
				return func() ([]byte, []byte, error) {
					if first {
						first = false
					} else {
						key, value = cursor.Next()
					}
					return key, value, nil
				}
			})
			return err
		})
		return page, err
	case *MemStorage:
		s.lock.RLock()
		defer s.lock.RUnlock()
		raw, err := memDatasetRaw(s.data[bucketConfig])
		if err != nil {
			return page, err
		}
		identity, err := decodeDataset(raw, false)
		if err != nil {
			return page, err
		}
		return catalogPage(identity, q, func(after []byte) func() ([]byte, []byte, error) {
			bucket := s.data[bucketTaskOperations]
			keys := make([]string, 0, len(bucket))
			for key := range bucket {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			start := catalogPrefix
			if after != nil {
				start = after
			}
			i := sort.SearchStrings(keys, string(start))
			if after != nil && i < len(keys) && keys[i] == string(after) {
				i++
			}
			return func() ([]byte, []byte, error) {
				if i == len(keys) {
					return nil, nil, nil
				}
				key := keys[i]
				i++
				if !bytes.HasPrefix([]byte(key), catalogPrefix) {
					return nil, nil, nil
				}
				raw, err := json.Marshal(bucket[key])
				return []byte(key), raw, err
			}
		})
	default:
		return page, ErrDatasetUnsupported
	}
}
