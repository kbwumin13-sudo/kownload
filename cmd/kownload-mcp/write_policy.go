//go:build darwin

package main

import (
	"io"
	"os"
	"path/filepath"

	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
	"golang.org/x/sys/unix"
)

var errWritePolicy = mcpwritepolicy.ErrInvalidPolicy

// Compatibility wrappers retain the existing private API; no flags or tools load it.
type writePolicy struct{ value *mcpwritepolicy.Policy }
type writeCreateGrant struct {
	outputRoot string
	effects    []string
}

func parseWritePolicy(data []byte) (*writePolicy, error) {
	value, err := mcpwritepolicy.Parse(data)
	if err != nil {
		return nil, err
	}
	return &writePolicy{value: value}, nil
}
func (p *writePolicy) allowCreate(dataset, rootName string) (writeCreateGrant, bool) {
	if p == nil {
		return writeCreateGrant{}, false
	}
	grant, ok := p.value.AllowCreate(dataset, rootName)
	return writeCreateGrant{outputRoot: grant.OutputRoot, effects: grant.Effects}, ok
}
func (p *writePolicy) allowTask(operation, dataset, task string) bool {
	return p != nil && p.value.AllowTask(operation, dataset, task)
}

// Explicit startup file only; not wired to flags or tools. Reuses the existing
// fd ancestor walk and exact 0400/0600 single-link, current-UID token-file rules.
// The immediate parent must be private. There is no discovery, chmod or reload.
func readWritePolicyFile(path string) (*writePolicy, error) {
	if !canonical(path) {
		return nil, errWritePolicy
	}
	dir, err := openInstallDir(filepath.Dir(path))
	if err != nil {
		return nil, errWritePolicy
	}
	defer unix.Close(dir)
	var parent, expected unix.Stat_t
	if unix.Fstat(dir, &parent) != nil || int(parent.Uid) != os.Getuid() || parent.Mode&0077 != 0 {
		return nil, errWritePolicy
	}
	name := filepath.Base(path)
	if unix.Fstatat(dir, name, &expected, unix.AT_SYMLINK_NOFOLLOW) != nil || !privateTokenFile(&expected) {
		return nil, errWritePolicy
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errWritePolicy
	}
	f := os.NewFile(uintptr(fd), "write-policy")
	if f == nil {
		unix.Close(fd)
		return nil, errWritePolicy
	}
	defer f.Close()
	var current unix.Stat_t
	if unix.Fstat(fd, &current) != nil || !privateTokenFile(&current) || !sameFile(&current, &expected) {
		return nil, errWritePolicy
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	if err != nil {
		return nil, errWritePolicy
	}
	return parseWritePolicy(data)
}
