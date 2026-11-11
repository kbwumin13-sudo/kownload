//go:build darwin || linux

package download

// Product checkpoint: dfd3a15e8adcfbbd55b5119e1aabe826bb5ebff3. Stored-fault
// cases exercise the real post-join completion seam with concrete Mem/Bolt
// authority; their quiesced writer is a component fixture, not network proof.
// The separate network case uses the real HTTP manager, scheduler and watch.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type s4g12EffectMessage struct {
	Event, TaskID, Status string
	PID                   int
}

// A real script invocation execs this one helper in the same test executable.
// The parent waits until its reported PID is reaped, rather than leaving an
// untracked script process after seeing its first side effect.
func TestS4G12EffectProcess(t *testing.T) {
	endpoint := os.Getenv("S4G12_EFFECT_ENDPOINT")
	if endpoint == "" {
		return
	}
	data, err := json.Marshal(s4g12EffectMessage{os.Getenv("GOPEED_EVENT"), os.Getenv("GOPEED_TASK_ID"), os.Getenv("GOPEED_TASK_STATUS"), os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Post(endpoint, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	client.CloseIdleConnections()
	if response.StatusCode != http.StatusNoContent {
		t.Fatal("script observer rejected event", response.StatusCode)
	}
}

type s4g12Effects struct {
	d               atomic.Pointer[Downloader]
	done, finally   atomic.Int32
	webhook, script atomic.Int32
	errorWebhook    atomic.Int32
	badOrder        atomic.Bool
	pid             atomic.Int64
	server          *httptest.Server
	events          <-chan VerifiedHTTPDoneEvent
	freshSeen       int
	changed         chan struct{}
	scriptPath      string
	scriptExitPath  string
	scriptPIDPath   string
}

func s4g12Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func s4g12NewEffects(t *testing.T) *s4g12Effects {
	t.Helper()
	o := &s4g12Effects{changed: make(chan struct{}, 32)}
	o.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var id string
		var effect string
		switch r.URL.Path {
		case "/script":
			var message s4g12EffectMessage
			if json.NewDecoder(r.Body).Decode(&message) != nil || message.PID <= 0 || message.Event != string(ScriptEventDownloadDone) || message.Status != string(base.DownloadStatusDone) {
				o.badOrder.Store(true)
			} else {
				id = message.TaskID
				o.pid.Store(int64(message.PID))
				effect = "script"
			}
		case "/webhook":
			var message WebhookData
			if json.NewDecoder(r.Body).Decode(&message) != nil || message.Payload == nil || message.Payload.Task == nil {
				o.badOrder.Store(true)
			} else if message.Event == WebhookEventDownloadDone {
				id = message.Payload.Task.ID
				effect = "webhook"
			} else {
				o.errorWebhook.Add(1) // Error reporting is not a success outlet.
			}
		default:
			o.badOrder.Store(true)
		}
		if id != "" {
			d := o.d.Load()
			if d == nil {
				o.badOrder.Store(true)
			} else {
				task := d.GetTask(id)
				views, err := d.TaskCompletionViewsForDataset(scopedDatasetA, []string{id})
				if err != nil || task == nil || task.Status != base.DownloadStatusDone || !views[id].Available || views[id].Phase != string(transfer.CompletionCommitted) {
					o.badOrder.Store(true)
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
		// Counts become visible only after the real Core API visibility check.
		if effect == "script" {
			o.script.Add(1)
		} else if effect == "webhook" {
			o.webhook.Add(1)
		}
		select {
		case o.changed <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(o.server.Close)
	o.scriptPath = filepath.Join(t.TempDir(), "success-observer.sh")
	o.scriptExitPath, o.scriptPIDPath = o.scriptPath+".exit", o.scriptPath+".pid"
	script := "#!/bin/sh\nexport S4G12_EFFECT_ENDPOINT=" + s4g12Quote(o.server.URL+"/script") +
		"\nprintf '%s' \"$$\" > " + s4g12Quote(o.scriptPIDPath) + "\n" + s4g12Quote(os.Args[0]) +
		" -test.run='^TestS4G12EffectProcess$' -test.timeout=8s\ns4g12_status=$?\nprintf '%s' \"$s4g12_status\" > " + s4g12Quote(o.scriptExitPath) + "\nexit \"$s4g12_status\"\n"
	if err := os.WriteFile(o.scriptPath, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return o
}

func (o *s4g12Effects) attach(t *testing.T, d *Downloader) {
	t.Helper()
	o.d.Store(d)
	cfg, err := d.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Webhook = &base.WebhookConfig{Enable: true, URLs: []string{o.server.URL + "/webhook"}}
	cfg.Script = &base.ScriptConfig{Enable: true, Paths: []string{o.scriptPath}}
	if err = d.PutConfig(cfg); err != nil {
		t.Fatal(err)
	}
	d.Listener(func(e *Event) {
		// emit supplies an owned snapshot; no store/Task/main lock reentry.
		if e.Key == EventKeyDone {
			if e.Task.Status != base.DownloadStatusDone {
				o.badOrder.Store(true)
			}
			o.done.Add(1)
		}
		if e.Key == EventKeyFinally && e.Task.Status == base.DownloadStatusDone && e.Err == nil {
			o.finally.Add(1)
		}
	})
	var release func()
	o.events, release, err = d.SubscribeVerifiedHTTPDone(context.Background(), scopedDatasetA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
}

func s4g12Await(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("bounded wait:", what)
		}
	}
}

func (o *s4g12Effects) require(t *testing.T, want int) {
	t.Helper()
	if want > 0 {
		s4g12Await(t, "real success script/webhook finished", func() bool {
			pid := o.pid.Load()
			exit, _ := os.ReadFile(o.scriptExitPath)
			pidBytes, _ := os.ReadFile(o.scriptPIDPath)
			shellPID, _ := strconv.Atoi(string(pidBytes))
			return int(o.script.Load()) >= want && int(o.webhook.Load()) >= want && len(exit) > 0 && pid > 0 && shellPID > 0 &&
				errors.Is(syscall.Kill(int(pid), 0), syscall.ESRCH) && errors.Is(syscall.Kill(shellPID, 0), syscall.ESRCH)
		})
		if exit, err := os.ReadFile(o.scriptExitPath); err != nil || string(exit) != "0" {
			t.Fatal("real success script did not exit normally", string(exit), err)
		}
	}
	for {
		select {
		case event, ok := <-o.events:
			if !ok {
				goto drained
			}
			if !event.ValidForDataset(scopedDatasetA) {
				t.Fatal("malformed actual fresh event")
			}
			o.freshSeen++
		default:
			goto drained
		}
	}
drained:
	if int(o.done.Load()) != want || int(o.finally.Load()) != want || int(o.script.Load()) != want || int(o.webhook.Load()) != want || o.freshSeen != want || o.badOrder.Load() {
		t.Fatalf("success counts want=%d done=%d finally=%d script=%d webhook=%d fresh=%d badOrder=%v", want, o.done.Load(), o.finally.Load(), o.script.Load(), o.webhook.Load(), o.freshSeen, o.badOrder.Load())
	}
}

func s4g12Downloader(t *testing.T, store Storage, root string) *Downloader {
	t.Helper()
	d := NewDownloader(&DownloaderConfig{Storage: store, StorageDir: root, StartupTaskPolicy: StartupTaskPolicyImportedReview, FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !d.closed.Load() {
			_ = d.Close()
		}
	})
	return d
}

// Same typed R/M/run/pin operations as verifiedWrittenFixture, with selectable
// concrete storage and explicit ownership so a new Downloader can reopen it.
func s4g12Written(t *testing.T, bolt bool, body []byte) (*Downloader, *Task, *transfer.RecoveryLease, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var store AtomicStorage = NewMemStorage()
	if bolt {
		store = NewBoltStorage(filepath.Join(root, "db"))
	}
	d := s4g12Downloader(t, store, root)
	if err := InitializeDatasetIdentity(store); err != nil {
		t.Fatal(err)
	}
	setScopedDataset(t, store, scopedDatasetA)
	sum := sha256.Sum256(body)
	r := operationRequest("https://fixture.invalid/s4g12", root)
	r.integrity = &VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", hex.EncodeToString(sum[:])}
	items, hash, err := canonicalBatch(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.reserveOperationInDataset(store, r, items, hash, scopedDatasetA); err != nil {
		t.Fatal(err)
	}
	created, err := d.createOperationMemberInDataset(store, r.OperationID, hash, 0, items[0], scopedDatasetA)
	if err != nil || !created {
		t.Fatal("actual M", err)
	}
	op, err := d.loadOperationInDataset(scopedDatasetA, r.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := d.readVerifiedTask(scopedDatasetA, op.Members[0].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	initTask(task)
	task.recoveryInitialEligible.Store(true)
	owner, err := newTaskRecoveryOwner(store, task)
	if err != nil {
		t.Fatal(err)
	}
	task.Recovery = &TaskRecoveryRef{Version: 1}
	task.recoveryOwner.Store(owner)
	task.Status, task.runGeneration, task.IsCreated = base.DownloadStatusRunning, 1, true
	task.Meta.Res = &base.Resource{Size: int64(len(body)), Files: []*base.FileInfo{{Name: "one.bin", Size: int64(len(body))}}}
	task.timer.Start()
	if _, err = owner.begin(1, ""); err != nil {
		t.Fatal(err)
	}
	if err = owner.persistSnapshot(task.clone(), nil, false); err != nil {
		t.Fatal(err)
	}
	d.lock.Lock()
	d.tasks = append(d.tasks, task)
	d.lock.Unlock()
	lease, err := owner.currentLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	file, err := d.prepareVerifiedOutput(context.Background(), scopedDatasetA, task, lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(body); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal("quiesced component writer", err, closeErr)
	}
	return d, task, lease, root
}

type s4g12WriteFault struct {
	AtomicStorage
	id      string
	phase   transfer.VerifiedCompletionPhase
	mode    string
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *s4g12WriteFault) unblock() { s.once.Do(func() { close(s.release) }) }

func (s *s4g12WriteFault) UpdateAtomic(fn func(StorageTransaction) error) error {
	matched := false
	err := s.AtomicStorage.UpdateAtomic(func(tx StorageTransaction) error {
		if err := fn(tx); err != nil {
			return err
		}
		var record transfer.VerifiedCompletionRecord
		found, err := tx.Get(bucketTaskCompletion, s.id, &record)
		if err != nil {
			return err
		}
		matched = found && record.Phase == s.phase && s.armed.CompareAndSwap(true, false)
		if matched && s.mode == "rollback" {
			return errors.New("s4g12 atomic rollback")
		}
		return nil
	})
	if matched {
		close(s.entered) // The DB transaction is finished; no held DB read/reentry.
		<-s.release
		if s.mode == "unknown" {
			return errors.New("s4g12 lost storage acknowledgement")
		}
	}
	return err
}

type s4g12CompleteResult struct {
	fresh bool
	err   error
}

func s4g12PauseAndClose(t *testing.T, d *Downloader, id string) {
	t.Helper()
	task := d.getTask(id)
	if task != nil && task.getStatus() == base.DownloadStatusRunning {
		if err := d.Pause(&TaskFilter{IDs: []string{id}}); err != nil {
			t.Fatal(err)
		}
	}
	if task != nil {
		task.statusLock.Lock()
		done := task.pauseDone
		task.statusLock.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("own pause did not settle before reopen")
			}
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestS4G12SuccessGatesStoredFaultsAndReopen(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		for _, scenario := range []struct {
			name, mode string
			phase      transfer.VerifiedCompletionPhase
		}{
			{"save-rollback", "rollback", transfer.CompletionVerifying},
			{"save-unknown", "unknown", transfer.CompletionVerifying},
			{"C-rollback", "rollback", transfer.CompletionCommitted},
			{"C-unknown", "unknown", transfer.CompletionCommitted},
			{"C-fresh-ACK", "ack", transfer.CompletionCommitted},
		} {
			t.Run(fmt.Sprintf("%s/%s", map[bool]string{false: "Mem", true: "Bolt"}[bolt], scenario.name), func(t *testing.T) {
				o := s4g12NewEffects(t)
				d, task, lease, root := s4g12Written(t, bolt, bytes.Repeat([]byte("s4g12 post-join bytes\n"), 100))
				o.attach(t, d)
				store := &s4g12WriteFault{AtomicStorage: d.storage.(AtomicStorage), id: task.ID, phase: scenario.phase, mode: scenario.mode, entered: make(chan struct{}), release: make(chan struct{})}
				store.armed.Store(true)
				defer store.unblock()
				result := make(chan s4g12CompleteResult, 1)
				finished := make(chan struct{})
				t.Cleanup(func() {
					store.unblock()
					select {
					case <-finished:
					case <-time.After(3 * time.Second):
						t.Error("own completion worker did not finish during cleanup")
					}
				})
				go func() {
					defer close(finished)
					fresh, err := d.completeVerifiedHTTPStored(task, quiescedVerifiedWriter{}, lease.Token.Generation, lease, store)
					result <- s4g12CompleteResult{fresh, err}
				}()
				select {
				case <-store.entered:
				case <-time.After(3 * time.Second):
					store.unblock()
					<-result
					t.Fatal("completion write did not reach fault seam")
				}
				// A stored C with ACK still gated cannot pass API visibility or
				// mint the private success permit, even with actual DB reads.
				views, err := d.TaskCompletionViewsForDataset(scopedDatasetA, []string{task.ID})
				if err != nil || views[task.ID].Phase == string(transfer.CompletionCommitted) || d.GetTask(task.ID).Status == base.DownloadStatusDone || task.verifiedSuccess.Load() != nil {
					store.unblock()
					<-result
					t.Fatal("API/fresh crossed unconfirmed C", err)
				}
				o.require(t, 0)
				_, pending, err := d.readVerifiedTask(scopedDatasetA, task.ID)
				if err != nil {
					store.unblock()
					<-result
					t.Fatal("real domain read after injected transaction", err)
				}
				expectedPhase := scenario.phase
				if scenario.mode == "rollback" {
					expectedPhase = transfer.CompletionWaiting
					if scenario.phase == transfer.CompletionCommitted {
						expectedPhase = transfer.CompletionPublished
					}
				}
				if pending.Phase != expectedPhase {
					store.unblock()
					<-result
					t.Fatal("wrong durable phase after rollback/unknown ACK", pending.Phase, expectedPhase)
				}
				store.unblock()
				completed := <-result
				wantFresh := scenario.mode == "ack"
				confirmed := scenario.phase == transfer.CompletionCommitted && scenario.mode != "rollback"
				if completed.fresh != wantFresh || confirmed && completed.err != nil || !confirmed && completed.err == nil {
					t.Fatal("completion result", completed.fresh, completed.err)
				}
				if (d.GetTask(task.ID).Status == base.DownloadStatusDone) != confirmed {
					t.Fatal("authoritative C reconciliation disagrees with memory")
				}
				if (task.verifiedSuccess.Load() != nil) != wantFresh {
					t.Fatal("only fresh confirmed ACK may install success")
				}
				d.deliverVerifiedSuccess(task)
				d.deliverVerifiedSuccess(task)
				wantEffects := 0
				if wantFresh {
					wantEffects = 1
				}
				o.require(t, wantEffects)
				for i := 0; i < 2; i++ {
					if _, err := d.queryVerifiedOperation(scopedDatasetA, "operation-1"); err != nil {
						t.Fatal(err)
					}
				}
				o.require(t, wantEffects)
				reused := d.storage
				s4g12PauseAndClose(t, d, task.ID)
				if bolt {
					reused = NewBoltStorage(filepath.Join(root, "db"))
				}
				reopened := s4g12Downloader(t, reused, root)
				o.attach(t, reopened)
				if reopened.getTask(task.ID).verifiedSuccess.Load() != nil {
					t.Fatal("reopen reconstructed ephemeral success permission")
				}
				if confirmed {
					if _, err := reopened.queryVerifiedOperation(scopedDatasetA, "operation-1"); err != nil {
						t.Fatal(err)
					}
					reopened.deliverVerifiedSuccess(reopened.getTask(task.ID))
					o.require(t, wantEffects) // Query/reopen must not replay.
				} else {
					if err := reopened.Continue(&TaskFilter{IDs: []string{task.ID}}); err != nil {
						t.Fatal(err)
					}
					waitStatus(t, reopened, task.ID, base.DownloadStatusDone, 5*time.Second)
					o.require(t, 1) // A new confirmed C is allowed exactly once.
				}
				_, recovered, err := reopened.readVerifiedTask(scopedDatasetA, task.ID)
				if err != nil || recovered.ID != pending.ID || recovered.StagingIdentity != pending.StagingIdentity || recovered.DirectoryIdentity != pending.DirectoryIdentity || recovered.Phase != transfer.CompletionCommitted {
					t.Fatal("reopen changed the original completion binding", err)
				}
				data, err := os.ReadFile(filepath.Join(root, recovered.FinalName))
				digest := sha256.Sum256(data)
				if err != nil || hex.EncodeToString(digest[:]) != recovered.ExpectedSHA256 {
					t.Fatal("reopened completion changed bytes", err)
				}
			})
		}
	}
}

func TestS4G12SuccessGatesRealHTTPWatchAndCommittedReopen(t *testing.T) {
	o := s4g12NewEffects(t)
	body := bytes.Repeat([]byte("s4g12 actual network success outlets\n"), 1024)
	var requests atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"s4g12-fixed"`)
		http.ServeContent(w, r, "network.bin", time.Time{}, bytes.NewReader(body))
	}))
	defer source.Close()
	root := t.TempDir()
	store := NewBoltStorage(filepath.Join(root, "db"))
	d := s4g12Downloader(t, store, root)
	if err := InitializeDatasetIdentity(store); err != nil {
		t.Fatal(err)
	}
	setScopedDataset(t, store, scopedDatasetA)
	o.attach(t, d)
	r := operationRequest(source.URL+"/payload", root)
	r.Reqs[0].Opts.Name = "network.bin"
	r.Reqs[0].Opts.Extra = map[string]any{"connections": 1, "autoTorrent": false, "autoExtract": false}
	sum := sha256.Sum256(body)
	r.integrity = &VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", hex.EncodeToString(sum[:])}
	op, err := d.createBatchOperationInDataset(context.Background(), r, scopedDatasetA)
	if err != nil || len(op.TaskIDs) != 1 {
		t.Fatal("real HTTP M→Q", err)
	}
	id := op.TaskIDs[0]
	waitStatus(t, d, id, base.DownloadStatusDone, 5*time.Second)
	o.require(t, 1)
	s4g12Await(t, "real watch teardown", func() bool { _, found := d.watchedTasks.Load(id); return !found })
	before := requests.Load()
	if before == 0 {
		t.Fatal("network case did not perform actual HTTP")
	}
	live := d.getTask(id)
	d.deliverVerifiedSuccess(live)
	if _, err := d.queryVerifiedOperation(scopedDatasetA, r.OperationID); err != nil {
		t.Fatal(err)
	}
	o.require(t, 1)
	s4g12PauseAndClose(t, d, id)
	reopened := s4g12Downloader(t, NewBoltStorage(filepath.Join(root, "db")), root)
	o.attach(t, reopened)
	for i := 0; i < 3; i++ {
		if _, err := reopened.queryVerifiedOperation(scopedDatasetA, r.OperationID); err != nil {
			t.Fatal(err)
		}
		reopened.deliverVerifiedSuccess(reopened.getTask(id))
	}
	o.require(t, 1)
	if requests.Load() != before || reopened.getTask(id).fetcher != nil || reopened.getTask(id).verifiedSuccess.Load() != nil {
		t.Fatal("committed reopen recreated IO or success permit")
	}
}

func TestS4G12SuccessGatesCanceledOldGeneration(t *testing.T) {
	o := s4g12NewEffects(t)
	d, task, lease, _ := s4g12Written(t, true, bytes.Repeat([]byte("cancel gated hash\n"), 100))
	o.attach(t, d)
	gated, gate := gateVerifiedLease(lease)
	result := make(chan s4g12CompleteResult, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		gate.unblock()
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Error("own canceled worker did not finish during cleanup")
		}
	})
	defer gate.unblock()
	go func() {
		defer close(finished)
		fresh, err := d.completeVerifiedHTTP(task, quiescedVerifiedWriter{}, lease.Token.Generation, gated)
		result <- s4g12CompleteResult{fresh, err}
	}()
	waitVerifiedGate(t, gate)
	if err := d.Pause(&TaskFilter{IDs: []string{task.ID}}); err != nil {
		gate.unblock()
		<-result
		t.Fatal(err)
	}
	gate.unblock()
	completed := <-result
	if completed.fresh || completed.err == nil || task.verifiedSuccess.Load() != nil {
		t.Fatal("canceled old generation granted success", completed)
	}
	d.deliverVerifiedSuccess(task)
	o.require(t, 0)
	views, err := d.TaskCompletionViewsForDataset(scopedDatasetA, []string{task.ID})
	if err != nil || views[task.ID].Phase == string(transfer.CompletionCommitted) || d.GetTask(task.ID).Status == base.DownloadStatusDone {
		t.Fatal("late canceled result exposed API success", err)
	}
	_, record, err := d.readVerifiedTask(scopedDatasetA, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(task.Meta.Opts.Path, record.FinalName)); !os.IsNotExist(err) {
		t.Fatal("canceled old hash published a final", err)
	}
	s4g12PauseAndClose(t, d, task.ID)
}
