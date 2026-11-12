package media

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// The child never chooses its workset, token, owner or budget. The caller binds
// one funded work name to this process; every actual HTTP hop borrows that same
// parent work until its response body closes.
type mediaBridgeRequest struct {
	ID    uint64 `json:"id"`
	Op    string `json:"op"`
	Lease uint64 `json:"lease"`
}
type mediaBridgeReply struct {
	ID uint64 `json:"id"`
	OK bool   `json:"ok"`
}
type mediaRecoveryBridge struct {
	cmd                      *exec.Cmd
	ctx                      context.Context
	cancel                   context.CancelFunc
	parent                   transfer.RecoveryParentController
	token                    transfer.RecoveryToken
	work                     string
	reader, writer           *os.File
	childReader, childWriter *os.File
	mu                       sync.Mutex
	writeMu                  sync.Mutex
	scopes                   map[uint64]*transfer.RecoveryWorkScope
	pending                  map[uint64]bool
	wg                       sync.WaitGroup
	done                     chan struct{}
	once                     sync.Once
}

type mediaWorkKey struct{}

func mediaWorkContext(ctx context.Context, work string) context.Context {
	return context.WithValue(ctx, mediaWorkKey{}, work)
}
func mediaRecoveryCheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lease := transfer.RecoveryLeaseFromContext(ctx); lease != nil {
		return lease.Check()
	}
	return nil
}
func newMediaRecoveryBridge(ctx context.Context, cmd *exec.Cmd) (*mediaRecoveryBridge, error) {
	lease := transfer.RecoveryLeaseFromContext(ctx)
	if lease == nil {
		return nil, nil
	}
	parent, ok := lease.Owner.(transfer.RecoveryParentController)
	work, _ := ctx.Value(mediaWorkKey{}).(string)
	if !ok || (work != "media/inspect" && work != "media/download") {
		return nil, transfer.ErrRecoveryHeld
	}
	if err := mediaRecoveryCheck(ctx); err != nil {
		return nil, err
	}
	requestReader, requestWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	responseReader, responseWriter, err := os.Pipe()
	if err != nil {
		requestReader.Close()
		requestWriter.Close()
		return nil, err
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	b := &mediaRecoveryBridge{cmd: cmd, ctx: bridgeCtx, cancel: cancel, parent: parent, token: lease.Token, work: work,
		reader: requestReader, writer: responseWriter, childReader: responseReader, childWriter: requestWriter,
		scopes: make(map[uint64]*transfer.RecoveryWorkScope), pending: make(map[uint64]bool), done: make(chan struct{})}
	cmd.ExtraFiles = append(cmd.ExtraFiles, responseReader, requestWriter)
	go b.serve()
	return b, nil
}
func (b *mediaRecoveryBridge) reply(id uint64, ok bool) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if err := json.NewEncoder(b.writer).Encode(mediaBridgeReply{ID: id, OK: ok}); err != nil {
		b.cancel()
	}
}
func (b *mediaRecoveryBridge) serve() {
	defer close(b.done)
	scanner := bufio.NewScanner(b.reader)
	scanner.Buffer(make([]byte, 1024), 4096)
	for scanner.Scan() {
		var req mediaBridgeRequest
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.ID == 0 {
			b.cancel()
			return
		}
		switch req.Op {
		case "enter":
			b.mu.Lock()
			valid := req.Lease == 0 && !b.pending[req.ID] && b.scopes[req.ID] == nil && len(b.pending)+len(b.scopes) < 64
			if valid {
				b.pending[req.ID] = true
			}
			b.mu.Unlock()
			if !valid {
				b.reply(req.ID, false)
				continue
			}
			// Admission may wait. Keep the pipe reader free to process body releases
			// from other fragment workers, rather than deadlocking a full work budget.
			b.wg.Add(1)
			go func(id uint64) {
				defer b.wg.Done()
				scope, err := b.parent.EnterRecoveryWork(b.ctx, b.token, b.work)
				if err == nil {
					err = scope.Check()
				}
				if err == nil {
					err = b.ctx.Err()
				}
				b.mu.Lock()
				delete(b.pending, id)
				if err == nil {
					b.scopes[id] = scope
				}
				b.mu.Unlock()
				if err != nil && scope != nil {
					scope.Leave()
				}
				b.reply(id, err == nil)
			}(req.ID)
		case "fail":
			b.cancel()
			b.reply(req.ID, true)
			_ = killProcessGroup(b.cmd)
		case "valid":
			b.reply(req.ID, mediaRecoveryCheck(b.ctx) == nil)
		case "check", "leave":
			b.mu.Lock()
			scope := b.scopes[req.Lease]
			if req.Op == "leave" {
				delete(b.scopes, req.Lease)
			}
			b.mu.Unlock()
			ok := scope != nil
			if scope != nil {
				if req.Op == "leave" {
					scope.Leave()
				} else {
					ok = scope.Check() == nil && b.ctx.Err() == nil
				}
			}
			b.reply(req.ID, ok)
		default:
			b.reply(req.ID, false)
		}
	}
	b.cancel()
}

// Close is called only after child Wait/pipe readers have joined, or Start
// failed. A cancelled context alone does not pretend the child has stopped IO.
func (b *mediaRecoveryBridge) Close() {
	if b == nil {
		return
	}
	b.once.Do(func() {
		b.cancel()
		b.reader.Close()
		b.writer.Close()
		b.childReader.Close()
		b.childWriter.Close()
		<-b.done
		b.wg.Wait()
		b.mu.Lock()
		scopes := b.scopes
		b.scopes = nil
		b.mu.Unlock()
		for _, scope := range scopes {
			scope.Leave()
		}
	})
}

// A response keeps its admission through EOF/Close, and rejects late data
// before it reaches inspection, output or a new source generation.
type mediaRecoveryBody struct {
	io.ReadCloser
	scope *transfer.RecoveryWorkScope
}

func (b *mediaRecoveryBody) Read(p []byte) (int, error) {
	if err := b.scope.Check(); err != nil {
		return 0, err
	}
	n, err := b.ReadCloser.Read(p)
	if checkErr := b.scope.Check(); checkErr != nil {
		return 0, checkErr
	}
	if errors.Is(err, io.EOF) {
		b.scope.Leave()
	}
	return n, err
}
func (b *mediaRecoveryBody) Close() error {
	err := b.ReadCloser.Close()
	b.scope.Leave()
	return err
}

// SetParentRecoveryContext is called by Core only after the previous run has
// joined. It does not create a recovery episode or replace its durable owner.
func (f *Fetcher) SetParentRecoveryContext(ctx context.Context) error {
	if ctx == nil || transfer.RecoveryLeaseFromContext(ctx) == nil {
		return transfer.ErrRecoveryHeld
	}
	if err := mediaRecoveryCheck(ctx); err != nil {
		return err
	}
	if _, ok := transfer.RecoveryLeaseFromContext(ctx).Owner.(transfer.RecoveryParentController); !ok {
		return transfer.ErrRecoveryHeld
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running || f.starting || f.resolveCancel != nil {
		return transfer.ErrRecoveryHeld
	}
	f.recoveryEpoch++
	f.parentContext = ctx
	return nil
}

// Source seals describe only this object's already-joined lifecycle. They are
// not a new source identity, a debit, or a reconstructed execution permit.
type mediaSourceSeal struct {
	f     *Fetcher
	lease *transfer.RecoveryLease
	epoch uint64
}

func (f *Fetcher) recoveryQuietLocked() bool {
	if f.running || f.starting || f.resolveCancel != nil {
		return false
	}
	for _, done := range []chan struct{}{f.runDone, f.resolveDone} {
		if done != nil {
			select {
			case <-done:
			default:
				return false
			}
		}
	}
	return true
}
func (f *Fetcher) RecoverySourceSeal() (transfer.RecoverySourceSeal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lease := transfer.RecoveryLeaseFromContext(f.parentContext)
	if lease == nil || !f.recoveryQuietLocked() {
		return nil, transfer.ErrRecoveryHeld
	}
	return &mediaSourceSeal{f: f, lease: lease, epoch: f.recoveryEpoch}, nil
}
func (s *mediaSourceSeal) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Core promised to call this outside its DB transaction/owner locks.
	if err := s.lease.Check(); err != nil {
		return err
	}
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.f.recoveryEpoch != s.epoch || !s.f.recoveryQuietLocked() {
		return transfer.ErrRecoveryStale
	}
	return nil
}
func (s *mediaSourceSeal) Snapshot(ctx context.Context) (transfer.RecoverySourceSnapshot, error) {
	if err := s.Check(ctx); err != nil {
		return transfer.RecoverySourceSnapshot{}, err
	}
	return transfer.RecoverySourceSnapshot{Token: s.lease.Token, SourceHash: s.lease.SourceHash,
		Epoch: s.epoch, Members: []string{"media/inspect", "media/download"}, IdentityHash: ""}, nil
}
