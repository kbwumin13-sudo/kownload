package http

import (
	"context"
	"sort"
	"sync"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// recoveryWaveBarrier counts actual IO/write scopes, never the goroutines
// parked waiting for admission. Its coordinator is separate from these scopes;
// drain must not run on a worker before that worker has called Leave.
// The run context and scheduler slot survive a short source cooldown.
type recoveryWaveBarrier struct {
	mu         sync.Mutex
	parent     context.Context
	token      transfer.RecoveryToken
	sourceHash string
	wave       context.Context
	cancel     context.CancelFunc
	epoch      uint64
	blocked    bool
	sealed     bool
	failure    error
	active     map[string]uint64
	idle       chan struct{}
	changed    chan struct{}
}

type recoveryWaveWork struct {
	Context context.Context
	wave    context.Context
	barrier *recoveryWaveBarrier
	epoch   uint64
	leave   func()
	once    sync.Once
}

func (work *recoveryWaveWork) Leave() {
	if work != nil {
		work.once.Do(work.leave)
	}
}

// RecoveryWaveSeal is created only after the current wave has no outstanding
// request/body/write/prefetch scope. It grants no network or budget authority.
// Core must validate its token/source/workset, perform atomic admission, then
// let the protocol reopen the same sealed barrier after the confirmed ACK.
type RecoveryWaveSeal struct {
	barrier *recoveryWaveBarrier
	epoch   uint64
	members []string
}

// RecoveryWaveSnapshot is an owned projection captured before UpdateAtomic.
// DB callbacks compare these values only; they never call back into the seal
// or take a barrier/run/owner/live Task lock. It is not an admission permit.
type RecoveryWaveSnapshot struct {
	Token      transfer.RecoveryToken
	SourceHash string
	Epoch      uint64
	Members    []string
}

func (seal *RecoveryWaveSeal) Snapshot(ctx context.Context) (RecoveryWaveSnapshot, error) {
	if seal == nil || seal.barrier == nil || ctx == nil {
		return RecoveryWaveSnapshot{}, transfer.ErrRecoveryWork
	}
	barrier := seal.barrier
	barrier.mu.Lock()
	defer barrier.mu.Unlock()
	if err := seal.checkLocked(ctx); err != nil {
		return RecoveryWaveSnapshot{}, err
	}
	return RecoveryWaveSnapshot{Token: barrier.token, SourceHash: barrier.sourceHash,
		Epoch: seal.epoch, Members: append([]string{}, seal.members...)}, nil
}

type sharedIOContextKey struct{}

func newRecoveryWaveBarrier(parent context.Context, lease *transfer.RecoveryLease) (*recoveryWaveBarrier, error) {
	if parent == nil || lease == nil || lease.Owner == nil || lease.Token.TaskID == "" ||
		lease.Token.Episode == "" || lease.Token.Generation == 0 || lease.Token.SourceRevision == 0 || lease.SourceHash == "" {
		return nil, transfer.ErrRecoveryWork
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	wave, cancel := context.WithCancel(parent)
	idle := make(chan struct{})
	close(idle)
	return &recoveryWaveBarrier{parent: parent, token: lease.Token, sourceHash: lease.SourceHash,
		wave: wave, cancel: cancel, epoch: 1, active: map[string]uint64{}, idle: idle, changed: make(chan struct{})}, nil
}

func (barrier *recoveryWaveBarrier) enter(ctx context.Context, workID string) (*recoveryWaveWork, error) {
	if ctx == nil || workID == "" {
		return nil, transfer.ErrRecoveryWork
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		barrier.mu.Lock()
		if err := barrier.parent.Err(); err != nil {
			barrier.mu.Unlock()
			return nil, err
		}
		if barrier.failure != nil {
			err := barrier.failure
			barrier.mu.Unlock()
			return nil, err
		}
		if barrier.blocked {
			changed := barrier.changed
			barrier.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-barrier.parent.Done():
				return nil, barrier.parent.Err()
			case <-changed:
			}
			continue
		}
		if _, exists := barrier.active[workID]; exists {
			barrier.mu.Unlock()
			return nil, transfer.ErrRecoveryWork
		}
		if len(barrier.active) == 0 {
			barrier.idle = make(chan struct{})
		}
		epoch := barrier.epoch
		barrier.active[workID] = epoch
		wave := barrier.wave
		barrier.mu.Unlock()

		ioContext, cancel := context.WithCancel(ctx)
		ioContext = context.WithValue(ioContext,sharedIOContextKey{},true)
		stop := context.AfterFunc(wave, cancel)
		work := &recoveryWaveWork{Context: ioContext, wave: wave, barrier: barrier, epoch: epoch}
		work.leave = func() {
			stop()
			cancel()
			barrier.mu.Lock()
			// Even a late cleanup is scoped to its original registration.
			if current, exists := barrier.active[workID]; exists && current == epoch && barrier.epoch == epoch {
				delete(barrier.active, workID)
				if len(barrier.active) == 0 {
					close(barrier.idle)
				}
			}
			barrier.mu.Unlock()
		}
		if err := ioContext.Err(); err != nil {
			work.Leave()
			return nil, err
		}
		return work, nil
	}
}

func waveMembers(input []string) ([]string, error) {
	if len(input) == 0 || len(input) > 1024 {
		return nil, transfer.ErrRecoveryWork
	}
	members := append([]string{}, input...)
	sort.Strings(members)
	for i, member := range members {
		if member == "" || i > 0 && member == members[i-1] {
			return nil, transfer.ErrRecoveryWork
		}
	}
	return members, nil
}

// drain closes admission and cancels only child IO. It waits without holding
// any fetcher/connection/Task/owner/DB lock, and never waits for the whole run.
func (barrier *recoveryWaveBarrier) drain(ctx context.Context, input []string) (_ *RecoveryWaveSeal, err error) {
	if ctx == nil {
		return nil, transfer.ErrRecoveryWork
	}
	members, err := waveMembers(input)
	if err != nil {
		return nil, err
	}
	barrier.mu.Lock()
	if barrier.failure != nil {
		failure := barrier.failure
		barrier.mu.Unlock()
		return nil, failure
	}
	if barrier.blocked || ctx.Err() != nil || barrier.parent.Err() != nil {
		barrier.mu.Unlock()
		return nil, transfer.ErrRecoveryStale
	}
	barrier.blocked = true
	cancel, idle, epoch, changed := barrier.cancel, barrier.idle, barrier.epoch, barrier.changed
	barrier.mu.Unlock()
	// Once this call closes admission, every unsuccessful exit owns failure
	// cleanup. Preserve the first close error and wake both enter and drain;
	// never manufacture quiescence or leave an ownerless blocked gate.
	defer func() {
		if err != nil {
			barrier.close(err)
			barrier.mu.Lock()
			err = barrier.failure
			barrier.mu.Unlock()
		}
	}()
	cancel()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-barrier.parent.Done():
		return nil, barrier.parent.Err()
	case <-idle:
	case <-changed:
		barrier.mu.Lock()
		failure := barrier.failure
		barrier.mu.Unlock()
		if failure == nil {
			failure = transfer.ErrRecoveryStale
		}
		return nil, failure
	}
	barrier.mu.Lock()
	defer barrier.mu.Unlock()
	if barrier.failure != nil {
		return nil, barrier.failure
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := barrier.parent.Err(); err != nil {
		return nil, err
	}
	if barrier.epoch != epoch || len(barrier.active) != 0 {
		return nil, transfer.ErrRecoveryStale
	}
	barrier.sealed = true
	return &RecoveryWaveSeal{barrier: barrier, epoch: epoch, members: members}, nil
}

func (seal *RecoveryWaveSeal) Token() transfer.RecoveryToken {
	if seal == nil || seal.barrier == nil {
		return transfer.RecoveryToken{}
	}
	return seal.barrier.token
}
func (seal *RecoveryWaveSeal) SourceHash() string {
	if seal == nil || seal.barrier == nil {
		return ""
	}
	return seal.barrier.sourceHash
}
func (seal *RecoveryWaveSeal) Members() []string {
	if seal == nil {
		return nil
	}
	return append([]string{}, seal.members...)
}
func (seal *RecoveryWaveSeal) Check(ctx context.Context) error {
	if seal == nil || seal.barrier == nil || ctx == nil {
		return transfer.ErrRecoveryWork
	}
	barrier := seal.barrier
	barrier.mu.Lock()
	defer barrier.mu.Unlock()
	return seal.checkLocked(ctx)
}
func (seal *RecoveryWaveSeal) checkLocked(ctx context.Context) error {
	barrier := seal.barrier
	if ctx.Err() != nil || barrier.parent.Err() != nil || barrier.failure != nil ||
		seal.epoch != barrier.epoch || !barrier.blocked || !barrier.sealed || len(barrier.active) != 0 {
		return transfer.ErrRecoveryStale
	}
	return nil
}

// reopen is called by the coordinator only after the owner's full-CAS next
// wave transaction ACK. A query, elapsed timer, or old seal never calls it.
func (seal *RecoveryWaveSeal) reopen(ctx context.Context) error {
	if seal == nil || seal.barrier == nil || ctx == nil {
		return transfer.ErrRecoveryWork
	}
	barrier := seal.barrier
	barrier.mu.Lock()
	defer barrier.mu.Unlock()
	if err := seal.checkLocked(ctx); err != nil {
		return err
	}
	barrier.wave, barrier.cancel = context.WithCancel(barrier.parent)
	barrier.epoch++
	barrier.blocked, barrier.sealed = false, false
	close(barrier.changed)
	barrier.changed = make(chan struct{})
	return nil
}

func (barrier *recoveryWaveBarrier) close(err error) {
	if err == nil {
		err = transfer.ErrRecoveryStale
	}
	barrier.mu.Lock()
	if barrier.failure == nil {
		barrier.failure = err
		barrier.cancel()
		close(barrier.changed)
	}
	barrier.mu.Unlock()
}
