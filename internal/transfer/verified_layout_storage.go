package transfer

import (
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
)

var ErrLayoutStorageHeld = errors.New("verified layout storage: held")

// VerifiedLayoutBinding describes actual opened files. Core persists this whole
// value with the frozen layout before Activate. It is not an IO capability.
type VerifiedLayoutBinding struct {
	Version           int                          `json:"version"`
	DirectoryIdentity CompletionFileIdentity       `json:"directoryIdentity"`
	Files             []VerifiedLayoutFileIdentity `json:"files"`
}

type VerifiedLayoutFileIdentity struct {
	Index        int                    `json:"index"`
	RelativePath string                 `json:"relativePath"`
	Identity     CompletionFileIdentity `json:"identity"`
}

// VerifiedLayoutStorage preserves the native final-file layout. Every file is
// exclusively created or reopened through a pinned directory, never a path
// based O_CREATE fallback. Its caller owns the lease until all native users join.
// guard must be a nonblocking, in-memory current-run/F-ACK check, never DB/Core
// locks, network or a callback which reenters this storage.
type VerifiedLayoutStorage struct {
	mu      sync.RWMutex
	lease   *VerifiedRootLease
	layout  pathpolicy.LayoutV1
	sizes   []int64
	files   []*os.File
	binding VerifiedLayoutBinding
	guard   func() error
	active  bool
	closed  bool
}

// CreateVerifiedLayoutStorage is called only after the complete F is durably
// ACKed. Even empty native files are created here, not at metadata resolution.
// A failed/unknown subsequent identity Save must Close without Activate. Failure
// preserves files: a retry cannot adopt an unrecorded same-name entry.
func CreateVerifiedLayoutStorage(lease *VerifiedRootLease, layout pathpolicy.LayoutV1, sizes []int64, guard func() error) (*VerifiedLayoutStorage, VerifiedLayoutBinding, error) {
	return openVerifiedLayoutStorage(lease, layout, sizes, nil, guard)
}

// OpenVerifiedLayoutStorage resumes only the original recorded identities. It
// never creates a missing directory/file, truncates or claims another inode.
func OpenVerifiedLayoutStorage(lease *VerifiedRootLease, layout pathpolicy.LayoutV1, sizes []int64, binding VerifiedLayoutBinding, guard func() error) (*VerifiedLayoutStorage, VerifiedLayoutBinding, error) {
	return openVerifiedLayoutStorage(lease, layout, sizes, &binding, guard)
}

func openVerifiedLayoutStorage(lease *VerifiedRootLease, layout pathpolicy.LayoutV1, sizes []int64, previous *VerifiedLayoutBinding, guard func() error) (*VerifiedLayoutStorage, VerifiedLayoutBinding, error) {
	if layout.Version != pathpolicy.LayoutVersion || guard == nil || lease == nil {
		return nil, VerifiedLayoutBinding{}, ErrLayoutStorageHeld
	}
	owned, err := pathpolicy.BuildLayoutV1(layout.ParentRoot, layout.Members)
	if err != nil || len(sizes) != len(owned.Members) {
		return nil, VerifiedLayoutBinding{}, ErrLayoutStorageHeld
	}
	paths := make(map[string]bool, len(owned.Members))
	for i, member := range owned.Members {
		if sizes[i] < 0 {
			return nil, VerifiedLayoutBinding{}, ErrLayoutStorageHeld
		}
		paths[member.RelativePath] = true
	}
	// Reject file/directory prefix collisions before the first mkdir or create.
	for name := range paths {
		for offset := strings.LastIndexByte(name, '/'); offset >= 0; offset = strings.LastIndexByte(name[:offset], '/') {
			if paths[name[:offset]] {
				return nil, VerifiedLayoutBinding{}, ErrLayoutStorageHeld
			}
		}
	}
	if previous != nil {
		if previous.Version != 1 || len(previous.Files) != len(owned.Members) || previous.DirectoryIdentity.Inode == 0 {
			return nil, VerifiedLayoutBinding{}, ErrLayoutStorageHeld
		}
		for i, member := range owned.Members {
			file := previous.Files[i]
			if file.Index != i || file.RelativePath != member.RelativePath || file.Identity.Inode == 0 {
				return nil, VerifiedLayoutBinding{}, ErrLayoutStorageHeld
			}
		}
	}
	if err = guard(); err != nil {
		return nil, VerifiedLayoutBinding{}, err
	}
	s := &VerifiedLayoutStorage{lease: lease, layout: owned, sizes: append([]int64(nil), sizes...), guard: guard,
		binding: VerifiedLayoutBinding{Version: 1, Files: make([]VerifiedLayoutFileIdentity, 0, len(owned.Members))}}
	err = lease.WithDirectory(func(root *CompletionDirectory) error {
		if lease.root.binding.Path != owned.ParentRoot {
			return ErrLayoutStorageHeld
		}
		s.binding.DirectoryIdentity = root.Identity()
		if previous != nil && previous.DirectoryIdentity != root.Identity() {
			return completionError(CompletionIdentity, nil)
		}
		for i, member := range owned.Members {
			if err := guard(); err != nil {
				return err
			}
			parts := strings.Split(member.RelativePath, "/")
			directory, err := openLayoutDirectory(root, parts[:len(parts)-1], previous == nil)
			if err != nil {
				return err
			}
			var file *os.File
			if previous == nil {
				file, err = directory.CreateStaging(parts[len(parts)-1])
			} else {
				file, err = directory.openStagingWriter(parts[len(parts)-1])
			}
			closeErr := directory.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				_ = file.Close()
				return closeErr
			}
			identity, err := CompletionIdentityOf(file)
			info, statErr := file.Stat()
			if err != nil || statErr != nil || info.Size() > sizes[i] || previous != nil && identity != previous.Files[i].Identity {
				_ = file.Close()
				return completionError(CompletionIdentity, errors.Join(err, statErr))
			}
			s.files = append(s.files, file)
			s.binding.Files = append(s.binding.Files, VerifiedLayoutFileIdentity{Index: i, RelativePath: member.RelativePath, Identity: identity})
		}
		return guard()
	})
	if err != nil {
		_ = s.Close()
		return nil, VerifiedLayoutBinding{}, err
	}
	return s, s.Binding(), nil
}

func (s *VerifiedLayoutStorage) Binding() VerifiedLayoutBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneLayoutBinding(s.binding)
}

func cloneLayoutBinding(binding VerifiedLayoutBinding) VerifiedLayoutBinding {
	binding.Files = append([]VerifiedLayoutFileIdentity(nil), binding.Files...)
	return binding
}

// MatchLayout binds protocol byte offsets to exactly the Core's frozen paths.
func (s *VerifiedLayoutStorage) MatchLayout(layout pathpolicy.LayoutV1, sizes []int64) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, err := pathpolicy.LayoutFingerprint(layout)
	if err != nil {
		return err
	}
	b, err := pathpolicy.LayoutFingerprint(s.layout)
	if err != nil || a != b || !reflect.DeepEqual(s.sizes, sizes) || s.closed {
		return ErrLayoutStorageHeld
	}
	return nil
}

func (s *VerifiedLayoutStorage) Ready() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || !s.active {
		return ErrLayoutStorageHeld
	}
	return s.guard()
}

// Activate accepts only the exact owned identity value that Core has just
// durably ACKed. Supplying this value without that ACK is a caller bug; no value
// helper can manufacture the Core's execution authority or decide unknown ACK.
func (s *VerifiedLayoutStorage) Activate(ack VerifiedLayoutBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !reflect.DeepEqual(s.binding, ack) {
		return ErrLayoutStorageHeld
	}
	if err := s.guard(); err != nil {
		return err
	}
	s.active = true
	return nil
}

func (s *VerifiedLayoutStorage) check(index int) error {
	if s.closed || !s.active || index < 0 || index >= len(s.files) {
		return ErrLayoutStorageHeld
	}
	if err := s.guard(); err != nil {
		return err
	}
	return checkLayoutFile(s.files[index], s.binding.Files[index].Identity)
}

func (s *VerifiedLayoutStorage) ReadAt(index int, data []byte, offset int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(index); err != nil {
		return 0, err
	}
	if offset < 0 || offset > s.sizes[index] {
		return 0, ErrLayoutStorageHeld
	}
	if len(data) == 0 {
		return 0, nil
	}
	if int64(len(data)) > s.sizes[index]-offset {
		n, err := s.files[index].ReadAt(data[:s.sizes[index]-offset], offset)
		if err == nil {
			err = io.EOF
		}
		return n, err
	}
	return s.files[index].ReadAt(data, offset)
}

func (s *VerifiedLayoutStorage) WriteAt(index int, data []byte, offset int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(index); err != nil {
		return 0, err
	}
	if offset < 0 || offset > s.sizes[index] || int64(len(data)) > s.sizes[index]-offset {
		return 0, ErrLayoutStorageHeld
	}
	return s.files[index].WriteAt(data, offset)
}

// NativeFile is only for libraries whose FileHandler requires *os.File. The
// file is borrowed, never reopened; caller must stop/join those native users
// before Close/Sync or releasing its root lease. BT uses ReadAt/WriteAt instead.
func (s *VerifiedLayoutStorage) NativeFile(index int) (*os.File, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(index); err != nil {
		return nil, err
	}
	return s.files[index], nil
}

// Sync is called after producer join, before persisting native completion.
func (s *VerifiedLayoutStorage) Sync() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrLayoutStorageHeld
	}
	for _, file := range s.files {
		if err := file.Sync(); err != nil {
			return err
		}
	}
	return nil
}

// Close joins this adapter's IO and closes its pins, without deleting files or
// closing the caller's lease. External native users must already be joined.
func (s *VerifiedLayoutStorage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed, s.active = true, false
	var result error
	for _, file := range s.files {
		result = errors.Join(result, file.Close())
	}
	return result
}
