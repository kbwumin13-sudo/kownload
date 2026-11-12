package transfer

import (
	"errors"
	"testing"
)

func TestVerifiedRootACLClassification(t *testing.T) {
	current := [16]byte{1}
	foreign := [16]byte{2}
	entry := func(tag uint32, rights uint64, flags uint32, id [16]byte) verifiedRootACLEntry {
		return verifiedRootACLEntry{tag: tag, rights: rights, flags: flags, qualifier: id}
	}
	cases := []struct {
		name        string
		acl         verifiedRootACL
		resolverErr bool
		want        error
	}{
		{name: "empty"},
		{name: "deny", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootDeny, verifiedRootWriteRights, 0, foreign)}}},
		{name: "foreign-read", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootAllow, verifiedRootReadRights, 0, foreign)}}},
		{name: "current-write", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootAllow, verifiedRootWriteRights, 0, current)}}},
		{name: "foreign-write", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootAllow, 1<<2, 0, foreign)}}, want: ErrVerifiedRootUnsafe},
		{name: "foreign-only-inherit", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootAllow, 1<<12, (1<<5)|(1<<8), foreign)}}, want: ErrVerifiedRootUnsafe},
		{name: "foreign-inherited", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootAllow, 1<<4, 1<<4, foreign)}}, want: ErrVerifiedRootUnsafe},
		{name: "current-inherit", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootAllow, verifiedRootWriteRights, (1<<5)|(1<<6)|(1<<7)|(1<<8), current)}}},
		{name: "read-inherit", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootAllow, verifiedRootReadRights, (1<<5)|(1<<6), foreign)}}},
		{name: "deny-does-not-cancel-allow", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootDeny, 1<<2, 0, foreign), entry(verifiedRootAllow, 1<<2, 0, foreign)}}, want: ErrVerifiedRootUnsafe},
		{name: "unknown-tag", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(3, 0, 0, current)}}, want: ErrVerifiedRootUnsupported},
		{name: "unknown-mask", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootAllow, 1<<63, 0, current)}}, want: ErrVerifiedRootUnsupported},
		{name: "unknown-entry-flag", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootDeny, 0, 1<<31, current)}}, want: ErrVerifiedRootUnsupported},
		{name: "deferred-inheritance", acl: verifiedRootACL{flags: 1 << 16}, want: ErrVerifiedRootUnsupported},
		{name: "unknown-global-flag", acl: verifiedRootACL{flags: 1 << 30}, want: ErrVerifiedRootUnsupported},
		{name: "no-inherit", acl: verifiedRootACL{flags: verifiedRootNoInherit}},
		{name: "only-inherit-without-target", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootDeny, 0, 1<<8, current)}}, want: ErrVerifiedRootUnsupported},
		{name: "too-many-entries", acl: verifiedRootACL{entries: make([]verifiedRootACLEntry, 129)}, want: ErrVerifiedRootUnsupported},
		{name: "membership-error", acl: verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootAllow, 1<<2, 0, current)}}, resolverErr: true, want: ErrVerifiedRootUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := classifyVerifiedRootACL(tc.acl, func() ([16]byte, error) {
				calls++
				if tc.resolverErr {
					return [16]byte{}, errors.New("synthetic lookup error")
				}
				return current, nil
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("classification got %v want %v", err, tc.want)
			}
			if tc.name == "foreign-read" && calls != 0 {
				t.Fatal("read-only ACL triggered membership lookup")
			}
		})
	}
	t.Run("resolve-once", func(t *testing.T) {
		calls := 0
		acl := verifiedRootACL{entries: []verifiedRootACLEntry{entry(verifiedRootAllow, 1<<2, 0, current), entry(verifiedRootAllow, 1<<4, 0, current)}}
		if err := classifyVerifiedRootACL(acl, func() ([16]byte, error) { calls++; return current, nil }); err != nil || calls != 1 {
			t.Fatalf("classification=%v lookup count=%d", err, calls)
		}
	})
}
