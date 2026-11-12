package download

import (
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	nd "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"github.com/GopeedLab/gopeed/pkg/util"
)

// pathExpected captures a CURRENT, same-view value. Native coordinates do not
// pretend to be recovery execution tokens; their lifetime is checked by Core.
func pathExpected(reader datasetReader, task *Task, record batchOperationRecord) (PathFreezeExpected, error) {
	e := PathFreezeExpected{Record: record, Meta: task.Meta, Token: transfer.RecoveryToken{TaskID: task.ID}}
	v, ok := pathVariantForCaps(record.RequiredCapabilities)
	if !ok {
		return e, ErrPathFreezeHeld
	}
	if _, err := pathTaskFromRecord(reader, task, &record); err != nil {
		return e, err
	}
	if v.Native {
		e.SourceHash = mcpWriteHash(task.Meta.Req)
	} else {
		_, r, _, err := readSharedRecovery(reader, task.ID)
		if err != nil {
			return e, err
		}
		e.Token = transfer.RecoveryToken{TaskID: task.ID, Episode: r.Episode, Generation: r.Generation, SourceRevision: r.SourceRevision}
		e.SourceHash, e.ExecutionDefaultsHash = r.SourceHash, r.ExecutionDefaultsHash
	}
	var raw json.RawMessage
	present, err := reader.Get(bucketSave, task.ID, &raw)
	if err != nil {
		return e, ErrPathFreezeUnavailable
	}
	e.CheckpointPresent = present
	if present {
		e.CheckpointHash, err = checkpointDigest(raw)
	}
	return e, err
}

func (d *Downloader) readPathExpected(dataset, id string) (*Task, PathFreezeExpected, error) {
	var task *Task
	var e PathFreezeExpected
	err := withDatasetReadView(d.storage, dataset, func(reader datasetReader) error {
		task = new(Task)
		found, err := reader.Get(bucketTask, id, task)
		if err != nil {
			return err
		}
		if !found || task.CreationOperation == nil {
			return ErrPathFreezeHeld
		}
		r, err := loadOperationFrom(reader, task.CreationOperation.OperationID)
		if err != nil {
			return err
		}
		if r == nil || r.Path == nil {
			return ErrPathFreezeHeld
		}
		e, err = pathExpected(reader, task, *r)
		return err
	})
	return task, e, err
}

// Called with Task.lock released. All member locks precede the value capture;
// no protocol callback or live lock is acquired by the storage transaction.
func (d *Downloader) confirmTaskPath(ctx context.Context, task *Task, resolved *fetcher.FetcherMeta, generation uint64) error {
	if task.PathFreeze == nil {
		return nil
	}
	identity, err := d.DatasetIdentity()
	if err != nil {
		return err
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	var record *batchOperationRecord
	err = withDatasetReadView(d.storage, identity.DatasetID, func(reader datasetReader) error {
		var err error
		record, err = loadOperationFrom(reader, task.CreationOperation.OperationID)
		return err
	})
	if err != nil {
		return err
	}
	if record == nil || record.Path == nil {
		return ErrPathFreezeHeld
	}
	tasks := make([]*Task, len(record.Members))
	for i, m := range record.Members {
		tasks[i] = d.getTask(m.TaskID)
		if tasks[i] == nil {
			return ErrPathFreezeHeld
		}
	}
	for _, t := range tasks {
		t.lock.Lock()
	}
	defer func() {
		for i := len(tasks) - 1; i >= 0; i-- {
			tasks[i].lock.Unlock()
		}
	}()
	if ctx.Err() != nil || d.closed.Load() || d.getTask(task.ID) != task {
		return transfer.ErrRecoveryStale
	}
	task.statusLock.Lock()
	current := task.runGeneration == generation && task.getStatus() == base.DownloadStatusRunning
	task.statusLock.Unlock()
	if !current {
		return transfer.ErrRecoveryStale
	}
	expected := make([]PathFreezeExpected, len(tasks))
	stored := make([]*Task, len(tasks))
	err = withDatasetReadView(d.storage, identity.DatasetID, func(reader datasetReader) error {
		r, e := loadOperationFrom(reader, record.OperationID)
		if e != nil {
			return e
		}
		if r == nil || r.Path == nil {
			return ErrPathFreezeHeld
		}
		record = r
		for i, m := range r.Members {
			t := new(Task)
			found, e := reader.Get(bucketTask, m.TaskID, t)
			if e != nil {
				return e
			}
			if !found {
				return ErrPathFreezeHeld
			}
			stored[i] = t
			expected[i], e = pathExpected(reader, t, *r)
			if e != nil {
				return e
			}
			// The shared R is retained only once. The kernel's tree CAS decodes it once.
			if i > 0 {
				expected[i].Record = batchOperationRecord{}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if record.Path.TreeDecision != nil {
		for i, t := range tasks {
			t.dataLock.Lock()
			t.Meta, t.PathFreeze = stored[i].Meta, stored[i].PathFreeze
			t.dataLock.Unlock()
		}
		return d.applyFrozenFetcherMeta(task)
	}
	input := pathpolicy.Input{Path: record.Path.Snapshot.InputRoot, Origin: record.Path.Request.PathIntent.Origin}
	if u, e := url.Parse(stored[0].Meta.Req.URL); e == nil {
		input.Domain = u.Hostname()
	}
	if len(tasks) == 1 && resolved != nil && resolved.Res != nil {
		input.Name = resolved.Opts.Name
		if input.Name == "" && len(resolved.Res.Files) == 1 {
			input.Name = resolved.Res.Files[0].Name
		}
	}
	decision, err := pathpolicy.Evaluate(input, record.Path.Snapshot.Config)
	if err != nil {
		return err
	}
	prepared := make([]PathFreezePrepared, len(tasks))
	for i, t := range tasks {
		var meta *fetcher.FetcherMeta
		if t == task {
			meta, err = cloneOperationJSON(resolved)
		} else if t.Protocol == "netdisk" {
			meta, err = cloneOperationJSON(stored[i].Meta)
			if err == nil {
				var extra nd.ReqExtra
				raw, e := json.Marshal(meta.Req.Extra)
				if e != nil {
					return e
				}
				if e = json.Unmarshal(raw, &extra); e != nil {
					return e
				}
				meta.Res = &base.Resource{Files: []*base.FileInfo{{Name: extra.File.Name, Path: extra.File.Path, Size: extra.File.Size}}}
			}
		} else {
			return ErrPathFreezeHeld
		}
		if err != nil {
			return err
		}
		if meta == nil || meta.Res == nil || meta.Opts == nil {
			return ErrPathFreezeHeld
		}
		// Runtime parsers may materialize typed defaults; R's caller payload stays exact.
		meta.Req = stored[i].Meta.Req
		meta.Opts.Extra = stored[i].Meta.Opts.Extra
		meta.Opts.SelectFiles = stored[i].Meta.Opts.SelectFiles
		meta.Opts.Path = decision.Root
		if t.Protocol == "http" && len(meta.Res.Files) == 1 && meta.Res.Name == "" {
			name := meta.Opts.Name
			if name == "" {
				name = meta.Res.Files[0].Name
			}
			preparation, e := pathpolicy.PrepareSingleFile(pathpolicy.Input{Path: decision.Root, Origin: pathpolicy.APIExplicit, Name: name}, pathpolicy.Config{})
			if e != nil {
				return e
			}
			name = preparation.Name
			if t.fetcherManager.AutoRename() {
				name, e = util.CheckDuplicateAndRename(strings.TrimRight(decision.Root, "/") + "/" + name)
				if e != nil {
					return e
				}
			}
			meta.Opts.Name = name
		}
		layout, e := pathResourceLayout(meta)
		if e != nil {
			return e
		}
		work := ""
		if t.Protocol == "media" {
			provider, ok := t.fetcher.(interface {
				MediaOutputLayout() (pathpolicy.LayoutV1, string, error)
			})
			if !ok {
				return ErrPathFreezeHeld
			}
			layout, work, e = provider.MediaOutputLayout()
			if e != nil {
				return e
			}
			layout.ParentRoot = decision.Root
		}
		hash, e := pathpolicy.LayoutFingerprint(layout)
		if e != nil {
			return e
		}
		prepared[i] = PathFreezePrepared{ResolvedMeta: meta, Decision: decision, Layout: layout, LayoutHash: hash, StagingName: "." + t.ID + ".path-part", WorkDirectory: work}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	owned, _, err := ConfirmPathFreezeTree(d.storage.(AtomicStorage), identity.DatasetID, expected, prepared)
	if err != nil {
		return err
	}
	if ctx.Err() != nil || d.closed.Load() {
		return transfer.ErrRecoveryStale
	}
	for i, t := range tasks {
		t.dataLock.Lock()
		t.Meta, t.PathFreeze = owned[i].Meta, owned[i].PathFreeze
		t.IsCreated = true
		t.dataLock.Unlock()
	}
	return d.applyFrozenFetcherMeta(task)
}

// Fetcher-specific Extra remains its parsed runtime value; the persisted Req
// and opaque options are never rewritten by a protocol's metadata parsing.
func (d *Downloader) applyFrozenFetcherMeta(task *Task) error {
	if task.PathFreeze == nil || task.PathFreeze.Phase != PathFreezeFrozen {
		return ErrPathFreezeHeld
	}
	runtime := task.fetcher.Meta()
	if runtime == nil || runtime.Opts == nil {
		return ErrPathFreezeHeld
	}
	runtime.Opts.Path = task.Meta.Opts.Path
	runtime.Opts.Name = task.Meta.Opts.Name
	resource, err := cloneOperationJSON(task.Meta.Res)
	if err != nil { return err }
	runtime.Res = resource
	return nil
}

// A row-local guard prevents a late sampler from erasing F/output ACKs. It
// deliberately does not reload the whole operation for every progress tick.
func guardPathSnapshot(current, snapshot *Task) error {
	if current == nil || snapshot == nil {
		return ErrPathFreezeHeld
	}
	if current.PathFreeze == nil && snapshot.PathFreeze == nil {
		return nil
	}
	if !reflect.DeepEqual(current.PathFreeze, snapshot.PathFreeze) || !reflect.DeepEqual(current.CreationOperation, snapshot.CreationOperation) || !pathReqEqual(current.Meta.Req, snapshot.Meta.Req) || !reflect.DeepEqual(current.Meta.Opts, snapshot.Meta.Opts) {
		return ErrPathFreezeConflict
	}
	if current.PathFreeze == nil {
		return ErrPathFreezeHeld
	}
	if current.PathFreeze.Phase == PathFreezeFrozen {
		layout, err := pathResolvedLayout(snapshot.Protocol, snapshot.Meta, *current.PathFreeze.Layout, current.PathFreeze.WorkDirectory)
		if err != nil {
			return err
		}
		hash, err := pathpolicy.LayoutFingerprint(layout)
		if err != nil || hash != current.PathFreeze.LayoutHash {
			return ErrPathFreezeConflict
		}
	}
	return nil
}
