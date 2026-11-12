package ed2k

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/monkeyWie/goed2k"
	gprotocol "github.com/monkeyWie/goed2k/protocol"
)

// This extends the SAME existing native protocol state object. D6 records must
// not appear in ClientState.Transfers: an older client could otherwise open
// them from another legacy task even though the R2 task itself is held.
type nativePathState struct {
	goed2k.ClientState
	PathFrozenTransfers []nativePathTransfer `json:"pathFrozenTransfers,omitempty"`
}
type nativePathTransfer struct {
	OwnerID  string                     `json:"ownerId"`
	Transfer goed2k.ClientTransferState `json:"transfer"`
}

// RegisterPathFreeze runs for EVERY restored/new D6 task before shared client
// initialization. Hash ownership is explicit Task.ID authority, not inferred
// from a filename or from a nonempty path. A conflicting legacy transfer is held.
func (fm *FetcherManager) RegisterPathFreeze(ownerID string, req *base.Request) error {
	if ownerID == "" || req == nil {
		return transfer.ErrLayoutStorageHeld
	}
	link, err := parseLink(req.URL)
	if err != nil {
		return err
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	store := fm.getStateStoreLocked()
	store.pathMu.Lock()
	defer store.pathMu.Unlock()
	if store.pathOwners == nil {
		store.pathOwners = make(map[gprotocol.Hash]string)
	}
	if record, known := store.pathRecords[link.Hash]; known && record.OwnerID != ownerID {
		return transfer.ErrLayoutStorageHeld
	}
	if old := store.pathOwners[link.Hash]; old != "" {
		if old == ownerID {
			return nil
		}
		return transfer.ErrLayoutStorageHeld
	}
	if fm.client != nil && fm.client.FindTransfer(link.Hash).IsValid() {
		return transfer.ErrLayoutStorageHeld
	}
	store.pathOwners[link.Hash] = ownerID
	return nil
}

// ForgetPathFreeze is only for a known-ACK Task deletion AFTER a successful
// lock-free Quiesce. Pause, Close, shutdown and unknown deletion ACK must not
// call this. It removes this owner's native resume metadata, never output files.
func (fm *FetcherManager) ForgetPathFreeze(ownerID string, req *base.Request) error {
	if ownerID == "" || req == nil {
		return transfer.ErrLayoutStorageHeld
	}
	link, err := parseLink(req.URL)
	if err != nil {
		return err
	}
	fm.mu.Lock()
	if fm.client != nil && fm.client.FindTransfer(link.Hash).IsValid() {
		fm.mu.Unlock()
		return transfer.ErrLayoutStorageHeld
	}
	store := fm.getStateStoreLocked()
	fm.mu.Unlock()
	store.stateMu.Lock()
	defer store.stateMu.Unlock()
	store.pathMu.Lock()
	if current := store.pathOwners[link.Hash]; current != "" && current != ownerID {
		store.pathMu.Unlock()
		return transfer.ErrLayoutStorageHeld
	}
	if record, ok := store.pathRecords[link.Hash]; ok && record.OwnerID != ownerID {
		store.pathMu.Unlock()
		return transfer.ErrLayoutStorageHeld
	}
	store.pathMu.Unlock()
	if store.store != nil {
		var state nativePathState
		exists, err := store.store.Load(&state)
		if err != nil {
			return err
		}
		if exists {
			if state.Version != 0 && state.Version != 1 && state.Version != 2 {
				return transfer.ErrLayoutStorageHeld
			}
			for _, record := range state.Transfers {
				if record.Hash == link.Hash {
					return transfer.ErrLayoutStorageHeld
				}
			}
			kept := make([]nativePathTransfer, 0, len(state.PathFrozenTransfers))
			for _, record := range state.PathFrozenTransfers {
				if record.Transfer.Hash != link.Hash {
					kept = append(kept, record)
					continue
				}
				if record.OwnerID != ownerID {
					return transfer.ErrLayoutStorageHeld
				}
			}
			state.PathFrozenTransfers = kept
			// Failure (including uncertain Save) does not release RAM ownership.
			if err := store.store.Save(&state); err != nil {
				return err
			}
		}
	} else {
		store.pathMu.Lock()
		_, pending := store.pathRecords[link.Hash]
		store.pathMu.Unlock()
		if pending {
			return transfer.ErrLayoutStorageHeld
		}
	}
	store.pathMu.Lock()
	delete(store.pathRecords, link.Hash)
	delete(store.pathOwners, link.Hash)
	store.pathMu.Unlock()
	return nil
}

func (f *Fetcher) RequirePathFreeze(ownerID string) error {
	f.pathMu.Lock()
	defer f.pathMu.Unlock()
	if ownerID == "" || f.pathOwner != "" && f.pathOwner != ownerID {
		return transfer.ErrLayoutStorageHeld
	}
	if f.handle.IsValid() && f.pathOwner == "" {
		return transfer.ErrLayoutStorageHeld
	}
	f.pathOwner = ownerID
	return nil
}

func (f *Fetcher) PathFreezeLayout() (pathpolicy.LayoutV1, []int64, error) {
	f.pathMu.Lock()
	defer f.pathMu.Unlock()
	return f.pathFreezeLayoutLocked()
}

func (f *Fetcher) pathFreezeLayoutLocked() (pathpolicy.LayoutV1, []int64, error) {
	if f.pathOwner == "" || f.meta == nil || f.meta.Res == nil || len(f.meta.Res.Files) != 1 || f.meta.Opts == nil || f.meta.Res.Files[0] == nil {
		return pathpolicy.LayoutV1{}, nil, transfer.ErrLayoutStorageHeld
	}
	file, opts := f.meta.Res.Files[0], f.meta.Opts
	if file.Size < 0 || len(opts.SelectFiles) != 0 && (len(opts.SelectFiles) != 1 || opts.SelectFiles[0] != 0) {
		return pathpolicy.LayoutV1{}, nil, pathpolicy.ErrLayout
	}
	name := file.Name
	if opts.Name != "" {
		name = opts.Name
	}
	if file.Path != "" {
		name = file.Path + "/" + name
	}
	layout, err := pathpolicy.BuildLayoutV1(opts.Path, []pathpolicy.Member{{Index: 0, RelativePath: name, Selected: true}})
	return layout, []int64{file.Size}, err
}

// BindPathFreeze does no network/client initialization. A previous handler
// must have completed the native release handshake before a new run is bound.
func (f *Fetcher) BindPathFreeze(ctx context.Context, layout pathpolicy.LayoutV1, output *transfer.VerifiedLayoutStorage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.pathMu.Lock()
	defer f.pathMu.Unlock()
	expected, sizes, err := f.pathFreezeLayoutLocked()
	if err != nil || output == nil {
		return transfer.ErrLayoutStorageHeld
	}
	a, err := pathpolicy.LayoutFingerprint(expected)
	if err != nil {
		return err
	}
	b, err := pathpolicy.LayoutFingerprint(layout)
	if err != nil || a != b {
		return transfer.ErrLayoutStorageHeld
	}
	if err = output.MatchLayout(expected, sizes); err != nil {
		return err
	}
	if err = output.Ready(); err != nil {
		return err
	}
	if f.pathHandler != nil {
		select {
		case <-f.pathHandler.done:
		default:
			return transfer.ErrLayoutStorageHeld
		}
	}
	f.pathOutput = output
	f.pathHandler = &pathFileHandler{output: output, path: filepath.Join(layout.ParentRoot, layout.Members[0].RelativePath), done: make(chan struct{})}
	return nil
}

type pathFileHandler struct {
	mu        sync.Mutex
	output    *transfer.VerifiedLayoutStorage
	path      string
	done      chan struct{}
	closed    bool
	stopping  bool
	submitted bool
	closeErr  error
}

// The native library asks File() before every block operation. No filename is
// opened here. An already-running operation can finish on this pin; Core holds
// its lease until the release handshake, outside Task/DB locks.
func (h *pathFileHandler) File() *os.File {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.stopping {
		return nil
	}
	file, err := h.output.NativeFile(0)
	if err != nil {
		return nil
	}
	return file
}
func (h *pathFileHandler) Path() string { return h.path }
func (h *pathFileHandler) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return h.closeErr
	}
	h.closed = true
	h.closeErr = errors.Join(h.output.Sync(), h.output.Close())
	close(h.done)
	return h.closeErr
}
func (h *pathFileHandler) DeleteFile() error { return transfer.ErrLayoutStorageHeld }

func (f *Fetcher) startPathTransfer() error {
	f.pathMu.Lock()
	handler, owner := f.pathHandler, f.pathOwner
	if handler == nil || f.pathOutput == nil {
		f.pathMu.Unlock()
		return transfer.ErrLayoutStorageHeld
	}
	if err := f.pathOutput.Ready(); err != nil {
		f.pathMu.Unlock()
		return err
	}
	req := f.meta.Req
	f.pathMu.Unlock()
	if f.manager == nil {
		return transfer.ErrLayoutStorageHeld
	}
	if err := f.manager.RegisterPathFreeze(owner, req); err != nil {
		return err
	}
	link, err := parseLink(req.URL)
	if err != nil {
		return err
	}
	client, err := f.getClient()
	if err != nil {
		return err
	}
	if client.FindTransfer(link.Hash).IsValid() {
		return transfer.ErrLayoutStorageHeld
	}
	atp := goed2k.AddTransferParams{Hash: link.Hash, CreateTime: time.Now().UnixMilli(), Size: link.NumberValue, FilePath: handler.path, Handler: handler, Paused: true}
	store := f.manager.pathStateStore()
	if record, ok, err := store.pathResume(owner, link.Hash); err != nil {
		return err
	} else if ok {
		if record.TargetPath != handler.path || record.Size != link.NumberValue {
			return transfer.ErrLayoutStorageHeld
		}
		atp.CreateTime, atp.ResumeData = record.CreateTime, record.ResumeData
	}
	handle, err := client.AddTransfer(atp)
	if err != nil {
		return err
	}
	handler.mu.Lock()
	handler.submitted = true
	handler.mu.Unlock()
	f.handle = handle
	return client.ResumeTransfer(link.Hash)
}

// QuiescePathFreeze is an explicit lock-free Core Fence step. Native disk work
// is serial and RemoveTransfer queues AsyncRelease behind already queued work;
// its FileHandler.Close signals completion. Context failure retains ownership,
// and must never be interpreted as permission to release the lease/new writer.
func (f *Fetcher) QuiescePathFreeze(ctx context.Context) error {
	f.pathMu.Lock()
	handler, owner := f.pathHandler, f.pathOwner
	f.pathMu.Unlock()
	if handler == nil {
		return nil
	}
	handler.mu.Lock()
	handler.stopping = true
	handler.mu.Unlock()
	if f.manager == nil {
		return transfer.ErrLayoutStorageHeld
	}
	client := f.manager.currentClient()
	if client != nil {
		hash, err := f.hash()
		if err != nil {
			return err
		}
		handle := client.FindTransfer(hash)
		if handle.IsValid() {
			if err = client.PauseTransfer(hash); err != nil {
				return err
			}
			record := goed2k.ClientTransferState{Hash: hash, Size: f.meta.Res.Files[0].Size, TargetPath: handler.path, Paused: true, ResumeData: handle.GetResumeData()}
			if err = f.manager.pathStateStore().retainPathResume(owner, record); err != nil {
				return err
			}
			if err = client.RemoveTransfer(hash, false); err != nil {
				return err
			}
		} else {
			// No transfer was submitted, hence no native async writer exists.
			handler.mu.Lock()
			submitted := handler.submitted
			handler.mu.Unlock()
			if !submitted {
				if err = handler.Close(); err != nil {
					return err
				}
			}
		}
	} else {
		handler.mu.Lock()
		submitted := handler.submitted
		handler.mu.Unlock()
		if !submitted {
			if err := handler.Close(); err != nil {
				return err
			}
		}
	}
	select {
	case <-handler.done:
		f.handle = goed2k.TransferHandle{}
		handler.mu.Lock()
		err := handler.closeErr
		handler.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (fm *FetcherManager) pathStateStore() *clientStateStore {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.getStateStoreLocked()
}

func (fm *FetcherManager) managedPathHash(hash gprotocol.Hash) bool {
	store := fm.pathStateStore()
	store.pathMu.Lock()
	defer store.pathMu.Unlock()
	_, known := store.pathRecords[hash]
	return known || store.pathOwners[hash] != ""
}

func cloneNativePath[T any](value T) (T, error) {
	var result T
	raw, err := json.Marshal(value)
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result, err
}

func (s *clientStateStore) loadNativePath(state *nativePathState) (*goed2k.ClientState, error) {
	s.pathMu.Lock()
	defer s.pathMu.Unlock()
	loaded := make(map[gprotocol.Hash]nativePathTransfer, len(state.PathFrozenTransfers))
	for _, record := range state.PathFrozenTransfers {
		if record.OwnerID == "" || record.Transfer.TargetPath == "" {
			return nil, transfer.ErrLayoutStorageHeld
		}
		if _, duplicate := loaded[record.Transfer.Hash]; duplicate {
			return nil, transfer.ErrLayoutStorageHeld
		}
		loaded[record.Transfer.Hash] = record
	}
	legacy := make([]goed2k.ClientTransferState, 0, len(state.Transfers))
	for _, record := range state.Transfers {
		if s.pathOwners[record.Hash] != "" {
			return nil, transfer.ErrLayoutStorageHeld
		}
		if _, known := loaded[record.Hash]; known {
			return nil, transfer.ErrLayoutStorageHeld
		}
		legacy = append(legacy, record)
	}
	state.Transfers = legacy
	s.pathRecords = loaded
	return &state.ClientState, nil
}

func (s *clientStateStore) saveNativePath(state *goed2k.ClientState) (*nativePathState, error) {
	s.pathMu.Lock()
	defer s.pathMu.Unlock()
	owned, err := cloneNativePath(state)
	if err != nil {
		return nil, err
	}
	if owned == nil {
		owned = &goed2k.ClientState{Version: 2}
	}
	result := &nativePathState{ClientState: *owned}
	result.Transfers = make([]goed2k.ClientTransferState, 0, len(owned.Transfers))
	if s.pathRecords == nil {
		s.pathRecords = make(map[gprotocol.Hash]nativePathTransfer)
	}
	for _, record := range owned.Transfers {
		owner := s.pathOwners[record.Hash]
		if owner == "" {
			if _, known := s.pathRecords[record.Hash]; known {
				return nil, transfer.ErrLayoutStorageHeld
			}
			result.Transfers = append(result.Transfers, record)
		} else {
			s.pathRecords[record.Hash] = nativePathTransfer{OwnerID: owner, Transfer: record}
		}
	}
	for _, record := range s.pathRecords {
		result.PathFrozenTransfers = append(result.PathFrozenTransfers, record)
	}
	sort.Slice(result.PathFrozenTransfers, func(i, j int) bool {
		return strings.Compare(result.PathFrozenTransfers[i].Transfer.Hash.String(), result.PathFrozenTransfers[j].Transfer.Hash.String()) < 0
	})
	return cloneNativePath(result)
}

func (s *clientStateStore) pathResume(owner string, hash gprotocol.Hash) (goed2k.ClientTransferState, bool, error) {
	s.pathMu.Lock()
	defer s.pathMu.Unlock()
	record, ok := s.pathRecords[hash]
	if !ok {
		return goed2k.ClientTransferState{}, false, nil
	}
	if record.OwnerID != owner || s.pathOwners[hash] != owner {
		return goed2k.ClientTransferState{}, false, transfer.ErrLayoutStorageHeld
	}
	owned, err := cloneNativePath(record.Transfer)
	return owned, true, err
}

func (s *clientStateStore) retainPathResume(owner string, record goed2k.ClientTransferState) error {
	s.pathMu.Lock()
	defer s.pathMu.Unlock()
	if s.pathOwners[record.Hash] != owner {
		return transfer.ErrLayoutStorageHeld
	}
	owned, err := cloneNativePath(record)
	if err != nil {
		return err
	}
	if s.pathRecords == nil {
		s.pathRecords = make(map[gprotocol.Hash]nativePathTransfer)
	}
	if prior, ok := s.pathRecords[record.Hash]; ok {
		owned.CreateTime, owned.UploadPrio = prior.Transfer.CreateTime, prior.Transfer.UploadPrio
	}
	s.pathRecords[record.Hash] = nativePathTransfer{OwnerID: owner, Transfer: owned}
	return nil
}
