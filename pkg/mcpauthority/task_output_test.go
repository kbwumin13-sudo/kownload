//go:build darwin && cgo

package mcpauthority_test

import (
	"errors"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/mcpauthority"
)

func TestVerifiedAuthorityResumeDoesNotRequireCreate(t *testing.T) {
	value, a, _ := verifiedAuthorityFixture(t)
	value["operations"] = map[string]any{"create": false, "pause": true, "resume": true}
	policy := parse(t, value)
	authority, err := mcpauthority.OpenVerifiedAuthority(policy, dataset, networkID)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if _, lease, err := authority.BorrowCreate(authority.PolicyID(), dataset, "a"); err == nil || lease != nil {
		if lease != nil {
			lease.Close()
		}
		t.Fatal("disabled create received a lease")
	}
	grant, lease, err := authority.BorrowTaskOutput(authority.PolicyID(), dataset, "task_a", "a")
	if err != nil {
		t.Fatal(err)
	}
	if grant.OutputRoot != a {
		lease.Close()
		t.Fatal("resume substituted an output root")
	}
	if err := authority.Close(); !errors.Is(err, transfer.ErrVerifiedRootBusy) {
		lease.Close()
		t.Fatal("owner closed while the resume lease was live", err)
	}
	if _, next, err := authority.BorrowTaskOutput(authority.PolicyID(), dataset, "task_a", "a"); err == nil || next != nil {
		if next != nil {
			next.Close()
		}
		lease.Close()
		t.Fatal("closing authority issued a resume lease")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifiedAuthorityResumeChecksCurrentTaskAndRootScope(t *testing.T) {
	value, _, _ := verifiedAuthorityFixture(t)
	authority, err := mcpauthority.OpenVerifiedAuthority(parse(t, value), dataset, networkID)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	for _, args := range [][4]string{
		{authority.PolicyID(), dataset, "new_task_not_in_policy", "a"},
		{authority.PolicyID(), dataset, "task_a", "z"},
		{authority.PolicyID(), otherDataset, "task_a", "a"},
		{"mcp-authority-v1:untrusted", dataset, "task_a", "a"},
	} {
		if _, lease, err := authority.BorrowTaskOutput(args[0], args[1], args[2], args[3]); err == nil || lease != nil {
			if lease != nil {
				lease.Close()
			}
			t.Fatal("scope mismatch received a resume lease")
		}
	}
}
