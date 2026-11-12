package download

import (
	"encoding/json"
	"path/filepath"

	"go.etcd.io/bbolt"
)

// readFrozenPathTargets observes durable reservations, including F whose ACK
// was lost or whose live Task was cancelled before its memory swap. It grants
// no execution or ownership permission and never attaches a Task. The caller
// serializes reservation/deletion through the existing operation lifecycle.
func readFrozenPathTargets(store Storage, dataset, exceptID string) (map[string]bool, error) {
	targets := make(map[string]bool)
	visit := func(raw func(string, string) ([]byte, error), walk func(func(string) error) error) error {
		reader := &datasetSnapshotReader{active: true, raw: raw}
		defer reader.close()
		if err := requireDataset(reader, dataset); err != nil {
			return err
		}
		return walk(func(key string) error {
			if exceptID != "" && key == exceptID {
				return nil
			}
			var task Task
			present, err := reader.Get(bucketTask, key, &task)
			if err != nil {
				return err
			}
			if !present || task.ID == "" || task.ID != key {
				return ErrPathFreezeHeld
			}
			state := task.PathFreeze
			if state == nil {
				// A declared path R2 without its required slot is not a free target.
				if ref := task.CreationOperation; ref != nil && ref.Namespace == batchOperationNamespace && ref.Version != 1 {
					return ErrPathFreezeHeld
				}
				return nil
			}
			ref := task.CreationOperation
			variant, known := pathVariantForCaps([]string{state.RequiredCapability})
			if !MCPReadValidID(task.ID) || task.MCPWrite != nil || ref == nil || ref.Version != 2 || ref.Namespace != batchOperationNamespace || !operationIDPattern.MatchString(ref.OperationID) || ref.Index < 0 || !known || variant.Protocol != task.Protocol || !pathFrozenValid(*state) {
				return ErrPathFreezeHeld
			}
			if state.Phase == PathFreezeFrozen {
				// Keep all members, including unselected/zero-length members. This is a
				// row-local value projection: no whole R or recovery record is reloaded.
				for _, member := range state.Layout.Members {
					targets[filepath.Join(state.Layout.ParentRoot, filepath.FromSlash(member.RelativePath))] = true
				}
			}
			return nil
		})
	}
	var err error
	switch s := store.(type) {
	case *BoltStorage:
		if s == nil || s.db == nil {
			return nil, ErrPathFreezeUnavailable
		}
		err = s.db.View(func(tx *bbolt.Tx) error {
			return visit(func(bucket, key string) ([]byte, error) {
				b := tx.Bucket([]byte(bucket))
				if b == nil {
					return nil, nil
				}
				return b.Get([]byte(key)), nil
			}, func(read func(string) error) error {
				bucket := tx.Bucket([]byte(bucketTask))
				if bucket == nil {
					return ErrPathFreezeHeld
				}
				return bucket.ForEach(func(key, value []byte) error {
					if value == nil {
						return ErrPathFreezeHeld
					}
					return read(string(key))
				})
			})
		})
	case *MemStorage:
		if s == nil || s.lock == nil {
			return nil, ErrPathFreezeUnavailable
		}
		s.lock.RLock()
		defer s.lock.RUnlock()
		err = visit(func(bucket, key string) ([]byte, error) {
			value, present := s.data[bucket][key]
			if !present {
				return nil, nil
			}
			// Stored Tasks are immutable owned snapshots, never live TaskMap objects.
			return json.Marshal(value)
		}, func(read func(string) error) error {
			bucket, exists := s.data[bucketTask]
			if !exists || bucket == nil {
				return ErrPathFreezeHeld
			}
			for key := range bucket {
				if err := read(key); err != nil {
					return err
				}
			}
			return nil
		})
	default:
		return nil, ErrDatasetUnsupported
	}
	if err != nil {
		return nil, pathStoreError(err)
	}
	return targets, nil
}
