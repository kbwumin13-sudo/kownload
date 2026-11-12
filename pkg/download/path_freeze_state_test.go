package download

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// Storage-only fixtures: no file, HTTP request, policy grant or execution token
// is manufactured from these synthetic filesystem/source values.
func pathK12Intent(f *mcpK12Fixture, protocol, op string) (HTTPPathCreateRequest, PathPolicySnapshot) {
	cap := HTTPPathRequiredCapability
	for _, v := range pathVariants {
		if v.Protocol == protocol {
			cap = v.Capability
		}
	}
	req := &base.Request{URL: "https://fixture.invalid/path", Protocol: protocol, Extra: map[string]any{"exact": json.Number("9007199254740993")}}
	if protocol == "bt" {
		req.URL = "magnet:?xt=urn:btih:0123456789012345678901234567890123456789"
	}
	header := HTTPPathCreateRequest{Version: 1, RequiredCapabilities: []string{cap}, OperationID: op, Request: &base.CreateTaskBatchItem{Req: req, Opts: &base.Options{Path: "/synthetic-input", Name: "input.bin", Extra: map[string]any{"keep": json.Number("-9007199254740993.125")}}}, PathIntent: PathIntent{Version: 1, Path: "/synthetic-input", Origin: pathpolicy.Default}}
	snapshot := PathPolicySnapshot{Version: 1, EffectiveDefaultRoot: "/synthetic-input", InputRoot: "/synthetic-input", Config: pathpolicy.Config{Rules: []pathpolicy.Rule{}}}
	return header, snapshot
}
func pathK12Candidate(t *testing.T, r batchOperationRecord, i int) (*Task, recoveryRecord) {
	t.Helper()
	m := r.Members[i]
	item := pathItems(r.Path.Request)[i]
	task := NewTask()
	task.ID = m.TaskID
	task.Protocol = m.Protocol
	task.Status = base.DownloadStatusPause
	task.Meta = &fetcher.FetcherMeta{Req: item.Req, Opts: m.Options}
	task.Progress = &Progress{}
	v, _ := pathVariantForCaps(r.RequiredCapabilities)
	if v.Native {
		return task, recoveryRecord{}
	}
	task.Recovery = &TaskRecoveryRef{Version: 4}
	var defaults, source string
	var err error
	if m.Protocol == "http" {
		defaults, err = transfer.HTTPExecutionDefaultsHash(task.Meta.Req, "", "direct")
		if err == nil {
			source, err = recoverySourceHash(task.Meta.Req, defaults)
		}
	} else {
		var context *transfer.ProtocolExecutionContext
		context, err = transfer.NewProtocolExecutionContext(m.Protocol, task.Meta.Req, json.RawMessage(`{"version":1}`), "direct", nil)
		if err == nil {
			defaults, source = context.DefaultsHash(), context.SourceHash()
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	initial := recoveryRecord{Version: 4, TaskID: task.ID, Episode: "path-episode", SourceRevision: 1, SourceHash: source, ExecutionDefaultsHash: defaults, InitialAttempt: recoveryInitialAttempt{State: "ungranted"}, Actions: []recoveryAction{}}
	return task, initial
}
func pathK12Expected(t *testing.T, f *mcpK12Fixture, id string) PathFreezeExpected {
	t.Helper()
	var e PathFreezeExpected
	err := withDatasetReadView(f.store, f.d, func(v datasetReader) error {
		task, _, err := ReadPathFreeze(v, id)
		if err != nil {
			return err
		}
		r, err := loadOperationFrom(v, task.CreationOperation.OperationID)
		if err != nil {
			return err
		}
		e.Record = *r
		e.Meta = task.Meta
		e.Token.TaskID = id
		variant, _ := pathVariantForCaps(r.RequiredCapabilities)
		if variant.Native {
			e.SourceHash = mcpWriteHash(task.Meta.Req)
		} else {
			_, record, _, err := readSharedRecovery(v, id)
			if err != nil {
				return err
			}
			e.Token.Episode = record.Episode
			e.Token.Generation = record.Generation
			e.Token.SourceRevision = record.SourceRevision
			e.SourceHash = record.SourceHash
			e.ExecutionDefaultsHash = record.ExecutionDefaultsHash
		}
		var raw json.RawMessage
		e.CheckpointPresent, err = v.Get(bucketSave, id, &raw)
		if err == nil && e.CheckpointPresent {
			e.CheckpointHash, err = checkpointDigest(raw)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func pathK12Prepared(t *testing.T, e PathFreezeExpected, root, name string) PathFreezePrepared {
	t.Helper()
	meta, err := cloneOperationJSON(e.Meta)
	if err != nil {
		t.Fatal(err)
	}
	meta.Res = &base.Resource{Files: []*base.FileInfo{{Name: "server.bin", Size: 4}}, Size: 4}
	meta.Opts.Path = root
	meta.Opts.Name = name
	layout, err := pathResourceLayout(meta)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := pathpolicy.LayoutFingerprint(layout)
	if err != nil {
		t.Fatal(err)
	}
	return PathFreezePrepared{ResolvedMeta: meta, Decision: pathpolicy.Decision{Version: 1, Root: root, Origin: pathpolicy.Default}, Layout: layout, LayoutHash: hash, StagingName: ".managed-stage"}
}
func pathK12Read(t *testing.T, f *mcpK12Fixture, id string) (*Task, TaskPathFreeze) {
	t.Helper()
	var task *Task
	var state TaskPathFreeze
	err := withDatasetReadView(f.store, f.d, func(v datasetReader) error { var err error; task, state, err = ReadPathFreeze(v, id); return err })
	if err != nil {
		t.Fatal(err)
	}
	return task, state
}
func TestPathFreezeK12HTTPACKAndReuse(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := mcpK12New(t, backend)
			header, snapshot := pathK12Intent(f, "http", "http-freeze")
			r, _, err := reservePathOperation(f.store, f.d, header, snapshot, header.Request.Opts)
			if err != nil {
				t.Fatal(err)
			}
			task, initial := pathK12Candidate(t, r, 0)
			task, fresh, err := createPathOperationTask(f.store, f.d, r, task, initial)
			if err != nil || !fresh {
				t.Fatalf("M %v", err)
			}
			checkpoint := json.RawMessage(`{"offset":0,"exact":9007199254740993}`)
			if err = f.store.Put(bucketSave, task.ID, checkpoint); err != nil {
				t.Fatal(err)
			}
			expected := pathK12Expected(t, f, task.ID)
			prepared := pathK12Prepared(t, expected, "/synthetic-input", "fixed.bin")
			if owned, fresh, err := ConfirmPathFreeze(mcpK12Fault{f.store, false}, f.d, expected, prepared); err == nil || fresh || owned != nil {
				t.Fatal("rollback F granted")
			}
			if owned, fresh, err := ConfirmPathFreeze(mcpK12Fault{f.store, true}, f.d, expected, prepared); err == nil || fresh || owned != nil {
				t.Fatal("unknown F ACK granted")
			}
			_, state := pathK12Read(t, f, task.ID)
			if state.Phase != PathFreezeFrozen || state.Layout.Members[0].RelativePath != "fixed.bin" {
				t.Fatal("F not durably confirmed")
			}
			// A later legitimate run/source revision does not cause path re-selection.
			err = f.store.UpdateAtomic(func(tx StorageTransaction) error {
				_, record, required, err := readSharedRecovery(tx, task.ID)
				if err != nil {
					return err
				}
				record.Generation = 2
				record.SourceRevision = 2
				record.InitialAttempt = recoveryInitialAttempt{State: "issued", Generation: 2}
				if err = tx.Put(bucketTaskRecovery, task.ID, &record); err != nil {
					return err
				}
				return tx.Put(bucketRecoveryRequired, task.ID, &required)
			})
			if err != nil {
				t.Fatal(err)
			}
			expected = pathK12Expected(t, f, task.ID)
			drift := pathK12Prepared(t, expected, "/must-not-reselect", "different.bin")
			owned, fresh, err := ConfirmPathFreeze(f.store, f.d, expected, drift)
			if err != nil || fresh || owned.Meta.Opts.Path != "/synthetic-input" || owned.Meta.Opts.Name != "fixed.bin" {
				t.Fatalf("frozen replay drift %v", err)
			}
			if expected.CheckpointHash != pathK12Expected(t, f, task.ID).CheckpointHash {
				t.Fatal("F changed checkpoint")
			}
			// New defaults cannot replace the first reservation's policy snapshot.
			snapshot.InputRoot = "/changed-default"
			snapshot.EffectiveDefaultRoot = snapshot.InputRoot
			opts, _ := cloneOperationJSON(header.Request.Opts)
			opts.Path = snapshot.InputRoot
			replay, fresh, err := reservePathOperation(f.store, f.d, header, snapshot, opts)
			if err != nil || fresh || replay.Path.Snapshot.InputRoot != "/synthetic-input" {
				t.Fatal("R reinterpreted defaults")
			}
			legacy, err := (&Downloader{storage: f.store}).QueryBatchOperationForDataset(f.d, header.OperationID)
			if err != nil || legacy.Version != 1 || len(legacy.TaskIDs) != 1 || legacy.TaskIDs[0] != task.ID {
				t.Fatalf("cold historical query %v", err)
			}
		})
	}
}
func TestPathFreezeK12NativeLayoutBinding(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := mcpK12New(t, backend)
			header, snapshot := pathK12Intent(f, "bt", "native-freeze")
			header.Request.Opts.Name = "bundle"
			header.Request.Opts.SelectFiles = []int{0}
			r, _, err := reservePathOperation(f.store, f.d, header, snapshot, header.Request.Opts)
			if err != nil {
				t.Fatal(err)
			}
			task, initial := pathK12Candidate(t, r, 0)
			task, _, err = createPathOperationTask(f.store, f.d, r, task, initial)
			if err != nil {
				t.Fatal(err)
			}
			expected := pathK12Expected(t, f, task.ID)
			meta, _ := cloneOperationJSON(expected.Meta)
			meta.Res = &base.Resource{Name: "bundle", Files: []*base.FileInfo{{Name: "data.bin", Path: "sub", Size: 4}, {Name: "zero.bin", Size: 0}}, Size: 4}
			layout, err := pathResourceLayout(meta)
			if err != nil {
				t.Fatal(err)
			}
			hash, _ := pathpolicy.LayoutFingerprint(layout)
			p := PathFreezePrepared{ResolvedMeta: meta, Decision: pathpolicy.Decision{Version: 1, Root: snapshot.InputRoot, Origin: pathpolicy.Default}, Layout: layout, LayoutHash: hash}
			task, _, err = ConfirmPathFreeze(f.store, f.d, expected, p)
			if err != nil {
				t.Fatal(err)
			}
			if task.Recovery != nil || len(task.PathFreeze.Layout.Members) != 2 || task.PathFreeze.Layout.Members[1].Selected {
				t.Fatal("native state/zero-length member changed")
			}
			expected = pathK12Expected(t, f, task.ID)
			binding := PathLayoutOutputState{Phase: MCPWriteOutputStaging, Version: 1, DirectoryIdentity: transfer.CompletionFileIdentity{Device: 7, Inode: 11}, Files: []PathFileIdentity{{Index: 0, RelativePath: layout.Members[0].RelativePath, Identity: transfer.CompletionFileIdentity{Device: 7, Inode: 13}}, {Index: 1, RelativePath: layout.Members[1].RelativePath, Identity: transfer.CompletionFileIdentity{Device: 7, Inode: 17}}}}
			if task, fresh, err := SavePathLayoutOutput(mcpK12Fault{f.store, true}, f.d, expected, binding); err == nil || fresh || task != nil {
				t.Fatal("unknown native ACK activated")
			}
			task, _ = pathK12Read(t, f, task.ID)
			if task.PathFreeze.LayoutOutput == nil || len(task.PathFreeze.LayoutOutput.Files) != 2 {
				t.Fatal("native binding not persisted")
			}
			var raw json.RawMessage
			found, err := f.store.Get(bucketTaskRecovery, task.ID, &raw)
			if err != nil || found {
				t.Fatal("native recovery ledger fabricated")
			}
		})
	}
}
func TestPathFreezeK12NetdiskTree(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := mcpK12New(t, backend)
			header, snapshot := pathK12Intent(f, "netdisk", "netdisk-tree")
			first := header.Request
			second, _ := cloneOperationJSON(first)
			first.Opts.Name = "a.bin"
			second.Opts.Name = "b.bin"
			first.Req.Extra = map[string]any{"batchId": "original-batch", "deferred": true, "session": "synthetic", "exact": json.Number("9007199254740993")}
			second.Req.Extra = first.Req.Extra
			header.Request = nil
			header.Reqs = []*base.CreateTaskBatchItem{first, second}
			header.BatchID = "original-batch"
			r, _, err := reservePathTreeOperation(f.store, f.d, header, snapshot, []*base.Options{first.Opts, second.Opts})
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{r.Members[0].TaskID, r.Members[1].TaskID}
			candidates := make([]*Task, len(ids))
			initials := make([]recoveryRecord, len(ids))
			for i := range ids {
				candidates[i], initials[i] = pathK12Candidate(t, r, i)
			}
			if _, fresh, e := createPathOperationTree(f.store, f.d, r, candidates, initials); e != nil || !fresh {
				t.Fatalf("tree M %v", e)
			}

			expected := []PathFreezeExpected{pathK12Expected(t, f, ids[0]), pathK12Expected(t, f, ids[1])}
			prepared := []PathFreezePrepared{pathK12Prepared(t, expected[0], snapshot.InputRoot, "a.bin"), pathK12Prepared(t, expected[1], snapshot.InputRoot, "b.bin")}
			if _, fresh, err := ConfirmPathFreeze(f.store, f.d, expected[0], prepared[0]); !errors.Is(err, ErrPathFreezeConflict) || fresh {
				t.Fatal("per-file root choice accepted")
			}
			owned, fresh, err := ConfirmPathFreezeTree(f.store, f.d, expected, prepared)
			if err != nil || !fresh || len(owned) != 2 {
				t.Fatalf("tree F %v", err)
			}
			if owned[0].PathFreeze.TreeLayoutHash != owned[1].PathFreeze.TreeLayoutHash {
				t.Fatal("members not bound to same tree")
			}
			var current *batchOperationRecord
			err = withDatasetReadView(f.store, f.d, func(v datasetReader) error {
				var err error
				current, err = loadOperationFrom(v, header.OperationID)
				return err
			})
			if err != nil || current.Path.Request.BatchID != header.BatchID || len(current.Path.TreeLayout.Members) != 2 || !reflect.DeepEqual(current.Path.Request.Reqs, header.Reqs) {
				t.Fatalf("prepared tree altered %v", err)
			}
			for _, id := range ids {
				task, _ := pathK12Read(t, f, id)
				if task.Meta.Opts.Path != snapshot.InputRoot {
					t.Fatal("tree root split")
				}
			}
		})
	}
}

func TestPathFreezeK12MediaSidecarsAndPublication(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := mcpK12New(t, backend)
			header, snapshot := pathK12Intent(f, "media", "media-layout")
			r, _, err := reservePathOperation(f.store, f.d, header, snapshot, header.Request.Opts)
			if err != nil {
				t.Fatal(err)
			}
			candidate, initial := pathK12Candidate(t, r, 0)
			task, _, err := createPathOperationTask(f.store, f.d, r, candidate, initial)
			if err != nil {
				t.Fatal(err)
			}
			expected := pathK12Expected(t, f, task.ID)
			p := pathK12Prepared(t, expected, snapshot.InputRoot, "movie.mp4")
			p.WorkDirectory = ".media-private-work"
			p.Layout, err = pathpolicy.BuildLayoutV1(snapshot.InputRoot, []pathpolicy.Member{{Index: 0, RelativePath: "movie.mp4", Selected: true}, {Index: 1, RelativePath: "movie.en.srt", Selected: true}})
			if err != nil {
				t.Fatal(err)
			}
			p.LayoutHash, _ = pathpolicy.LayoutFingerprint(p.Layout)
			wrong, _ := cloneOperationJSON(p)
			wrong.Layout.Members[1].RelativePath = "elsewhere/movie.en.srt"
			wrong.LayoutHash, _ = pathpolicy.LayoutFingerprint(wrong.Layout)
			if _, fresh, err := ConfirmPathFreeze(f.store, f.d, expected, wrong); err == nil || fresh {
				t.Fatal("non-sibling sidecar accepted")
			}
			task, _, err = ConfirmPathFreeze(f.store, f.d, expected, p)
			if err != nil {
				t.Fatal(err)
			}
			if len(task.Meta.Res.Files) != 1 || len(task.PathFreeze.Layout.Members) != 2 {
				t.Fatal("historical Resource falsified")
			}
			root := transfer.CompletionFileIdentity{Device: 7, Inode: 11}
			work := transfer.CompletionFileIdentity{Device: 7, Inode: 13}
			binding := PathLayoutOutputState{Version: 1, Phase: MCPWriteOutputStaging, DirectoryIdentity: root, WorkDirectoryIdentity: &work, Files: []PathFileIdentity{}}
			expected = pathK12Expected(t, f, task.ID)
			task, _, err = SavePathLayoutOutput(f.store, f.d, expected, binding)
			if err != nil {
				t.Fatal(err)
			}
			binding.Phase = MCPWriteOutputPublishing
			binding.Files = []PathFileIdentity{{Index: 0, RelativePath: "movie.mp4", Identity: transfer.CompletionFileIdentity{Device: 7, Inode: 17}}, {Index: 1, RelativePath: "movie.en.srt", Identity: transfer.CompletionFileIdentity{Device: 7, Inode: 19}}}
			expected = pathK12Expected(t, f, task.ID)
			if owned, fresh, err := SavePathLayoutOutput(mcpK12Fault{f.store, true}, f.d, expected, binding); err == nil || fresh || owned != nil {
				t.Fatal("unknown publication ACK granted")
			}
			task, _ = pathK12Read(t, f, task.ID)
			if task.PathFreeze.LayoutOutput.Phase != MCPWriteOutputPublishing {
				t.Fatal("unknown commit not preserved")
			}
			expected = pathK12Expected(t, f, task.ID)
			rewind := binding
			rewind.Phase = MCPWriteOutputStaging
			if _, fresh, err := SavePathLayoutOutput(f.store, f.d, expected, rewind); err == nil || fresh {
				t.Fatal("publishing regained RW staging")
			}
			uncertain := binding
			uncertain.Phase = MCPWriteOutputUncertain
			if _, _, err = SavePathLayoutOutput(f.store, f.d, expected, uncertain); err != nil {
				t.Fatal(err)
			}
			expected = pathK12Expected(t, f, task.ID)
			if _, fresh, err := SavePathLayoutOutput(f.store, f.d, expected, rewind); err == nil || fresh {
				t.Fatal("uncertain regained RW")
			}
			// An old writer route may query R2 history, but cannot cancel it.
			next := header
			next.OperationID = "cancel-pending"
			if _, _, err = reservePathOperation(f.store, f.d, next, snapshot, next.Request.Opts); err != nil {
				t.Fatal(err)
			}
			if _, err = (&Downloader{storage: f.store}).CancelBatchOperationForDataset(f.d, next.OperationID); !errors.Is(err, ErrOperationConflict) {
				t.Fatalf("legacy cancel entered R2: %v", err)
			}
			if err = cancelPathOperation(f.store, f.d, next.OperationID, "http-path-create/v1"); !errors.Is(err, ErrPathFreezeHeld) {
				t.Fatal("wrong-kind cancel accepted")
			}
			if err = cancelPathOperation(f.store, f.d, next.OperationID, "media-path-create/v1"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
