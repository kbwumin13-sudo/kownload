package mcpauthority

import (
	"errors"
	"sync"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
)

var ErrVerifiedAuthorityDenied = errors.New("verified authority: denied")

// VerifiedAuthority owns a policy value and verified directory pins. Only a
// trusted startup caller may construct it. P is an identifier, not a credential;
// neither this object nor a lease enables Core writes or supplies a durable ACK.
// The object must not be copied after first use.
type VerifiedAuthority struct {
	mu        sync.Mutex
	authority *Authority
	roots     map[string]*transfer.VerifiedPolicyRoot
	order     []*transfer.VerifiedPolicyRoot
	borrowing int
	closing   bool
	taskRoots map[string]mcpwritepolicy.CreateGrant
}

// OpenVerifiedAuthority verifies every explicitly configured registry root so
// the original New/P semantics remain unchanged, including roots outside D's
// scope. Only original AllowCreate grants can borrow. No directory content is
// read; no path is discovered, repaired or substituted. A failed construction
// closes all pins it opened. The caller must not mutate p during construction.
func OpenVerifiedAuthority(p *mcpwritepolicy.Policy, dataset, networkPolicyID string) (_ *VerifiedAuthority, err error) {
	snapshot, err := p.SnapshotV1()
	if err != nil || !datasetPattern.MatchString(dataset) || !networkPattern.MatchString(networkPolicyID) {
		return nil, ErrInvalidAuthority
	}
	v := &VerifiedAuthority{roots: make(map[string]*transfer.VerifiedPolicyRoot), taskRoots: make(map[string]mcpwritepolicy.CreateGrant)}
	defer func() {
		if err != nil {
			for _, root := range v.order {
				err = errors.Join(err, root.Close())
			}
		}
	}()
	bindings := make([]VerifiedRootBinding, 0, len(snapshot.Roots))
	for _, spec := range snapshot.Roots {
		root, openErr := transfer.OpenVerifiedPolicyRoot(spec.ID, spec.Path)
		if openErr != nil {
			return nil, openErr
		}
		v.roots[spec.ID] = root
		v.order = append(v.order, root)
		binding := root.Binding()
		bindings = append(bindings, VerifiedRootBinding{RootID: binding.RootID, Path: binding.Path, Identity: binding.Identity})
	}
	for _, scope := range snapshot.Datasets {
		if scope.ID != dataset {
			continue
		}
		for _, id := range scope.RootIDs {
			for _, spec := range snapshot.Roots {
				if spec.ID == id {
					v.taskRoots[id] = mcpwritepolicy.CreateGrant{OutputRoot: spec.Path, Effects: append([]string{}, snapshot.Effects...)}
				}
			}
		}
	}
	v.authority, err = New(p, dataset, networkPolicyID, bindings)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// PolicyID remains readable after Close as metadata; it never grants a lease.
func (v *VerifiedAuthority) PolicyID() string {
	if v == nil {
		return ""
	}
	return v.authority.PolicyID()
}

func (v *VerifiedAuthority) AllowTask(policyID, dataset, operation, taskID string) bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return !v.closing && v.authority.AllowTask(policyID, dataset, operation, taskID)
}

// BorrowCreate returns an owned grant and an independent dup lease, never an
// owner pin. Borrow performs the pin's same-fd Check and safe pathname probe
// outside this object's mutex. A successful probe is not an atomic guarantee
// against subsequent same-UID renames. Execution still needs namespace control,
// first-byte ACK, writer join/Fence and effects enforcement from the caller.
func (v *VerifiedAuthority) BorrowCreate(policyID, dataset, rootID string) (mcpwritepolicy.CreateGrant, *transfer.VerifiedRootLease, error) {
	if v == nil {
		return mcpwritepolicy.CreateGrant{}, nil, ErrVerifiedAuthorityDenied
	}
	v.mu.Lock()
	if v.closing {
		v.mu.Unlock()
		return mcpwritepolicy.CreateGrant{}, nil, transfer.ErrVerifiedRootClosed
	}
	grant, allowed := v.authority.AllowCreate(policyID, dataset, rootID)
	root := v.roots[rootID]
	if !allowed || root == nil {
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
	return grant, lease, nil
}

// Close immediately disables all new grants. It does not wait for tasks or
// external IO: in-flight borrowing or a live lease/snapshot returns the existing
// Busy error. Callers join/Fence and drain leases outside their locks, then retry
// Close. Existing leases retain their original lifecycle; no output is deleted.
func (v *VerifiedAuthority) Close() error {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	v.closing = true
	if v.borrowing != 0 {
		v.mu.Unlock()
		return transfer.ErrVerifiedRootBusy
	}
	roots := v.order // immutable after construction; never exposed
	v.mu.Unlock()
	var result error
	for _, root := range roots {
		result = errors.Join(result, root.Close())
	}
	return result
}
