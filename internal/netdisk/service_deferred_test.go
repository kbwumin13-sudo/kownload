package netdisk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

type deferredStore struct {
	data   []byte
	saves  int
	failAt int
}

func (s *deferredStore) Load(v any) (bool, error) {
	if len(s.data) == 0 {
		return false, nil
	}
	return true, json.Unmarshal(s.data, v)
}
func (s *deferredStore) Save(v any) error {
	s.saves++
	if s.saves == s.failAt {
		return errors.New("state unavailable")
	}
	s.data, _ = json.Marshal(v)
	return nil
}
func (s *deferredStore) Delete() error { s.data = nil; return nil }

type deferredProvider struct {
	testProvider
	mu            sync.Mutex
	transfers     int
	cleanups      int
	resolveCalls  []string
	transferFiles []model.File
	require       string
	partial       bool
	ambiguous     bool
	badMapping    bool
	badDownload   bool
	cleanupErr    error
	started       chan struct{}
	release       chan struct{}
}

func (*deferredProvider) List(context.Context, *APIClient, *Session, model.Source, string) (*model.Listing, error) {
	return &model.Listing{Files: []model.File{
		{ID: "A", Name: "a.bin", Size: 3, Hash: "abc", Meta: map[string]string{"parent_id": "folder", "share_fid_token": "private-directory-token"}},
		{ID: "B", Name: "b.bin", Size: 4, Meta: map[string]string{"parent_id": "folder"}},
		{ID: "C", Name: "c.bin", Size: 5, Meta: map[string]string{"parent_id": "folder"}},
	}}, nil
}
func (p *deferredProvider) Resolve(_ context.Context, _ *APIClient, _ *Session, src model.Source, f model.File) (*Download, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resolveCalls = append(p.resolveCalls, f.ID)
	if src.ShareID != "" && (p.require == "all" || p.require == f.ID) {
		return nil, TransferRequired()
	}
	if src.ShareID == "" && p.badDownload {
		f.ID = "wrong-copy"
	}
	return &Download{URL: "https://cdn.quark.cn/" + f.ID + "?token=private-download-token", File: f, Header: map[string]string{"Authorization": "private-header"}}, nil
}
func (p *deferredProvider) Transfer(ctx context.Context, _ *APIClient, _ *Session, _ model.Source, fs []model.File, _ string) (*model.TransferReceipt, error) {
	p.mu.Lock()
	p.transfers++
	p.transferFiles = clone(fs)
	p.mu.Unlock()
	if p.started != nil {
		close(p.started)
	}
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.ambiguous {
		return nil, errors.New("uncertain remote result")
	}
	r := &model.TransferReceipt{FolderID: "temporary-folder"}
	for _, f := range fs {
		copyFile := mappedFile(f, "copy-"+f.ID, r.FolderID)
		r.Files = append(r.Files, copyFile)
		r.CreatedIDs = append(r.CreatedIDs, copyFile.ID)
		if p.partial {
			return r, errors.New("transfer stopped")
		}
	}
	if p.badMapping {
		r.Files[1].Meta["source_id"] = "A"
	}
	return r, nil
}
func (p *deferredProvider) Cleanup(_ context.Context, _ *APIClient, _ *Session, r *model.TransferReceipt) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(r.Remaining) > 0 {
		return errors.New("cleaned active batch")
	}
	if p.cleanupErr != nil {
		return p.cleanupErr
	}
	p.cleanups++
	return nil
}

func deferredPrepare(t *testing.T, p *deferredProvider, store *deferredStore) (*Service, *Prepared) {
	t.Helper()
	s := NewService(p)
	if store != nil {
		s.SetStateStore(store)
	}
	cap := testCapture(t, s)
	out, err := s.Prepare(context.Background(), controller.NewController(), &base.Request{URL: cap.Source.URL}, cap.Source, cap.SessionID, []model.File{{ID: "folder", Name: "Folder", IsDir: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.resolveCalls) != 0 || p.transfers != 0 || len(out.Reqs) != 3 {
		t.Fatal("Prepare did network link/transfer work or lost files")
	}
	return s, out
}

func deferredResolve(s *Service, out *Prepared, i int) (*Download, error) {
	item := out.Reqs[i]
	return s.Resolve(context.Background(), controller.NewController(), item.Req, item.Req.Extra.(*model.ReqExtra))
}
func deferredComplete(s *Service, out *Prepared, i int) {
	item := out.Reqs[i]
	s.Complete(context.Background(), controller.NewController(), item.Req, item.Req.Extra.(*model.ReqExtra))
}

func TestDeferredLaterTransferReusesWholeBatchAfterRestart(t *testing.T) {
	p := &deferredProvider{require: "C"}
	store := &deferredStore{}
	s, out := deferredPrepare(t, p, store)
	for i := 0; i < 2; i++ {
		d, err := deferredResolve(s, out, i)
		if err != nil || d.File.ID != []string{"A", "B"}[i] {
			t.Fatal(d, err)
		}
		deferredComplete(s, out, i)
	}
	if _, err := deferredResolve(s, out, 2); err != nil {
		t.Fatal(err)
	}
	if p.transfers != 1 || len(p.transferFiles) != 3 || p.cleanups != 0 {
		t.Fatal("late transfer lost batch or cleaned early")
	}
	if r := s.Transfers()[0]; len(r.Remaining) != 1 || r.Remaining[0] != "C" {
		t.Fatal("lost completed originals", r)
	}
	for _, content := range [][]byte{store.data, mustDeferredJSON(t, out)} {
		if strings.Contains(string(content), "private-") || strings.Contains(string(content), "cdn.quark.cn") || strings.Contains(string(content), "share_fid_token") {
			t.Fatal("persisted credentials or link")
		}
	}
	s = NewService(p)
	s.SetStateStore(store)
	testCapture(t, s)
	d, err := deferredResolve(s, out, 2)
	if err != nil || d.File.ID != "C" || d.File.Path != "Folder" || !strings.Contains(d.URL, "copy-C") {
		t.Fatal(d, err)
	}
	if p.transfers != 1 {
		t.Fatal("restart recreated copies")
	}
	deferredComplete(s, out, 2)
	deferredComplete(s, out, 2)
	if p.cleanups != 1 || s.Transfers()[0].State != "cleaned" {
		t.Fatal("batch did not clean exactly once")
	}
}

func mustDeferredJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDeferredConcurrentResolveTransfersOnceAndWaitsForAllSuccess(t *testing.T) {
	p := &deferredProvider{require: "all", started: make(chan struct{}), release: make(chan struct{})}
	s, out := deferredPrepare(t, p, &deferredStore{})
	var wg sync.WaitGroup
	results := make(chan error, len(out.Reqs))
	for i := range out.Reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := deferredResolve(s, out, i)
			if err == nil && d.File.ID != []string{"A", "B", "C"}[i] {
				err = fmt.Errorf("wrong recipe identity: %s", d.File.ID)
			}
			results <- err
		}(i)
	}
	<-p.started
	if err := s.Cleanup(context.Background(), controller.NewController(), out.Reqs[0].Req, out.BatchID); err == nil {
		t.Fatal("cleaned in-flight transfer")
	}
	close(p.release)
	wg.Wait()
	for range out.Reqs {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if p.transfers != 1 {
		t.Fatal("concurrent transfer duplicated", p.transfers)
	}
	deferredComplete(s, out, 0)
	deferredComplete(s, out, 1)
	if p.cleanups != 0 {
		t.Fatal("partial success cleaned")
	}
	deferredComplete(s, out, 2)
	if p.cleanups != 1 {
		t.Fatal("all successes did not clean")
	}
}

func TestDeferredFailedOrUncertainTransferNeverRecreatesCopies(t *testing.T) {
	for _, mode := range []string{"partial", "ambiguous", "mapping", "save-before", "save-after"} {
		t.Run(mode, func(t *testing.T) {
			p := &deferredProvider{require: "all", partial: mode == "partial", ambiguous: mode == "ambiguous", badMapping: mode == "mapping"}
			store := &deferredStore{}
			s, out := deferredPrepare(t, p, store)
			if mode == "save-before" {
				store.failAt = store.saves + 1
			}
			if mode == "save-after" {
				store.failAt = store.saves + 2
			}
			if _, err := deferredResolve(s, out, 0); err == nil {
				t.Fatal("failed transfer accepted")
			}
			calls := p.transfers
			if mode == "save-before" && calls != 0 {
				t.Fatal("copied before intent save succeeded")
			}
			if _, err := deferredResolve(s, out, 1); err == nil || p.transfers != calls {
				t.Fatal("retried uncertain transfer")
			}
			if r := s.Transfers()[0]; r.State != "transfer_failed" || (mode == "partial" && len(r.CreatedIDs) != 1) {
				t.Fatal("lost failed receipt", r)
			}
			// A failed save BEFORE the first mutation left a planned manifest on
			// disk; restart can safely attempt its first transfer. Every attempted
			// remote mutation must instead remain blocked across restart.
			if mode != "save-before" {
				s = NewService(p)
				s.SetStateStore(store)
				testCapture(t, s)
				if _, err := deferredResolve(s, out, 1); err == nil || p.transfers != calls {
					t.Fatal("restart repeated an uncertain transfer")
				}
			}
			for i := range out.Reqs {
				deferredComplete(s, out, i)
			}
			if err := s.Cleanup(context.Background(), controller.NewController(), out.Reqs[0].Req, out.BatchID); err == nil || p.cleanups != 0 {
				t.Fatal("failed transfer enabled cleanup")
			}
		})
	}
}

func TestDeferredWrongAccountMissingManifestAndChangedRecipeFailClosed(t *testing.T) {
	p := &deferredProvider{require: "all"}
	store := &deferredStore{}
	s, out := deferredPrepare(t, p, store)
	if _, err := deferredResolve(s, out, 0); err != nil {
		t.Fatal(err)
	}
	s = NewService(p)
	s.SetStateStore(store)
	_, err := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/share", InstanceID: "other", ContextID: "tab", AccountID: "other-account"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deferredResolve(s, out, 1); err == nil {
		t.Fatal("copy read under wrong account")
	}
	testCapture(t, s)
	changed := clone(out.Reqs[1].Req.Extra.(*model.ReqExtra))
	changed.File.ID = "unselected"
	if _, err := s.Resolve(context.Background(), controller.NewController(), out.Reqs[1].Req, changed); err == nil {
		t.Fatal("accepted foreign file")
	}
	s.Complete(context.Background(), controller.NewController(), out.Reqs[1].Req, changed)
	if len(s.Transfers()[0].Remaining) != 3 {
		t.Fatal("foreign completion modified batch")
	}
	s = NewService(p)
	testCapture(t, s)
	if _, err := deferredResolve(s, out, 1); err == nil || p.transfers != 1 {
		t.Fatal("missing manifest bypassed identity")
	}
}

func TestDeferredCopyDownloadIdentityValidatedBeforeRemapping(t *testing.T) {
	p := &deferredProvider{require: "all", badDownload: true}
	s, out := deferredPrepare(t, p, &deferredStore{})
	if _, err := deferredResolve(s, out, 0); err == nil {
		t.Fatal("wrong provider copy identity was remapped")
	}
	if p.cleanups != 0 || len(s.Transfers()[0].Remaining) != 3 {
		t.Fatal("failed download enabled cleanup")
	}
}

func TestDeferredOriginalReauthorizationDoesNotAuthorizeCopies(t *testing.T) {
	p := &deferredProvider{}
	store := &deferredStore{}
	s, out := deferredPrepare(t, p, store)
	s = NewService(p)
	s.SetStateStore(store)
	_, err := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/share", InstanceID: "new-browser", ContextID: "new-tab", AccountID: "rotated-cookie-identity"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = deferredResolve(s, out, 0); err != nil {
		t.Fatalf("reauthorized shared original failed: %v", err)
	}
	p.require = "B"
	if _, err = deferredResolve(s, out, 1); err == nil {
		t.Fatal("reauthorization permitted copying into another account")
	}
	if p.transfers != 0 {
		t.Fatal("wrong account created copies")
	}
}

func TestDeferredTransferMayFillUnknownSize(t *testing.T) {
	f := model.File{ID: "A", Name: "a", Path: "Folder"}
	copyFile := mappedFile(f, "copy-A", "temporary-folder")
	copyFile.Size = 5
	r := &model.TransferReceipt{FolderID: "temporary-folder", CreatedIDs: []string{"copy-A"}, Files: []model.File{copyFile}}
	if _, err := deferredCopies([]model.File{f}, r); err != nil {
		t.Fatalf("unknown size could not be filled: %v", err)
	}
	f.Size = 3
	if _, err := deferredCopies([]model.File{f}, r); err == nil {
		t.Fatal("known size mismatch was accepted")
	}
}

func TestDeferredPersonalPrepareResolvesNoLinks(t *testing.T) {
	p := &deferredProvider{}
	s := NewService(p)
	cap, err := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/list", InstanceID: "browser", ContextID: "tab", AccountID: "account-one"})
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range [][]model.File{
		{{ID: "folder", Name: "Folder", IsDir: true}},
		{{ID: "A", Name: "a.bin", Size: 1 << 40}},
	} {
		out, err := s.Prepare(context.Background(), controller.NewController(), &base.Request{URL: cap.Source.URL}, cap.Source, cap.SessionID, selected, nil)
		if err != nil || len(out.Reqs) == 0 || len(p.resolveCalls) != 0 {
			t.Fatal("personal Prepare resolved links", err)
		}
	}
}

func TestDeferredInterruptedTransferAndCanceledWaitDoNotCopyAgain(t *testing.T) {
	p := &deferredProvider{require: "all", started: make(chan struct{}), release: make(chan struct{})}
	store := &deferredStore{}
	s, out := deferredPrepare(t, p, store)
	done := make(chan error, 1)
	go func() { _, err := deferredResolve(s, out, 0); done <- err }()
	<-p.started
	// Model a process restart from the durable intent while the provider has
	// not returned a receipt. That state must not authorize a second transfer.
	restarted := NewService(p)
	restarted.SetStateStore(&deferredStore{data: append([]byte(nil), store.data...)})
	testCapture(t, restarted)
	if _, err := deferredResolve(restarted, out, 1); err == nil {
		t.Fatal("interrupted transfer recreated copies")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	item := out.Reqs[1]
	wait := make(chan error, 1)
	go func() {
		_, err := s.Resolve(ctx, controller.NewController(), item.Req, item.Req.Extra.(*model.ReqExtra))
		wait <- err
	}()
	select {
	case err := <-wait:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(p.release)
		<-done
		t.Fatal("canceled slot waited for the whole transfer")
	}
	close(p.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p.transfers != 1 {
		t.Fatal("copied twice")
	}
}

func TestDeferredCleanupSaveFailurePreservesCopiesForRetry(t *testing.T) {
	for _, stage := range []string{"completion", "cleanup-intent"} {
		t.Run(stage, func(t *testing.T) {
			p := &deferredProvider{require: "all"}
			store := &deferredStore{}
			s, out := deferredPrepare(t, p, store)
			for i := range out.Reqs {
				if _, err := deferredResolve(s, out, i); err != nil {
					t.Fatal(err)
				}
				if i == 2 {
					store.failAt = store.saves + 1
					if stage == "cleanup-intent" {
						store.failAt++
					}
				}
				deferredComplete(s, out, i)
			}
			if p.cleanups != 0 {
				t.Fatal("deleted before durable success/intent")
			}
			if err := s.Cleanup(context.Background(), controller.NewController(), out.Reqs[0].Req, out.BatchID); err != nil {
				t.Fatal(err)
			}
			if p.cleanups != 1 {
				t.Fatal("safe cleanup retry failed")
			}
		})
	}
}

func TestDeferredCleanupErrorNeverPersistsSignedURL(t *testing.T) {
	p := &deferredProvider{require: "all", cleanupErr: errors.New("https://api.quark.cn/cleanup?token=private-cleanup-token")}
	store := &deferredStore{}
	s, out := deferredPrepare(t, p, store)
	for i := range out.Reqs {
		if _, err := deferredResolve(s, out, i); err != nil {
			t.Fatal(err)
		}
		deferredComplete(s, out, i)
	}
	if s.Transfers()[0].State != "cleanup_failed" || strings.Contains(string(store.data), "private-") {
		t.Fatal("lost cleanup receipt or persisted provider credentials")
	}
}
