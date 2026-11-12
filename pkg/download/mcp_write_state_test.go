package download

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type mcpK12Fixture struct {
	store AtomicStorage
	d     string
	path  string
}

func mcpK12New(t *testing.T, backend string) *mcpK12Fixture {
	t.Helper()
	var store AtomicStorage = NewMemStorage()
	path := ""
	if backend == "bolt" {
		path = t.TempDir()
		store = NewBoltStorage(path)
	}
	f := &mcpK12Fixture{store: store, path: path}
	t.Cleanup(func() { _ = f.store.Close() })
	if e := store.Setup([]string{bucketConfig, bucketTask, bucketSave, bucketTaskRecovery, bucketRecoveryRequired, bucketTaskOperations, bucketOperationTasks, bucketTaskCompletion, bucketCompletionRequired}); e != nil {
		t.Fatal(e)
	}
	if e := InitializeDatasetIdentity(store); e != nil {
		t.Fatal(e)
	}
	v, e := (&Downloader{storage: store}).DatasetIdentity()
	if e != nil {
		t.Fatal(e)
	}
	f.d = v.DatasetID
	return f
}
func (f *mcpK12Fixture) intent(op string) (MCPWriteRequest, MCPExecutionBinding) {
	p := "mcp-authority-v1:" + strings.Repeat("a", 64)
	c := MCPWriteRequest{Version: 1, DatasetID: f.d, PolicyID: p, OperationID: op, Kind: MCPWriteCreate, Create: &MCPWriteCreateRequest{URL: "https://fixture.invalid/file", OutputRootID: "fixture", FileName: "output.bin"}}
	b := MCPExecutionBinding{Version: 1, RequiredCapability: MCPWriteNamespace, DatasetID: f.d, PolicyID: p, NetworkPolicyID: "mcp-network/v1:" + strings.Repeat("b", 64), Root: &MCPWriteRootBinding{RootID: "fixture", Path: "/synthetic-mcp-root-not-opened", Identity: transfer.CompletionFileIdentity{Device: 7, Inode: 11}}, FileName: "output.bin", StagingName: ".private-stage", EffectsVersion: "effects/v1", Effects: []string{}}
	return c, b
}
func (f *mcpK12Fixture) candidate(t *testing.T, r mcpWriteRecord) (*Task, recoveryRecord) {
	t.Helper()
	task := NewTask()
	task.ID = r.Member.TaskID
	task.Protocol = "http"
	task.Status = base.DownloadStatusPause
	task.Recovery = &TaskRecoveryRef{Version: 4}
	task.Progress = &Progress{}
	task.Meta = &fetcher.FetcherMeta{Req: &base.Request{URL: r.Command.Create.URL}, Opts: &base.Options{Path: r.Binding.Root.Path, Name: r.Binding.FileName, Extra: map[string]any{"exact": json.Number("9007199254740993"), "nested": map[string]any{"signed": json.Number("-9007199254740993.125")}}}}
	defaults, e := transfer.HTTPExecutionDefaultsHash(task.Meta.Req, "", "direct")
	if e != nil {
		t.Fatal(e)
	}
	source, e := recoverySourceHash(task.Meta.Req, defaults)
	if e != nil {
		t.Fatal(e)
	}
	initial := recoveryRecord{Version: 4, TaskID: task.ID, Episode: "_initial-episode", SourceRevision: 1, SourceHash: source, ExecutionDefaultsHash: defaults, InitialAttempt: recoveryInitialAttempt{State: "ungranted"}, Actions: []recoveryAction{}}
	return task, initial
}
func (f *mcpK12Fixture) read(t *testing.T, id string) (*Task, mcpWriteRecord) {
	t.Helper()
	var task *Task
	var r mcpWriteRecord
	e := withDatasetReadView(f.store, f.d, func(v datasetReader) error { var e error; task, r, e = readMCPWriteTask(v, id); return e })
	if e != nil {
		t.Fatal(e)
	}
	return task, r
}
func (f *mcpK12Fixture) create(t *testing.T, op string) (*Task, mcpWriteRecord, transfer.RecoveryToken) {
	t.Helper()
	c, b := f.intent(op)
	r, fresh, e := reserveMCPWrite(f.store, f.d, c, b)
	if e != nil || !fresh {
		t.Fatalf("R %v %v", fresh, e)
	}
	candidate, initial := f.candidate(t, r)
	task, fresh, e := createMCPWriteTask(f.store, f.d, r, candidate, initial)
	if e != nil || !fresh {
		t.Fatalf("M %v %v", fresh, e)
	}
	// A private component fixture declares an issued run. This is not a real
	// writer, RAM permission or evidence that HTTP/FS pipeline is integrated.
	e = f.store.UpdateAtomic(func(tx StorageTransaction) error {
		current, record, required, e := readSharedRecovery(tx, task.ID)
		if e != nil {
			return e
		}
		record.Generation = 1
		record.InitialAttempt = recoveryInitialAttempt{State: "issued", Generation: 1}
		if e = tx.Put(bucketTask, current.ID, current); e != nil {
			return e
		}
		if e = tx.Put(bucketTaskRecovery, current.ID, &record); e != nil {
			return e
		}
		return tx.Put(bucketRecoveryRequired, current.ID, &required)
	})
	if e != nil {
		t.Fatal(e)
	}
	task, r = f.read(t, task.ID)
	return task, r, transfer.RecoveryToken{TaskID: task.ID, Episode: initial.Episode, Generation: 1, SourceRevision: 1}
}
func mcpK12Stage(s MCPTaskState) MCPWriteOutputState {
	return MCPWriteOutputState{Phase: MCPWriteOutputStaging, Binding: &transfer.MCPOutputBinding{OwnerID: s.OperationID, DirectoryIdentity: s.CreationBinding.Root.Identity, StagingIdentity: transfer.CompletionFileIdentity{Device: 7, Inode: 17}, StagingName: s.CreationBinding.StagingName, FinalName: s.CreationBinding.FileName}}
}
func mcpK12Code(e error, c MCPWriteErrorCode) bool {
	var fixed *MCPWriteError
	return errors.As(e, &fixed) && fixed.Code == c
}

func TestMCPWriteK12CreateOutputAndReopen(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := mcpK12New(t, backend)
			task, r, token := f.create(t, "create-main")
			if task.CreationOperation.Version != 2 || task.CreationOperation.Namespace != MCPWriteNamespace || checkCreationAssociationFrom(f.store, task) != nil || task.creationHeld() {
				t.Fatal("strict association failed")
			}
			raw, e := json.Marshal(task.Meta.Opts.Extra)
			if e != nil || !strings.Contains(string(raw), "9007199254740993") {
				t.Fatalf("precision %s %v", raw, e)
			}
			c, b := f.intent("create-main")
			b.NetworkPolicyID = "mcp-network/v1:" + strings.Repeat("c", 64)
			replay, fresh, e := reserveMCPWrite(f.store, f.d, c, b)
			if e != nil || fresh || replay.ExecutionBindingHash != r.ExecutionBindingHash {
				t.Fatalf("replay changed server binding %v", e)
			}
			stage := mcpK12Stage(*task.MCPWrite)
			task, fresh, e = saveMCPWriteOutput(f.store, f.d, token, *task.MCPWrite, stage, nil)
			if e != nil || !fresh {
				t.Fatalf("stage %v", e)
			}
			pub := task.MCPWrite.Output
			pub.Phase = MCPWriteOutputPublishing
			task, _, e = saveMCPWriteOutput(f.store, f.d, token, *task.MCPWrite, pub, nil)
			if e != nil {
				t.Fatal(e)
			}
			done := task.MCPWrite.Output
			done.Phase = MCPWriteOutputPublished
			task, fresh, e = saveMCPWriteOutput(f.store, f.d, token, *task.MCPWrite, done, &Progress{Downloaded: 20480, Used: 3})
			if e != nil || !fresh || task.Status != base.DownloadStatusDone {
				t.Fatalf("C %v", e)
			}
			if backend == "bolt" {
				if e = f.store.Close(); e != nil {
					t.Fatal(e)
				}
				f.store = NewBoltStorage(f.path)
			}
			owned, stored := f.read(t, task.ID)
			if owned.Status != base.DownloadStatusDone || owned.Progress.Downloaded != 20480 || stored.Member.TaskState.Output.Phase != MCPWriteOutputPublished {
				t.Fatal("published not durable")
			}
			owned.MCPWrite.CreationBinding.Root.Path = "/mutated-owned-copy"
			again, _ := f.read(t, task.ID)
			if again.MCPWrite.CreationBinding.Root.Path == owned.MCPWrite.CreationBinding.Root.Path {
				t.Fatal("read alias")
			}
			var q MCPWriteReceipt
			e = withDatasetReadView(f.store, f.d, func(v datasetReader) error {
				var err error
				q, err = queryMCPWrite(v, MCPWriteQuery{1, f.d, c.PolicyID, c.OperationID})
				return err
			})
			if e != nil || q.Result.Outcome != MCPWriteCreated || q.QueryRequired {
				t.Fatalf("query %v", e)
			}
		})
	}
}

type mcpK12Fault struct {
	AtomicStorage
	commit bool
}

func (s mcpK12Fault) UpdateAtomic(fn func(StorageTransaction) error) error {
	if s.commit {
		if e := s.AtomicStorage.UpdateAtomic(fn); e != nil {
			return e
		}
		return errors.New("synthetic lost ACK")
	}
	return s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error {
		if e := fn(tx); e != nil {
			return e
		}
		return errors.New("synthetic rollback")
	})
}
func TestMCPWriteK12ACKAndPermissions(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := mcpK12New(t, backend)
			c, b := f.intent("ack-op")
			zero, fresh, e := reserveMCPWrite(mcpK12Fault{f.store, false}, f.d, c, b)
			if e == nil || fresh || !reflect.DeepEqual(zero, mcpWriteRecord{}) {
				t.Fatal("rollback granted")
			}
			r, fresh, e := reserveMCPWrite(f.store, f.d, c, b)
			if e != nil || !fresh {
				t.Fatal(e)
			}
			candidate, initial := f.candidate(t, r)
			task, fresh, e := createMCPWriteTask(mcpK12Fault{f.store, true}, f.d, r, candidate, initial)
			if e == nil || fresh || task != nil {
				t.Fatal("unknown M granted")
			}
			task, stored := f.read(t, candidate.ID)
			if task == nil || stored.State != MCPWriteSettled {
				t.Fatal("committed M lost")
			}
			if _, fresh, e = createMCPWriteTask(f.store, f.d, r, candidate, initial); e == nil || fresh {
				t.Fatal("query/replay reconstructed M admission")
			}
			c.Kind = MCPWritePause
			c.Create = nil
			c.Action = &MCPWriteActionRequest{TaskID: candidate.ID, Expected: MCPWriteTarget{"ep", 1, 1}}
			if _, _, e = reserveMCPWrite(f.store, f.d, c, b); !mcpK12Code(e, MCPWriteOperationConflict) {
				t.Fatalf("cross-kind replay %v", e)
			}
			// No implicit repair when one required MCP part is missing/corrupt.
			if e = f.store.Delete(bucketOperationTasks, candidate.ID); e != nil {
				t.Fatal(e)
			}
			e = withDatasetReadView(f.store, f.d, func(v datasetReader) error { _, _, err := readMCPWriteTask(v, candidate.ID); return err })
			if !mcpK12Code(e, MCPWriteTaskUnavailable) {
				t.Fatalf("missing reverse %v", e)
			}
			// Unknown field in a required R2 envelope is held, not ignored.
			encoded, _ := json.Marshal(stored)
			encoded = append(encoded[:len(encoded)-1], []byte(`,"unrecognizedCapability":true}`)...)
			var bad mcpWriteRecord
			if json.Unmarshal(encoded, &bad) == nil {
				t.Fatal("unknown R2 field accepted")
			}
		})
	}
}
func TestMCPWriteK12ActionBindingAndUnknownACK(t *testing.T) {
	for _, backend := range []string{"mem", "bolt"} {
		t.Run(backend, func(t *testing.T) {
			f := mcpK12New(t, backend)
			task, _, token := f.create(t, "original-create")
			var e error
			task, _, e = saveMCPWriteOutput(f.store, f.d, token, *task.MCPWrite, mcpK12Stage(*task.MCPWrite), nil)
			if e != nil {
				t.Fatal(e)
			}
			creation := task.MCPWrite.CreationBinding
			next := task.MCPWrite.CurrentExecutionBinding
			next.PolicyID = "mcp-authority-v1:" + strings.Repeat("c", 64)
			target := MCPWriteTarget{token.Episode, token.Generation, token.SourceRevision}
			command := MCPWriteRequest{Version: 1, DatasetID: f.d, PolicyID: next.PolicyID, OperationID: "resume-op", Kind: MCPWriteResume, Action: &MCPWriteActionRequest{TaskID: task.ID, Expected: target}}
			r, _, e := reserveMCPWrite(f.store, f.d, command, next)
			if e != nil {
				t.Fatal(e)
			}
			stale := target
			stale.Generation++
			if _, fresh, e := commitMCPWriteAction(f.store, f.d, r, stale); e == nil || fresh {
				t.Fatal("stale target applied")
			}
			owned, fresh, e := commitMCPWriteAction(mcpK12Fault{f.store, true}, f.d, r, target)
			if e == nil || fresh || owned != nil {
				t.Fatal("unknown action ACK granted")
			}
			after, _ := f.read(t, task.ID)
			if !reflect.DeepEqual(after.MCPWrite.CreationBinding, creation) || after.MCPWrite.CurrentExecutionBinding.PolicyID != next.PolicyID {
				t.Fatal("binding history lost")
			}
			if _, fresh, e = commitMCPWriteAction(f.store, f.d, r, target); e == nil || fresh {
				t.Fatal("action replay authorized")
			}
			var receipt MCPWriteReceipt
			e = withDatasetReadView(f.store, f.d, func(v datasetReader) error {
				var err error
				receipt, err = queryMCPWrite(v, MCPWriteQuery{1, f.d, command.PolicyID, command.OperationID})
				return err
			})
			if e != nil || receipt.Result.Outcome != MCPWriteApplied || receipt.QueryRequired {
				t.Fatal("query did not reconcile receipt")
			}
			// Unknown op does not fabricate kind or Task identity, nor write an R.
			e = withDatasetReadView(f.store, f.d, func(v datasetReader) error {
				var err error
				receipt, err = queryMCPWrite(v, MCPWriteQuery{1, f.d, command.PolicyID, "no-record"})
				return err
			})
			if e != nil || receipt.State != MCPWriteNotRecorded || receipt.Kind != nil || receipt.Result != nil || receipt.QueryRequired {
				t.Fatalf("absence %v %+v", e, receipt)
			}
		})
	}
}
