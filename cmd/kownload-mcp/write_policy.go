//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

var errWritePolicy = errors.New("invalid write policy")
var writePolicyID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var writePolicyDatasetID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// This local policy is only one prerequisite for future writes. It does not
// establish Core capabilities, network target safety, filesystem identity,
// no-replace publication, or permission to bypass a receiver's required guard.
// No caller in the executable loads or uses it yet; tools remain read-only.
type writePolicy struct {
	operations map[string]bool
	datasets   map[string]writeDatasetScope
	roots      map[string]string
	effects    []string
}
type writeDatasetScope struct {
	tasks map[string]bool
	roots map[string]bool
}
type writeCreateGrant struct {
	outputRoot string
	effects    []string
}

// All members are exact-case, unknown/duplicate keys fail closed. Empty explicit
// scopes authorize nothing. Omitted allowedEffects means an empty whitelist.
func parseWritePolicy(data []byte) (*writePolicy, error) {
	if len(data) == 0 || len(data) > maxTokenFile || !utf8.Valid(data) || rejectDuplicateKeys(data) != nil {
		return nil, errWritePolicy
	}
	top, err := writePolicyObject(data, []string{"version", "operations", "datasets", "outputRoots"}, "allowedEffects")
	if err != nil || !bytes.Equal(bytes.TrimSpace(top["version"]), []byte("1")) {
		return nil, errWritePolicy
	}
	ops, err := writePolicyObject(top["operations"], nil, "create", "pause", "resume")
	if err != nil {
		return nil, errWritePolicy
	}
	p := &writePolicy{operations: map[string]bool{}, datasets: map[string]writeDatasetScope{}, roots: map[string]string{}}
	for key, raw := range ops {
		switch string(bytes.TrimSpace(raw)) {
		case "true":
			p.operations[key] = true
		case "false":
			p.operations[key] = false
		default:
			return nil, errWritePolicy
		}
	}
	var roots map[string]json.RawMessage
	if json.Unmarshal(top["outputRoots"], &roots) != nil || roots == nil || len(roots) > 16 {
		return nil, errWritePolicy
	}
	for name, raw := range roots {
		var path string
		if !writePolicyID.MatchString(name) || json.Unmarshal(raw, &path) != nil || !canonical(path) || len(path) > 1024 || !writePolicyText(path) {
			return nil, errWritePolicy
		}
		p.roots[name] = path // Lexical only; no stat, mkdir, symlink resolution or repair.
	}
	var datasets []json.RawMessage
	if json.Unmarshal(top["datasets"], &datasets) != nil || datasets == nil || len(datasets) > 32 {
		return nil, errWritePolicy
	}
	for _, raw := range datasets {
		scope, e := writePolicyObject(raw, []string{"id", "taskIds", "outputRoots"})
		if e != nil {
			return nil, errWritePolicy
		}
		var id string
		if json.Unmarshal(scope["id"], &id) != nil || !writePolicyDatasetID.MatchString(id) {
			return nil, errWritePolicy
		}
		if _, exists := p.datasets[id]; exists {
			return nil, errWritePolicy
		}
		tasks, e := writePolicyList(scope["taskIds"], 64, writePolicyID.MatchString)
		if e != nil {
			return nil, errWritePolicy
		}
		names, e := writePolicyList(scope["outputRoots"], 16, func(s string) bool { _, ok := p.roots[s]; return ok })
		if e != nil {
			return nil, errWritePolicy
		}
		d := writeDatasetScope{tasks: map[string]bool{}, roots: map[string]bool{}}
		for _, task := range tasks {
			d.tasks[task] = true
		}
		for _, name := range names {
			d.roots[name] = true
		}
		p.datasets[id] = d
	}
	if raw, ok := top["allowedEffects"]; ok {
		p.effects, err = writePolicyList(raw, 4, func(s string) bool {
			switch s {
			case "script", "auto_open", "webhook", "post_process":
				return true
			}
			return false
		})
		if err != nil {
			return nil, errWritePolicy
		}
	}
	return p, nil
}

func writePolicyText(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
	}
	return true
}
func writePolicyObject(data []byte, required []string, optional ...string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return nil, errWritePolicy
	}
	allowed := map[string]bool{}
	for _, k := range required {
		if _, ok := obj[k]; !ok {
			return nil, errWritePolicy
		}
		allowed[k] = true
	}
	for _, k := range optional {
		allowed[k] = true
	}
	for k := range obj {
		if !allowed[k] {
			return nil, errWritePolicy
		}
	}
	return obj, nil
}
func writePolicyList(data []byte, max int, valid func(string) bool) ([]string, error) {
	var values []string
	if json.Unmarshal(data, &values) != nil || values == nil || len(values) > max {
		return nil, errWritePolicy
	}
	seen := map[string]bool{}
	for _, v := range values {
		if !valid(v) || seen[v] {
			return nil, errWritePolicy
		}
		seen[v] = true
	}
	return values, nil
}

// Returns local-policy permission only, never an executable write command.
// Effects come exclusively from the trusted policy, not model parameters.
func (p *writePolicy) allowCreate(dataset, rootName string) (writeCreateGrant, bool) {
	if p == nil || !p.operations["create"] || !p.datasets[dataset].roots[rootName] {
		return writeCreateGrant{}, false
	}
	path, ok := p.roots[rootName]
	if !ok {
		return writeCreateGrant{}, false
	}
	return writeCreateGrant{outputRoot: path, effects: append([]string(nil), p.effects...)}, true
}
func (p *writePolicy) allowTask(operation, dataset, task string) bool {
	return p != nil && (operation == "pause" || operation == "resume") && p.operations[operation] && p.datasets[dataset].tasks[task]
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
