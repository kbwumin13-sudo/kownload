//go:build darwin && cgo

package download

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
)

// This wraps only the command's real reserve transaction. Core lifecycle uses
// the unchanged concrete Bolt store, including its trusted same-view identity.
type mcpActionReserveGate struct {
	AtomicStorage
	once      sync.Once
	committed chan struct{}
	release   chan struct{}
	ctx       context.Context
}

func (s *mcpActionReserveGate) UpdateAtomic(fn func(StorageTransaction) error) error {
	if err := s.AtomicStorage.UpdateAtomic(fn); err != nil {
		return err
	}
	s.once.Do(func() {
		close(s.committed)
		select {
		case <-s.release:
		case <-s.ctx.Done():
		}
	})
	return nil
}

func TestMCPStalePauseCannotRevokeNewGUIRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("GUI current generation retains its writer\n"), 32768)
	var blocked atomic.Bool
	blocked.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"gui-action-object"`)
		http.ServeContent(&mcpRuntimeBodyGate{ResponseWriter: w, ctx: r.Context(), block: &blocked}, r, "gui.bin", time.Unix(1700000000, 0), bytes.NewReader(payload))
	}))
	defer server.Close()
	db := filepath.Join(dir, "db")
	d := operationDownloader(t, NewBoltStorage(db), dir)
	id, err := d.CreateDirect(&base.Request{URL: server.URL}, &base.Options{Path: dir, Name: "gui.bin"})
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	waitWriter := func() {
		t.Helper()
		for ctx.Err() == nil {
			live := d.getTask(id)
			live.lock.Lock()
			written := live.fetcher != nil && live.fetcher.Progress().TotalDownloaded() > 0
			live.lock.Unlock()
			owner := live.recoveryOwner.Load()
			if written && owner != nil && owner.active.Load() && live.getStatus() == base.DownloadStatusRunning {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("current GUI writer did not start")
	}
	waitWriter()
	coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Generation > 0 })
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	store := NewBoltStorage(db)
	d = NewDownloader(&DownloaderConfig{Storage: store, StorageDir: dir, FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
	defer d.Close()
	identity, err := d.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"version": 1, "operations": map[string]bool{"create": false, "pause": true, "resume": true}, "datasets": []any{map[string]any{"id": identity.DatasetID, "taskIds": []string{id}, "outputRoots": []string{}}}, "outputRoots": map[string]string{}, "allowedEffects": []string{}})
	policy, err := mcpwritepolicy.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	network, err := transfer.NewMCPNetworkPolicy(transfer.MCPNetworkPolicyConfig{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := transfer.NewMCPHTTPClient(network, transfer.MCPHTTPOptions{DialTimeout: time.Second, ResponseHeaderTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ConfigureMCPWrites(policy, client); err != nil {
		t.Fatal(err)
	}
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	if err := d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	before := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Generation > 1 })
	waitWriter()
	holder := d.mcpWriteHolder()
	command := MCPWriteRequest{Version: 1, DatasetID: identity.DatasetID, PolicyID: holder.authority.PolicyID(), OperationID: "stale-pause-at-ack", Kind: MCPWritePause, Action: &MCPWriteActionRequest{TaskID: id, Expected: mcpTarget(before)}}
	gate := &mcpActionReserveGate{AtomicStorage: store, committed: make(chan struct{}), release: make(chan struct{}), ctx: ctx}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	defer release()
	type actionResult struct {
		receipt MCPWriteReceipt
		err     error
	}
	result := make(chan actionResult, 1)
	go func() {
		receipt, err := d.submitMCPTaskAction(ctx, holder, gate, command)
		result <- actionResult{receipt, err}
	}()
	select {
	case <-gate.committed:
	case <-ctx.Done():
		t.Fatal("reserve never reached ACK gate")
	}
	if err := d.Pause(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	live := d.getTask(id)
	live.statusLock.Lock()
	paused := live.pauseDone
	live.statusLock.Unlock()
	if paused != nil {
		select {
		case <-paused:
		case <-ctx.Done():
			t.Fatal("GUI pause did not join")
		}
	}
	if err := d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	current := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Generation > before.Generation })
	waitWriter()
	live.statusLock.Lock()
	generation, runCtx := live.runGeneration, live.runCancel
	live.statusLock.Unlock()
	if runCtx == nil || live.getStatus() != base.DownloadStatusRunning {
		t.Fatal("new GUI run not running")
	}
	owner := live.recoveryOwner.Load()
	lease, err := owner.currentLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case got := <-result:
		if got.err != nil || got.receipt.Result == nil || got.receipt.Result.Outcome != MCPWriteRejected || got.receipt.Result.ErrorCode == nil || *got.receipt.Result.ErrorCode != MCPWriteStaleTarget {
			t.Fatalf("stale command result: %+v %v", got.receipt, got.err)
		}
	case <-ctx.Done():
		t.Fatal("stale pause did not return")
	}
	live.statusLock.Lock()
	unchanged := live.runGeneration == generation && live.getStatus() == base.DownloadStatusRunning
	live.statusLock.Unlock()
	if !unchanged || lease.Check() != nil {
		t.Fatal("stale command revoked the newer run")
	}
	after := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Generation == current.Generation })
	if after.Episode != current.Episode || after.SourceRevision != current.SourceRevision || after.TotalUsed != current.TotalUsed {
		t.Fatal("stale pause mutated the current recovery account")
	}

	// A persisted-generation task in Wait can be selected by the real queue
	// scheduler while MCP pause is checking its target. Pin the existing holder
	// mutex only to stop the check under statusLock before notifyRunning takes
	// d.lock; after release neither path may wait on the other's lock.
	if err := d.Pause(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	live.statusLock.Lock()
	paused = live.pauseDone
	live.statusLock.Unlock()
	if paused != nil {
		select {
		case <-paused:
		case <-ctx.Done():
			t.Fatal("queue fixture pause did not join")
		}
	}
	d.lock.Lock()
	live.updateStatus(base.DownloadStatusWait)
	d.enqueueLocked(live)
	d.lock.Unlock()
	queued := coverageRuntimeRecord(t, d, id, func(r recoveryRecord) bool { return r.Generation == current.Generation })
	queueCommand := command
	queueCommand.OperationID = "pause-racing-queue"
	queueCommand.Action = &MCPWriteActionRequest{TaskID: id, Expected: mcpTarget(queued)}
	queueGate := &mcpActionReserveGate{AtomicStorage: store, committed: make(chan struct{}), release: make(chan struct{}), ctx: ctx}
	go func() {
		receipt, err := d.submitMCPTaskAction(ctx, holder, queueGate, queueCommand)
		result <- actionResult{receipt, err}
	}()
	select {
	case <-queueGate.committed:
	case <-ctx.Done():
		t.Fatal("queued reserve did not reach gate")
	}
	d.mcpConfigMu.Lock()
	var unlockOnce sync.Once
	unlockConfig := func() { unlockOnce.Do(d.mcpConfigMu.Unlock) }
	defer unlockConfig()
	close(queueGate.release)
	for {
		if !live.statusLock.TryLock() {
			break
		}
		live.statusLock.Unlock()
		if ctx.Err() != nil {
			t.Fatal("pause check did not take status barrier")
		}
		time.Sleep(time.Millisecond)
	}
	d.notifyRunning()
	for {
		if !d.lock.TryLock() {
			break
		}
		d.lock.Unlock()
		if ctx.Err() != nil {
			t.Fatal("queue scheduler did not enter main lock")
		}
		time.Sleep(time.Millisecond)
	}
	unlockConfig()
	select {
	case got := <-result:
		if got.err != nil || got.receipt.Result == nil || got.receipt.Result.Outcome != MCPWriteApplied {
			t.Fatalf("queued pause blocked or failed: %+v %v", got.receipt, got.err)
		}
	case <-ctx.Done():
		t.Fatal("queue and MCP pause deadlocked")
	}
	queried := make(chan struct{})
	go func() { _ = d.GetTasks(); close(queried) }()
	select {
	case <-queried:
	case <-ctx.Done():
		t.Fatal("main task queries remained blocked")
	}
	if live.getStatus() != base.DownloadStatusPause {
		t.Fatal("queue restarted the paused task")
	}
	blocked.Store(false)
	if err := d.Continue(&TaskFilter{IDs: []string{id}}); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, id, base.DownloadStatusDone, 8*time.Second)
	body, err := os.ReadFile(filepath.Join(dir, "gui.bin"))
	if err != nil || !bytes.Equal(body, payload) {
		t.Fatal("new run lost complete bytes", err)
	}
	if d.GetTask(id).MCPWrite != nil {
		t.Fatal("GUI action changed creation provenance")
	}
}
