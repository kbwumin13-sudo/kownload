package download

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"

	"go.etcd.io/bbolt"
)

var errTransactionBucketMissing = errors.New("missing transaction bucket")

// AtomicStorage is optional: old custom Storage implementations remain valid,
// but durable operations never fall back to a sequence of independent Puts.
type AtomicStorage interface {
	Storage
	UpdateAtomic(func(StorageTransaction) error) error
}
type StorageTransaction interface {
	Get(bucket, key string, v any) (bool, error)
	Put(bucket, key string, v any) error
}

func decodeStoredJSON(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple stored JSON values")
		}
		return err
	}
	return nil
}

type boltStorageTransaction struct{ tx *bbolt.Tx }

func (t boltStorageTransaction) Get(bucket, key string, v any) (bool, error) {
	b := t.tx.Bucket([]byte(bucket))
	if b == nil {
		return false, errTransactionBucketMissing
	}
	raw := b.Get([]byte(key))
	if raw == nil {
		return false, nil
	}
	return true, decodeStoredJSON(raw, v)
}
func (t boltStorageTransaction) Put(bucket, key string, v any) error {
	b := t.tx.Bucket([]byte(bucket))
	if b == nil {
		return errTransactionBucketMissing
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Put([]byte(key), raw)
}
func (b *BoltStorage) UpdateAtomic(fn func(StorageTransaction) error) error {
	return b.db.Update(func(tx *bbolt.Tx) error { return fn(boltStorageTransaction{tx}) })
}

type memStorageTransaction struct{ data map[string]map[string]any }

func (t memStorageTransaction) Get(bucket, key string, v any) (bool, error) {
	b, ok := t.data[bucket]
	if !ok {
		return false, errTransactionBucketMissing
	}
	value, ok := b[key]
	if !ok {
		return false, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	return true, decodeStoredJSON(raw, v)
}
func (t memStorageTransaction) Put(bucket, key string, v any) error {
	b, ok := t.data[bucket]
	if !ok {
		return errTransactionBucketMissing
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// Preserve the dynamic type expected by the existing MemStorage List/Get,
	// while detaching all mutable pointers from both the caller and old snapshot.
	typ := reflect.TypeOf(v)
	if typ == nil {
		return errors.New("nil transaction value")
	}
	target := reflect.New(typ)
	if err = decodeStoredJSON(raw, target.Interface()); err != nil {
		return err
	}
	b[key] = target.Elem().Interface()
	return nil
}
func (s *MemStorage) UpdateAtomic(fn func(StorageTransaction) error) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	next := make(map[string]map[string]any, len(s.data))
	for bucket, old := range s.data {
		copied := make(map[string]any, len(old))
		for k, v := range old {
			copied[k] = v
		}
		next[bucket] = copied
	}
	if err := fn(memStorageTransaction{next}); err != nil {
		return err
	}
	s.data = next
	return nil
}
