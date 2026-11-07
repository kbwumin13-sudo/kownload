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

// Fingerprint identifies the legacy database bytes an import was taken from.
// Only the legacy app writes that file after a completed import (it commits a
// transaction whenever it starts), so a different fingerprint means the old
// version ran on its root after the migration.
type Fingerprint struct {
	Present bool   `json:"present"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256,omitempty"`
}

// record is the marker and pending-manifest format. Markers written before
// fingerprints existed decode with a nil SourceDatabase and are not compared.
type record struct {
	Summary
	SourceDatabase *Fingerprint `json:"sourceDatabase,omitempty"`
}

// ErrLegacyChanged reports that the legacy root changed after the import, so
// opening the imported copy would silently use outdated data.
var ErrLegacyChanged = errors.New("legacy_changed_after_import")

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
		var done record
		if json.Unmarshal(data, &done) != nil || done.Version != 1 {
			return zero, errors.New("invalid_marker")
		}
		if err = sourceUnchanged(source, done.SourceDatabase); err != nil {
			return zero, err
		}
		return done.Summary, nil
	}
	// A crash between the atomic rename and final marker can finish only after
	// verifying the previously verified tree; arbitrary existing data is refused.
	if data, err := os.ReadFile(filepath.Join(target, pending)); err == nil {
		var s record
		if json.Unmarshal(data, &s) != nil {
			return zero, errors.New("invalid_pending_import")
		}
		actual, err := summarize(target, s.Imported)
		if err != nil || actual != s.Summary {
			return zero, errors.New("pending_verification_failed")
		}
		if err = sourceUnchanged(source, s.SourceDatabase); err != nil {
			return zero, err
		}
		if err = publishMarker(target); err != nil {
			return zero, err
		}
		return s.Summary, nil
	}
	if entries, err := os.ReadDir(target); err == nil && len(entries) != 0 {
		return zero, errors.New("target_has_unmarked_data")
	} else if err != nil && !os.IsNotExist(err) {
		return zero, errors.New("target_unavailable")
	}
	stage := target + ".importing"
	identity := sha256.Sum256([]byte(source + "\x00" + target))
	token := hex.EncodeToString(identity[:])
	// Scratch names are created only by this importer while it holds the
	// migration lock, so any that exist belong to an interrupted run.
	sweepScratch(target)
	if info, err := os.Lstat(stage); err == nil {
		if !info.IsDir() {
			return zero, errors.New("unknown_staging_directory")
		}
		// Only this importer creates the staging name, and the checks above
		// established that nothing was published: no marker, no pending
		// manifest, and the target is missing or empty. The importer never
		// writes the source, so the staging copy is disposable even when its
		// owner token is missing or different (older versions created the
		// directory before writing the token; a crash could also interrupt a
		// cleanup). The migration lock excludes a concurrent import.
		if err = discardStage(stage, target); err != nil {
			return zero, errors.New("staging_cleanup_failed")
		}
	} else if !os.IsNotExist(err) {
		return zero, errors.New("staging_unavailable")
	}
	// Write the owner token under a scratch name first, then rename: the
	// staging path never exists without its token.
	scratch, err := os.MkdirTemp(filepath.Dir(target), filepath.Base(target)+scratchNew)
	if err != nil {
		return zero, errors.New("staging_create_failed")
	}
	if err = writeSync(filepath.Join(scratch, owner), []byte(token)); err != nil {
		os.RemoveAll(scratch)
		return zero, errors.New("staging_owner_failed")
	}
	if err = call(o, "staging-scratch"); err != nil {
		return zero, err
	}
	if err = os.Rename(scratch, stage); err != nil {
		os.RemoveAll(scratch)
		return zero, errors.New("staging_create_failed")
	}
	if err = syncDir(filepath.Dir(target)); err != nil {
		return zero, err
	}
	if err = call(o, "staging"); err != nil {
		return zero, err
	}
	imported := false
	fingerprint := Fingerprint{}
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
			// The read-only store lock is held until Import returns, so the
			// legacy app cannot write between this fingerprint and the copy.
			if fingerprint, err = fingerprintFile(filepath.Join(source, "gopeed.db")); err != nil {
				return zero, errors.New("source_read_failed")
			}
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
	data, _ := json.Marshal(record{Summary: summary, SourceDatabase: &fingerprint})
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
func fingerprintFile(path string) (Fingerprint, error) {
	f, err := os.Open(path)
	if err != nil {
		return Fingerprint{}, err
	}
	defer f.Close()
	return fingerprintOpen(f)
}

func fingerprintOpen(f *os.File) (Fingerprint, error) {
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Fingerprint{}, err
	}
	return Fingerprint{Present: true, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// sourceUnchanged refuses to reopen an import whose legacy root was used
// afterwards, for example by build 25 after a rollback. It never changes
// either root; the caller shows how to keep one side.
func sourceUnchanged(source string, recorded *Fingerprint) error {
	if recorded == nil {
		return nil
	}
	f, err := os.Open(filepath.Join(source, "gopeed.db"))
	if err != nil {
		// A missing database means the legacy app has not run on this root
		// since (it creates the file on start); moving the legacy root aside is
		// the documented way to keep the imported data. An unreadable file
		// cannot have been used by the legacy app either.
		return nil
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil {
		// The running legacy app holds its exclusive store lock, so its state
		// cannot be compared now; startup keeps its previous behaviour.
		return nil
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if !recorded.Present {
		return ErrLegacyChanged
	}
	actual, err := fingerprintOpen(f)
	if err != nil {
		return nil
	}
	if actual != *recorded {
		return ErrLegacyChanged
	}
	return nil
}

// Scratch suffixes appended to the target name. ".importing-new-" holds an
// owner token before it becomes the staging directory; ".importing-old-" is a
// staging directory renamed aside so that deleting it can never leave a
// half-removed staging path behind.
const (
	scratchNew = ".importing-new-"
	scratchOld = ".importing-old-"
)

func sweepScratch(target string) {
	parent, base := filepath.Dir(target), filepath.Base(target)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base+scratchNew) && !strings.HasPrefix(name, base+scratchOld) {
			continue
		}
		// Best effort: a leftover never blocks an import, it only uses space.
		if info, err := os.Lstat(filepath.Join(parent, name)); err == nil && info.IsDir() {
			os.RemoveAll(filepath.Join(parent, name))
		}
	}
}

func discardStage(stage, target string) error {
	aside, err := os.MkdirTemp(filepath.Dir(target), filepath.Base(target)+scratchOld)
	if err != nil {
		return err
	}
	if err = os.Remove(aside); err != nil {
		return err
	}
	if err = os.Rename(stage, aside); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(target)); err != nil {
		return err
	}
	return os.RemoveAll(aside)
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
