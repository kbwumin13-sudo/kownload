// Package identitymigration copies an inactive legacy store without changing its
// format. It deliberately does not import the engine or decode private records.
package identitymigration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	bolt "go.etcd.io/bbolt"
)

const Marker = ".kownload-identity.json"
const pending = ".kownload-import-pending.json"
const owner = ".kownload-import-owner"

type Summary struct {
	Version        int    `json:"version"`
	Imported       bool   `json:"imported"`
	Files          int    `json:"files"`
	Digest         string `json:"digest"`
	DatabaseDigest string `json:"databaseDigest"`
}
type Options struct {
	Source string
	Target string
	// Hook is only for deterministic interruption fixtures, never a CLI option.
	Hook func(string) error
}

func call(o Options, phase string) error {
	if o.Hook != nil {
		return o.Hook(phase)
	}
	return nil
}

func Import(o Options) (Summary, error) {
	var zero Summary
	source, err := filepath.Abs(o.Source)
	if err != nil {
		return zero, errors.New("invalid_source")
	}
	target, err := filepath.Abs(o.Target)
	if err != nil {
		return zero, errors.New("invalid_target")
	}
	if source == target || strings.HasPrefix(target, source+string(os.PathSeparator)) || strings.HasPrefix(source, target+string(os.PathSeparator)) {
		return zero, errors.New("overlapping_roots")
	}
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return zero, errors.New("target_parent_unavailable")
	}
	lock, err := os.OpenFile(target+".migration-lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return zero, errors.New("migration_lock_unavailable")
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return zero, errors.New("migration_busy")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return zero, errors.New("target_symlink")
	}
	if data, err := os.ReadFile(filepath.Join(target, Marker)); err == nil {
		var done Summary
		if json.Unmarshal(data, &done) != nil || done.Version != 1 {
			return zero, errors.New("invalid_marker")
		}
		return done, nil
	}
	// A crash between the atomic rename and final marker can finish only after
	// verifying the previously verified tree; arbitrary existing data is refused.
	if data, err := os.ReadFile(filepath.Join(target, pending)); err == nil {
		var s Summary
		if json.Unmarshal(data, &s) != nil {
			return zero, errors.New("invalid_pending_import")
		}
		actual, err := summarize(target, s.Imported)
		if err != nil || actual != s {
			return zero, errors.New("pending_verification_failed")
		}
		if err = publishMarker(target); err != nil {
			return zero, err
		}
		return s, nil
	}
	if entries, err := os.ReadDir(target); err == nil && len(entries) != 0 {
		return zero, errors.New("target_has_unmarked_data")
	} else if err != nil && !os.IsNotExist(err) {
		return zero, errors.New("target_unavailable")
	}
	stage := target + ".importing"
	identity := sha256.Sum256([]byte(source + "\x00" + target))
	token := hex.EncodeToString(identity[:])
	if _, err := os.Lstat(stage); err == nil {
		data, readErr := os.ReadFile(filepath.Join(stage, owner))
		if readErr != nil || string(data) != token {
			return zero, errors.New("unknown_staging_directory")
		}
		if err = os.RemoveAll(stage); err != nil {
			return zero, errors.New("staging_cleanup_failed")
		}
	}
	if err = os.Mkdir(stage, 0700); err != nil {
		return zero, errors.New("staging_create_failed")
	}
	if err = os.WriteFile(filepath.Join(stage, owner), []byte(token), 0600); err != nil {
		return zero, errors.New("staging_owner_failed")
	}
	if err = call(o, "staging"); err != nil {
		return zero, err
	}
	imported := false
	if info, err := os.Lstat(source); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return zero, errors.New("source_not_directory")
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return zero, errors.New("source_unavailable")
		}
		imported = len(entries) > 0
		var db *bolt.DB
		if _, err = os.Stat(filepath.Join(source, "gopeed.db")); err == nil {
			db, err = bolt.Open(filepath.Join(source, "gopeed.db"), 0600, &bolt.Options{ReadOnly: true, Timeout: time.Millisecond * 100})
			if err != nil {
				return zero, errors.New("legacy_database_locked_or_invalid")
			}
			defer db.Close()
		}
		if err = copyTree(source, stage); err != nil {
			return zero, err
		}
		if db != nil {
			if err = db.View(func(tx *bolt.Tx) error { return tx.CopyFile(filepath.Join(stage, "gopeed.db"), 0600) }); err != nil {
				return zero, errors.New("database_snapshot_failed")
			}
		}
		before, err := summarize(source, imported)
		if err != nil {
			return zero, err
		}
		after, err := summarize(stage, imported)
		if err != nil {
			return zero, err
		}
		if before != after {
			return zero, errors.New("snapshot_verification_failed")
		}
	} else if !os.IsNotExist(err) {
		return zero, errors.New("source_unavailable")
	}
	summary, err := summarize(stage, imported)
	if err != nil {
		return zero, err
	}
	data, _ := json.Marshal(summary)
	if err = writeSync(filepath.Join(stage, pending), data); err != nil {
		return zero, err
	}
	if err = syncDir(stage); err != nil {
		return zero, err
	}
	if err = call(o, "verified"); err != nil {
		return zero, err
	}
	if _, err = os.Stat(target); err == nil {
		if err = os.Remove(target); err != nil {
			return zero, errors.New("target_changed")
		}
	}
	if err = os.Rename(stage, target); err != nil {
		return zero, errors.New("publish_failed")
	}
	if err = syncDir(filepath.Dir(target)); err != nil {
		return zero, err
	}
	if err = call(o, "published"); err != nil {
		return zero, err
	}
	if err = publishMarker(target); err != nil {
		return zero, err
	}
	return summary, nil
}
func publishMarker(root string) error {
	if err := os.Remove(filepath.Join(root, owner)); err != nil && !os.IsNotExist(err) {
		return errors.New("staging_owner_cleanup_failed")
	}
	if err := os.Rename(filepath.Join(root, pending), filepath.Join(root, Marker)); err != nil {
		return errors.New("marker_publish_failed")
	}
	return syncDir(root)
}
func writeSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("manifest_write_failed")
	}
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return errors.New("manifest_write_failed")
	}
	if f.Sync() != nil {
		return errors.New("manifest_sync_failed")
	}
	return nil
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("directory_sync_failed")
	}
	defer f.Close()
	if f.Sync() != nil {
		return errors.New("directory_sync_failed")
	}
	return nil
}
func skipped(rel string) bool {
	base := filepath.Base(rel)
	return rel == Marker || rel == pending || rel == owner || rel == "ipc" || rel == "logs" || rel == "host" || rel == "kownload-native-host" || base == ".DS_Store" || strings.HasSuffix(base, ".sock") || strings.HasSuffix(base, ".lock") || strings.HasSuffix(base, ".log")
}
func copyTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return errors.New("source_read_failed")
		}
		rel, _ := filepath.Rel(source, path)
		if rel == "." {
			return nil
		}
		if skipped(rel) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.Type()&os.ModeSymlink != 0 {
			return errors.New("source_contains_symlink")
		}
		dst := filepath.Join(target, rel)
		if e.IsDir() {
			if os.Mkdir(dst, 0700) != nil {
				return errors.New("directory_copy_failed")
			}
			return nil
		}
		if !e.Type().IsRegular() {
			return errors.New("source_contains_special_file")
		}
		if rel == "gopeed.db" {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return errors.New("file_copy_failed")
		}
		defer in.Close()
		info, err := e.Info()
		if err != nil {
			return errors.New("file_stat_failed")
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()&0700)
		if err != nil {
			return errors.New("file_copy_failed")
		}
		_, err = io.Copy(out, in)
		syncErr := out.Sync()
		closeErr := out.Close()
		if err != nil || syncErr != nil || closeErr != nil {
			return errors.New("file_copy_failed")
		}
		return nil
	})
}
func summarize(root string, imported bool) (Summary, error) {
	s := Summary{Version: 1, Imported: imported}
	var records []string
	err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return errors.New("verification_read_failed")
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		if skipped(rel) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() {
			records = append(records, "dir\x00"+filepath.ToSlash(rel))
			return nil
		}
		if !e.Type().IsRegular() {
			return errors.New("verification_special_file")
		}
		var digest string
		if rel == "gopeed.db" {
			var err error
			digest, err = databaseDigest(path)
			if err != nil {
				return err
			}
			s.DatabaseDigest = digest
		} else {
			f, err := os.Open(path)
			if err != nil {
				return errors.New("verification_read_failed")
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return errors.New("verification_read_failed")
			}
			digest = hex.EncodeToString(h.Sum(nil))
		}
		s.Files++
		records = append(records, filepath.ToSlash(rel)+"\x00"+digest)
		return nil
	})
	if err != nil {
		return s, err
	}
	sort.Strings(records)
	h := sha256.New()
	for _, r := range records {
		fmt.Fprintf(h, "%d:%s", len(r), r)
	}
	s.Digest = hex.EncodeToString(h.Sum(nil))
	return s, nil
}
func databaseDigest(path string) (string, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Millisecond * 100})
	if err != nil {
		return "", errors.New("verification_database_locked_or_invalid")
	}
	defer db.Close()
	h := sha256.New()
	var walk func(*bolt.Bucket) error
	walk = func(b *bolt.Bucket) error {
		fmt.Fprintf(h, "sequence:%d;", b.Sequence())
		return b.ForEach(func(k, v []byte) error {
			fmt.Fprintf(h, "key:%d:", len(k))
			h.Write(k)
			if v == nil {
				h.Write([]byte("bucket"))
				return walk(b.Bucket(k))
			}
			fmt.Fprintf(h, "value:%d:", len(v))
			h.Write(v)
			return nil
		})
	}
	err = db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(k []byte, b *bolt.Bucket) error { fmt.Fprintf(h, "root:%d:", len(k)); h.Write(k); return walk(b) })
	})
	if err != nil {
		return "", errors.New("verification_database_failed")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
