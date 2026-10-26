package netdisk

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

// cooldownTransferProvider copies with one API call and then polls with a
// second; a 429 for the account arrives between them. The API is a fake
// round tripper.
type cooldownTransferProvider struct {
	*deferredProvider
	budget *transfer.Budget
	key    transfer.SourceKey
	mu     sync.Mutex
	calls  []time.Time
}

func (p *cooldownTransferProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.calls = append(p.calls, time.Now())
	p.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
}

func (p *cooldownTransferProvider) Transfer(ctx context.Context, c *APIClient, ss *Session, src model.Source, fs []model.File, batch string) (*model.TransferReceipt, error) {
	c.Client.Transport = p
	for i, endpoint := range []string{"https://drive-pc.quark.cn/share/save", "https://drive-pc.quark.cn/task"} {
		res, err := c.Do(ctx, http.MethodPost, endpoint, nil, nil)
		if err != nil {
			return nil, err
		}
		res.Body.Close()
		if i == 0 {
			p.budget.APIRateLimited(p.key, 3*time.Second, true)
		}
	}
	return p.deferredProvider.Transfer(ctx, c, ss, src, fs, batch)
}

func (p *cooldownTransferProvider) callTimes() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.calls...)
}

// A long account API cooldown is handed back to the download queue only
// before a batch's transfer intent, leaving the batch planned and nothing
// sent. Once the transfer has begun its calls wait out a cooldown even with
// tasks queued, so the copy is made once and never replayed.
func TestTransferDefersOnlyBeforeItsIntent(t *testing.T) {
	inner := &deferredProvider{require: "all"}
	p := &cooldownTransferProvider{deferredProvider: inner}
	s := NewService(p)
	captured := testCapture(t, s)
	ctl := controller.NewController()
	ctl.Budget = transfer.NewBudget()
	p.budget, p.key = ctl.Budget, apiSource(p.Info(), &Session{Capture: model.Capture{AccountID: "account-one"}})
	out, err := s.Prepare(context.Background(), ctl, &base.Request{URL: captured.Source.URL}, captured.Source, captured.SessionID,
		[]model.File{{ID: "folder", Name: "Folder", IsDir: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := out.Reqs[0].Req.Extra.(*model.ReqExtra)
	batchState := func() (string, *model.TransferReceipt) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.batches[first.BatchID].State, s.transfers[first.BatchID]
	}

	until := ctl.Budget.APIRateLimited(p.key, time.Minute, true)
	_, err = s.Resolve(transfer.WithDeferral(context.Background(), nil, nil), ctl, out.Reqs[0].Req, first)
	if deferred, ok := transfer.AsDeferred(err); !ok || !deferred.Until.Equal(until) {
		t.Fatalf("a run before the transfer got %v, want a deferral until the cooldown end", err)
	}
	if state, receipt := batchState(); state != "planned" || receipt != nil || inner.transfers != 0 || len(p.callTimes()) != 0 {
		t.Fatalf("the deferral changed the batch: state=%s receipt=%v transfers=%d calls=%d", state, receipt, inner.transfers, len(p.callTimes()))
	}

	// The account's cooldown is over (a fresh budget stands for that).
	ctl.Budget.Close()
	ctl.Budget = transfer.NewBudget()
	defer ctl.Budget.Close()
	p.budget = ctl.Budget
	queued := transfer.WithDeferral(context.Background(), func() bool { return true }, nil)
	d, err := s.Resolve(queued, ctl, out.Reqs[0].Req, first)
	if err != nil {
		t.Fatalf("the transfer failed instead of waiting out a cooldown met midway: %v", err)
	}
	calls := p.callTimes()
	if len(calls) != 2 || calls[1].Sub(calls[0]) < 2900*time.Millisecond {
		t.Fatalf("the transfer's second call did not wait out the cooldown: %v", calls)
	}
	if state, _ := batchState(); state != "downloading" || inner.transfers != 1 || d.File.ID != first.File.ID {
		t.Fatalf("after the transfer: state=%s transfers=%d file=%s", state, inner.transfers, d.File.ID)
	}
	second := out.Reqs[1].Req.Extra.(*model.ReqExtra)
	if _, err := s.Resolve(queued, ctl, out.Reqs[1].Req, second); err != nil || inner.transfers != 1 {
		t.Fatalf("another member transferred again (transfers=%d) or failed: %v", inner.transfers, err)
	}
}
