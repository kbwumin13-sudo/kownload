//go:build !darwin || !cgo

package transfer

import (
	"errors"
	"testing"
)

func TestVerifiedRootUnsupportedPlatform(t *testing.T) {
	if r, err := OpenVerifiedPolicyRoot("root", "/unused-synthetic-root"); r != nil || !errors.Is(err, ErrVerifiedRootUnsupported) {
		t.Fatalf("unsupported platform returned root=%v error=%v", r, err)
	}
}
