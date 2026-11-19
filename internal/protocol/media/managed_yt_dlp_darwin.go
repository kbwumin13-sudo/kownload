package media

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode"

	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
	"github.com/GopeedLab/gopeed/pkg/util"
	"golang.org/x/sys/unix"
)

const managedNamespace = "io.github.kbwumin13-sudo.kownload"

type managedYtDlp struct {
	root   string
	fetch  func(context.Context) (managedRelease, error)
	rename func(string, string) error
}

type managedVersion struct {
	Tag     string   `json:"tag"`
	Digest  string   `json:"digest"`
	Source  string   `json:"source"`
	Curl    string   `json:"curl"`
	Targets []string `json:"targets"`
}

type managedSelection struct {
	active, previous string
}

var managedVersionDir = regexp.MustCompile(`^version-[0-9]{4}\.[0-9]{2}\.[0-9]{2}-[0-9]+$`)
var managedSlotDir = regexp.MustCompile(`^slot-[0-9]+$`)

func NewManagedYtDlp(storageDir, socket string) ManagedYtDlp {
	if !filepath.IsAbs(storageDir) ||
		filepath.Join(storageDir, "ipc", "api.sock") != socket {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil || !filepath.IsAbs(home) || home == "/" {
		return nil
	}
	storage, err := filepath.EvalSymlinks(storageDir)
	if err != nil {
		return nil
	}
	scope := sha256.Sum256([]byte(storage))
	return newManagedYtDlp(filepath.Join(home, ".local", "share", managedNamespace, fmt.Sprintf("media-%x", scope[:12])))
}

func newManagedYtDlp(root string) *managedYtDlp {
	return &managedYtDlp{root: root, fetch: officialYtDlp, rename: os.Rename}
}

func (m *managedYtDlp) entry() string {
	return filepath.Join(m.root, "current", "active", "yt-dlp")
}

func (m *managedYtDlp) State(stored any) (ManagedYtDlpState, error) {
	cfg, err := managedMediaConfig(stored)
	if err != nil {
		return ManagedYtDlpState{}, err
	}
	if err := m.ensureRoot(false); errors.Is(err, os.ErrNotExist) {
		return ManagedYtDlpState{}, nil
	} else if err != nil {
		return ManagedYtDlpState{}, err
	}
	selection, err := m.selection()
	if err != nil {
		return ManagedYtDlpState{}, err
	}
	return m.state(cfg, selection)
}

func managedMediaConfig(stored any) (model.Config, error) {
	cfg := configDefaults()
	err := util.MapToStruct(stored, &cfg)
	return effectiveConfig(cfg), err
}

func (m *managedYtDlp) state(cfg model.Config, selection managedSelection) (ManagedYtDlpState, error) {
	if selection.active == "" {
		return ManagedYtDlpState{}, nil
	}
	active, err := m.version(selection.active)
	if err != nil {
		return ManagedYtDlpState{}, err
	}
	state := ManagedYtDlpState{Entry: m.entry(), ActiveVersion: active.Tag, InUse: cfg.YtDlpPath == m.entry()}
	if selection.previous != "" {
		previous, err := m.version(selection.previous)
		if err != nil {
			return ManagedYtDlpState{}, err
		}
		state.PreviousVersion = previous.Tag
	}
	return state, nil
}

func (m *managedYtDlp) Update(ctx context.Context, stored any) (ManagedYtDlpState, error) {
	return m.change(stored, true, func(cfg model.Config, current managedSelection) (managedSelection, error) {
		var oldVersion managedVersion
		if current.active != "" {
			var err error
			oldVersion, err = m.version(current.active)
			if err != nil {
				return current, err
			}
		}
		var source string
		if cfg.YtDlpPath == m.entry() {
			source = oldVersion.Source
		} else {
			if len(cfg.YtDlpPath) > maxToolPath {
				return current, ErrManagedYtDlpPython
			}
			if _, state := executableToolPath(cfg.YtDlpPath); state != toolReady {
				return current, ErrManagedYtDlpPython
			}
			var err error
			source, err = mediaBridgeInterpreter(cfg)
			if err != nil {
				return current, ErrManagedYtDlpPython
			}
		}
		if !trustedManagedPython(source) || pathUnder(m.root, source) {
			return current, ErrManagedYtDlpPython
		}

		// Check locally before any request. An unavailable or incompatible
		// existing Python never triggers an official release fetch.
		sourceInfo, err := managedPythonInfo(ctx, source, true)
		if err != nil {
			return current, err
		}
		release, err := m.fetch(ctx)
		if err != nil {
			return current, err
		}
		if release.version < oldVersion.Tag {
			return current, ErrManagedYtDlpOlder
		}
		if release.version == oldVersion.Tag && release.digest != oldVersion.Digest {
			return current, ErrManagedYtDlpAsset
		}
		if release.version == oldVersion.Tag && oldVersion.Source == source {
			// An identical release and Python selection is idempotent. Recheck
			// compatibility in case the external Python changed in place.
			return current, verifyManagedPython(ctx, m.releasePath(current.active), oldVersion)
		}
		// The directory is unpublished until the single current pointer moves.
		dir, err := os.MkdirTemp(filepath.Join(m.root, "releases"), "version-"+release.version+"-")
		if err != nil {
			return current, ErrManagedYtDlpLocation
		}
		keep := false
		defer func() {
			if !keep {
				discardManagedVersion(dir)
			}
		}()
		version, err := prepareManagedPython(ctx, dir, release, source, sourceInfo)
		if err != nil {
			return current, err
		}
		if !includesTargets(version.Targets, sourceInfo.Targets) ||
			!includesTargets(version.Targets, oldVersion.Targets) {
			return current, ErrManagedYtDlpPython
		}
		if err := writeManagedVersion(dir, version); err != nil {
			return current, err
		}
		if err := verifyManagedPython(ctx, dir, version); err != nil {
			return current, err
		}
		if err := freezeManagedVersion(dir); err != nil {
			return current, ErrManagedYtDlpLocation
		}
		if err := ctx.Err(); err != nil {
			return current, err
		}
		next := managedSelection{active: filepath.Base(dir), previous: current.active}
		if err := m.publish(next); err != nil {
			return current, err
		}
		keep = true
		return next, nil
	})
}

func (m *managedYtDlp) Rollback(ctx context.Context, stored any) (ManagedYtDlpState, error) {
	return m.change(stored, false, func(cfg model.Config, current managedSelection) (managedSelection, error) {
		if current.previous == "" {
			return current, ErrManagedYtDlpNoPrevious
		}
		previous, err := m.version(current.previous)
		if err != nil {
			return current, err
		}
		if err := verifyManagedPython(ctx, m.releasePath(current.previous), previous); err != nil {
			return current, err
		}
		if err := ctx.Err(); err != nil {
			return current, err
		}
		next := managedSelection{active: current.previous, previous: current.active}
		if err := m.publish(next); err != nil {
			return current, err
		}
		return next, nil
	})
}

func (m *managedYtDlp) change(stored any, create bool, op func(model.Config, managedSelection) (managedSelection, error)) (ManagedYtDlpState, error) {
	cfg, err := managedMediaConfig(stored)
	if err != nil {
		return ManagedYtDlpState{}, err
	}
	if err := m.ensureRoot(create); err != nil {
		if !create && errors.Is(err, os.ErrNotExist) {
			return ManagedYtDlpState{}, ErrManagedYtDlpNoPrevious
		}
		return ManagedYtDlpState{}, err
	}
	// Lock the private directory itself. The lock survives concurrent Apps or
	// test instances and never races with removal of a separate lock file.
	fd, err := unix.Open(m.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return ManagedYtDlpState{}, ErrManagedYtDlpLocation
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return ManagedYtDlpState{}, ErrManagedYtDlpBusy
	}
	for _, name := range []string{"releases", "slots"} {
		if err := ownedMediaDirectory(filepath.Join(m.root, name), create, true); err != nil {
			return ManagedYtDlpState{}, err
		}
	}
	current, err := m.selection()
	if err != nil {
		return ManagedYtDlpState{}, err
	}
	if _, err := m.state(cfg, current); err != nil {
		return ManagedYtDlpState{}, err
	}
	next, err := op(cfg, current)
	if err != nil {
		return ManagedYtDlpState{}, err
	}
	return m.state(cfg, next)
}

func (m *managedYtDlp) ensureRoot(create bool) error {
	if !validShebangPath(m.root) {
		return ErrManagedYtDlpLocation
	}
	home := m.root
	// The managed directory is four components beneath the physical HOME.
	for range 4 {
		home = filepath.Dir(home)
	}
	if home == "/" {
		return ErrManagedYtDlpLocation
	}
	if err := checkOwnedPath(home, true, false); err != nil {
		return err
	}
	relative, _ := filepath.Rel(home, m.root)
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		home = filepath.Join(home, part)
		if err := ownedMediaDirectory(home, create, home == m.root); err != nil {
			return err
		}
	}
	return nil
}

func validShebangPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path &&
		!strings.ContainsRune(path, 0) && strings.IndexFunc(path, unicode.IsSpace) == -1
}

func ownedMediaDirectory(path string, create, private bool) error {
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return ErrManagedYtDlpLocation
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.ErrNotExist
	}
	if err != nil || !info.IsDir() || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) ||
		info.Mode().Perm()&0022 != 0 || private && info.Mode().Perm() != 0700 {
		return ErrManagedYtDlpLocation
	}
	return nil
}

func checkOwnedPath(path string, leafDirectory, pythonRuntime bool) error {
	leaf := true
	for {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm()&0002 != 0 {
			return ErrManagedYtDlpLocation
		}
		stat := info.Sys().(*syscall.Stat_t)
		uid := stat.Uid
		if uid != 0 && uid != uint32(os.Geteuid()) {
			return ErrManagedYtDlpLocation
		}
		// A selected Homebrew runtime can use the macOS admin group (80).
		// This exception never applies to the application's write tree.
		if info.Mode().Perm()&0020 != 0 && !(pythonRuntime && stat.Gid == 80 &&
			(pathUnder("/opt/homebrew", path) || pathUnder("/usr/local", path))) {
			return ErrManagedYtDlpLocation
		}
		if leaf && !leafDirectory {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				return ErrManagedYtDlpLocation
			}
		} else if !info.IsDir() {
			return ErrManagedYtDlpLocation
		}
		next := filepath.Dir(path)
		if next == path {
			return nil
		}
		leaf, path = false, next
	}
}

func trustedManagedPython(source string) bool {
	if !filepath.IsAbs(source) || strings.ContainsAny(source, "\x00\r\n") {
		return false
	}
	// Check both trees. A user-selected venv often links its executable to a
	// package-manager interpreter, and its own pyvenv.cfg defines capabilities.
	if checkOwnedPath(filepath.Dir(source), true, true) != nil {
		return false
	}
	physical, err := filepath.EvalSymlinks(source)
	return err == nil && checkOwnedPath(physical, false, true) == nil
}

func pathUnder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (m *managedYtDlp) releasePath(name string) string {
	return filepath.Join(m.root, "releases", name)
}

func (m *managedYtDlp) selection() (managedSelection, error) {
	var selection managedSelection
	slot, err := os.Readlink(filepath.Join(m.root, "current"))
	if errors.Is(err, os.ErrNotExist) {
		return selection, nil
	}
	name := strings.TrimPrefix(slot, "slots/")
	if err != nil || slot != "slots/"+name || !managedSlotDir.MatchString(name) {
		return selection, ErrManagedYtDlpLocation
	}
	if err := ownedMediaDirectory(filepath.Join(m.root, "slots"), false, true); err != nil {
		return selection, ErrManagedYtDlpLocation
	}
	if err := ownedMediaDirectory(filepath.Join(m.root, "releases"), false, true); err != nil {
		return selection, ErrManagedYtDlpLocation
	}
	slot = filepath.Join(m.root, "slots", name)
	if !immutableManagedDir(slot) {
		return selection, ErrManagedYtDlpLocation
	}
	active, err := readManagedSlot(slot, "active", false)
	if err != nil {
		return selection, err
	}
	previous, err := readManagedSlot(slot, "previous", true)
	if err != nil || active == previous {
		return selection, ErrManagedYtDlpLocation
	}
	return managedSelection{active: active, previous: previous}, nil
}

func readManagedSlot(slot, link string, optional bool) (string, error) {
	target, err := os.Readlink(filepath.Join(slot, link))
	if optional && errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	const prefix = "../../releases/"
	name := strings.TrimPrefix(target, prefix)
	if err != nil || target != prefix+name || !managedVersionDir.MatchString(name) {
		return "", ErrManagedYtDlpLocation
	}
	return name, nil
}

func (m *managedYtDlp) publish(next managedSelection) error {
	slot, err := os.MkdirTemp(filepath.Join(m.root, "slots"), "slot-")
	if err != nil {
		return ErrManagedYtDlpLocation
	}
	keep := false
	defer func() {
		if !keep {
			discardManagedVersion(slot)
		}
	}()
	if err = os.Symlink("../../releases/"+next.active, filepath.Join(slot, "active")); err != nil {
		return ErrManagedYtDlpLocation
	}
	if next.previous != "" {
		if err := os.Symlink("../../releases/"+next.previous, filepath.Join(slot, "previous")); err != nil {
			return ErrManagedYtDlpLocation
		}
	}
	if err := os.Chmod(slot, 0500); err != nil {
		return ErrManagedYtDlpLocation
	}
	stage, err := os.MkdirTemp(m.root, ".pointer-")
	if err != nil {
		return ErrManagedYtDlpLocation
	}
	defer os.RemoveAll(stage)
	link := filepath.Join(stage, "current")
	if err := os.Symlink("slots/"+filepath.Base(slot), link); err != nil {
		return ErrManagedYtDlpLocation
	}
	if err := m.rename(link, filepath.Join(m.root, "current")); err != nil {
		return ErrManagedYtDlpLocation
	}
	keep = true
	return nil
}

func immutableManagedDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode().Perm() == 0500 &&
		info.Sys().(*syscall.Stat_t).Uid == uint32(os.Geteuid())
}

func (m *managedYtDlp) version(name string) (managedVersion, error) {
	var version managedVersion
	dir := m.releasePath(name)
	if !managedVersionDir.MatchString(name) || !immutableManagedDir(dir) {
		return version, ErrManagedYtDlpLocation
	}
	path := filepath.Join(dir, "release.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 ||
		info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || info.Sys().(*syscall.Stat_t).Nlink != 1 ||
		info.Size() > maxToolOutput*4 {
		return version, ErrManagedYtDlpLocation
	}
	f, err := os.Open(path)
	if err != nil {
		return version, ErrManagedYtDlpLocation
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(io.LimitReader(f, maxToolOutput*4+1)))
	if err := dec.Decode(&version); err != nil || !validManagedVersion(version.Tag) ||
		!managedDigest.MatchString(version.Digest) || !strings.HasPrefix(name, "version-"+version.Tag+"-") ||
		!filepath.IsAbs(version.Source) || version.Curl == "" || len(version.Targets) == 0 {
		return version, ErrManagedYtDlpLocation
	}
	return version, nil
}

func writeManagedVersion(dir string, version managedVersion) error {
	data, err := json.Marshal(version)
	if err != nil || len(data) > maxToolOutput*4 {
		return ErrManagedYtDlpLocation
	}
	if err := os.WriteFile(filepath.Join(dir, "release.json"), data, 0600); err != nil {
		return ErrManagedYtDlpLocation
	}
	return nil
}

func freezeManagedVersion(dir string) error {
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0500)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
			return ErrManagedYtDlpLocation
		}
		if info.Mode().Perm()&0111 != 0 {
			return os.Chmod(path, 0500)
		}
		return os.Chmod(path, 0400)
	})
}

func discardManagedVersion(dir string) {
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			return os.Chmod(path, 0700)
		}
		return err
	})
	_ = os.RemoveAll(dir)
}
