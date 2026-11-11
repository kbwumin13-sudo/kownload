package download

import (
	"errors"
	"testing"
)

func TestOperationAtomicStorageRollbackAndDetachment(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			var s AtomicStorage = NewMemStorage()
			if backend == "bolt" {
				s = NewBoltStorage(t.TempDir())
			}
			defer s.Close()
			if e := s.Setup([]string{"one", "two"}); e != nil {
				t.Fatal(e)
			}
			input := &operationTaskLink{TaskID: "stable", Ref: CreationOperation{Version: 1, Namespace: batchOperationNamespace, OperationID: "op", Index: 0}}
			injected := errors.New("fail after both writes")
			e := s.UpdateAtomic(func(tx StorageTransaction) error {
				if e := tx.Put("one", "task", input); e != nil {
					return e
				}
				if e := tx.Put("two", "link", input); e != nil {
					return e
				}
				return injected
			})
			if !errors.Is(e, injected) {
				t.Fatal(e)
			}
			for _, bucket := range []string{"one", "two"} {
				var v operationTaskLink
				ok, e := s.Get(bucket, map[string]string{"one": "task", "two": "link"}[bucket], &v)
				if e != nil || ok {
					t.Fatal("partial transaction survived", bucket, e)
				}
			}
			if e = s.UpdateAtomic(func(tx StorageTransaction) error { return tx.Put("one", "task", input) }); e != nil {
				t.Fatal(e)
			}
			input.TaskID = "mutated"
			var stored operationTaskLink
			ok, e := s.Get("one", "task", &stored)
			if e != nil || !ok || stored.TaskID != "stable" {
				t.Fatal("transaction stored caller-owned mutable value", e)
			}
			e = s.UpdateAtomic(func(tx StorageTransaction) error {
				var v operationTaskLink
				_, e := tx.Get("one", "task", &v)
				if e != nil {
					return e
				}
				v.TaskID = "changed-in-aborted-tx"
				return injected
			})
			if !errors.Is(e, injected) {
				t.Fatal(e)
			}
			_, _ = s.Get("one", "task", &stored)
			if stored.TaskID != "stable" {
				t.Fatal("aborted Get aliased stored value")
			}
		})
	}
}
