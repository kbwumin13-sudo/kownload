package download

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
)

// Capture both the digest and the actual values once. Protocol callers must
// execute through this owned snapshot rather than reread global configuration.
func (d *Downloader) captureParentRecoveryContext(task *Task) (*transfer.ProtocolExecutionContext, bool, error) {
	ctl := d.protocolController(task.Protocol)
	handler, proxyID, supported := ctl.GetRecoveryProxySnapshot(task.Meta.Req.Proxy)
	if !supported { return nil, false, nil }
	var config json.RawMessage
	ctl.GetConfig(&config)
	if len(config) == 0 { config = json.RawMessage(`{}`) }
	snapshot, err := transfer.NewProtocolExecutionContext(task.Protocol,task.Meta.Req,config,proxyID,handler)
	return snapshot, err == nil, err
}

// This observes a same-run result coordinate only. It creates no IO permit.
// A confirmed source handoff may have advanced S since watch was registered.
func (o *taskRecoveryOwner) parentResultLease(lease *transfer.RecoveryLease) (*transfer.RecoveryLease,error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if lease == nil || lease.Token.TaskID != o.token.TaskID ||
		lease.Token.Episode != o.token.Episode || lease.Token.Generation != o.token.Generation ||
		o.runContext == nil || o.runContext.Err() != nil {
		return nil,transfer.ErrRecoveryStale
	}
	return o.sharedLeaseLocked(),nil
}

type parentRecoveryRun struct {
	token transfer.RecoveryToken
	epoch uint64
	active map[string]int
	protocol string
	children map[string]string
	committing bool
}

func parentRecoveryWork(protocol, work string) bool {
	switch protocol {
	case "netdisk": return work == "netdisk/api" || work == "netdisk/http"
	case "media": return work == "media/inspect" || work == "media/download"
	}
	return false
}

func (o *taskRecoveryOwner) CurrentRecoveryRound(ctx context.Context, token transfer.RecoveryToken) (uint64, error) {
	o.mu.Lock(); defer o.mu.Unlock()
	if err := o.sharedCurrentLocked(ctx,token); err != nil { return 0, err }
	if _, err := o.observeSharedActionLocked(ctx,token,o.record); err != nil { return 0,err }
	return o.record.NextAction,nil
}

func (o *taskRecoveryOwner) EnterRecoveryWork(ctx context.Context, token transfer.RecoveryToken, work string) (*transfer.RecoveryWorkScope, error) {
	o.mu.Lock()
	if err := o.sharedCurrentLocked(ctx,token); err != nil { o.mu.Unlock(); return nil,err }
	parent := o.parentRun
	if parent == nil || parent.committing || parent.token != token || !parentRecoveryWork(parent.protocol,work) { o.mu.Unlock(); return nil,transfer.ErrRecoveryWork }
	if err := checkSharedRecoveryUntil(o.record, time.Now()); err != nil { o.mu.Unlock(); return nil,err }
	if _, err := o.observeSharedActionLocked(ctx,token,o.record); err != nil { o.mu.Unlock(); return nil,err }
	parent.active[work]++
	lease := o.sharedLeaseLocked()
	child, cancel := context.WithCancel(transfer.WithRecoveryLease(ctx,lease))
	stop := context.AfterFunc(o.runContext,cancel)
	o.mu.Unlock()
	return transfer.IssueRecoveryWorkScope(child,func(check context.Context) error {
		if err := check.Err(); err != nil { return err }
		return o.CheckRecovery(token)
	},func(){
		stop();cancel()
		o.mu.Lock();defer o.mu.Unlock()
		if o.parentRun == parent && parent.token == token && parent.active[work] > 0 { parent.active[work]-- }
	}),nil
}

// The source seal and all its methods are inspected outside owner/DB locks.
// Only the immutable owned snapshot enters full-CURRENT transactional CAS.
func (o *taskRecoveryOwner) CommitRecoverySource(ctx context.Context, token transfer.RecoveryToken, actionID uint64, seal transfer.RecoverySourceSeal, checkpoint json.RawMessage) (*transfer.RecoveryLease,error) {
	if seal == nil || ctx == nil { return nil,transfer.ErrRecoveryWork }
	snapshot, err := seal.Snapshot(ctx)
	if err != nil || snapshot.Token != token || snapshot.Epoch == 0 { return nil,transfer.ErrRecoveryStale }
	checkpoint = append(json.RawMessage(nil),checkpoint...)
	if err := seal.Check(ctx); err != nil { return nil,err }
	if err := o.prepareSharedAction(ctx,token); err != nil { return nil,err }
	o.mu.Lock()
	if err := o.sharedCurrentLocked(ctx,token); err != nil { o.mu.Unlock();return nil,err }
	if snapshot.SourceHash != o.record.SourceHash { o.mu.Unlock();return nil,transfer.ErrRecoveryStale }
	if o.parentRun != nil { for _, n := range o.parentRun.active { if n != 0 { o.mu.Unlock();return nil,transfer.ErrRecoveryWork } } }
	expected, err := o.observeSharedActionLocked(ctx,token,o.record)
	if err != nil { o.mu.Unlock();return nil,err }
	if actionID == 0 || actionID != o.record.NextAction || !sameRecoveryMembers(snapshot.Members,o.record.Actions[actionID-1].Members) || o.sharedActionPermit == nil || o.sharedActionPermit.ActionID != actionID || o.sharedActionPermit.Kind != transfer.RecoveryActionRefresh {
		o.mu.Unlock();return nil,transfer.ErrRecoveryWork
	}
	if o.parentRun != nil { o.parentRun.committing = true }
	if o.parentContext == nil {
		checkpoint,err = o.ownedHTTPRefreshCheckpoint(ctx,expected,checkpoint)
		if err != nil { o.sharedUnknownLocked();o.mu.Unlock();return nil,err }
	}
	record, err := commitSharedRecoveryRefresh(o.store,o.sharedDataset,expected,actionID,checkpoint)
	if err != nil { o.sharedUnknownLocked();o.mu.Unlock();return nil,err }
	// Keep the sealed old token just long enough to verify its physical epoch
	// after ACK. Enter is closed above. No DB callback calls into the protocol.
	o.mu.Unlock()
	sealErr := seal.Check(ctx)
	o.mu.Lock()
	if sealErr != nil || ctx.Err() != nil || o.sharedCurrentLocked(ctx,token) != nil || !reflect.DeepEqual(o.record,expected.Record) {
		o.record = record
		o.sharedUnknownLocked()
		o.mu.Unlock()
		return nil,transfer.ErrRecoveryStale
	}
	o.record = record
	o.sharedInitialMembers = append([]string{},snapshot.Members...)
	newToken := token;newToken.SourceRevision = record.SourceRevision
	o.token = newToken
	o.sharedActionPermit = nil
	o.sharedContinuation.Store(nil)
	o.coverageInstallAuthorization.Store(nil)
	o.coverageSettlementAuthorization.Store(nil)
	o.coverageObservationHeld.Store(true)
	if o.parentRun != nil { o.parentRun = &parentRecoveryRun{token:newToken,epoch:o.parentRun.epoch+1,active:map[string]int{},children:map[string]string{},protocol:o.parentRun.protocol,committing:true} }
	o.mu.Unlock()
	// The protocol seal still describes OLD source quiescence; do not ask its
	// old lease to validate the new token. Epoch/context/domain are rechecked
	// before the caller may replace the old child or apply a late response.
	if ctx.Err() != nil { o.invalidateToken(newToken);return nil,ctx.Err() }
	o.mu.Lock();defer o.mu.Unlock()
	if _, err := o.observeSharedActionLocked(ctx,newToken,record); err != nil { return nil,err }
	if !reflect.DeepEqual(o.record,record) { o.sharedUnknownLocked();return nil,transfer.ErrRecoveryStale }
	if o.parentRun != nil { o.parentRun.committing = false }
	return o.sharedLeaseLocked(),nil
}

func (o *taskRecoveryOwner) BindRecoveryChild(ctx context.Context, token transfer.RecoveryToken, work string, snapshot *transfer.HTTPExecutionContext) (*transfer.RecoveryLease,error) {
	if snapshot == nil { return nil,transfer.ErrRecoveryWork }
	o.mu.Lock();defer o.mu.Unlock()
	if err := o.sharedCurrentLocked(ctx,token); err != nil { return nil,err }
	if o.parentRun == nil || o.parentRun.committing || o.parentRun.token != token || !parentRecoveryWork(o.parentRun.protocol,work) || o.parentContext == nil || snapshot.ProxyID() != o.parentContext.ProxyID() { return nil,transfer.ErrRecoveryWork }
	if _, err := o.observeSharedActionLocked(ctx,token,o.record); err != nil { return nil,err }
	lease := o.sharedLeaseLocked()
	if o.parentRun.children == nil { o.parentRun.children = map[string]string{} }
	o.parentRun.children[snapshot.SourceHash()] = work
	lease.SourceHash = snapshot.SourceHash()
	lease.Owner = &parentHTTPRecoveryOwner{parent:o,token:token,work:work,source:snapshot.SourceHash()}
	return lease,nil
}

type parentHTTPRecoveryOwner struct { parent *taskRecoveryOwner;token transfer.RecoveryToken;work,source string }
func (p *parentHTTPRecoveryOwner) CheckRecovery(t transfer.RecoveryToken) error { if t!=p.token{return transfer.ErrRecoveryStale};return p.parent.CheckRecovery(t) }
func (p *parentHTTPRecoveryOwner) RetryRecovery(ctx context.Context,t transfer.RecoveryToken,seen uint64,work string,members []string)(*transfer.RecoveryPermit,error){return p.parent.RetryRecovery(ctx,t,seen,work,members)}
func (p *parentHTTPRecoveryOwner) FinishRecovery(ctx context.Context,t transfer.RecoveryToken,round uint64,work string)error{return p.parent.FinishRecovery(ctx,t,round,work)}
func (p *parentHTTPRecoveryOwner) ReserveRecoveryAction(ctx context.Context,t transfer.RecoveryToken,seen uint64,kind transfer.RecoveryActionKind,work string,members []string,until time.Time)(*transfer.RecoveryActionPermit,error){return p.parent.ReserveRecoveryAction(ctx,t,seen,kind,work,members,until)}
func (p *parentHTTPRecoveryOwner) ExtendRecoveryDeferral(ctx context.Context,t transfer.RecoveryToken,id uint64,work string,members []string,until time.Time)(time.Time,error){return p.parent.ExtendRecoveryDeferral(ctx,t,id,work,members,until)}
func (p *parentHTTPRecoveryOwner) AdmitRecoveryWave(ctx context.Context,seal *httpProtocol.RecoveryWaveSeal,id uint64)(map[string]*transfer.RecoveryPermit,error){return p.parent.AdmitRecoveryWave(ctx,seal,id)}

// Preserve CURRENT HTTP Store coordinates other than the explicit source
// selection patch. The proposal may have been captured before Core drained a
// pending startup/sampler Store. The kernel still CASes this exact checkpoint;
// this is not a retry or permission to ignore a later real Store.
func (o *taskRecoveryOwner) ownedHTTPRefreshCheckpoint(ctx context.Context, expected recoverySharedExpected, proposal json.RawMessage) (json.RawMessage,error) {
	var patch map[string]json.RawMessage
	if json.Unmarshal(proposal,&patch) != nil || patch == nil || len(patch["RedirectURL"]) == 0 { return nil,transfer.ErrRecoveryHeld }
	o.mu.Unlock()
	var owned json.RawMessage
	err := withDatasetReadView(o.sharedReadStore(),o.sharedDataset,func(reader datasetReader) error {
		_,record,required,e := readSharedRecovery(reader,o.taskID)
		if e != nil { return e }
		if !reflect.DeepEqual(record,expected.Record) || !reflect.DeepEqual(required,expected.Required) { return transfer.ErrRecoveryStale }
		var current json.RawMessage
		present,e := reader.Get(bucketSave,o.taskID,&current)
		if e != nil { return transfer.ErrRecoveryUnavailable }
		if present != expected.CheckpointPresent { return transfer.ErrRecoveryStale }
		if present {
			hash,e := checkpointDigest(current)
			if e != nil || hash != expected.CheckpointHash { return transfer.ErrRecoveryStale }
		}
		base := map[string]json.RawMessage{}
		if present {
			if json.Unmarshal(current,&base) != nil || base == nil { return transfer.ErrRecoveryHeld }
		} else {
			// A genuinely absent initial Store may be installed, but no read
			// error is absence and a present Store is never reconstructed.
			for key,value := range patch { base[key] = append(json.RawMessage(nil),value...) }
		}
		base["RedirectURL"] = append(json.RawMessage(nil),patch["RedirectURL"]...)
		if value,ok := patch["PublicSource"];ok { base["PublicSource"] = append(json.RawMessage(nil),value...) }
		owned,e = json.Marshal(base)
		return e
	})
	o.mu.Lock()
	if err != nil { return nil,err }
	if o.sharedCurrentLocked(ctx,expected.Token) != nil || !reflect.DeepEqual(o.record,expected.Record) { return nil,transfer.ErrRecoveryStale }
	return owned,nil
}
