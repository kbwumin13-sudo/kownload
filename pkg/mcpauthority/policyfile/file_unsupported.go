//go:build !darwin

package policyfile

import "github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"

// LoadPolicyFile has no supported file-safety adapter on this platform.
// It rejects without opening the supplied path; no portable fallback is used.
func LoadPolicyFile(path string) (*mcpwritepolicy.Policy, error) {
	return nil, ErrInvalidPolicyFile
}
