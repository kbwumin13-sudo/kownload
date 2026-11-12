// Package mcpwritepolicy parses the local MCP write policy without granting Core capabilities.
package mcpwritepolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// MaxBytes preserves the existing trusted policy input bound.
const MaxBytes = 4096

// ErrInvalidPolicy is the fixed, redacted error for invalid input.
var ErrInvalidPolicy = errors.New("invalid write policy")
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var datasetIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// This local policy is only one prerequisite for future writes. It does not
// establish Core capabilities, network target safety, filesystem identity,
// no-replace publication, or permission to bypass a receiver's required guard.
// This package performs no filesystem or network access.
// Policy owns an immutable, default-deny set of local permissions.
// Its zero value and a nil pointer authorize nothing.
type Policy struct {
	operations map[string]bool
	datasets   map[string]datasetScope
	roots      map[string]string
	effects    []string
}
type datasetScope struct {
	tasks map[string]bool
	roots map[string]bool
}

// CreateGrant is an owned copy of a local-policy decision, not execution authority.
type CreateGrant struct {
	OutputRoot string
	Effects    []string
}

// Parse accepts version 1 only. All members are exact-case; unknown/duplicate keys fail closed. Empty explicit
// scopes authorize nothing. Omitted allowedEffects means an empty whitelist.
func Parse(data []byte) (*Policy, error) {
	if len(data) == 0 || len(data) > MaxBytes || !utf8.Valid(data) || rejectDuplicateKeys(data) != nil {
		return nil, ErrInvalidPolicy
	}
	top, err := object(data, []string{"version", "operations", "datasets", "outputRoots"}, "allowedEffects")
	if err != nil || !bytes.Equal(bytes.TrimSpace(top["version"]), []byte("1")) {
		return nil, ErrInvalidPolicy
	}
	ops, err := object(top["operations"], nil, "create", "pause", "resume")
	if err != nil {
		return nil, ErrInvalidPolicy
	}
	p := &Policy{operations: map[string]bool{}, datasets: map[string]datasetScope{}, roots: map[string]string{}}
	for key, raw := range ops {
		switch string(bytes.TrimSpace(raw)) {
		case "true":
			p.operations[key] = true
		case "false":
			p.operations[key] = false
		default:
			return nil, ErrInvalidPolicy
		}
	}
	var roots map[string]json.RawMessage
	if json.Unmarshal(top["outputRoots"], &roots) != nil || roots == nil || len(roots) > 16 {
		return nil, ErrInvalidPolicy
	}
	for name, raw := range roots {
		var path string
		if !idPattern.MatchString(name) || json.Unmarshal(raw, &path) != nil || !canonical(path) || len(path) > 1024 || !validText(path) {
			return nil, ErrInvalidPolicy
		}
		p.roots[name] = path // Lexical only; no stat, mkdir, symlink resolution or repair.
	}
	var datasets []json.RawMessage
	if json.Unmarshal(top["datasets"], &datasets) != nil || datasets == nil || len(datasets) > 32 {
		return nil, ErrInvalidPolicy
	}
	for _, raw := range datasets {
		scope, e := object(raw, []string{"id", "taskIds", "outputRoots"})
		if e != nil {
			return nil, ErrInvalidPolicy
		}
		var id string
		if json.Unmarshal(scope["id"], &id) != nil || !datasetIDPattern.MatchString(id) {
			return nil, ErrInvalidPolicy
		}
		if _, exists := p.datasets[id]; exists {
			return nil, ErrInvalidPolicy
		}
		tasks, e := list(scope["taskIds"], 64, idPattern.MatchString)
		if e != nil {
			return nil, ErrInvalidPolicy
		}
		names, e := list(scope["outputRoots"], 16, func(s string) bool { _, ok := p.roots[s]; return ok })
		if e != nil {
			return nil, ErrInvalidPolicy
		}
		d := datasetScope{tasks: map[string]bool{}, roots: map[string]bool{}}
		for _, task := range tasks {
			d.tasks[task] = true
		}
		for _, name := range names {
			d.roots[name] = true
		}
		p.datasets[id] = d
	}
	if raw, ok := top["allowedEffects"]; ok {
		p.effects, err = list(raw, 4, func(s string) bool {
			switch s {
			case "script", "auto_open", "webhook", "post_process":
				return true
			}
			return false
		})
		if err != nil {
			return nil, ErrInvalidPolicy
		}
	}
	return p, nil
}

func validText(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
	}
	return true
}
func object(data []byte, required []string, optional ...string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return nil, ErrInvalidPolicy
	}
	allowed := map[string]bool{}
	for _, k := range required {
		if _, ok := obj[k]; !ok {
			return nil, ErrInvalidPolicy
		}
		allowed[k] = true
	}
	for _, k := range optional {
		allowed[k] = true
	}
	for k := range obj {
		if !allowed[k] {
			return nil, ErrInvalidPolicy
		}
	}
	return obj, nil
}
func list(data []byte, max int, valid func(string) bool) ([]string, error) {
	var values []string
	if json.Unmarshal(data, &values) != nil || values == nil || len(values) > max {
		return nil, ErrInvalidPolicy
	}
	seen := map[string]bool{}
	for _, v := range values {
		if !valid(v) || seen[v] {
			return nil, ErrInvalidPolicy
		}
		seen[v] = true
	}
	return values, nil
}

// AllowCreate returns local-policy permission only, never an executable write command.
// Effects come exclusively from the trusted policy, not model parameters.
func (p *Policy) AllowCreate(dataset, rootName string) (CreateGrant, bool) {
	if p == nil || !p.operations["create"] || !p.datasets[dataset].roots[rootName] {
		return CreateGrant{}, false
	}
	path, ok := p.roots[rootName]
	if !ok {
		return CreateGrant{}, false
	}
	return CreateGrant{OutputRoot: path, Effects: append([]string(nil), p.effects...)}, true
}

// AllowTask checks only an explicit pause/resume permission and dataset/task scope.
func (p *Policy) AllowTask(operation, dataset, task string) bool {
	return p != nil && (operation == "pause" || operation == "resume") && p.operations[operation] && p.datasets[dataset].tasks[task]
}

// canonical preserves the original Darwin policy's POSIX lexical path rule.
// It neither resolves filesystem identities nor authorizes the named directory.
func canonical(value string) bool {
	return path.IsAbs(value) && path.Clean(value) == value && value != "/"
}

func rejectDuplicateKeys(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 16 {
			return ErrInvalidPolicy
		}
		token, err := d.Token()
		if err != nil {
			return ErrInvalidPolicy
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return ErrInvalidPolicy
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return ErrInvalidPolicy
				}
				seen[name] = true
				if value(depth+1) != nil {
					return ErrInvalidPolicy
				}
			}
		case '[':
			for d.More() {
				if value(depth+1) != nil {
					return ErrInvalidPolicy
				}
			}
		default:
			return ErrInvalidPolicy
		}
		_, err = d.Token()
		return err
	}
	if value(0) != nil {
		return ErrInvalidPolicy
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalidPolicy
	}
	return nil
}
