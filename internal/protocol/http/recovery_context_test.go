package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type recoveryContextOwner struct{ active atomic.Bool }

func (o *recoveryContextOwner) CheckRecovery(transfer.RecoveryToken) error {
	if !o.active.Load() {
		return transfer.ErrRecoveryStale
	}
	return nil
}
func (o *recoveryContextOwner) RetryRecovery(context.Context, transfer.RecoveryToken, uint64, string, []string) (*transfer.RecoveryPermit, error) {
	return nil, transfer.ErrRecoveryWork
}
func (o *recoveryContextOwner) FinishRecovery(context.Context, transfer.RecoveryToken, uint64, string) error {
	return nil
}

func TestRecoveryCapturedDefaultsEqualWireAfterConfigurationChanges(t *testing.T) {
	var a, b atomic.Int32
	proxyA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.Add(1)
		if r.Header.Get("User-Agent") != "snapshot-UA-A" {
			t.Error("wire consumed a different UA")
		}
		w.Header().Set("Content-Length", "13")
		_, _ = w.Write([]byte("snapshot-body"))
	}))
	defer proxyA.Close()
	proxyB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Add(1)
		t.Error("wire reread changed proxy")
		w.WriteHeader(500)
	}))
	defer proxyB.Close()
	currentUA := "snapshot-UA-A"
	currentProxy := &base.DownloaderProxyConfig{Enable: true, Scheme: "http", Host: strings.TrimPrefix(proxyA.URL, "http://")}
	f := buildFetcher()
	f.ctl.GetConfig = func(value any) { *value.(**config) = &config{UserAgent: currentUA, Connections: 1} }
	f.ctl.GetRecoveryProxySnapshot = func(req *base.RequestProxy) (func(*http.Request) (*url.URL, error), string, bool) {
		return base.CaptureProxySnapshot(req, currentProxy)
	}
	request := &base.Request{URL: "http://target.invalid/snapshot.bin"}
	snapshot, supported, err := f.CaptureRecoveryContext(request)
	if err != nil || !supported {
		t.Fatal("context not captured", err)
	}
	owner := &recoveryContextOwner{}
	owner.active.Store(true)
	lease := &transfer.RecoveryLease{Token: transfer.RecoveryToken{TaskID: "synthetic", Episode: "synthetic", Generation: 1, SourceRevision: 1}, Owner: owner, SourceHash: snapshot.SourceHash()}
	f.SetRecoveryContext(snapshot)
	f.SetRecoveryLease(lease)
	currentUA = "snapshot-UA-B"
	currentProxy.Host = strings.TrimPrefix(proxyB.URL, "http://")
	f.ctl.GetProxy = func(*base.RequestProxy) func(*http.Request) (*url.URL, error) {
		t.Error("managed path called dynamic GetProxy")
		return nil
	}
	f.ctl.GetProxyPolicy = func(*base.RequestProxy) (func(*http.Request) (*url.URL, error), string) {
		t.Error("managed path called dynamic GetProxyPolicy")
		return nil, "unproved"
	}
	wait := f.BindRunWait(context.Background())
	if err = f.ResolveContext(transfer.WithRecoveryLease(context.Background(), lease), request, rangeProbeOptions(t)); err != nil {
		t.Fatal(err)
	}
	if err = f.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("captured download did not settle")
	}
	defer f.Pause()
	if a.Load() != 1 || b.Load() != 0 {
		t.Fatalf("proxy snapshot mismatch A=%d B=%d", a.Load(), b.Load())
	}
	owner.active.Store(false)
	ctx := transfer.WithRecoveryLease(context.Background(), lease)
	if _, err = f.buildRequest(ctx, request); !errors.Is(err, transfer.ErrRecoveryStale) {
		t.Fatal("invalidated lease built a late request", err)
	}
}
func TestRecoveryCaptureCannotInventMissingOrUnsupportedProxyProof(t *testing.T) {
	f := buildFetcher()
	req := &base.Request{URL: "https://source.example/file"}
	for _, hook := range []func(*base.RequestProxy) (func(*http.Request) (*url.URL, error), string, bool){nil, func(*base.RequestProxy) (func(*http.Request) (*url.URL, error), string, bool) {
		return nil, "system-name-only", false
	}, func(*base.RequestProxy) (func(*http.Request) (*url.URL, error), string, bool) { return nil, "", true }} {
		f.ctl.GetRecoveryProxySnapshot = hook
		if snapshot, supported, err := f.CaptureRecoveryContext(req); err != nil || supported || snapshot != nil {
			t.Fatal("unproved context was certified", supported, err)
		}
	}
}

type recoveryWorkTestOwner struct {
	recoveryContextOwner
	retries atomic.Int32
}

func (o *recoveryWorkTestOwner) RetryRecovery(_ context.Context, token transfer.RecoveryToken, round uint64, work string, _ []string) (*transfer.RecoveryPermit, error) {
	o.retries.Add(1)
	return transfer.IssueRecoveryPermit(&transfer.RecoveryLease{Token: token, Owner: o}, round+1, work), nil
}
func TestRecoveryInitialSlotsAreBoundedAndReplacementCannotBeFree(t *testing.T) {
	owner := &recoveryWorkTestOwner{}
	owner.active.Store(true)
	lease := &transfer.RecoveryLease{Token: transfer.RecoveryToken{TaskID: "synthetic", Episode: "synthetic", Generation: 1, SourceRevision: 1}, Owner: owner}
	ctx, cancel := context.WithCancel(transfer.WithRecoveryLease(context.Background(), lease))
	defer cancel()
	f := buildFetcher()
	f.cancel = cancel
	f.ctx = ctx
	f.SetRecoveryLease(lease)
	if err := f.configureInitialRecoveryWork(2); err != nil {
		t.Fatal(err)
	}
	a := &connection{ID: 0, ctx: ctx, Chunk: newChunk(0, 100)}
	b := &connection{ID: 1, ctx: ctx, Chunk: newChunk(101, 200)}
	f.connections = []*connection{a, b}
	for _, conn := range []*connection{a, b} {
		if err := f.authorizeInitialRecoveryWork(conn); err != nil {
			t.Fatal(err)
		}
	}
	if owner.retries.Load() != 0 {
		t.Fatal("normal slow-start slots charged as retries")
	}
	for i := int32(1); i <= 2; i++ {
		replacement := &connection{ID: 0, ctx: ctx, Chunk: newChunk(int64(i), 100)}
		if err := f.authorizeInitialRecoveryWork(replacement); err != nil {
			t.Fatal(err)
		}
		if owner.retries.Load() != i {
			t.Fatal("replacement acquired another free initial attempt")
		}
	}
	if err := f.authorizeInitialRecoveryWork(&connection{ID: 2, ctx: ctx}); !errors.Is(err, transfer.ErrRecoveryWork) {
		t.Fatal("work churn entered frozen initial set", err)
	}
	if owner.retries.Load() != 2 {
		t.Fatal("out-of-scope work charged or sent IO")
	}
}

func TestRecoveryLateInitialSlotCannotJoinAnEarlierPaidWave(t *testing.T) {
	owner := &recoveryWorkTestOwner{}
	owner.active.Store(true)
	lease := &transfer.RecoveryLease{Token: transfer.RecoveryToken{TaskID: "synthetic", Episode: "synthetic", Generation: 1, SourceRevision: 1}, Owner: owner}
	ctx, cancel := context.WithCancel(transfer.WithRecoveryLease(context.Background(), lease))
	defer cancel()
	f := buildFetcher()
	f.cancel = cancel
	f.ctx = ctx
	f.SetRecoveryLease(lease)
	if err := f.configureInitialRecoveryWork(2); err != nil {
		t.Fatal(err)
	}
	a := &connection{ID: 0, ctx: ctx, Chunk: newChunk(0, 100)}
	b := &connection{ID: 1, ctx: ctx}
	f.connections = []*connection{a, b}
	if err := f.authorizeInitialRecoveryWork(a); err != nil {
		t.Fatal(err)
	}
	if f.retryManagedConnection(a, io.ErrUnexpectedEOF) {
		t.Fatal("first paid wave refused")
	}
	b.Chunk = newChunk(101, 200)
	if err := f.authorizeInitialRecoveryWork(b); err != nil {
		t.Fatal(err)
	}
	if f.retryManagedConnection(b, io.ErrUnexpectedEOF) {
		t.Fatal("late slot starved instead of paying for a new wave")
	}
	if owner.retries.Load() != 2 {
		t.Fatal("late slot reused an earlier wave")
	}
}
