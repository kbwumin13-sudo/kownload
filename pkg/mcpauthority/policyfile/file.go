package policyfile

import "github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"

// ErrInvalidPolicyFile preserves the original loader's fixed, redacted error
// identity and text, including Parse failures. No path or file contents escape.
var ErrInvalidPolicyFile = mcpwritepolicy.ErrInvalidPolicy
