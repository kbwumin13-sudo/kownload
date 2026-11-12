package bt

import (
	"context"
	"errors"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// RequirePathFreeze must precede Resolve/Prepare/Upload on an opted-in task.
// Existing native tasks are never silently enrolled into this storage contract.
func (f *Fetcher) RequirePathFreeze(ownerID string) error {
	f.opMu.Lock()
	defer f.opMu.Unlock()
	if ownerID == "" {
		return transfer.ErrLayoutStorageHeld
	}
	if f.pathStorage != nil {
		if f.pathOwner != ownerID {
			return transfer.ErrLayoutStorageHeld
		}
		return nil
	}
	if f.heldTorrent != nil {
		return transfer.ErrLayoutStorageHeld
	}
	f.pathStorage = &pathStorage{}
	f.pathOwner = ownerID
	return nil
}

// PathFreezeLayout projects all resolved native members, not only selection.
// Core first chooses and freezes the one root/name; this never evaluates rules.
func (f *Fetcher) PathFreezeLayout() (pathpolicy.LayoutV1, []int64, error) {
	f.opMu.Lock()
	defer f.opMu.Unlock()
	return f.pathFreezeLayoutLocked()
}

func (f *Fetcher) pathFreezeLayoutLocked() (pathpolicy.LayoutV1, []int64, error) {
	if f.pathStorage == nil || !f.torrentReady.Load() || f.meta == nil || f.meta.Res == nil || f.meta.Opts == nil {
		return pathpolicy.LayoutV1{}, nil, transfer.ErrLayoutStorageHeld
	}
	res, opts := f.meta.Res, f.meta.Opts
	selection := opts.SelectFiles
	if f.pathSelection != nil {
		selection = f.pathSelection
	}
	selected := make(map[int]bool, len(selection))
	for _, index := range selection {
		if index < 0 || index >= len(res.Files) {
			return pathpolicy.LayoutV1{}, nil, pathpolicy.ErrLayout
		}
		selected[index] = true
	}
	folder := res.Name
	if folder != "" && opts.Name != "" {
		folder = opts.Name
	}
	members := make([]pathpolicy.Member, len(res.Files))
	sizes := make([]int64, len(res.Files))
	for i, file := range res.Files {
		if file == nil || file.Size < 0 {
			return pathpolicy.LayoutV1{}, nil, pathpolicy.ErrLayout
		}
		name := file.Name
		if folder == "" && len(res.Files) == 1 && opts.Name != "" {
			name = opts.Name
		}
		// Preserve components for validation instead of path.Join cleaning '..'.
		parts := []string{}
		if folder != "" {
			parts = append(parts, folder)
		}
		if file.Path != "" {
			parts = append(parts, file.Path)
		}
		parts = append(parts, name)
		members[i] = pathpolicy.Member{Index: i, RelativePath: strings.Join(parts, "/"), Selected: len(selection) == 0 || selected[i]}
		sizes[i] = file.Size
	}
	layout, err := pathpolicy.BuildLayoutV1(opts.Path, members)
	return layout, sizes, err
}

// BindPathFreeze runs outside Task/status and torrent locks, after F and the
// whole per-file identity Save are known ACKs. Rehashing belongs to the native
// protocol and keeps its own cancellation policy; no HTTP recovery is invented.
func (f *Fetcher) BindPathFreeze(ctx context.Context, layout pathpolicy.LayoutV1, output *transfer.VerifiedLayoutStorage) error {
	f.opMu.Lock()
	expected, sizes, err := f.pathFreezeLayoutLocked()
	if err == nil {
		a, e := pathpolicy.LayoutFingerprint(expected)
		b, e2 := pathpolicy.LayoutFingerprint(layout)
		if e != nil || e2 != nil || a != b || output == nil {
			err = transfer.ErrLayoutStorageHeld
		}
	}
	if err == nil {
		err = output.MatchLayout(expected, sizes)
	}
	if err == nil {
		err = output.Ready()
	}
	store, t := f.pathStorage, f.torrent
	var selection []int
	if err == nil {
		selection, err = f.pathSelectionLocked()
	}
	if err == nil {
		err = store.bind(output, sizes)
	}
	if err == nil {
		f.pathSelection = selection
	}
	f.opMu.Unlock()
	if err != nil {
		return err
	}
	// No opMu is held while the torrent library drains/rechecks pieces.
	return t.VerifyDataContext(ctx)
}

func (f *Fetcher) pathSelectionLocked() ([]int, error) {
	if f.pathSelection != nil {
		return slices.Clone(f.pathSelection), nil
	}
	if f.meta == nil || f.meta.Opts == nil || f.meta.Res == nil || len(f.meta.Res.Files) == 0 {
		return nil, transfer.ErrLayoutStorageHeld
	}
	selection := slices.Clone(f.meta.Opts.SelectFiles)
	if len(selection) == 0 {
		selection = make([]int, len(f.meta.Res.Files))
		for i := range selection {
			selection[i] = i
		}
	}
	seen := make(map[int]bool, len(selection))
	for _, index := range selection {
		if index < 0 || index >= len(f.meta.Res.Files) || seen[index] {
			return nil, pathpolicy.ErrLayout
		}
		seen[index] = true
	}
	return selection, nil
}

// Raw empty selection means all files. Restored progress already has entries,
// so it cannot be used to decide whether runtime selection is initialized.
// Before Bind, return the stored native snapshot without indexing raw options.
func (f *Fetcher) pathProgress() fetcher.Progress {
	f.opMu.Lock()
	defer f.opMu.Unlock()
	if !f.torrentReady.Load() || len(f.pathSelection) == 0 {
		return slices.Clone(f.data.Progress)
	}
	files := f.torrent.Files()
	progress := make(fetcher.Progress, len(f.pathSelection))
	for i, index := range f.pathSelection {
		progress[i] = files[index].BytesCompleted()
	}
	f.data.Progress = progress
	return slices.Clone(progress)
}

// BindRunWait creates no goroutine or new recovery identity. Core invokes the
// closure after Start; it captures that run's torrent, selection and output,
// rather than polling a future run on the same Fetcher. Metadata waiting keeps
// its original policy, and the legacy Wait branch above is unchanged.
func (f *Fetcher) BindRunWait(ctx context.Context) func() error {
	return func() error {
		if ctx == nil {
			return transfer.ErrLayoutStorageHeld
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f.opMu.Lock()
		t, selection, drop := f.torrent, slices.Clone(f.pathSelection), f.torrentDropCtx
		var output *transfer.VerifiedLayoutStorage
		if f.pathStorage != nil {
			f.pathStorage.mu.RLock()
			output = f.pathStorage.output
			f.pathStorage.mu.RUnlock()
		}
		f.opMu.Unlock()
		if t == nil || len(selection) == 0 || output == nil || drop == nil {
			return transfer.ErrLayoutStorageHeld
		}
		files := t.Files()
		for _, index := range selection {
			if index < 0 || index >= len(files) {
				return transfer.ErrLayoutStorageHeld
			}
		}
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-drop.Done():
				return context.Canceled
			case <-t.Closed():
				return context.Canceled
			case <-tick.C:
				if err := ctx.Err(); err != nil {
					return err
				}
				done := true
				for _, index := range selection {
					if files[index].BytesCompleted() < files[index].Length() {
						done = false
						break
					}
				}
				if !done {
					continue
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				err := output.Sync()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
		}
	}
}

// QuiescePathFreeze joins all adapter IO before the Core releases its root
// lease or replaces a run. Piece callbacks hold the read lock for their whole
// syscall; after this exclusive section future callbacks cannot reach a file.
func (f *Fetcher) QuiescePathFreeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.opMu.Lock()
	store := f.pathStorage
	if f.torrent != nil && store != nil {
		f.torrent.DisallowDataDownload()
	}
	f.opMu.Unlock()
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.output == nil {
		return nil
	}
	err := errors.Join(store.output.Sync(), store.output.Close())
	store.output = nil
	return err
}

type pathExtent struct{ offset, length int64 }

// OpenTorrent is metadata-only. In particular it must not create zero-length
// files, which the library's default file storage does during this callback.
type pathStorage struct {
	mu       sync.RWMutex
	extents  []pathExtent
	complete map[int]bool
	output   *transfer.VerifiedLayoutStorage
	opened   bool
	closed   bool
}

func (s *pathStorage) OpenTorrent(_ context.Context, info *metainfo.Info, _ metainfo.Hash) (storage.TorrentImpl, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opened || s.closed {
		return storage.TorrentImpl{}, transfer.ErrLayoutStorageHeld
	}
	files := info.UpvertedFiles()
	extents := make([]pathExtent, len(files))
	var end int64
	for i, file := range files {
		if file.Length < 0 || file.TorrentOffset < end || file.TorrentOffset > math.MaxInt64-file.Length {
			return storage.TorrentImpl{}, pathpolicy.ErrLayout
		}
		extents[i] = pathExtent{offset: file.TorrentOffset, length: file.Length}
		end = file.TorrentOffset + file.Length
	}
	s.extents, s.complete, s.opened = extents, make(map[int]bool), true
	return storage.TorrentImpl{Piece: func(piece metainfo.Piece) storage.PieceImpl {
		return &pathPiece{store: s, index: piece.Index(), offset: piece.Offset(), length: piece.Length()}
	}, Close: s.Close}, nil
}

func (s *pathStorage) bind(output *transfer.VerifiedLayoutStorage, sizes []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.opened || s.closed || len(sizes) != len(s.extents) {
		return transfer.ErrLayoutStorageHeld
	}
	for i, size := range sizes {
		if size != s.extents[i].length {
			return pathpolicy.ErrLayout
		}
	}
	if s.output != nil && s.output != output {
		if s.output.Ready() == nil {
			return transfer.ErrLayoutStorageHeld
		}
		if err := s.output.Close(); err != nil {
			return err
		}
	}
	s.output, s.complete = output, make(map[int]bool)
	return nil
}

func (s *pathStorage) ready() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.output == nil {
		return transfer.ErrLayoutStorageHeld
	}
	return s.output.Ready()
}

func (s *pathStorage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.output != nil {
		return s.output.Close()
	}
	return nil
}

type pathPiece struct {
	store          *pathStorage
	index          int
	offset, length int64
}

func (p *pathPiece) ReadAt(data []byte, offset int64) (int, error)  { return p.io(data, offset, false) }
func (p *pathPiece) WriteAt(data []byte, offset int64) (int, error) { return p.io(data, offset, true) }

func (p *pathPiece) io(data []byte, offset int64, write bool) (int, error) {
	s := p.store
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.output == nil || offset < 0 || offset > p.length || int64(len(data)) > p.length-offset {
		return 0, transfer.ErrLayoutStorageHeld
	}
	if err := s.output.Ready(); err != nil {
		return 0, err
	}
	position := p.offset + offset
	done := 0
	for i, extent := range s.extents {
		if done == len(data) {
			break
		}
		if position >= extent.offset+extent.length {
			continue
		}
		if position < extent.offset {
			return done, pathpolicy.ErrLayout
		}
		length := min(int64(len(data)-done), extent.offset+extent.length-position)
		var n int
		var err error
		if write {
			n, err = s.output.WriteAt(i, data[done:done+int(length)], position-extent.offset)
		} else {
			n, err = s.output.ReadAt(i, data[done:done+int(length)], position-extent.offset)
		}
		done += n
		position += int64(n)
		if err != nil {
			return done, err
		}
		if int64(n) != length {
			if write {
				return done, io.ErrShortWrite
			}
			return done, io.ErrUnexpectedEOF
		}
	}
	if done != len(data) {
		return done, io.ErrUnexpectedEOF
	}
	return done, nil
}

func (p *pathPiece) MarkComplete() error {
	s := p.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.output == nil {
		return transfer.ErrLayoutStorageHeld
	}
	if err := s.output.Ready(); err != nil {
		return err
	}
	s.complete[p.index] = true
	return nil
}

func (p *pathPiece) MarkNotComplete() error {
	s := p.store
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.complete, p.index)
	return nil
}

func (p *pathPiece) Completion() storage.Completion {
	s := p.store
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.output == nil {
		return storage.Completion{}
	}
	if err := s.output.Ready(); err != nil {
		return storage.Completion{Err: err}
	}
	return storage.Completion{Ok: true, Complete: s.complete[p.index]}
}

// Frozen selection cannot be changed by legacy Patch after F. Order alone is
// immaterial; both are normalized as sets without rewriting runtime options.
func sameFrozenSelection(a, b []int, count int) bool {
	set := func(values []int) map[int]bool {
		m := map[int]bool{}
		if len(values) == 0 {
			for i := 0; i < count; i++ {
				m[i] = true
			}
		}
		for _, i := range values {
			m[i] = true
		}
		return m
	}
	return reflect.DeepEqual(set(a), set(b))
}
