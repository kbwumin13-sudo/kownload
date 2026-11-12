package mcpauthority

import (
	"github.com/GopeedLab/gopeed/pkg/mcpauthority/policyfile"
	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
)

// ErrInvalidPolicyFile preserves the nested loader's original error identity.
var ErrInvalidPolicyFile = policyfile.ErrInvalidPolicyFile

// LoadPolicyFile delegates explicit file loading to the lightweight platform
// adapter. It does not construct Authority or wire any startup/write capability.
func LoadPolicyFile(path string) (*mcpwritepolicy.Policy, error) {
	return policyfile.LoadPolicyFile(path)
}
