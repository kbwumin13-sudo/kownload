package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const maxTokenFile = 4096

var errTokenFile = errors.New("invalid API token file")

// readAPITokenFile reads only the explicitly supplied startup path. Walk via
// directory descriptors so a symlink replacement cannot redirect a later open.
// No discovery, database access, chmod, creation, or automatic token export.
func readAPITokenFile(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return "", errTokenFile
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", errTokenFile
	}
	defer func() { unix.Close(fd) }()
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	var expected unix.Stat_t
	for i, part := range parts {
		var dir unix.Stat_t
		if unix.Fstat(fd, &dir) != nil || (dir.Uid != 0 && int(dir.Uid) != os.Getuid()) {
			return "", errTokenFile
		}
		last := i == len(parts)-1
		if last {
			if int(dir.Uid) != os.Getuid() || dir.Mode&0077 != 0 {
				return "", errTokenFile
			}
			if unix.Fstatat(fd, part, &expected, unix.AT_SYMLINK_NOFOLLOW) != nil || !privateTokenFile(&expected) {
				return "", errTokenFile
			}
		} else if dir.Mode&0022 != 0 && dir.Mode&unix.S_ISVTX == 0 {
			return "", errTokenFile
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if last {
			// Opening a FIFO or device must not block before the regular-file check.
			flags |= unix.O_NONBLOCK
		} else {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		if err != nil {
			return "", errTokenFile
		}
		unix.Close(fd)
		fd = next
	}
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || !privateTokenFile(&stat) || stat.Dev != expected.Dev || stat.Ino != expected.Ino {
		return "", errTokenFile
	}
	f := os.NewFile(uintptr(fd), "api-token")
	if f == nil {
		return "", errTokenFile
	}
	fd = -1 // os.File now owns it; no second close of a potentially reused FD.
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	if err != nil || len(data) > maxTokenFile {
		return "", errTokenFile
	}
	// Permit one terminal LF from a text editor, but never trim or transform a
	// token otherwise. Reject whitespace, control bytes, CRLF and multiple lines.
	data = bytes.TrimSuffix(data, []byte{'\n'})
	if len(data) == 0 {
		return "", errTokenFile
	}
	for _, b := range data {
		if b < 0x21 || b > 0x7e {
			return "", errTokenFile
		}
	}
	return string(data), nil
}

func privateTokenFile(stat *unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && int(stat.Uid) == os.Getuid() &&
		stat.Nlink == 1 && (stat.Mode&07777 == 0400 || stat.Mode&07777 == 0600) &&
		stat.Size >= 0 && stat.Size <= maxTokenFile
}
