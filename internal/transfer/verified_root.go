package transfer

import (
	"errors"
	"strings"
	"sync"
	"unicode/utf8"
)

var (
	ErrVerifiedRootInvalid     = errors.New("verified root: invalid")
	ErrVerifiedRootUnsupported = errors.New("verified root: unsupported")
	ErrVerifiedRootUnsafe      = errors.New("verified root: unsafe")
	ErrVerifiedRootIdentity    = errors.New("verified root: identity changed")
	ErrVerifiedRootClosed      = errors.New("verified root: closed")
	ErrVerifiedRootBusy        = errors.New("verified root: busy")
)

// VerifiedPolicyRootBinding is an owned description, not an authorization token.
// Authority callers may copy it into their value model; execution needs a live lease.
type VerifiedPolicyRootBinding struct {
	RootID   string
	Path     string
	Identity CompletionFileIdentity
}

// VerifiedPolicyRoot pins an explicitly selected, existing policy directory.
// The mutex protects only this module's descriptors, counters and lifecycle.
// No ACL/membership lookup, path probe, callback or wait runs under it.
type VerifiedPolicyRoot struct {
	mu         sync.Mutex
	directory  *CompletionDirectory
	binding    VerifiedPolicyRootBinding
	epoch      uint64
	leases     int
	operations int
	closing    bool
	closed     bool
}

// VerifiedRootLease owns a duplicate of the root's descriptor. It never owns
// the registry's descriptor. Neither this type nor a successful Check grants IO.
type VerifiedRootLease struct {
	root      *VerifiedPolicyRoot
	directory *CompletionDirectory // protected by root.mu
	closed    bool
}

// OpenVerifiedPolicyRoot does not discover, create, chmod or canonicalize roots.
// The caller supplies the exact root from its trusted policy. Filesystem checks
// authorize the pinned directory object, not a perpetual pathname association.
func OpenVerifiedPolicyRoot(rootID, path string) (*VerifiedPolicyRoot, error) {
	if rootID == "" || len(rootID) > 128 || !utf8.ValidString(rootID) || strings.IndexByte(rootID, 0) >= 0 {
		return nil, ErrVerifiedRootInvalid
	}
	d, err := openVerifiedRootDirectory(path)
	if err != nil {
		return nil, err
	}
	if err = verifyRootHandle(d.handle, d.identity); err != nil {
		_ = d.Close()
		return nil, err
	}
	return &VerifiedPolicyRoot{directory: d, binding: VerifiedPolicyRootBinding{RootID: rootID, Path: path, Identity: d.Identity()}, epoch: 1}, nil
}

// Binding returns copied metadata even after Close; it never proves liveness.
func (r *VerifiedPolicyRoot) Binding() VerifiedPolicyRootBinding {
	if r == nil {
		return VerifiedPolicyRootBinding{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.binding
}

// Borrow verifies a controlled duplicate, outside the lifecycle lock. A closing
// registry rejects new leases. Failure neither rebinds the root nor changes files.
func (r *VerifiedPolicyRoot) Borrow() (*VerifiedRootLease, error) {
	if r == nil {
		return nil, ErrVerifiedRootClosed
	}
	r.mu.Lock()
	if r.closed || r.closing {
		r.mu.Unlock()
		return nil, ErrVerifiedRootClosed
	}
	d, err := duplicateVerifiedRootDirectory(r.directory)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.leases++
	l := &VerifiedRootLease{root: r, directory: d}
	epoch := r.epoch
	r.mu.Unlock()
	if err = l.Check(); err != nil {
		_ = l.Close()
		return nil, err
	}
	r.mu.Lock()
	valid := !r.closed && !r.closing && r.epoch == epoch && !l.closed
	r.mu.Unlock()
	if !valid {
		_ = l.Close()
		return nil, ErrVerifiedRootClosed
	}
	return l, nil
}

// Close first stops new borrowing. Busy leaves the owner's fd intact. The
// caller must stop producers, join writers outside Task/DB locks, close leases,
// then call Close again. It never waits for external work or deletes data.
func (r *VerifiedPolicyRoot) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closing = true
	if r.leases != 0 || r.operations != 0 {
		r.mu.Unlock()
		return ErrVerifiedRootBusy
	}
	r.closed = true
	r.epoch++
	d := r.directory
	r.directory = nil
	r.mu.Unlock()
	if d != nil {
		if err := d.Close(); err != nil {
			return ErrVerifiedRootUnsupported
		}
	}
	return nil
}

// snapshot duplicates a protected fd before releasing its lock. Never capture
// a bare fd number for later use: Close could otherwise recycle that number.
func (l *VerifiedRootLease) snapshot() (*CompletionDirectory, VerifiedPolicyRootBinding, uint64, error) {
	if l == nil || l.root == nil {
		return nil, VerifiedPolicyRootBinding{}, 0, ErrVerifiedRootClosed
	}
	r := l.root
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.closed || r.closed {
		return nil, VerifiedPolicyRootBinding{}, 0, ErrVerifiedRootClosed
	}
	d, err := duplicateVerifiedRootDirectory(l.directory)
	if err != nil {
		return nil, VerifiedPolicyRootBinding{}, 0, err
	}
	r.operations++
	return d, r.binding, r.epoch, nil
}

func (l *VerifiedRootLease) finish(d *CompletionDirectory, epoch uint64, result error) error {
	closeErr := d.Close()
	r := l.root
	r.mu.Lock()
	r.operations--
	valid := !l.closed && !r.closed && r.epoch == epoch
	r.mu.Unlock()
	if result != nil {
		return result
	}
	if closeErr != nil {
		return ErrVerifiedRootUnsupported
	}
	if !valid {
		return ErrVerifiedRootClosed
	}
	return nil
}

func checkVerifiedRootSnapshot(d *CompletionDirectory, binding VerifiedPolicyRootBinding) error {
	if err := verifyRootHandle(d.handle, binding.Identity); err != nil {
		return err
	}
	// This is only a safe pathname probe. Actual work stays on the pin. A
	// same-UID mutator after this observation remains the caller's namespace
	// trust boundary; no atomic path-to-object guarantee is claimed.
	probe, err := openVerifiedRootDirectory(binding.Path)
	if err != nil {
		return err
	}
	defer probe.Close()
	if probe.Identity() != binding.Identity {
		return ErrVerifiedRootIdentity
	}
	return nil
}

// Check revalidates same-fd ownership/mode/ACL and the observed path association.
// ACL and membership calls can block in system services; no deadline is promised.
func (l *VerifiedRootLease) Check() error {
	d, binding, epoch, err := l.snapshot()
	if err != nil {
		return err
	}
	return l.finish(d, epoch, checkVerifiedRootSnapshot(d, binding))
}

// WithDirectory borrows a separate checked descriptor for a synchronous callback.
// It must not be retained or closed by fn. This feeds the existing anchored FS
// primitives without reopening a pathname for IO. The caller still supplies
// policy authority, durable first-byte ACK, writer join, namespace exclusion and
// run Fence. On a concurrent Close, an error cannot imply rollback of fn's IO.
func (l *VerifiedRootLease) WithDirectory(fn func(*CompletionDirectory) error) (err error) {
	if fn == nil {
		return ErrVerifiedRootInvalid
	}
	d, binding, epoch, err := l.snapshot()
	if err != nil {
		return err
	}
	// Ensure a panicking callback cannot leak a descriptor or keep Close busy.
	defer func() { err = l.finish(d, epoch, err) }()
	if err = checkVerifiedRootSnapshot(d, binding); err != nil {
		return err
	}
	r := l.root
	r.mu.Lock()
	valid := !l.closed && !r.closed && r.epoch == epoch
	r.mu.Unlock()
	if !valid {
		return ErrVerifiedRootClosed
	}
	return fn(d)
}

func (l *VerifiedRootLease) Close() error {
	if l == nil || l.root == nil {
		return nil
	}
	r := l.root
	r.mu.Lock()
	if l.closed {
		r.mu.Unlock()
		return nil
	}
	l.closed = true
	d := l.directory
	l.directory = nil
	r.leases--
	r.mu.Unlock()
	if d != nil {
		if err := d.Close(); err != nil {
			return ErrVerifiedRootUnsupported
		}
	}
	return nil
}

// ACL values are owned snapshots of public SDK queries, never opaque C layouts.
type verifiedRootACLEntry struct {
	tag       uint32
	flags     uint32
	rights    uint64
	qualifier [16]byte
}
type verifiedRootACL struct {
	flags   uint32
	entries []verifiedRootACLEntry
}

const (
	verifiedRootAllow       uint32 = 1
	verifiedRootDeny        uint32 = 2
	verifiedRootEntryFlags  uint32 = 0x1f0 // inherited/file/dir/limit/only-inherit
	verifiedRootNoInherit   uint32 = 1 << 17
	verifiedRootReadRights  uint64 = (1 << 1) | (1 << 3) | (1 << 7) | (1 << 9) | (1 << 11) | (1 << 20) | (1 << 22) | (1 << 24)
	verifiedRootWriteRights uint64 = (1 << 2) | (1 << 4) | (1 << 5) | (1 << 6) | (1 << 8) | (1 << 10) | (1 << 12) | (1 << 13) | (1 << 21) | (1 << 23)
)

func classifyVerifiedRootACL(acl verifiedRootACL, currentUUID func() ([16]byte, error)) error {
	if acl.flags & ^verifiedRootNoInherit != 0 || len(acl.entries) > 128 {
		return ErrVerifiedRootUnsupported
	}
	var current [16]byte
	resolved := false
	for _, entry := range acl.entries {
		if (entry.tag != verifiedRootAllow && entry.tag != verifiedRootDeny) || entry.flags & ^verifiedRootEntryFlags != 0 || entry.rights & ^(verifiedRootReadRights|verifiedRootWriteRights) != 0 {
			return ErrVerifiedRootUnsupported
		}
		// only/limit inheritance without a file/dir target has no proven meaning.
		if entry.flags&((1<<7)|(1<<8)) != 0 && entry.flags&((1<<5)|(1<<6)) == 0 {
			return ErrVerifiedRootUnsupported
		}
		if entry.tag == verifiedRootDeny || entry.rights&verifiedRootWriteRights == 0 {
			continue
		}
		if !resolved {
			if currentUUID == nil {
				return ErrVerifiedRootUnsupported
			}
			var err error
			current, err = currentUUID()
			if err != nil || current == ([16]byte{}) {
				return ErrVerifiedRootUnsupported
			}
			resolved = true
		}
		if entry.qualifier != current {
			return ErrVerifiedRootUnsafe
		}
	}
	return nil
}
