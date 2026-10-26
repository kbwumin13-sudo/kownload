package netdisk

import (
	"context"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

type folderTransferProvider struct {
	fixtureProvider
	budget chan time.Duration
}

func (*folderTransferProvider) List(context.Context, *cloud.APIClient, *cloud.Session, model.Source, string) (*model.Listing, error) {
	return &model.Listing{Files: []model.File{{ID: "A", Name: "a", Size: 1}}}, nil
}
func (*folderTransferProvider) Resolve(context.Context, *cloud.APIClient, *cloud.Session, model.Source, model.File) (*cloud.Download, error) {
	return nil, cloud.TransferRequired()
}
func (p *folderTransferProvider) Transfer(ctx context.Context, _ *cloud.APIClient, _ *cloud.Session, _ model.Source, _ []model.File, _ string) (*model.TransferReceipt, error) {
	deadline, _ := ctx.Deadline()
	p.budget <- time.Until(deadline)
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestDirectoryTransferRetainsBatchBudgetAndCanBeCanceled(t *testing.T) {
	p := &folderTransferProvider{budget: make(chan time.Duration, 1)}
	s := cloud.NewService(p)
	cap, err := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/share", InstanceID: "browser", ContextID: "tab", AccountID: "account"})
	if err != nil {
		t.Fatal(err)
	}
	ctl := controller.NewController()
	prepared, err := s.Prepare(context.Background(), ctl, &base.Request{URL: cap.Source.URL}, cap.Source, cap.SessionID, []model.File{{ID: "dir", Name: "Folder", IsDir: true}}, &base.Options{Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	f := (&FetcherManager{Service: s}).Build().(*Fetcher)
	f.Setup(ctl)
	t.Cleanup(func() { _ = f.Close() })
	done := make(chan error, 1)
	go func() { done <- f.Resolve(prepared.Reqs[0].Req, prepared.Reqs[0].Opts) }()
	select {
	case budget := <-p.budget:
		if budget < 4*time.Minute || budget > 5*time.Minute {
			t.Fatalf("whole-directory transfer got single-file budget %v", budget)
		}
	case <-time.After(time.Second):
		t.Fatal("transfer did not start")
	}
	f.CancelResolve()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("canceled transfer succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("directory transfer ignored pause cancellation")
	}
}
