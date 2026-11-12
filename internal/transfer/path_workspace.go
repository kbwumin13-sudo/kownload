package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
)

// VerifiedPathWorkspace is a Core-owned media workspace. Its value and inode
// evidence carry no permission: the caller must ACK F and these identities,
// then bind a current-run guard before starting the actual producer.
type VerifiedPathWorkspace struct {
	mu      sync.Mutex
	lease   *VerifiedRootLease
	name    string
	work    *CompletionDirectory
	layout  pathpolicy.LayoutV1
	guard   func() error
	binding VerifiedLayoutBinding
	files   []*os.File
	closed  bool
}

func OpenVerifiedPathWorkspace(lease *VerifiedRootLease, layout pathpolicy.LayoutV1, name string, previous *CompletionFileIdentity, guard func() error) (*VerifiedPathWorkspace, CompletionFileIdentity, error) {
	if lease == nil || guard == nil || !completionName(name) {
		return nil, CompletionFileIdentity{}, ErrLayoutStorageHeld
	}
	canonical, err := pathpolicy.BuildLayoutV1(layout.ParentRoot, layout.Members)
	if err != nil {
		return nil, CompletionFileIdentity{}, err
	}
	for _, m := range canonical.Members {
		if !completionName(m.RelativePath) || m.RelativePath == name {
			return nil, CompletionFileIdentity{}, ErrLayoutStorageHeld
		}
	}
	w := &VerifiedPathWorkspace{lease: lease, layout: canonical, guard: guard, name: name}
	err = lease.WithDirectory(func(root *CompletionDirectory) error {
		if err := guard(); err != nil {
			return err
		}
		if lease.root.binding.Path != layout.ParentRoot {
			return ErrLayoutStorageHeld
		}
		var err error
		w.work, err = openPathWorkspaceDirectory(root, name, previous)
		if err != nil {
			return err
		}
		w.binding = VerifiedLayoutBinding{Version: 1, DirectoryIdentity: root.Identity(), Files: []VerifiedLayoutFileIdentity{}}
		return nil
	})
	if err != nil {
		w.Close()
		return nil, CompletionFileIdentity{}, err
	}
	return w, w.work.Identity(), nil
}
func (w *VerifiedPathWorkspace) Seal() (VerifiedLayoutBinding, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return VerifiedLayoutBinding{}, ErrLayoutStorageHeld
	}
	if err := w.guard(); err != nil {
		return VerifiedLayoutBinding{}, err
	}
	if len(w.files) != 0 {
		return cloneLayoutBinding(w.binding), nil
	}
	for _, m := range w.layout.Members {
		f, err := w.work.openFile(m.RelativePath)
		if err != nil {
			return VerifiedLayoutBinding{}, err
		}
		id, err := CompletionIdentityOf(f)
		if err == nil {
			err = errors.Join(checkLayoutFile(f, id), completionReadOnly(f), f.Sync())
		}
		if err != nil {
			f.Close()
			return VerifiedLayoutBinding{}, err
		}
		w.files = append(w.files, f)
		w.binding.Files = append(w.binding.Files, VerifiedLayoutFileIdentity{Index: m.Index, RelativePath: m.RelativePath, Identity: id})
	}
	return cloneLayoutBinding(w.binding), nil
}
func (w *VerifiedPathWorkspace) Publish(index int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || index < 0 || index >= len(w.files) || len(w.files) != len(w.layout.Members) {
		return ErrLayoutStorageHeld
	}
	if err := w.guard(); err != nil {
		return err
	}
	file := w.files[index]
	id := w.binding.Files[index].Identity
	if err := checkLayoutFile(file, id); err != nil {
		return err
	}
	name := w.layout.Members[index].RelativePath
	return w.lease.WithDirectory(func(root *CompletionDirectory) error {
		if root.Identity() != w.binding.DirectoryIdentity {
			return ErrLayoutStorageHeld
		}
		source, err := w.work.openFile(name)
		if err != nil {
			return err
		}
		defer source.Close()
		actual, err := CompletionIdentityOf(source)
		if err != nil || actual != id {
			return ErrLayoutStorageHeld
		}
		if err = linkPathWorkspaceFile(w.work, root, name); err != nil {
			return err
		}
		final, err := root.openFile(name)
		if err != nil {
			return err
		}
		defer final.Close()
		actual, err = CompletionIdentityOf(final)
		if err != nil || actual != id {
			return ErrLayoutStorageHeld
		}
		return root.handle.Sync()
	})
}
func (w *VerifiedPathWorkspace) WorkPath() string { return filepath.Join(w.layout.ParentRoot, w.name) }
func (w *VerifiedPathWorkspace) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	var err error
	for _, f := range w.files {
		err = errors.Join(err, f.Close())
	}
	if w.work != nil {
		err = errors.Join(err, w.work.Close())
	}
	return err
}
