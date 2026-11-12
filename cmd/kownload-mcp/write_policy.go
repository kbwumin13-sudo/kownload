//go:build darwin

package main

import (
	"github.com/GopeedLab/gopeed/pkg/mcpauthority/policyfile"
	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
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

// Compatibility wrapper only; no startup flag or tool loads this file.
func readWritePolicyFile(path string) (*writePolicy, error) {
	value, err := policyfile.LoadPolicyFile(path)
	if err != nil {
		return nil, err
	}
	return &writePolicy{value: value}, nil
}
