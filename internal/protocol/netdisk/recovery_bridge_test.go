package netdisk

import (
	"context"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

type bridgeParentOwner struct {
	mu sync.Mutex
	lease *transfer.RecoveryLease
	refreshes, commits, activeAPI int
	unknown bool
}

func (owner *bridgeParentOwner) CheckRecovery(token transfer.RecoveryToken) error {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if token != owner.lease.Token {
		return transfer.ErrRecoveryStale
	}
	return nil
}
func (owner *bridgeParentOwner) RetryRecovery(context.Context, transfer.RecoveryToken, uint64, string, []string) (*transfer.RecoveryPermit, error) {
	return nil, transfer.ErrRecoveryExhausted
}
func (owner *bridgeParentOwner) FinishRecovery(context.Context, transfer.RecoveryToken, uint64, string) error { return nil }
func (owner *bridgeParentOwner) CurrentRecoveryRound(ctx context.Context, token transfer.RecoveryToken) (uint64, error) {
	if err := owner.CheckRecovery(token); err != nil { return 0, err }
	owner.mu.Lock(); defer owner.mu.Unlock()
	return uint64(owner.refreshes), ctx.Err()
}
func (owner *bridgeParentOwner) ReserveRecoveryAction(ctx context.Context, token transfer.RecoveryToken, seen uint64, kind transfer.RecoveryActionKind, work string, members []string, until time.Time) (*transfer.RecoveryActionPermit, error) {
	if err := owner.CheckRecovery(token); err != nil { return nil, err }
	owner.mu.Lock(); defer owner.mu.Unlock()
	if seen != uint64(owner.refreshes) || kind != transfer.RecoveryActionRefresh || work != "netdisk/api" || len(members) != 2 {
		return nil, transfer.ErrRecoveryWork
	}
	owner.refreshes++
	return transfer.IssueRecoveryActionPermit(token, uint64(owner.refreshes), kind, until, func(ctx context.Context) error {
		if err := ctx.Err(); err != nil { return err }; return owner.CheckRecovery(token)
	}), nil
}
func (owner *bridgeParentOwner) EnterRecoveryWork(ctx context.Context, token transfer.RecoveryToken, work string) (*transfer.RecoveryWorkScope, error) {
	if err := owner.CheckRecovery(token); err != nil { return nil, err }
	if work != "netdisk/api" { return nil, transfer.ErrRecoveryWork }
	owner.mu.Lock()
	lease := *owner.lease
	owner.activeAPI++
	owner.mu.Unlock()
	return transfer.IssueRecoveryWorkScope(transfer.WithRecoveryLease(ctx, &lease), func(ctx context.Context) error {
		if err := ctx.Err(); err != nil { return err }; return owner.CheckRecovery(token)
	}, func() { owner.mu.Lock(); owner.activeAPI--; owner.mu.Unlock() }), nil
}
func (owner *bridgeParentOwner) BindRecoveryChild(ctx context.Context, token transfer.RecoveryToken, work string, snapshot *transfer.HTTPExecutionContext) (*transfer.RecoveryLease, error) {
	if err := owner.CheckRecovery(token); err != nil { return nil, err }
	if work != "netdisk/http" { return nil, transfer.ErrRecoveryWork }
	owner.mu.Lock(); defer owner.mu.Unlock()
	return &transfer.RecoveryLease{Token: token, Owner: owner, SourceHash: snapshot.SourceHash(), Round: uint64(owner.refreshes), Context: owner.lease.Context, ParentContext: owner.lease.ParentContext}, ctx.Err()
}
func (owner *bridgeParentOwner) CommitRecoverySource(ctx context.Context, token transfer.RecoveryToken, action uint64, seal transfer.RecoverySourceSeal, checkpoint json.RawMessage) (*transfer.RecoveryLease, error) {
	snapshot, err := seal.Snapshot(ctx)
	if err != nil { return nil, err }
	var stored saved
	if json.Unmarshal(checkpoint, &stored) != nil || stored.Resource == nil || snapshot.IdentityHash != "" || snapshot.Token != token { return nil, transfer.ErrRecoveryWork }
	owner.mu.Lock(); defer owner.mu.Unlock()
	owner.commits++
	if owner.activeAPI != 0 || action != uint64(owner.refreshes) { return nil, transfer.ErrRecoveryWork }
	if owner.unknown { return nil, transfer.ErrRecoveryUnavailable }
	next := *owner.lease
	next.Token.SourceRevision++
	next.Round = action
	owner.lease = &next
	return &next, nil
}

type bridgeAPIProvider struct { fixtureProvider; api string }
func (provider *bridgeAPIProvider) Info() model.ProviderInfo {
	info := provider.fixtureProvider.Info(); info.APIHosts = []string{"127.0.0.1"}; return info
}
func (provider *bridgeAPIProvider) Resolve(ctx context.Context, client *cloud.APIClient, _ *cloud.Session, _ model.Source, file model.File) (*cloud.Download, error) {
	var result struct { URL string `json:"url"`; Hash string `json:"hash"` }
	if err := client.JSON(ctx, "GET", provider.api, nil, nil, &result); err != nil { return nil, err }
	file.Hash = result.Hash
	return &cloud.Download{URL: result.URL, File: file}, nil
}

// A real provider/APIClient -> HTTP child path; the durable Core owner is a
// bounded substitute here. Core/F/D/B persistence is verified only after
// integration with the Core owner's immutable implementation.
func TestParentRecoveryProviderRefresh(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknownACK=%v", unknown), func(t *testing.T) {
			body := []byte(strings.Repeat("parent-recovery-", 2048))
			digest := sha256.Sum256(body)
			var apiCalls, childCalls atomic.Int32
			var baseURL string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api":
					link := apiCalls.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]string{"url": fmt.Sprintf("%s/file?link=%d", baseURL, link), "hash": "sha256:"+hex.EncodeToString(digest[:])})
				case "/file":
					if r.UserAgent() != "frozen-fixture" { t.Errorf("child read mutable/default UA: %q", r.UserAgent()) }
					if r.URL.Query().Get("link") == "1" { w.WriteHeader(http.StatusForbidden); return }
					childCalls.Add(1)
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
					_, _ = w.Write(body)
				default: http.NotFound(w, r)
				}
			}))
			baseURL = server.URL
			t.Cleanup(server.Close)
			service := cloud.NewService(&bridgeAPIProvider{api: server.URL+"/api"})
			t.Cleanup(service.Close)
			capture, err := service.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/share", InstanceID: "fixture", ContextID: "fixture", AccountID: "fixture"})
			if err != nil { t.Fatal(err) }
			ctl := controller.NewController()
			ctl.GetRecoveryProxySnapshot = func(*base.RequestProxy) (func(*http.Request) (*url.URL, error), string, bool) { return nil, "fixture", true }
			f := (&FetcherManager{Service: service}).Build().(*Fetcher)
			f.Setup(ctl)
			t.Cleanup(func() { _ = f.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := &base.Request{URL: capture.Source.URL, Protocol: "netdisk", Extra: &model.ReqExtra{Provider: "quark", Source: capture.Source, SessionID: capture.SessionID, AccountID: "fixture", File: model.File{ID: "file", Name: "a.bin", Size: int64(len(body))}}}
			parentSnapshot, snapshotErr := transfer.NewProtocolExecutionContext("netdisk", req, json.RawMessage(`{"userAgent":"frozen-fixture","connections":1}`), "fixture", nil)
			if snapshotErr != nil { t.Fatal(snapshotErr) }
			owner := &bridgeParentOwner{unknown: unknown}
			owner.lease = &transfer.RecoveryLease{Token: transfer.RecoveryToken{TaskID: "fixture", Episode: "episode", Generation: 1, SourceRevision: 1}, Owner: owner, SourceHash: parentSnapshot.SourceHash(), ActionVersion: 4, Context: ctx, ParentContext: parentSnapshot}
			ctx = transfer.WithRecoveryLease(ctx, owner.lease)
			if err = f.SetParentRecoveryContext(ctx); err != nil { t.Fatal(err) }
			ctl.GetConfig = func(any) { panic("mutable global config consulted after parent capture") }
			ctl.GetProxy = func(*base.RequestProxy) func(*http.Request) (*url.URL,error) { panic("mutable global proxy consulted after parent capture") }
			ctl.GetProxyPolicy = func(*base.RequestProxy) (func(*http.Request) (*url.URL,error),string) { panic("mutable global proxy policy consulted after parent capture") }
			ctl.GetRecoveryProxySnapshot = func(*base.RequestProxy) (func(*http.Request) (*url.URL,error),string,bool) { panic("mutable global recovery proxy consulted after parent capture") }
			dir := t.TempDir()
			if err = f.ResolveTask(ctx, req, &base.Options{Path: dir}); err != nil { t.Fatal(err) }
			rawParent, _ := json.Marshal(f.Meta().Req)
			sentinel := []byte("untouched-before-source-ACK")
			if err = os.WriteFile(filepath.Join(dir,"a.bin"), sentinel, 0600); err != nil { t.Fatal(err) }
			wait := f.BindRunWait(ctx)
			if err = f.Start(); err != nil { t.Fatal(err) }
			done := make(chan error,1)
			go func(){ done <- wait() }()
			select { case err = <-done: case <-time.After(5*time.Second): t.Fatal("provider/child run blocked") }
			got, readErr := os.ReadFile(filepath.Join(dir,"a.bin"))
			if readErr != nil { t.Fatal(readErr) }
			afterParent, _ := json.Marshal(f.Meta().Req)
			if !bytes.Equal(rawParent, afterParent) { t.Fatal("provider metadata rewrote frozen parent raw request") }
			if unknown {
				if err == nil || childCalls.Load() != 0 || !bytes.Equal(got,sentinel) { t.Fatalf("unknown ACK sent IO or changed bytes: err=%v requests=%d",err,childCalls.Load()) }
			} else if err != nil || !bytes.Equal(got,body) { t.Fatalf("refresh failed or wrong bytes: %v bytes=%d",err,len(got)) }
			owner.mu.Lock()
			defer owner.mu.Unlock()
			if apiCalls.Load() != 2 || owner.refreshes != 1 || owner.commits != 1 || owner.activeAPI != 0 { t.Fatalf("unexpected bounded plan: API=%d refresh=%d commits=%d active=%d",apiCalls.Load(),owner.refreshes,owner.commits,owner.activeAPI) }
		})
	}
}

type lateRecoveryProvider struct {
	fixtureProvider
	block atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (p *lateRecoveryProvider) Resolve(ctx context.Context, client *cloud.APIClient, session *cloud.Session, source model.Source, file model.File) (*cloud.Download, error) {
	if p.block.Load() {
		close(p.entered)
		// Deliberately return a successful link after cancellation. The parent
		// must reject this result before it constructs another HTTP child.
		<-p.release
	}
	return p.fixtureProvider.Resolve(ctx, client, session, source, file)
}

func TestCancelledProviderRefreshCannotPublishLateChild(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Length", "3")
		_, _ = w.Write([]byte("abc"))
	}))
	t.Cleanup(server.Close)
	provider := &lateRecoveryProvider{fixtureProvider: fixtureProvider{url: server.URL, size: 3}, entered: make(chan struct{}), release: make(chan struct{})}
	service := cloud.NewService(provider)
	t.Cleanup(service.Close)
	capture, err := service.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/share", InstanceID: "fixture", ContextID: "fixture", AccountID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	f := (&FetcherManager{Service: service}).Build().(*Fetcher)
	f.Setup(controller.NewController())
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(provider.release) }); _ = f.Close() })
	req := &base.Request{URL: capture.Source.URL, Protocol: "netdisk", Extra: &model.ReqExtra{Provider: "quark", Source: capture.Source, SessionID: capture.SessionID, AccountID: "fixture", File: model.File{ID: "file", Name: "a", Size: 3}}}
	if err = f.Resolve(req, &base.Options{Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	original := f.child
	f.refresh = true
	f.mu.Unlock()
	before := requests.Load()
	provider.block.Store(true)
	if err = f.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("provider refresh did not start")
	}
	paused := make(chan error, 1)
	go func() { paused <- f.Pause() }()
	// CancelResolve observes the same operation context as Pause; the late
	// provider remains under our control until both cancellation and invalidation.
	f.CancelResolve()
	f.mu.Lock()
	if f.child != original {
		t.Error("child changed while provider was blocked")
	}
	f.mu.Unlock()
	release.Do(func() { close(provider.release) })
	select {
	case err = <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Pause did not join the cancelled refresh")
	}
	f.mu.Lock()
	unchanged := f.child == original && !f.running
	f.mu.Unlock()
	if !unchanged || requests.Load() != before {
		t.Fatalf("late provider published or sent HTTP: same child=%v HTTP before=%d after=%d", unchanged, before, requests.Load())
	}
}
