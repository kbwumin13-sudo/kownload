package mcpauthority

import (
	"errors"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
)

// BorrowTaskOutput borrows an explicitly scoped root for an explicitly allowed
// resume. It does not require create permission, or confer a right to replace
// the Task's historical output binding. Core must compare that binding and the
// current network identity before it can use the independent lease.
func (v *VerifiedAuthority) BorrowTaskOutput(policyID, dataset, taskID, rootID string) (mcpwritepolicy.CreateGrant, *transfer.VerifiedRootLease, error) {
	if v == nil {
		return mcpwritepolicy.CreateGrant{}, nil, ErrVerifiedAuthorityDenied
	}
	v.mu.Lock()
	grant, scoped := v.taskRoots[rootID]
	root := v.roots[rootID]
	if v.closing || !scoped || root == nil || !v.authority.AllowTask(policyID, dataset, "resume", taskID) {
		v.mu.Unlock()
		return mcpwritepolicy.CreateGrant{}, nil, ErrVerifiedAuthorityDenied
	}
	v.borrowing++
	v.mu.Unlock()
	lease, err := root.Borrow()
	v.mu.Lock()
	v.borrowing--
	closing := v.closing
	v.mu.Unlock()
	if err != nil {
		return mcpwritepolicy.CreateGrant{}, nil, err
	}
	if closing {
		return mcpwritepolicy.CreateGrant{}, nil, errors.Join(transfer.ErrVerifiedRootClosed, lease.Close())
	}
	grant.Effects = append([]string{}, grant.Effects...)
	return grant, lease, nil
}
