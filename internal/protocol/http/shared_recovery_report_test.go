package http

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// This controller observes the protocol boundary, not a real database. The
// existing Core multi-Range test separately checks the durable action writes.
// FinishRecovery supplies a legal post-Leave gate without a product test hook.
type sharedLateReportOwner struct {
	lease          *transfer.RecoveryLease
	reserves       atomic.Int32
	admissions     atomic.Int32
	lateLeft       chan struct{}
	lateRelease    chan struct{}
	secondReserved chan struct{}
	leftOnce       sync.Once
}

func (*sharedLateReportOwner) CheckRecovery(transfer.RecoveryToken) error { return nil }
func (*sharedLateReportOwner) RetryRecovery(context.Context, transfer.RecoveryToken, uint64, string, []string) (*transfer.RecoveryPermit, error) {
	return nil, transfer.ErrRecoveryWork
}
func (o *sharedLateReportOwner) FinishRecovery(ctx context.Context, _ transfer.RecoveryToken, round uint64, work string) error {
	if round == 0 && work == "worker/1" {
		o.leftOnce.Do(func() { close(o.lateLeft) })
		select {
		case <-o.lateRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (o *sharedLateReportOwner) ReserveRecoveryAction(_ context.Context, token transfer.RecoveryToken, _ uint64, kind transfer.RecoveryActionKind, _ string, members []string, until time.Time) (*transfer.RecoveryActionPermit, error) {
	id := o.reserves.Add(1)
	if len(members) != 2 || members[0] != "worker/0" || members[1] != "worker/1" {
		return nil, transfer.ErrRecoveryWork
	}
	if id == 2 {
		close(o.secondReserved)
	}
	return transfer.IssueRecoveryActionPermit(token, uint64(id), kind, until, func(context.Context) error { return nil }), nil
}
func (*sharedLateReportOwner) ExtendRecoveryDeferral(_ context.Context, _ transfer.RecoveryToken, _ uint64, _ string, _ []string, until time.Time) (time.Time, error) {
	return until, nil
}
func (o *sharedLateReportOwner) AdmitRecoveryWave(ctx context.Context, seal *RecoveryWaveSeal, id uint64) (map[string]*transfer.RecoveryPermit, error) {
	if err := seal.Check(ctx); err != nil {
		return nil, err
	}
	o.admissions.Add(1)
	permits := map[string]*transfer.RecoveryPermit{}
	for _, work := range seal.Members() {
		permits[work] = transfer.IssueRecoveryPermit(o.lease, id, work)
	}
	return permits, nil
}

func TestSharedRecoveryWaveLate429ReportDoesNotChargeReopenedWave(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	var initial atomic.Int32
	initialReady := make(chan struct{})
	resumedA, resumedB := make(chan struct{}), make(chan struct{})
	var readyOnce, aOnce, bOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		begin, end := 0, 0
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if initial.Add(1) <= 2 {
			if initial.Load() == 2 {
				readyOnce.Do(func() { close(initialReady) })
			}
			select {
			case <-initialReady:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/1048576", begin, end))
		w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
		w.WriteHeader(206)
		w.(http.Flusher).Flush()
		if begin == 0 {
			aOnce.Do(func() { close(resumedA) })
		} else {
			bOnce.Do(func() { close(resumedB) })
		}
		<-r.Context().Done() // Keep the next wave active until the assertion.
	}))
	defer server.Close()
	f := buildFetcher()
	f.ctl.GetRecoveryProxySnapshot = func(req *base.RequestProxy) (func(*http.Request) (*url.URL, error), string, bool) {
		return base.CaptureProxySnapshot(req, nil)
	}
	request := &base.Request{URL: server.URL + "/two-ranges"}
	snapshot, supported, err := f.CaptureRecoveryContext(request)
	if err != nil || !supported {
		t.Fatal(err)
	}
	owner := &sharedLateReportOwner{lateLeft: make(chan struct{}), lateRelease: make(chan struct{}), secondReserved: make(chan struct{})}
	lease := &transfer.RecoveryLease{Token: transfer.RecoveryToken{TaskID: "late-wave", Episode: "same-episode", Generation: 1, SourceRevision: 1},
		SourceHash: snapshot.SourceHash(), Owner: owner, ActionVersion: 4, Context: ctx}
	owner.lease = lease
	f.SetRecoveryContext(snapshot)
	f.SetRecoveryLease(lease)
	f.meta = &fetcher.FetcherMeta{Req: request, Opts: &base.Options{}, Res: &base.Resource{Range: true, Size: 1048576}}
	f.ctx, f.cancel = context.WithCancel(transfer.WithRecoveryLease(ctx, lease))
	file, err := os.CreateTemp(t.TempDir(), "target")
	if err != nil {
		t.Fatal(err)
	}
	f.file = file
	var releaseOnce sync.Once
	releaseB := func() { releaseOnce.Do(func() { close(owner.lateRelease) }) }
	defer func() { cancel(); releaseB(); f.wg.Wait(); _ = file.Close() }()
	if err = f.configureInitialRecoveryWork(2); err != nil {
		t.Fatal(err)
	}
	for id := 0; id < 2; id++ {
		conn := &connection{ID: id, Chunk: newChunk(int64(id*524288), int64((id+1)*524288-1)), ctx: f.ctx, permit: true}
		conn.recoveryPermit = transfer.IssueRecoveryPermit(lease, 0, f.recoveryWork(conn))
		if err := conn.recoveryPermit.Consume(); err != nil {
			t.Fatal(err)
		}
		f.connections = append(f.connections, conn)
	}
	f.wg.Add(2)
	f.liveStreams.Add(2)
	for _, conn := range f.connections {
		go f.runConnection(conn)
	}
	select {
	case <-owner.lateLeft:
	case <-ctx.Done():
		t.Fatal("B did not leave its original request scope before its report")
	}
	select {
	case <-resumedA:
	case <-ctx.Done():
		t.Fatal("A did not reopen the wave while B's old report was held")
	}
	if owner.reserves.Load() != 1 || owner.admissions.Load() != 1 {
		t.Fatal("original wave was not reserved/admitted exactly once")
	}
	releaseB()
	select {
	case <-owner.secondReserved:
		t.Fatal("late B charged a second deferral for the already reopened IO wave")
	case <-resumedB:
	case <-ctx.Done():
		t.Fatal("late reporter did not join its already funded next wave")
	}
	if owner.reserves.Load() != 1 || owner.admissions.Load() != 1 || initial.Load() != 4 {
		t.Fatalf("late report opened an extra wave: reserves=%d admits=%d requests=%d", owner.reserves.Load(), owner.admissions.Load(), initial.Load())
	}
}
