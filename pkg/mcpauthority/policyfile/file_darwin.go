//go:build darwin

package policyfile

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
	"golang.org/x/sys/unix"
)

// LoadPolicyFile reads only the explicit path using the former nested loader's
// fd-based safety checks. It neither discovers a path, repairs permissions nor
// reloads the returned Policy. Missing/invalid files always fail; startup policy
// for an unspecified path belongs to the caller. No Authority is constructed.
func LoadPolicyFile(path string) (*mcpwritepolicy.Policy, error) {
	if !policyFileCanonical(path) {
		return nil, ErrInvalidPolicyFile
	}
	dir, err := openPolicyDirectory(filepath.Dir(path))
	if err != nil {
		return nil, ErrInvalidPolicyFile
	}
	defer unix.Close(dir)
	var parent, expected unix.Stat_t
	if unix.Fstat(dir, &parent) != nil || int(parent.Uid) != os.Getuid() || parent.Mode&0077 != 0 {
		return nil, ErrInvalidPolicyFile
	}
	name := filepath.Base(path)
	if unix.Fstatat(dir, name, &expected, unix.AT_SYMLINK_NOFOLLOW) != nil || !privatePolicyFile(&expected) {
		return nil, ErrInvalidPolicyFile
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrInvalidPolicyFile
	}
	f := os.NewFile(uintptr(fd), "write-policy")
	if f == nil {
		unix.Close(fd)
		return nil, ErrInvalidPolicyFile
	}
	defer f.Close()
	var current unix.Stat_t
	if unix.Fstat(fd, &current) != nil || !privatePolicyFile(&current) || !samePolicyFile(&current, &expected) {
		return nil, ErrInvalidPolicyFile
	}
	data, err := io.ReadAll(io.LimitReader(f, mcpwritepolicy.MaxBytes+1))
	if err != nil {
		return nil, ErrInvalidPolicyFile
	}
	return mcpwritepolicy.Parse(data)
}

func policyFileCanonical(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/"
}
func samePolicyFile(a, b *unix.Stat_t) bool { return a.Dev == b.Dev && a.Ino == b.Ino }
func trustedPolicyDirectory(s *unix.Stat_t) bool {
	return s.Mode&unix.S_IFMT == unix.S_IFDIR && (s.Uid == 0 || int(s.Uid) == os.Getuid()) && (s.Mode&0022 == 0 || s.Mode&unix.S_ISVTX != 0)
}

// Walk every component through directory descriptors. Never follow links or
// repair permissions; a sticky shared /private/tmp ancestor is allowed.
func openPolicyDirectory(path string) (int, error) {
	if !policyFileCanonical(path) {
		return -1, ErrInvalidPolicyFile
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, ErrInvalidPolicyFile
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return -1, ErrInvalidPolicyFile
		}
		fd = next
		var s unix.Stat_t
		if unix.Fstat(fd, &s) != nil || !trustedPolicyDirectory(&s) {
			unix.Close(fd)
			return -1, ErrInvalidPolicyFile
		}
	}
	return fd, nil
}

func privatePolicyFile(stat *unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && int(stat.Uid) == os.Getuid() &&
		stat.Nlink == 1 && (stat.Mode&07777 == 0400 || stat.Mode&07777 == 0600) &&
		stat.Size >= 0 && stat.Size <= mcpwritepolicy.MaxBytes
}
