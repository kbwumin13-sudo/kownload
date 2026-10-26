package netdisk

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

const deferredFailureMessage = "批次转存未确认完成，副本已保留，请在原账号网盘确认；不会自动重复转存"

// Only public identities are durable. In particular, neither download URLs nor
// credentials from directory listing responses belong in this manifest.
type deferredBatch struct {
	Source    model.Source          `json:"source"`
	AccountID string                `json:"accountId"`
	Files     []model.File          `json:"files"`
	Remaining []string              `json:"remaining"`
	Copies    map[string]model.File `json:"copies,omitempty"`
	State     string                `json:"state"`
}

func (s *Service) prepareDeferred(id string, src model.Source, ss *Session, files []model.File) error {
	b := &deferredBatch{Source: src, AccountID: ss.Capture.AccountID, Files: publicFiles(files), State: "planned"}
	for _, f := range b.Files {
		b.Remaining = append(b.Remaining, f.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches[id] = b
	if err := s.persistLocked(); err != nil {
		delete(s.batches, id)
		return err
	}
	return nil
}

// Called with Service.mu held. The recipe must still name the original batch
// member, even when the actual download comes from an owned copy.
func (s *Service) deferredMember(e *model.ReqExtra) (*deferredBatch, model.File, error) {
	b := s.batches[e.BatchID]
	if b == nil || e.Provider != b.Source.Provider || e.Source != b.Source || e.AccountID != b.AccountID {
		return nil, model.File{}, Fail("invalid_batch", "目录批次记录缺失或身份不匹配，请保留任务并检查批次记录")
	}
	for _, f := range b.Files {
		if f.ID == e.File.ID && !e.File.IsDir && f.Name == e.File.Name && f.Path == e.File.Path &&
			(f.Size == 0 || f.Size == e.File.Size) && (f.Hash == "" || strings.EqualFold(f.Hash, e.File.Hash)) {
			return b, f, nil
		}
	}
	return nil, model.File{}, Fail("invalid_batch", "任务文件不属于此目录批次")
}

func (s *Service) deferredLock(id string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.batchLocks[id]
	if l == nil {
		l = make(chan struct{}, 1)
		s.batchLocks[id] = l
	}
	return l
}

func (s *Service) resolveDeferred(ctx context.Context, ctl *controller.Controller, req *base.Request, e *model.ReqExtra, p Provider) (*Download, error) {
	// Only transfer creation is serialized. Normal link resolution can use all
	// granted downloader slots, including while another member needs a copy.
	s.mu.Lock()
	b, original, err := s.deferredMember(e)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	state := b.State
	copyFile, copied := b.Copies[original.ID]
	account := b.AccountID
	s.mu.Unlock()
	if state == "transfer_failed" {
		return nil, Fail("transfer_failed", deferredFailureMessage)
	}
	// Shared originals can be reauthorized, but this batch can create or read
	// copies only under the account captured when its manifest was created.
	ss, err := s.session(e.Provider, e.SessionID, account)
	if err != nil && state == "planned" {
		// A fresh permission to the same shared originals survives cookie/UID
		// rotation. It never authorizes copying into another account.
		ss = s.sourceSession(e)
		if ss != nil {
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	defer s.rememberSessionCache(ss, e.Source)
	client := NewAPIClient(ctl, req, p.Info(), ss)
	if copied {
		return resolveDeferredCopy(ctx, client, ss, p, e.Source, original, copyFile)
	}
	if state == "planned" {
		d, resolveErr := p.Resolve(ctx, client, ss, e.Source, original)
		var typed *Error
		if !errors.As(resolveErr, &typed) || typed.Code != "transfer_required" {
			return d, resolveErr
		}
	}
	if ss.Capture.AccountID != account {
		return nil, LoginRequired(e.Provider)
	}
	copyFile, err = s.transferDeferred(ctx, client, ss, e, p)
	if err != nil {
		return nil, err
	}
	return resolveDeferredCopy(ctx, client, ss, p, e.Source, original, copyFile)
}

func (s *Service) transferDeferred(ctx context.Context, client *APIClient, ss *Session, e *model.ReqExtra, p Provider) (model.File, error) {
	l := s.deferredLock(e.BatchID)
	select {
	case l <- struct{}{}:
		defer func() { <-l }()
	case <-ctx.Done():
		return model.File{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return model.File{}, err
	}
	s.mu.Lock()
	b, original, err := s.deferredMember(e)
	if err != nil {
		s.mu.Unlock()
		return model.File{}, err
	}
	if b.State == "downloading" {
		copyFile, copied := b.Copies[original.ID]
		s.mu.Unlock()
		if !copied {
			return model.File{}, Fail("invalid_transfer", deferredFailureMessage)
		}
		return copyFile, nil
	}
	if b.State != "planned" {
		s.mu.Unlock()
		return model.File{}, Fail("transfer_failed", deferredFailureMessage)
	}
	// A long API cooldown is handed back to the download queue only here,
	// before the intent and before any side effect. Once the transfer starts its
	// calls wait instead, so a copy is never split from work already done.
	if client.budget != nil {
		if until := client.budget.APICooldown(client.source); !until.IsZero() && transfer.ShouldDefer(ctx, time.Until(until)) {
			s.mu.Unlock()
			return model.File{}, &Error{Code: "rate_limited", Message: "网盘接口请求过于频繁，请稍后重试",
				cause: &transfer.DeferredError{Until: until}}
		}
	}
	ctx = transfer.WithoutDeferral(ctx)
	// Write intent BEFORE any side effect. A crash, ambiguous provider error, or
	// failed final save must never turn this back into permission to copy again.
	b.State = "transferring"
	s.transfers[e.BatchID] = &model.TransferReceipt{BatchID: e.BatchID, Provider: e.Provider, AccountID: b.AccountID,
		Source: b.Source, Remaining: clone(b.Remaining), State: "transferring", Message: deferredFailureMessage}
	files := clone(b.Files)
	err = s.persistLocked()
	if err != nil {
		b.State = "transfer_failed"
		s.transfers[e.BatchID].State = "transfer_failed"
		s.mu.Unlock()
		return model.File{}, err
	}
	s.mu.Unlock()
	r, transferErr := p.Transfer(ctx, client, ss, e.Source, files, e.BatchID)
	if r == nil {
		r = &model.TransferReceipt{}
	} else {
		r = clone(r)
	}
	r.BatchID, r.Provider, r.AccountID, r.Source = e.BatchID, e.Provider, e.AccountID, e.Source
	r.Files = publicFiles(r.Files)
	r.State, r.Message = "transfer_failed", deferredFailureMessage
	copies, mappingErr := deferredCopies(files, r)
	if transferErr == nil {
		transferErr = mappingErr
	}
	s.mu.Lock()
	// Complete may have run for direct members while Transfer was in flight.
	b = s.batches[e.BatchID]
	r.Remaining = clone(b.Remaining)
	b.State = "transfer_failed"
	if transferErr == nil {
		b.Copies, b.State = copies, "downloading"
		r.State, r.Message = "downloading", ""
	}
	s.transfers[e.BatchID] = r
	saveErr := s.persistLocked()
	if saveErr != nil {
		b.State = "transfer_failed"
		r.State, r.Message = "transfer_failed", deferredFailureMessage
	}
	s.mu.Unlock()
	if saveErr != nil {
		return model.File{}, saveErr
	}
	if transferErr != nil {
		return model.File{}, transferErr
	}
	return copies[original.ID], nil
}

// Provider order and names do not prove identity. Every copy must explicitly
// map to one original, have a distinct created ID, and retain its content.
func deferredCopies(files []model.File, r *model.TransferReceipt) (map[string]model.File, error) {
	bad := Fail("invalid_transfer", deferredFailureMessage)
	if r.FolderID == "" || len(r.Files) != len(files) || len(r.CreatedIDs) != len(files) {
		return nil, bad
	}
	originals := map[string]model.File{}
	for _, f := range files {
		originals[f.ID] = f
	}
	created := map[string]bool{}
	for _, id := range r.CreatedIDs {
		if id == "" || id == r.FolderID || created[id] {
			return nil, bad
		}
		if _, isOriginal := originals[id]; isOriginal {
			return nil, bad
		}
		created[id] = true
	}
	copies := map[string]model.File{}
	seen := map[string]bool{}
	for _, f := range r.Files {
		id := f.Meta["source_id"]
		original, ok := originals[id]
		if !ok || !created[f.ID] || seen[f.ID] || copies[id].ID != "" || f.IsDir || f.Meta["parent_id"] != r.FolderID ||
			validateFile(f) != nil || f.Name != original.Name || f.Path != original.Path || (original.Size > 0 && f.Size != original.Size) ||
			(original.Hash != "" && !strings.EqualFold(f.Hash, original.Hash)) {
			return nil, bad
		}
		seen[f.ID] = true
		copies[id] = f
	}
	return copies, nil
}

func resolveDeferredCopy(ctx context.Context, client *APIClient, ss *Session, p Provider, src model.Source, original, copyFile model.File) (*Download, error) {
	src.ShareID, src.ParentID = "", copyFile.Meta["parent_id"]
	d, err := p.Resolve(ctx, client, ss, src, copyFile)
	if err != nil {
		return nil, err
	}
	if d == nil || d.File.ID != copyFile.ID || (d.File.Size > 0 && original.Size > 0 && d.File.Size != original.Size) ||
		(d.File.Hash != "" && original.Hash != "" && !strings.EqualFold(d.File.Hash, original.Hash)) {
		return nil, Fail("invalid_file", "网盘返回的转存副本身份或内容不匹配")
	}
	// Validate the provider's copy identity before restoring the public recipe
	// identity expected by the fetcher. Keep the returned size/hash for its checks.
	d = clone(d)
	d.File.ID, d.File.Name, d.File.Path = original.ID, original.Name, original.Path
	return d, nil
}

func (s *Service) completeDeferred(ctx context.Context, ctl *controller.Controller, req *base.Request, e *model.ReqExtra) {
	s.mu.Lock()
	b, _, err := s.deferredMember(e)
	if err != nil {
		s.mu.Unlock()
		return
	}
	left := []string{}
	for _, id := range b.Remaining {
		if id != e.File.ID {
			left = append(left, id)
		}
	}
	b.Remaining = left
	r := s.transfers[e.BatchID]
	if r != nil {
		r.Remaining = clone(left)
	}
	err = s.persistLocked()
	ready := err == nil && len(left) == 0 && b.State == "downloading" && r != nil && r.State == "downloading"
	s.mu.Unlock()
	if ready {
		_ = s.Cleanup(ctx, ctl, req, e.BatchID)
	}
}
