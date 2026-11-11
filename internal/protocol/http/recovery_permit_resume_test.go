package http

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type permitResumeOwner struct {
	generation    atomic.Uint64
	debits        atomic.Int32
	consumed      chan struct{}
	cancelBackoff func()
}

func (o *permitResumeOwner) CheckRecovery(token transfer.RecoveryToken) error {
	if token.Generation != o.generation.Load() {
		return transfer.ErrRecoveryStale
	}
	return nil
}
func (o *permitResumeOwner) RetryRecovery(_ context.Context, token transfer.RecoveryToken, round uint64, work string, _ []string) (*transfer.RecoveryPermit, error) {
	o.debits.Add(1)
	tracking := &permitResumeConsumeOwner{parent: o}
	return transfer.IssueRecoveryPermit(&transfer.RecoveryLease{Token: token, Owner: tracking}, round+1, work), nil
}
func (o *permitResumeOwner) FinishRecovery(_ context.Context, token transfer.RecoveryToken, _ uint64, _ string) error {
	return o.CheckRecovery(token)
}

type permitResumeConsumeOwner struct {
	parent *permitResumeOwner
	checks atomic.Int32
}

func (o *permitResumeConsumeOwner) CheckRecovery(token transfer.RecoveryToken) error {
	if err := o.parent.CheckRecovery(token); err != nil {
		return err
	}
	if o.checks.Add(1) == 2 {
		// Consume has set its one-shot bit; cancel before retryManagedConnection's
		// backoff can admit any repeat IO, matching Pause's cancellation ordering.
		o.parent.cancelBackoff()
		close(o.parent.consumed)
	}
	return nil
}
func (o *permitResumeConsumeOwner) RetryRecovery(ctx context.Context, token transfer.RecoveryToken, round uint64, work string, members []string) (*transfer.RecoveryPermit, error) {
	return o.parent.RetryRecovery(ctx, token, round, work, members)
}
func (o *permitResumeConsumeOwner) FinishRecovery(ctx context.Context, token transfer.RecoveryToken, round uint64, work string) error {
	return o.parent.FinishRecovery(ctx, token, round, work)
}

func TestRecoveryPauseInConsumedPermitBackoffResumesSameConnection(t *testing.T) {
	payload := bytes.Repeat([]byte("permit-pause-resume"), 1<<16)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"same-permit-resource"`)
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			_, _ = w.Write(payload[:4096])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if requests.Add(1) == 1 {
			w.WriteHeader(500)
			return
		}
		http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(payload))
	}))
	defer server.Close()
	f := buildFetcher()
	defer f.Pause()
	f.ctl.GetRecoveryProxySnapshot = func(req *base.RequestProxy) (func(*http.Request) (*url.URL, error), string, bool) {
		return base.CaptureProxySnapshot(req, nil)
	}
	request := &base.Request{URL: server.URL + "/payload"}
	snapshot, supported, err := f.CaptureRecoveryContext(request)
	if err != nil || !supported {
		t.Fatal(err)
	}
	owner := &permitResumeOwner{consumed: make(chan struct{})}
	owner.generation.Store(1)
	owner.cancelBackoff = func() {
		if f.cancel != nil {
			f.cancel()
		}
	}
	oldLease := &transfer.RecoveryLease{Token: transfer.RecoveryToken{TaskID: "synthetic-resume", Episode: "same-episode", Generation: 1, SourceRevision: 1}, Owner: owner, SourceHash: snapshot.SourceHash()}
	f.SetRecoveryContext(snapshot)
	f.SetRecoveryLease(oldLease)
	f.BindRunWait(context.Background())
	opts := rangeProbeOptions(t)
	opts.Name = "permit-resume.bin"
	if err = f.ResolveContext(transfer.WithRecoveryLease(context.Background(), oldLease), request, opts); err != nil {
		t.Fatal(err)
	}
	if err = f.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-owner.consumed:
	case <-time.After(3 * time.Second):
		t.Fatal("real failed range did not consume retry permit")
	}
	if err = f.Pause(); err != nil {
		t.Fatal(err)
	}
	f.connMu.Lock()
	oldConnection := f.connections[0]
	pending := oldConnection.recoveryPermit != nil
	f.connMu.Unlock()
	if !pending || owner.debits.Load() != 1 || requests.Load() != 1 {
		t.Fatal("did not stop after consumed permit and before retry IO", pending, owner.debits.Load(), requests.Load())
	}
	owner.generation.Store(2)
	lease := &transfer.RecoveryLease{Token: transfer.RecoveryToken{TaskID: "synthetic-resume", Episode: "same-episode", Generation: 2, SourceRevision: 1}, Round: 1, Owner: owner, SourceHash: snapshot.SourceHash()}
	f.SetRecoveryLease(lease)
	f.SetRecoveryContext(snapshot)
	wait := f.BindRunWait(context.Background())
	if err = f.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- wait() }()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal("continued request consumed old token", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed download did not finish")
	}
	body, err := os.ReadFile(f.Meta().SingleFilepath())
	if err != nil || !bytes.Equal(body, payload) {
		t.Fatal("resumed payload differs", err)
	}
	f.connMu.Lock()
	same := len(f.connections) > 0 && f.connections[0] == oldConnection
	f.connMu.Unlock()
	if !same || owner.debits.Load() != 1 || requests.Load() != 2 {
		t.Fatal("connection replaced, debit reset or repeat IO", same, owner.debits.Load(), requests.Load())
	}
}
