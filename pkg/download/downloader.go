package download

import (
	"context"
	"errors"
	"fmt"
	"math"
	gohttp "net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/logger"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/protocol/media"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/util"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"
)

const (
	// task info bucket
	bucketTask = "task"
	// task download data bucket
	bucketSave = "save"
	// protocol-level shared client state bucket
	bucketProtocolState = "protocol_state"
	// downloader config bucket
	bucketConfig = "config"
	// downloader extension bucket
	bucketExtension = "extension"
	// downloader extension storage bucket
	bucketExtensionStorage = "extension_storage"
)

var (
	loggerSetupOnce        sync.Once
	ErrTaskNotFound        = errors.New("task not found")
	ErrUnSupportedProtocol = errors.New("unsupported protocol")
)

type Listener func(event *Event)

// ExtractStatus represents the current status of archive extraction
type ExtractStatus string

const (
	// ExtractStatusNone indicates extraction has not started
	ExtractStatusNone ExtractStatus = ""
	// ExtractStatusQueued indicates extraction is waiting in the queue
	ExtractStatusQueued ExtractStatus = "queued"
	// ExtractStatusWaitingParts indicates waiting for other multi-part archive parts to complete
	ExtractStatusWaitingParts ExtractStatus = "waitingParts"
	// ExtractStatusExtracting indicates extraction is in progress
	ExtractStatusExtracting ExtractStatus = "extracting"
	// ExtractStatusDone indicates extraction completed successfully
	ExtractStatusDone ExtractStatus = "done"
	// ExtractStatusError indicates extraction failed
	ExtractStatusError ExtractStatus = "error"
)

type Progress struct {
	// Total download time(ns)
	Used int64 `json:"used"`
	// Download speed(bytes/s)
	Speed int64 `json:"speed"`
	// Downloaded size(bytes)
	Downloaded int64 `json:"downloaded"`
	// MediaPhase keeps merging distinct from a completed media task.
	MediaPhase string `json:"mediaPhase,omitempty"`
	// Uploaded speed(bytes/s)
	UploadSpeed int64 `json:"uploadSpeed"`
	// Uploaded size(bytes)
	Uploaded int64 `json:"uploaded"`
	// ExtractStatus indicates the current status of archive extraction
	ExtractStatus ExtractStatus `json:"extractStatus"`
	// ExtractProgress is the percentage of extraction completed (0-100)
	ExtractProgress int `json:"extractProgress"`
	// MultiPartBaseName is set for multi-part archives to group related parts
	MultiPartBaseName string `json:"multiPartBaseName,omitempty"`
	// MultiPartNumber is the part number for multi-part archives (1-indexed)
	MultiPartNumber int `json:"multiPartNumber,omitempty"`
	// MultiPartIsFirst indicates if this is the first part of a multi-part archive
	MultiPartIsFirst bool `json:"multiPartIsFirst,omitempty"`
}

type Downloader struct {
	pathOutputs       sync.Map
	mcpConfigMu       sync.Mutex
	mcpConfigured     *mcpWriteHolder
	mcpConfiguring    bool
	mcpSetupStarted   bool
	configSnapshotMu  sync.RWMutex
	operationMu       sync.Mutex
	handoffMu         sync.Mutex
	startupTaskPolicy StartupTaskPolicy
	Logger            *logger.Logger
	ExtensionLogger   *logger.Logger

	cfg          *DownloaderConfig
	fetcherCache map[string]fetcher.Fetcher
	storage      Storage
	tasks        []*Task
	// Guarded by lock; changed only while also holding queueMu, under which
	// fetchers read it.
	waitTasks    []*Task
	watchedTasks sync.Map
	// Serializes failed-run persistence with deletion, without holding task
	// lifecycle locks while looking up membership in the downloader's task list.
	errorPersistenceLocks sync.Map // *Task -> *sync.Mutex
	listenerLock          sync.RWMutex
	listener              Listener
	verifiedDone          *verifiedHTTPDoneBroker
	verifiedCompleter     *transfer.VerifiedCompleter

	lock               *sync.Mutex
	fetcherMapLock     *sync.RWMutex
	checkDuplicateLock *sync.Mutex
	closed             atomic.Bool

	// claimedExtractions tracks which multi-part archives have been claimed for extraction
	// Key: fullBaseName (e.g., "/path/archive.7z"), Value: taskID that claimed it
	claimedExtractions sync.Map

	extensionLock sync.RWMutex
	extensions    []*Extension

	// Shared by every fetcher: reusable connections and per-source budgets.
	transports *transfer.TransportPool
	budget     *transfer.Budget
	// Fetchers read the wait queue while the downloader may hold lock and
	// wait for them to stop (a synchronous pause), so they take only queueMu.
	// It also guards queueSignal, which is closed and replaced when a task
	// joins the queue or a deferred task may start again, so tasks idling in
	// a source cooldown check again whether to give their running slot up.
	queueMu     sync.Mutex
	queueSignal chan struct{}
}

func NewDownloader(cfg *DownloaderConfig) *Downloader {
	if cfg == nil {
		cfg = &DownloaderConfig{}
	}
	cfg.Init()

	d := &Downloader{
		startupTaskPolicy: cfg.StartupTaskPolicy,
		cfg:               cfg,
		fetcherCache:      make(map[string]fetcher.Fetcher),
		waitTasks:         make([]*Task, 0),
		storage:           cfg.Storage,

		lock:               &sync.Mutex{},
		fetcherMapLock:     &sync.RWMutex{},
		checkDuplicateLock: &sync.Mutex{},

		extensions:   make([]*Extension, 0),
		transports:   transfer.NewTransportPool(),
		budget:       transfer.NewBudget(),
		queueSignal:  make(chan struct{}),
		verifiedDone: newVerifiedHTTPDoneBroker(),
	}
	d.verifiedCompleter, _ = transfer.NewVerifiedCompleter(2)

	loggerSetupOnce.Do(func() { zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack })
	d.Logger = logger.NewLogger(cfg.ProductionMode, filepath.Join(cfg.StorageDir, "logs", "core.log"))
	d.ExtensionLogger = logger.NewLogger(cfg.ProductionMode, filepath.Join(cfg.StorageDir, "logs", "extension.log"))
	if cfg.ProductionMode {
		logPanic(filepath.Join(cfg.StorageDir, "logs"))
	}
	return d
}

func (d *Downloader) Setup() error {
	if err := d.beginMCPSetup(); err != nil {
		return err
	}
	if !d.startupTaskPolicy.Valid() {
		return ErrStartupTaskPolicy
	}
	// setup storage
	if err := d.storage.Setup([]string{bucketTask, bucketSave, bucketProtocolState, bucketConfig, bucketExtension, bucketExtensionStorage, bucketTaskOperations, bucketOperationTasks, bucketTaskRecovery, bucketRecoveryRequired, bucketTaskCompletion, bucketCompletionRequired}); err != nil {
		return err
	}
	// Core's public NewDownloader+Setup path must establish the same domain
	// before restoring or enrolling tasks as the REST startup path. Unknown
	// Storage implementations keep their original managed recovery capability;
	// they are not silently asserted to provide a trusted same-view domain.
	if supportsSharedRecoveryDomain(d.storage) {
		if err := InitializeDatasetIdentity(d.storage); err != nil {
			return err
		}
	}
	if err := d.checkMCPSetupDataset(); err != nil {
		return err
	}
	// load config from storage
	var cfg base.DownloaderStoreConfig
	exist, err := d.storage.Get(bucketConfig, "config", &cfg)
	if err != nil {
		return err
	}
	if exist {
		d.cfg.DownloaderStoreConfig = &cfg
	} else {
		d.cfg.DownloaderStoreConfig = &base.DownloaderStoreConfig{
			FirstLoad: true,
		}
	}
	// init default config
	d.cfg.DownloaderStoreConfig.Init()
	// init protocol config, if not exist, use default config
	for _, fm := range d.cfg.FetchManagers {
		protocol := fm.Name()
		if _, ok := d.cfg.DownloaderStoreConfig.ProtocolConfig[protocol]; !ok {
			d.cfg.DownloaderStoreConfig.ProtocolConfig[protocol] = fm.DefaultConfig()
		}
		if sfm, ok := fm.(fetcher.StatefulFetcherManager); ok {
			sfm.SetStateStore(&protocolStateStore{
				storage:  d.storage,
				protocol: protocol,
			})
		}
	}

	// load tasks from storage
	var tasks []*Task
	if err = d.storage.List(bucketTask, &tasks); err != nil {
		return err
	}
	if tasks == nil {
		tasks = make([]*Task, 0)
	} else {
		for i := len(tasks) - 1; i >= 0; i-- {
			task := tasks[i]
			// Remove broken tasks
			if task.Meta == nil {
				tasks = append(tasks[:i], tasks[i+1:]...)
				continue
			}
			d.assignFetcherManager(task)
			initTask(task)
			task.creationBlocked = d.checkCreationAssociation(task) != nil
			task.recoveryBlocked = d.checkTaskRecoveryState(task) != nil
			if task.getStatus() != base.DownloadStatusDone && task.getStatus() != base.DownloadStatusError {
				task.updateStatus(base.DownloadStatusPause)
			}
			if !task.recoveryBlocked {
				if until, e := d.taskSharedUntil(task); e == nil {
					task.dataLock.Lock()
					task.notBefore = until
					task.dataLock.Unlock()
				}
			}

		}
	}
	for _, task := range tasks {
		if task.PathFreeze == nil || task.Protocol != "ed2k" {
			continue
		}
		if manager, ok := task.fetcherManager.(interface {
			RegisterPathFreeze(string, *base.Request) error
		}); ok {
			if e := manager.RegisterPathFreeze(task.ID, task.Meta.Req); e != nil {
				return e
			}
		} else {
			return ErrPathFreezeHeld
		}
	}
	// Sort before publishing the task list to background readers.
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})
	d.lock.Lock()
	d.tasks = tasks
	d.lock.Unlock()

	// load extensions from storage
	var extensions []*Extension
	if err = d.storage.List(bucketExtension, &extensions); err != nil {
		return err
	}
	if extensions == nil {
		extensions = make([]*Extension, 0)
	}
	d.extensionLock.Lock()
	d.extensions = extensions
	d.extensionLock.Unlock()

	// Auto-cleanup non-existing tasks on startup
	if d.startupTaskPolicy != StartupTaskPolicyImportedReview {
		d.cleanupNonExistingTasks()
	}

	// Restore uploads only from this startup's task snapshot.
	uploadTasks := d.taskList()
	go func() {
		for _, task := range uploadTasks {
			if d.startupTaskPolicy == StartupTaskPolicyImportedReview {
				continue
			}
			if task.creationHeld() || task.handoffHeld() || task.recoveryHeld() {
				continue
			}
			task.lock.Lock()
			task.statusLock.Lock()
			if task.getStatus() == base.DownloadStatusDone && task.Uploading {
				if err := d.restoreTask(task); err != nil {
					d.Logger.Error().Stack().Err(err).Msgf("task upload restore fetcher failed, task id: %s", task.ID)
				}
				if task.PathFreeze != nil {
					task.statusLock.Unlock()
					task.lock.Unlock()
					if e := requireTaskPathFreeze(task); e == nil {
						if e = d.rebindPathUpload(task); e == nil {
							go d.watch(task)
						}
					}
					continue
				}
				if uploader, ok := task.fetcher.(fetcher.Uploader); ok {
					if err := uploader.Upload(); err != nil {
						d.Logger.Error().Stack().Err(err).Msgf("task upload failed, task id: %s", task.ID)
					}
				}
			}
			task.statusLock.Unlock()
			task.lock.Unlock()
		}
	}()

	// calculate download speed every tick
	go func() {
		for !d.closed.Load() {
			if tasks := d.taskList(); len(tasks) > 0 {
				for _, task := range tasks {
					notifyProgress := false
					func() {
						if d.getTask(task.ID) == nil || !task.lock.TryLock() {
							return
						}
						defer task.lock.Unlock()
						task.statusLock.Lock()
						defer task.statusLock.Unlock()
						if task.verificationOnly.Load() || (task.getStatus() != base.DownloadStatusRunning && !task.Uploading) {
							return
						}
						// check if task is deleted
						if task.fetcher == nil {
							return
						}

						var progressLease *transfer.RecoveryLease
						if task.Recovery != nil {
							owner := task.recoveryOwner.Load()
							if owner == nil {
								return
							}
							var err error
							progressLease, err = owner.currentLease(context.Background())
							if err != nil || progressLease.Token.Generation != task.runGeneration {
								return
							}
						}
						current := task.fetcher.Progress().TotalDownloaded()
						var observed httpProtocol.PublishedReadStats
						if task.Protocol == "http" {
							if publisher, ok := task.fetcher.(interface {
								PublishedReadSnapshot() httpProtocol.PublishedReadStats
							}); ok {
								observed = publisher.PublishedReadSnapshot()
							}
						}
						if progressLease != nil && progressLease.Check() != nil {
							return
						}
						task.dataLock.Lock()
						if task.Protocol == "http" {
							value := MCPReadPublishedStats{}
							valid := observed.Observed && observed.Context != nil && observed.Context.Err() == nil
							if progressLease != nil {
								valid = valid && observed.Managed && observed.Token == progressLease.Token && progressLease.Check() == nil
							}
							if valid {
								value = MCPReadPublishedStats{ActiveStreamsKnown: true, ActiveStreams: observed.ActiveStreams, TargetStreamsKnown: observed.TargetStreamsKnown, TargetStreams: observed.TargetStreams}
							}
							_ = task.publishReadStatsLocked(value)
						}
						tick := float64(d.cfg.RefreshInterval) / 1000
						downloadDataChanged := false
						if task.Status == base.DownloadStatusRunning {
							downloadDataChanged = current != task.Progress.Downloaded
							task.Progress.Used = task.timer.Used()
							task.Progress.Speed = task.updateSpeed(current-task.Progress.Downloaded, tick)
							task.Progress.Downloaded = current
							if mediaFetcher, ok := task.fetcher.(interface{ MediaPhase() string }); ok {
								task.Progress.MediaPhase = mediaFetcher.MediaPhase()
							}
						}

						uploadDataChanged := false
						if task.Uploading {
							uploader := task.fetcher.(fetcher.Uploader)
							currentUploaded := uploader.UploadedBytes()
							uploadDataChanged = currentUploaded != task.Progress.Uploaded
							task.Progress.UploadSpeed = task.updateUploadSpeed(currentUploaded-task.Progress.Uploaded, tick)
							task.Progress.Uploaded = currentUploaded
						}
						task.dataLock.Unlock()
						notifyProgress = true

						// store fetcher progress when download/upload data changed
						if !downloadDataChanged && !uploadDataChanged {
							return
						}
						if task.Protocol == "netdisk" {
							d.saveActiveTask(task)
						} else {
							d.saveTask(task)
						}
					}()
					// A listener may synchronously Delete while a completion Fence
					// holds operationMu and waits for this Task. Release both Task
					// locks (and finish persistence) before calling external code.
					if notifyProgress {
						d.emit(EventKeyProgress, task)
					}
				}
			}
			time.Sleep(time.Millisecond * time.Duration(d.cfg.RefreshInterval))
		}
	}()
	return nil
}

// cleanupNonExistingTasks checks for tasks whose files are missing on disk
// and removes them if the AutoCleanMissingFiles config is enabled.
func (d *Downloader) cleanupNonExistingTasks() {
	cfg, err := d.GetConfig()
	if err != nil {
		return
	}

	// If the feature is disabled, do nothing
	if !cfg.AutoDeleteMissingFileTasks {
		return
	}

	var tasksToDelete []string

	for _, task := range d.taskList() {
		if task.creationHeld() || task.recoveryHeld() {
			continue
		}
		if task.Meta == nil || task.Meta.Res == nil {
			continue
		}
		// Frozen staged outputs have no final name until publication. Missing
		// finals cannot justify deleting an unfinished durable Task at startup.
		if task.PathFreeze != nil && task.getStatus() != base.DownloadStatusDone {
			continue
		}
		// Media has no final file until muxing completes; its resumable fragments
		// must not be mistaken for a deleted finished download.
		if (task.Protocol == "media" || task.Protocol == "netdisk") && task.getStatus() != base.DownloadStatusDone {
			continue
		}

		var targetPath string
		// Determine if it is a single file or a directory (multi-file torrent)
		if task.Meta.Res.Name != "" {
			targetPath = task.Meta.FolderPath()
		} else {
			targetPath = task.Meta.SingleFilepath()
		}

		// Skip if path is empty
		if targetPath == "" {
			continue
		}

		// Check if file/folder exists
		if _, err := os.Stat(targetPath); os.IsNotExist(err) {
			d.Logger.Info().Msgf("Auto-cleanup: task %s file not found at %s, removing from list", task.ID, targetPath)
			tasksToDelete = append(tasksToDelete, task.ID)
		}
	}

	if len(tasksToDelete) > 0 {
		d.Delete(&TaskFilter{IDs: tasksToDelete}, false)
	}
}

func (d *Downloader) parseFm(url string) (fetcher.FetcherManager, error) {
	for _, fm := range d.cfg.FetchManagers {
		for _, filter := range fm.Filters() {
			if filter.Match(url) {
				return fm, nil
			}
		}
	}
	return nil, ErrUnSupportedProtocol
}

func (d *Downloader) parseRequestFm(req *base.Request) (fetcher.FetcherManager, error) {
	if req == nil {
		return nil, fmt.Errorf("invalid request")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if req.Protocol != "" {
		for _, fm := range d.cfg.FetchManagers {
			if fm.Name() == req.Protocol {
				return fm, nil
			}
		}
		return nil, ErrUnSupportedProtocol
	}
	return d.parseFm(req.URL)
}

// InspectMedia uses the same configuration and proxy policy as media tasks.
func (d *Downloader) InspectMedia(ctx context.Context, req *base.Request) (*media.Inspection, error) {
	if req == nil {
		return nil, fmt.Errorf("invalid request")
	}
	ctl := d.protocolController("media")
	return media.Inspect(ctx, req, ctl)
}

func (d *Downloader) setupFetcher(fm fetcher.FetcherManager, fetcher fetcher.Fetcher) {
	fetcher.Setup(d.protocolController(fm.Name()))
}

func (d *Downloader) protocolController(protocol string) *controller.Controller {
	ctl := controller.NewController()
	ctl.GetConfig = func(v any) {
		d.getProtocolConfig(protocol, v)
	}
	// Get proxy config, task request proxy config has higher priority, then use global proxy config
	ctl.GetProxy = func(requestProxy *base.RequestProxy) func(*gohttp.Request) (*url.URL, error) {
		if requestProxy == nil {
			return d.cfg.Proxy.ToHandler()
		}
		switch requestProxy.Mode {
		case base.RequestProxyModeNone:
			return nil
		case base.RequestProxyModeCustom:
			return requestProxy.ToHandler()
		default:
			return d.cfg.Proxy.ToHandler()
		}
	}
	// The same policy as GetProxy, with an identity so pooled connections are
	// only shared by requests that would take the same proxy path.
	ctl.GetProxyPolicy = func(requestProxy *base.RequestProxy) (func(*gohttp.Request) (*url.URL, error), string) {
		handler := ctl.GetProxy(requestProxy)
		if handler == nil {
			return nil, "direct"
		}
		if id := requestProxy.PolicyID(); id != "" {
			return handler, id
		}
		return handler, d.cfg.Proxy.PolicyID()
	}
	ctl.GetRecoveryProxySnapshot = func(req *base.RequestProxy) (func(*gohttp.Request) (*url.URL, error), string, bool) {
		d.configSnapshotMu.RLock()
		var snapshot *base.DownloaderProxyConfig
		if d.cfg.Proxy != nil {
			copy := *d.cfg.Proxy
			snapshot = &copy
		}
		d.configSnapshotMu.RUnlock()
		return base.CaptureProxySnapshot(req, snapshot)
	}
	ctl.Transports = d.transports
	ctl.Budget = d.budget
	ctl.QueueWaiting = d.queueWaiting
	ctl.QueueChanged = d.queueChanged
	return ctl
}

// queueChanged returns the channel closed on the next change of the wait
// queue. Callers take it before checking queueWaiting.
func (d *Downloader) queueChanged() <-chan struct{} {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	return d.queueSignal
}

// enqueueLocked puts a task in the wait queue and tells tasks idling in a
// source cooldown. Caller holds d.lock.
func (d *Downloader) enqueueLocked(task *Task) {
	if task.handoffHeld() {
		return
	}
	d.queueMu.Lock()
	d.waitTasks = append(d.waitTasks, task)
	d.queueMu.Unlock()
	d.signalQueue()
}

// dequeueLocked removes waitTasks[i]. Caller holds d.lock.
func (d *Downloader) dequeueLocked(i int) {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	d.waitTasks = append(d.waitTasks[:i], d.waitTasks[i+1:]...)
}

// clearQueueLocked empties the wait queue. Caller holds d.lock.
func (d *Downloader) clearQueueLocked() {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	d.waitTasks = make([]*Task, 0)
}

// signalQueue wakes tasks idling in a cooldown: a task joined the queue or a
// queued task may have become able to run.
func (d *Downloader) signalQueue() {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	close(d.queueSignal)
	d.queueSignal = make(chan struct{})
}

// queueWaiting reports whether a queued task could start now if a slot freed.
func (d *Downloader) queueWaiting() bool {
	d.queueMu.Lock()
	defer d.queueMu.Unlock()
	now := time.Now()
	for _, t := range d.waitTasks {
		if !t.handoffHeld() && t.getStatus() == base.DownloadStatusWait && !t.notBeforeTime().After(now) {
			return true
		}
	}
	return false
}

// Serialize late progress/start/pause saves with deletion. A removed task
// must never be recreated when asynchronous startup or polling finishes.
func (d *Downloader) saveActiveTask(task *Task) error {
	lock := d.errorPersistenceLock(task)
	lock.Lock()
	defer lock.Unlock()
	if d.getTask(task.ID) != task {
		return nil
	}
	if task.Completion != nil {
		return d.saveTask(task)
	}
	if task.fetcherManager == nil || task.fetcher == nil {
		return d.storage.Put(bucketTask, task.ID, task.clone())
	}
	return d.saveTask(task)
}

func (d *Downloader) saveTask(task *Task) error {
	// A joined pause/shutdown must retain its checkpoint after execution
	// authority has been revoked. This is declaration/association validation,
	// not permission for a new request or writable output.
	if err := d.checkTaskRecoveryState(task); err != nil {
		return err
	}
	// Sealed verification deliberately has no protocol instance or new wire
	// checkpoint. Keep its existing checkpoint and atomically guard the Task.
	if task.Completion != nil && task.fetcher == nil {
		owner := task.recoveryOwner.Load()
		if owner == nil {
			return transfer.ErrRecoveryHeld
		}
		return owner.persistSnapshot(task.clone(), nil, false)
	}
	data, err := task.fetcherManager.Store(task.fetcher)
	if err != nil {
		d.Logger.Error().Stack().Err(err).Msgf("serialize fetcher failed: %s", task.ID)
		return err
	}
	if task.Recovery != nil {
		owner := task.recoveryOwner.Load()
		if owner == nil {
			return transfer.ErrRecoveryHeld
		}
		return owner.persistSnapshot(task.clone(), data, true)
	}
	if task.PathFreeze != nil {
		identity, e := d.DatasetIdentity()
		if e != nil {
			return e
		}
		snapshot := task.clone()
		return d.storage.(AtomicStorage).UpdateAtomic(func(tx StorageTransaction) error {
			if e := requireDataset(tx, identity.DatasetID); e != nil {
				return e
			}
			current := new(Task)
			found, e := tx.Get(bucketTask, task.ID, current)
			if e != nil {
				return e
			}
			if !found {
				return ErrPathFreezeHeld
			}
			if e = guardPathSnapshot(current, snapshot); e != nil {
				return e
			}
			// Native ED2K has no wire checkpoint; its resume state belongs to
			// the existing protocol store. A nil Store preserves CURRENT here.
			if data != nil {
				e = tx.Put(bucketSave, task.ID, data)
			}
			if e != nil {
				return e
			}
			return tx.Put(bucketTask, task.ID, snapshot)
		})
	}
	if data != nil {
		if err := d.storage.Put(bucketSave, task.ID, data); err != nil {
			d.Logger.Error().Stack().Err(err).Msgf("persist fetcher failed: %s", task.ID)
			return err
		}
	} else {
		if err := d.storage.Delete(bucketSave, task.ID); err != nil {
			d.Logger.Error().Stack().Err(err).Msgf("clear fetcher state failed: %s", task.ID)
			return err
		}
	}
	if err := d.storage.Put(bucketTask, task.ID, task.clone()); err != nil {
		d.Logger.Error().Stack().Err(err).Msgf("persist task failed: %s", task.ID)
		return err
	}
	return nil
}

func (d *Downloader) Resolve(req *base.Request, opts *base.Options) (rr *ResolveResult, err error) {
	return d.ResolveContext(context.Background(), req, opts)
}

// ResolveContext resolves metadata without tying the resulting task to ctx.
func (d *Downloader) ResolveContext(ctx context.Context, req *base.Request, opts *base.Options) (rr *ResolveResult, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if req == nil {
		return nil, fmt.Errorf("invalid request")
	}
	rrId, err := gonanoid.New()
	if err != nil {
		return
	}

	if req.Protocol == "" {
		res, resolveErr := d.triggerOnResolve(req)
		if resolveErr != nil {
			err = resolveErr
			return
		}
		if res != nil && len(res.Files) > 0 {
			rr = &ResolveResult{Res: res}
			return
		}
	}

	resolvedFetcher, err := d.buildFetcher(req)
	if err != nil {
		return
	}
	resolver, supportsContext := resolvedFetcher.(fetcher.ContextResolver)
	defer func() {
		// Legacy resolvers may only inspect a hash without owning the matching
		// transfer. Closing those (notably ED2K) can remove an existing task.
		if err != nil && supportsContext {
			_ = resolvedFetcher.Close()
		}
	}()
	initOpt, err := d.initOptions(opts)
	if err != nil {
		return
	}
	if supportsContext {
		err = resolver.ResolveContext(ctx, req, initOpt)
	} else {
		err = resolvedFetcher.Resolve(req, initOpt)
	}
	if err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	d.fetcherMapLock.Lock()
	d.fetcherCache[rrId] = resolvedFetcher
	d.fetcherMapLock.Unlock()
	rr = &ResolveResult{
		ID:  rrId,
		Res: resolvedFetcher.Meta().Res,
	}
	return
}

func (d *Downloader) notifyRunning() {
	go func() {
		d.lock.Lock()
		defer d.lock.Unlock()

		remainRunningCount := d.remainRunningCount()
		if remainRunningCount == 0 {
			return
		}
		now := time.Now()
		for i := 0; i < len(d.waitTasks); i++ {
			wt := d.waitTasks[i]
			if wt.handoffHeld() || wt.creationHeld() || wt.getStatus() != base.DownloadStatusWait {
				d.dequeueLocked(i)
				i--
				continue
			}
			// A rate-limited task keeps its place but lets later tasks run
			// until its source cooldown ends.
			if wt.notBeforeTime().After(now) {
				continue
			}
			d.dequeueLocked(i)
			// A selective pause can leave stale entries in this queue. Recheck
			// under the status lock so scheduling never resumes a paused task.
			if err := d.startTask(wt, true); err != nil {
				if deferred, ok := transfer.AsDeferred(err); ok {
					wt.dataLock.Lock()
					wt.notBefore = deferred.Until
					wt.dataLock.Unlock()
					d.enqueueLocked(wt)
				}
			}
			break
		}
	}()
}

func (d *Downloader) remainRunningCount() int {
	runningCount := 0
	for _, t := range d.tasks {
		if t.getStatus() == base.DownloadStatusRunning && !t.verificationOnly.Load() {
			runningCount++
		}
	}
	return d.cfg.MaxRunning - runningCount
}

func (d *Downloader) CreateDirect(req *base.Request, opts *base.Options) (taskId string, err error) {
	var fetcher fetcher.Fetcher
	fetcher, err = d.buildFetcher(req)
	if err != nil {
		return
	}
	fetcher.Meta().Req = req
	initOpt, err := d.initOptions(opts)
	if err != nil {
		return
	}
	return d.doCreate(fetcher, initOpt)
}

func (d *Downloader) CreateDirectBatch(req *base.CreateTaskBatch) (taskId []string, err error) {
	taskIds := make([]string, 0)
	for _, ir := range req.Reqs {
		opts := ir.Opts
		if opts == nil {
			opts = req.Opts
		}
		taskId, err := d.CreateDirect(ir.Req, opts.Clone())
		if err != nil {
			return nil, err
		}
		taskIds = append(taskIds, taskId)
	}
	return taskIds, nil
}

func (d *Downloader) Create(rrId string) (taskId string, err error) {
	d.fetcherMapLock.RLock()
	fetcher, ok := d.fetcherCache[rrId]
	d.fetcherMapLock.RUnlock()
	if !ok {
		return "", errors.New("invalid resource id")
	}
	defer func() {
		d.fetcherMapLock.Lock()
		delete(d.fetcherCache, rrId)
		d.fetcherMapLock.Unlock()
	}()
	return d.doCreate(fetcher, nil)
}

// Patch modifies task-specific data based on the protocol.
// For HTTP protocol, it can modify Request info.
// For BT protocol, it can modify SelectFiles.
func (d *Downloader) Patch(id string, req *base.Request, opts *base.Options) error {
	task := d.getTask(id)
	if task == nil {
		return ErrTaskNotFound
	}

	err := func() error {
		// The operation lock waits for an in-flight resolve/pause; statusLock makes
		// the eligibility check atomic with Start/Continue's running transition.
		task.lock.Lock()
		defer task.lock.Unlock()
		task.statusLock.Lock()
		defer task.statusLock.Unlock()
		if err := d.checkTaskRecovery(task); err != nil {
			return err
		}
		if task.creationHeld() {
			return ErrOperationConflict
		}
		if task.handoffHeld() {
			return ErrHandoffHeld
		}
		status := task.getStatus()
		if (task.Protocol == "media" || task.Protocol == "netdisk") && status != base.DownloadStatusPause && status != base.DownloadStatusError {
			return errors.New("media task must be paused or failed before patching")
		}
		err := func() error {
			task.dataLock.Lock()
			defer task.dataLock.Unlock()
			if task.PathFreeze != nil {
				// R2 freezes the caller intent. Reject a changed patch before the
				// protocol can mutate live headers or an already frozen target.
				if req != nil && !pathReqEqual(req, task.Meta.Req) || opts != nil && mcpWriteHash(opts) != mcpWriteHash(task.Meta.Opts) {
					return ErrPathFreezeHeld
				}
			}
			if task.fetcher == nil {
				if err := d.restoreFetcher(task); err != nil {
					return err
				}
			}
			if err := task.fetcher.Patch(req, opts); err != nil {
				return err
			}
			invalidateTaskRecovery(task)
			task.clearReadPublishedLocked()
			if task.PathFreeze == nil {
				task.Meta = task.fetcher.Meta()
			}
			if task.Protocol == "media" || task.Protocol == "netdisk" {
				task.Meta = util.DeepClone(task.Meta)
			}
			return nil
		}()
		if err != nil {
			return err
		}

		// Save task to storage
		if err := d.saveTask(task); err != nil {
			return err
		}

		return nil
	}()
	if err != nil {
		return err
	}

	// Emit progress event to notify listeners
	d.emit(EventKeyProgress, task)

	return nil
}

func (d *Downloader) Pause(filter *TaskFilter) (err error) {
	if filter == nil || filter.IsEmpty() {
		return d.pauseAll()
	}

	filter.NotStatuses = []base.Status{base.DownloadStatusPause, base.DownloadStatusError, base.DownloadStatusDone}
	pauseTasks := d.getTasksByFilter(filter)
	if len(pauseTasks) == 0 {
		return ErrTaskNotFound
	}

	for _, task := range pauseTasks {
		if err = d.doPause(task); err != nil {
			return
		}
	}
	d.notifyRunning()

	return
}

func (d *Downloader) pauseAll() (err error) {
	func() {
		d.lock.Lock()
		defer d.lock.Unlock()

		// Clear wait tasks
		d.clearQueueLocked()
	}()

	for _, task := range d.taskList() {
		if err = d.doPause(task); err != nil {
			return
		}
	}

	return
}

// Continue specific tasks, if continue tasks will exceed maxRunning, it needs pause some running tasks before that
func (d *Downloader) Continue(filter *TaskFilter) (err error) {
	if err := d.checkStartupContinue(filter); err != nil {
		return err
	}
	if err := d.checkRecoverySelection(filter); err != nil {
		return err
	}
	if filter == nil || filter.IsEmpty() {
		return d.continueAll()
	}

	filter.NotStatuses = []base.Status{base.DownloadStatusRunning, base.DownloadStatusDone}
	selectedTasks := d.getTasksByFilter(filter)
	continueTasks := creationEligible(handoffEligible(selectedTasks))
	if len(continueTasks) == 0 {
		if len(selectedTasks) > 0 {
			if selectedTasks[0].creationHeld() {
				return ErrOperationConflict
			}
			return ErrHandoffHeld
		}
		return ErrTaskNotFound
	}

	verificationTasks, networkCount, selectionErr := d.verificationOnlySelection(continueTasks)
	if selectionErr != nil {
		return selectionErr
	}
	pauseTasks := make([]*Task, 0)
	func() {
		d.lock.Lock()
		defer d.lock.Unlock()

		remainRunningCount := d.remainRunningCount()
		needRunningCount := int(math.Min(float64(d.cfg.MaxRunning), float64(networkCount)))
		needPauseCount := 0
		if networkCount > 0 {
			needPauseCount = needRunningCount - remainRunningCount
		}
		if needPauseCount > 0 {
			for _, task := range d.tasks {
				if task.getStatus() == base.DownloadStatusRunning && !task.verificationOnly.Load() {
					pauseTasks = append(pauseTasks, task)
				}
				if len(pauseTasks) == needPauseCount {
					break
				}
			}
		}
	}()

	// Startup and progress persistence can hold task.lock while checking task
	// membership under d.lock. Never hold d.lock while joining their pause.
	for _, task := range pauseTasks {
		if err = d.doPause(task); err != nil {
			return
		}
		d.lock.Lock()
		if d.getTaskLocked(task.ID) == task && task.getStatus() == base.DownloadStatusPause {
			task.updateStatus(base.DownloadStatusWait)
			d.enqueueLocked(task)
		}
		d.lock.Unlock()
	}

	realContinueTasks := make([]*Task, 0)
	func() {
		d.lock.Lock()
		defer d.lock.Unlock()
		// A task may have been deleted or another slot claimed while Pause ran.
		remainRunningCount := d.remainRunningCount()
		networkAdmitted := 0
		for _, task := range continueTasks {
			if d.getTaskLocked(task.ID) != task || task.getStatus() == base.DownloadStatusRunning || task.getStatus() == base.DownloadStatusDone {
				continue
			}
			if verificationTasks[task] || networkAdmitted < remainRunningCount {
				realContinueTasks = append(realContinueTasks, task)
				if !verificationTasks[task] {
					networkAdmitted++
				}
			} else {
				task.updateStatus(base.DownloadStatusWait)
				d.enqueueLocked(task)
			}
		}
	}()

	for _, task := range realContinueTasks {
		if err = d.doStart(task); err != nil {
			if deferred, ok := transfer.AsDeferred(err); ok {
				d.queueSharedUntil(task, deferred.Until)
				err = nil
				continue
			}
			return
		}
	}

	return
}

// continueAll continue all tasks but does not affect tasks already running
func (d *Downloader) continueAll() (err error) {
	if err := d.checkStartupContinue(nil); err != nil {
		return err
	}
	continuedTasks := make([]*Task, 0)
	verificationTasks, _, selectionErr := d.verificationOnlySelection(d.taskList())
	if selectionErr != nil {
		return selectionErr
	}

	func() {
		d.lock.Lock()
		defer d.lock.Unlock()
		// calculate how many tasks can be continued, can't exceed maxRunning
		remainCount := d.remainRunningCount()
		networkAdmitted := 0
		for _, task := range d.tasks {
			if !task.handoffHeld() && !task.creationHeld() && task.getStatus() != base.DownloadStatusRunning && task.getStatus() != base.DownloadStatusDone {
				if verificationTasks[task] || networkAdmitted < remainCount {
					continuedTasks = append(continuedTasks, task)
					if !verificationTasks[task] {
						networkAdmitted++
					}
				} else {
					task.updateStatus(base.DownloadStatusWait)
					d.enqueueLocked(task)
				}
			}
		}
	}()

	for _, task := range continuedTasks {
		if err = d.doStart(task); err != nil {
			if deferred, ok := transfer.AsDeferred(err); ok {
				d.queueSharedUntil(task, deferred.Until)
				err = nil
				continue
			}
			return
		}
	}

	return
}

func (d *Downloader) ContinueBatch(filter *TaskFilter) (err error) {
	if err := d.checkStartupContinue(filter); err != nil {
		return err
	}
	if err := d.checkRecoverySelection(filter); err != nil {
		return err
	}
	if filter == nil || filter.IsEmpty() {
		return d.continueAll()
	}

	continueTasks := creationEligible(handoffEligible(d.getTasksByFilter(filter)))
	for _, task := range continueTasks {
		if err = d.doStart(task); err != nil {
			if deferred, ok := transfer.AsDeferred(err); ok {
				d.queueSharedUntil(task, deferred.Until)
				err = nil
				continue
			}
			return
		}
	}
	return
}

func (d *Downloader) Delete(filter *TaskFilter, force bool) (err error) {
	// Serialize removal and persistence with operation publication. Listeners
	// may reenter lifecycle APIs, so deliver events only after releasing this lock.
	d.operationMu.Lock()
	var deleted []*Task
	defer func() {
		d.operationMu.Unlock()
		for _, task := range deleted {
			d.emit(EventKeyDelete, task)
		}
	}()

	var selected []*Task
	all := filter == nil || filter.IsEmpty()
	if all {
		selected = d.taskList()
	} else {
		selected = d.getTasksByFilter(filter)
	}
	if force {
		if err := d.checkPathForceDelete(selected); err != nil {
			return err
		}
		if err := d.checkCompletionForceDelete(selected); err != nil {
			return err
		}
	}
	// Use exactly the preflight selection. A changing status filter or a new
	// task must not add an unexamined member after force admission.
	d.lock.Lock()
	for _, task := range selected {
		invalidateDeletedRun(task)
		for i, current := range d.tasks {
			if current == task {
				d.tasks = append(d.tasks[:i], d.tasks[i+1:]...)
				break
			}
		}
		for i, current := range d.waitTasks {
			if current == task {
				d.dequeueLocked(i)
				break
			}
		}
	}
	if all && len(d.tasks) == 0 {
		d.clearQueueLocked()
	}
	d.lock.Unlock()

	for _, task := range selected {
		invalidateTaskRecovery(task)
		d.operationMu.Unlock()
		deleteErr := d.doDelete(task, force)
		d.operationMu.Lock()
		if deleteErr != nil {
			return deleteErr
		}
		task.dataLock.Lock()
		task.clearReadPublishedLocked()
		task.dataLock.Unlock()
		deleted = append(deleted, task)
	}
	d.notifyRunning()
	return
}

func (d *Downloader) Stats(id string) (sr any, err error) {
	task := d.getTask(id)
	if task == nil {
		return sr, ErrTaskNotFound
	}
	if err := d.checkTaskRecovery(task); err != nil {
		return nil, err
	}
	if task.creationHeld() {
		return nil, ErrOperationConflict
	}
	task.statusLock.Lock()
	defer task.statusLock.Unlock()
	if task.fetcher == nil {
		if err = d.restoreFetcher(task); err != nil {
			return
		}
	}
	sr = task.fetcher.Stats()
	switch stats := sr.(type) {
	case *http.Stats:
		if stats.Timings == nil {
			stats.Timings = &http.StatsTimings{}
		}
		stats.Timings.QueueMs = task.queueTime().Milliseconds()
	case map[string]any:
		stats["queueMs"] = task.queueTime().Milliseconds()
	}
	return
}

func (d *Downloader) doDelete(task *Task, force bool) (err error) {
	err = func() error {
		persistenceLock := d.errorPersistenceLock(task)
		persistenceLock.Lock()
		defer persistenceLock.Unlock()

		if err := d.storage.Delete(bucketTask, task.ID); err != nil {
			return err
		}
		if err := d.storage.Delete(bucketSave, task.ID); err != nil {
			return err
		}

		if task.fetcher != nil {
			if err := d.quiesceTaskPath(task); err != nil {
				return err
			}
			if err := task.fetcher.Close(); err != nil {
				return err
			}
		}
		if task.PathFreeze != nil {
			if forget, ok := task.fetcher.(interface {
				ForgetPathFreeze(string, *base.Request) error
			}); ok {
				if err := forget.ForgetPathFreeze(task.ID, task.Meta.Req); err != nil {
					return err
				}
			}
			if err := d.releasePathOutput(task); err != nil {
				return err
			}
		}
		if force && task.Meta.Res != nil {
			if task.Meta.Res.Name != "" {
				if err := os.RemoveAll(task.Meta.FolderPath()); err != nil {
					return err
				}
			} else {
				if err := util.SafeRemove(task.Meta.SingleFilepath()); err != nil {
					return err
				}
			}
		}
		return nil
	}()
	d.errorPersistenceLocks.Delete(task)

	if err != nil {
		d.Logger.Error().Stack().Err(err).Msgf("delete task failed, task id: %s", task.ID)
		return
	}
	return
}

func (d *Downloader) closeNetdiskFetchers() error {
	var lastErr error
	for _, task := range d.taskList() {
		if task.Protocol != "netdisk" {
			continue
		}
		task.statusLock.Lock()
		f := task.fetcher
		task.statusLock.Unlock()
		if f != nil {
			if err := f.Close(); err != nil {
				lastErr = err
				d.Logger.Error().Err(err).Msgf("close netdisk fetcher failed, task id: %s", task.ID)
			}
		}
	}
	return lastErr
}

func (d *Downloader) Close() error {
	d.stopMCPWrites()
	d.handoffMu.Lock()
	d.operationMu.Lock()
	d.closed.Store(true)
	d.operationMu.Unlock()
	d.handoffMu.Unlock()
	d.verifiedDone.close()
	d.lock.Lock()
	for _, task := range d.tasks {
		cancelVerifiedSuccess(task)
		if task.PathFreeze != nil {
			invalidateDeletedRun(task)
		}
	}
	d.lock.Unlock()

	closeArr := []func() error{
		d.pauseAll,
		// Pause persists resumable state; Close then releases netdisk Wait
		// callers before its shared service and the storage backend are closed.
		d.closeNetdiskFetchers,
		d.closeMCPWrites,
	}
	for _, fm := range d.cfg.FetchManagers {
		closeArr = append(closeArr, fm.Close)
	}
	closeArr = append(closeArr, func() error {
		var result error
		d.pathOutputs.Range(func(key, value any) bool {
			entry := value.(*pathOutputRuntime)
			if err := entry.close(); err != nil {
				result = err
			} else {
				d.pathOutputs.Delete(key)
			}
			return true
		})
		return result
	})
	// Fetchers are paused; wake budget waiters and drop idle connections.
	if d.budget != nil {
		closeArr = append(closeArr, d.budget.Close)
	}
	if d.transports != nil {
		closeArr = append(closeArr, d.transports.Close)
	}
	closeArr = append(closeArr, d.storage.Close)
	// Make sure all resources are released, if had error, return the last error
	var lastErr error
	for i, close := range closeArr {
		if err := close(); err != nil {
			lastErr = err
			d.Logger.Error().Stack().Err(err).Msgf("downloader close failed, index: %d", i)
		}
	}
	return lastErr
}

func (d *Downloader) Clear() error {
	if !d.closed.Load() {
		if err := d.Close(); err != nil {
			return err
		}
	}
	d.lock.Lock()
	d.tasks = make([]*Task, 0)
	d.lock.Unlock()
	d.extensionLock.Lock()
	defer d.extensionLock.Unlock()
	d.extensions = make([]*Extension, 0)
	if err := d.storage.Clear(); err != nil {
		return err
	}
	return nil
}

type protocolStateStore struct {
	storage  Storage
	protocol string
}

func (s *protocolStateStore) Load(v any) (bool, error) {
	return s.storage.Get(bucketProtocolState, s.protocol, v)
}

func (s *protocolStateStore) Save(v any) error {
	if v == nil {
		return s.Delete()
	}
	return s.storage.Put(bucketProtocolState, s.protocol, v)
}

func (s *protocolStateStore) Delete() error {
	return s.storage.Delete(bucketProtocolState, s.protocol)
}

func (d *Downloader) Listener(fn Listener) {
	d.listenerLock.Lock()
	d.listener = fn
	d.listenerLock.Unlock()
}

func (d *Downloader) emit(eventKey EventKey, task *Task, errs ...error) {
	d.listenerLock.RLock()
	listener := d.listener
	d.listenerLock.RUnlock()
	if listener != nil {
		var err error
		if len(errs) > 0 {
			err = errs[0]
		}
		listener(&Event{
			Key:  eventKey,
			Task: task.clone(),
			Err:  err,
		})
	}
}

// GetTask returns a snapshot safe to read while the task is running.
func (d *Downloader) GetTask(id string) *Task {
	if task := d.getTask(id); task != nil {
		return task.clone()
	}
	return nil
}

func (d *Downloader) getTask(id string) *Task {
	d.lock.Lock()
	defer d.lock.Unlock()
	return d.getTaskLocked(id)
}

// getTaskLocked requires d.lock and preserves task identity across async work.
func (d *Downloader) getTaskLocked(id string) *Task {
	for _, task := range d.tasks {
		if task.ID == id {
			return task
		}
	}
	return nil
}

func (d *Downloader) taskList() []*Task {
	d.lock.Lock()
	defer d.lock.Unlock()
	return append([]*Task{}, d.tasks...)
}

func (d *Downloader) GetTasks() []*Task {
	return d.GetTasksByFilter(nil)
}

// GetTasksByFilter get tasks by filter, if filter is nil, return all tasks
// return tasks and if match all tasks
func (d *Downloader) GetTasksByFilter(filter *TaskFilter) []*Task {
	tasks := d.getTasksByFilter(filter)
	for i, task := range tasks {
		tasks[i] = task.clone()
	}
	return tasks
}

func (d *Downloader) getTasksByFilter(filter *TaskFilter) []*Task {
	d.lock.Lock()
	defer d.lock.Unlock()

	if filter == nil || filter.IsEmpty() {
		return append([]*Task{}, d.tasks...)
	}

	idMatch := func(task *Task) bool {
		if len(filter.IDs) == 0 {
			return true
		}
		for _, id := range filter.IDs {
			if task.ID == id {
				return true
			}
		}
		return false
	}
	statusMatch := func(task *Task) bool {
		if len(filter.Statuses) == 0 {
			return true
		}
		for _, status := range filter.Statuses {
			if task.getStatus() == status {
				return true
			}
		}
		return false
	}
	notStatusMatch := func(task *Task) bool {
		if len(filter.NotStatuses) == 0 {
			return true
		}
		for _, status := range filter.NotStatuses {
			if task.getStatus() == status {
				return false
			}
		}
		return true
	}

	tasks := make([]*Task, 0)
	for _, task := range d.tasks {
		if idMatch(task) && statusMatch(task) && notStatusMatch(task) {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func (d *Downloader) GetConfig() (*base.DownloaderStoreConfig, error) {
	d.configSnapshotMu.RLock()
	defer d.configSnapshotMu.RUnlock()
	return d.cfg.DownloaderStoreConfig, nil
}

func (d *Downloader) PutConfig(v *base.DownloaderStoreConfig) error {
	d.configSnapshotMu.Lock()
	d.cfg.DownloaderStoreConfig = v
	d.configSnapshotMu.Unlock()
	return d.storage.Put(bucketConfig, "config", v)
}

func (d *Downloader) getProtocolConfig(name string, v any) bool {
	cfg, err := d.GetConfig()
	if err != nil {
		return false
	}
	if cfg.ProtocolConfig == nil || cfg.ProtocolConfig[name] == nil {
		return false
	}
	if err := util.MapToStruct(cfg.ProtocolConfig[name], v); err != nil {
		d.Logger.Warn().Err(err).Msgf("get protocol config failed")
		return false
	}
	return true
}

// wait task done
func (d *Downloader) watch(task *Task) {
	task.statusLock.Lock()
	generation, f := task.runGeneration, task.fetcher
	task.statusLock.Unlock()
	var wait func() error
	if task.Protocol == "http" {
		if binder, ok := f.(interface {
			BindRunWait(context.Context) func() error
		}); ok {
			wait = binder.BindRunWait(context.Background())
		}
	}
	d.watchRun(task, f, generation, wait)
}

type taskRunWatch struct {
	generation uint64
	task       *Task
	bound      bool
	token      transfer.RecoveryToken
}

func (d *Downloader) watchRun(task *Task, f fetcher.Fetcher, generation uint64, wait func() error, leases ...*transfer.RecoveryLease) {
	var runLease *transfer.RecoveryLease
	if len(leases) > 0 {
		runLease = leases[0]
	}
	if runLease != nil && task.MCPWrite != nil {
		defer d.releaseMCPTaskLease(task, runLease.Token)
	}

	if d.closed.Load() || d.getTask(task.ID) != task {
		return
	}
	bound := wait != nil && (task.Protocol == "http" || parentRecoveryProtocol(task.Protocol) || task.PathFreeze != nil)
	registrationGeneration := generation
	if !bound {
		registrationGeneration = 0
	}
	registration := &taskRunWatch{generation: registrationGeneration, task: task, bound: bound}
	if bound && runLease != nil {
		registration.token = runLease.Token
	}
	for {
		old, loaded := d.watchedTasks.LoadOrStore(task.ID, registration)
		if !loaded {
			break
		}
		if current, ok := old.(*taskRunWatch); ok && current.generation == registrationGeneration && current.task == task && current.bound == bound && current.token == registration.token {
			return
		}
		if d.watchedTasks.CompareAndSwap(task.ID, old, registration) {
			break
		}
	}
	defer d.watchedTasks.CompareAndDelete(task.ID, registration)
	if bound && runLease != nil {
		defer func(token transfer.RecoveryToken) {
			if owner := task.recoveryOwner.Load(); owner != nil {
				owner.invalidateToken(token)
			}
		}(runLease.Token)
	}

	task.statusLock.Lock()
	task.dataLock.RLock()
	uploading := task.Uploading
	task.dataLock.RUnlock()
	status := task.getStatus()
	task.statusLock.Unlock()
	// wait task upload done
	if uploading {
		if uploader, ok := f.(fetcher.Uploader); ok {
			go func() {
				err := uploader.WaitUpload()
				if err != nil {
					d.Logger.Warn().Err(err).Msgf("task wait upload failed, task id: %s", task.ID)
				}

				// Check if the task is deleted
				if d.getTask(task.ID) != nil {
					task.statusLock.Lock()
					task.dataLock.Lock()
					task.Uploading = false
					task.dataLock.Unlock()
					task.statusLock.Unlock()
					d.storage.Put(bucketTask, task.ID, task.clone())
				}
			}()
		}
	}

	if status == base.DownloadStatusDone {
		return
	}

	if wait == nil {
		wait = f.Wait
	}
	err := wait()
	if d.closed.Load() || d.getTask(task.ID) != task {
		return
	}
	var expected []uint64
	if bound {
		if runLease != nil {
			owner := task.recoveryOwner.Load()
			if owner != nil && runLease.ActionVersion == recoverySharedVersion {
				var leaseErr error
				runLease, leaseErr = owner.parentResultLease(runLease)
				if leaseErr != nil {
					return
				}
			}
			if owner == nil || !owner.acceptsFailure(runLease.Token) || (err == nil && runLease.Check() != nil) {
				return
			}
		}
		task.statusLock.Lock()
		current := task.runGeneration == generation && task.getStatus() == base.DownloadStatusRunning
		task.statusLock.Unlock()
		if !current {
			return
		}
		expected = []uint64{generation}
	}
	coverageSeal := d.recordRunRecoveryCoverage(task, f, runLease)
	if err != nil {
		if task.PathFreeze != nil {
			if joinErr := d.quiesceTaskPath(task); joinErr != nil {
				err = errors.Join(err, joinErr)
			} else {
				_ = d.releasePathOutput(task)
			}
		}
		d.doOnError(task, err, expected...)
		if coverageSeal != nil && task.Completion == nil {
			if owner := task.recoveryOwner.Load(); owner != nil {
				if ctx, e := owner.coverageRunContext(runLease.Token); e == nil {
					_ = d.storeRunRecoveryCoverage(ctx, task, runLease, coverageSeal, true)
				}
			}
		}
		return
	}
	var committed bool
	var completionErr error
	if task.MCPWrite != nil {
		httpFetcher, ok := f.(*httpProtocol.Fetcher)
		if !ok || runLease == nil {
			completionErr = transfer.ErrRecoveryHeld
		} else {
			committed, completionErr = d.completeMCPHTTP(runLease.Context, task, httpFetcher, generation, runLease)
		}
	} else if task.PathFreeze != nil && task.Completion == nil {
		committed, completionErr = d.completeTaskPath(task, f, generation, runLease)
	} else if task.Completion != nil {
		committed, completionErr = d.completeVerifiedHTTP(task, f, generation, runLease)
	} else {
		committed, completionErr = d.commitTaskCompletion(task, f, expected...)
	}
	if completionErr != nil {
		d.doOnError(task, fmt.Errorf("persist task completion: %w", completionErr), expected...)
		if coverageSeal != nil && task.Completion == nil {
			if owner := task.recoveryOwner.Load(); owner != nil {
				if ctx, e := owner.coverageRunContext(runLease.Token); e == nil {
					_ = d.storeRunRecoveryCoverage(ctx, task, runLease, coverageSeal, true)
				}
			}
		}
		return
	}
	if !committed {
		return
	}
	if task.PathFreeze != nil && runLease != nil && runLease.Check() == nil && !d.closed.Load() {
		if success, ok := f.(interface{ PathFreezeSucceeded(context.Context) error }); ok {
			// Only this run's known published+Done winner reaches this hook.
			// Query/reopen/unknown ACK cannot reconstruct it.
			if e := success.PathFreezeSucceeded(runLease.Context); e != nil {
				d.Logger.Warn().Err(e).Msgf("post-completion cleanup failed, task id: %s", task.ID)
			}
		}
	}
	if task.PathFreeze != nil {
		if task.Protocol == "bt" && task.Uploading {
			_ = d.rebindPathUpload(task)
		} else {
			_ = d.releasePathOutput(task)
		}
	}
	if task.Completion != nil {
		d.deliverVerifiedSuccess(task)
		d.notifyRunning()
		return // The verified v1 execution policy freezes both auto actions false.
	}
	d.emit(EventKeyDone, task)
	d.emit(EventKeyFinally, task, err)
	d.notifyRunning()
	d.triggerOnDone(task)
	d.triggerWebhooks(WebhookEventDownloadDone, task, nil)
	d.triggerScripts(ScriptEventDownloadDone, task, nil)

	if e, ok := task.Meta.Opts.Extra.(*http.OptsExtra); ok {
		downloadFilePath := task.Meta.SingleFilepath()

		cfg, _ := d.GetConfig()

		// Determine if auto-torrent is enabled (use global config if not explicitly set)
		autoTorrentEnabled := false
		if e.AutoTorrent != nil {
			autoTorrentEnabled = *e.AutoTorrent
		} else if cfg != nil && cfg.AutoTorrent != nil {
			autoTorrentEnabled = cfg.AutoTorrent.Enable
		}

		if autoTorrentEnabled && d.MCPTaskEffectAllowed(task, "post_process") && strings.HasSuffix(downloadFilePath, ".torrent") {
			// Determine if should delete torrent file after creating BT task
			shouldDelete := false
			if e.DeleteTorrentAfterDownload != nil {
				shouldDelete = *e.DeleteTorrentAfterDownload
			} else if cfg != nil && cfg.AutoTorrent != nil {
				shouldDelete = cfg.AutoTorrent.DeleteAfterDownload
			}

			go func() {
				if !d.MCPTaskEffectAllowed(task, "post_process") {
					return
				}
				_, err2 := d.CreateDirect(
					&base.Request{
						URL: downloadFilePath,
					},
					&base.Options{
						Path:        task.Meta.Opts.Path,
						SelectFiles: make([]int, 0),
					})
				if err2 != nil {
					d.Logger.Error().Err(err2).Msgf("auto create torrent task failed, task id: %s", task.ID)
					return
				}

				if shouldDelete {
					d.Delete(&TaskFilter{IDs: []string{task.ID}}, true)
				}
			}()
		}

		// Determine if auto-extract is enabled (use global config if not explicitly set)
		autoExtractEnabled := false
		if e.AutoExtract != nil {
			autoExtractEnabled = *e.AutoExtract
		} else if cfg != nil && cfg.Archive != nil {
			autoExtractEnabled = cfg.Archive.AutoExtract
		}

		// Auto-extract archive files using the extraction queue
		// This ensures only one extraction runs at a time to prevent resource exhaustion
		if autoExtractEnabled && d.MCPTaskEffectAllowed(task, "post_process") && isArchiveFile(downloadFilePath) {
			d.enqueueExtraction(task, downloadFilePath, e)
		}
	}
}

// commitTaskCompletion shares deletion's persistence guard. The task operation
// lock precedes the guard, like pause/start saves; no protocol or user callbacks
// run under this guard. Public done is published only after acknowledged storage.
func (d *Downloader) commitTaskCompletion(task *Task, f fetcher.Fetcher, expectedGeneration ...uint64) (bool, error) {
	task.lock.Lock()
	defer task.lock.Unlock()
	// An old Wait may finish after Pause requeued the task. Discard that
	// completion before attempting a membership lookup or final snapshot.
	task.statusLock.Lock()
	if task.getStatus() != base.DownloadStatusRunning || task.Meta.Res == nil || (len(expectedGeneration) > 0 && task.runGeneration != expectedGeneration[0]) {
		task.statusLock.Unlock()
		return false, nil
	}
	generation := task.runGeneration
	task.statusLock.Unlock()

	// Protocol snapshots do not hold the main/state/persistence locks.
	var finalMeta *fetcher.FetcherMeta
	if task.Protocol == "media" || task.Protocol == "netdisk" {
		finalMeta = util.DeepClone(f.Meta())
	}
	task.dataLock.RLock()
	needSize := task.Meta.Res != nil && task.Meta.Res.Size == 0
	task.dataLock.RUnlock()
	if finalMeta != nil && finalMeta.Res != nil {
		needSize = finalMeta.Res.Size == 0
	}
	var downloaded int64
	if needSize {
		downloaded = f.Progress().TotalDownloaded()
	}

	persistenceLock := d.errorPersistenceLock(task)
	persistenceLock.Lock()
	defer persistenceLock.Unlock()
	var candidate *Task
	err := func() error {
		// notifyRunning takes main->status; never take the opposite order.
		d.lock.Lock()
		task.statusLock.Lock()
		present := !d.closed.Load() && d.getTaskLocked(task.ID) == task
		d.lock.Unlock()
		defer task.statusLock.Unlock()
		if !present || task.getStatus() != base.DownloadStatusRunning || task.runGeneration != generation || task.Meta.Res == nil {
			return nil
		}
		task.dataLock.Lock()
		if finalMeta != nil {
			if task.PathFreeze == nil {
				task.Meta = finalMeta
			} else if finalMeta.Res != nil && len(finalMeta.Res.Files) == len(task.Meta.Res.Files) {
				for i, file := range finalMeta.Res.Files {
					task.Meta.Res.Files[i].Size = file.Size
				}
				task.Meta.Res.Size = finalMeta.Res.Size
			}
		}
		task.Progress.Used = task.timer.Used()
		if task.Meta.Res.Size == 0 {
			task.Meta.Res.Size = downloaded
		}
		used := task.Progress.Used / int64(time.Second)
		if used == 0 {
			used = 1
		}
		totalSize := task.Meta.Res.Size
		task.Progress.Speed = totalSize / used
		task.Progress.Downloaded = totalSize
		task.dataLock.Unlock()

		candidate = task.clone()
		candidate.updateStatus(base.DownloadStatusDone)
		// An error can mean an unknown ACK. It never authorizes success effects.
		if task.PathFreeze != nil {
			identity, e := d.DatasetIdentity()
			if e != nil {
				return e
			}
			return d.storage.(AtomicStorage).UpdateAtomic(func(tx StorageTransaction) error {
				if e := requireDataset(tx, identity.DatasetID); e != nil {
					return e
				}
				current := new(Task)
				found, e := tx.Get(bucketTask, task.ID, current)
				if e != nil {
					return e
				}
				if !found {
					return ErrPathFreezeHeld
				}
				if e = guardPathSnapshot(current, candidate); e != nil {
					return e
				}
				if current.PathFreeze.LayoutOutput == nil || current.PathFreeze.LayoutOutput.Phase != MCPWriteOutputPublished {
					return ErrPathFreezeHeld
				}
				return tx.Put(bucketTask, task.ID, candidate)
			})
		}
		return d.storage.Put(bucketTask, task.ID, candidate)
	}()
	if err != nil {
		return false, err
	}
	if candidate == nil {
		return false, nil
	}

	// Release status before reacquiring main. Delete may have removed membership
	// while the Put was in flight; Pause/requeue may also have advanced the run.
	d.lock.Lock()
	defer d.lock.Unlock()
	task.statusLock.Lock()
	defer task.statusLock.Unlock()
	if d.closed.Load() || d.getTaskLocked(task.ID) != task || task.getStatus() != base.DownloadStatusRunning || task.runGeneration != generation {
		return false, nil
	}
	task.dataLock.Lock()
	task.Status = candidate.Status
	task.clearReadPublishedLocked()
	task.UpdatedAt = candidate.UpdatedAt
	task.notBefore = time.Time{}
	task.dataLock.Unlock()
	return true, nil
}

func (d *Downloader) errorPersistenceLock(task *Task) *sync.Mutex {
	lock, _ := d.errorPersistenceLocks.LoadOrStore(task, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (d *Downloader) doOnError(task *Task, err error, generation ...uint64) {
	if d.closed.Load() || d.getTask(task.ID) != task {
		return
	}
	if deferred, ok := transfer.AsDeferred(err); ok {
		d.deferTask(task, deferred, generation...)
		return
	}
	// Managed resolution uses the same closed categories as managed streams.
	// Existing HTTP sanitization remains in place; Core does not forward
	// request-specific error objects to events, webhooks or scripts.
	if task.Protocol == "http" && task.recoveryOwner.Load() != nil {
		var requestError *url.Error
		if errors.As(err, &requestError) {
			err = httpProtocol.ClassifyRecoveryError(err)
		}
	}
	persistenceLock := d.errorPersistenceLock(task)
	persistenceLock.Lock()
	handled := func() bool {
		// Delete removes membership before waiting for an earlier save to finish.
		// Check again under the persistence lock so a late error cannot recreate it.
		if d.closed.Load() || d.getTask(task.ID) != task {
			return false
		}
		task.statusLock.Lock()
		defer task.statusLock.Unlock()
		if task.getStatus() == base.DownloadStatusPause || d.closed.Load() ||
			(len(generation) > 0 && task.runGeneration != generation[0]) {
			return false
		}
		failureLog := d.Logger.Warn().Err(err)
		if task.Protocol == "http" {
			// Fixed recovery categories omit the original request error. Keep
			// the existing host diagnostic without logging userinfo, path,
			// signed query parameters or request headers.
			task.dataLock.RLock()
			var host string
			if task.Meta != nil && task.Meta.Req != nil {
				if parsed, parseErr := url.Parse(task.Meta.Req.URL); parseErr == nil {
					host = parsed.Host
				}
			}
			task.dataLock.RUnlock()
			if host != "" {
				failureLog.Str("host", host)
			}
		}
		failureLog.Msgf("task download failed, task id: %s", task.ID)
		task.updateStatus(base.DownloadStatusError)
		task.dataLock.Lock()
		task.clearReadPublishedLocked()
		if task.Protocol == "http" {
			_ = task.publishReadErrorLocked(acceptedHTTPReadError(err))
		}
		task.dataLock.Unlock()
		if saveErr := d.saveTask(task); saveErr != nil {
			d.Logger.Error().Err(saveErr).Msgf("persist failed task: %s", task.ID)
		}
		return true
	}()
	persistenceLock.Unlock()
	if !handled {
		if d.closed.Load() || d.getTask(task.ID) != task {
			d.errorPersistenceLocks.Delete(task)
		}
		return
	}
	d.triggerOnError(task, err)
	if task.getStatus() == base.DownloadStatusError {
		d.emit(EventKeyError, task, err)
		d.emit(EventKeyFinally, task, err)
		d.notifyRunning()
		d.triggerWebhooks(WebhookEventDownloadError, task, err)
	}
}

// deferTask requeues a running task whose source asked clients to wait longer
// than a task should hold its slot. The task is not failed: it keeps its
// progress, frees the slot for other sources and starts again after the wait.
func (d *Downloader) deferTask(task *Task, deferred *transfer.DeferredError, generation ...uint64) {
	persistenceLock := d.errorPersistenceLock(task)
	persistenceLock.Lock()
	handled := func() bool {
		if d.closed.Load() || d.getTask(task.ID) != task {
			return false
		}
		task.statusLock.Lock()
		defer task.statusLock.Unlock()
		if task.getStatus() != base.DownloadStatusRunning ||
			(len(generation) > 0 && task.runGeneration != generation[0]) {
			return false
		}
		d.Logger.Info().Msgf("task deferred by source rate limit until %s, task id: %s",
			deferred.Until.Format(time.RFC3339), task.ID)
		task.runGeneration++
		task.timer.Pause()
		task.dataLock.Lock()
		task.updateStatusLocked(base.DownloadStatusWait)
		task.notBefore = deferred.Until
		task.clearReadPublishedLocked()
		task.dataLock.Unlock()
		if saveErr := d.saveTask(task); saveErr != nil {
			d.Logger.Error().Err(saveErr).Msgf("persist deferred task: %s", task.ID)
		}
		return true
	}()
	persistenceLock.Unlock()
	if !handled {
		return
	}
	if task.PathFreeze != nil {
		if err := d.quiesceTaskPath(task); err != nil {
			d.Logger.Warn().Err(err).Msgf("deferred output has not joined, task id: %s", task.ID)
			return
		}
		_ = d.releasePathOutput(task)
	}
	// Delete may have removed the task after the check above; only a task
	// still listed and still waiting is queued, so it cannot be revived.
	d.lock.Lock()
	queued := false
	for _, t := range d.tasks {
		if t == task && task.getStatus() == base.DownloadStatusWait {
			d.enqueueLocked(task)
			queued = true
			break
		}
	}
	d.lock.Unlock()
	if !queued {
		return
	}
	d.emit(EventKeyProgress, task)
	d.notifyRunning()
	// Once the cooldown is over the task can run again: start it if a slot is
	// free, and otherwise let tasks idling in a cooldown reconsider their slot.
	time.AfterFunc(time.Until(deferred.Until)+10*time.Millisecond, func() {
		d.signalQueue()
		d.notifyRunning()
	})
}

func (d *Downloader) restoreTask(task *Task) error {
	if err := d.checkTaskRecovery(task); err != nil {
		return err
	}
	if task.fetcher == nil {
		if err := d.restoreFetcher(task); err != nil {
			return err
		}
	}
	if task.PathFreeze == nil && ((task.Protocol != "http" && !parentRecoveryProtocol(task.Protocol)) || (task.getStatus() == base.DownloadStatusDone && task.Uploading)) {
		go d.watch(task)
	}
	return nil
}

// Callers validate required state before taking task.dataLock: Patch can call
// this helper while holding that write lock.
func (d *Downloader) restoreFetcher(task *Task) error {
	v, f := task.fetcherManager.Restore()
	var checkpointPresent bool
	if v != nil {
		// Keep the durable checkpoint until the next successful save replaces
		// it. A crash in OnStart/Start must not erase the last resumable state.
		var err error
		checkpointPresent, err = d.storage.Get(bucketSave, task.ID, v)
		if err != nil {
			return err
		}
	}
	meta, err := task.cloneExecutionMeta()
	if err != nil {
		return err
	}
	initial := task.initialHTTPExecution.Swap(false)
	task.fetcher = f(meta, v)
	if task.fetcher == nil {
		task.fetcher = task.fetcherManager.Build()
	}
	if initial && !checkpointPresent && (task.Protocol == "http" || parentRecoveryProtocol(task.Protocol)) && meta.Res == nil {
		task.recoveryInitialEligible.Store(true)
		// Keep the manager's Restore/precision semantics; only HTTP's narrow
		// runtime handshake distinguishes a fresh M->Q from historical state.
		if first, ok := task.fetcher.(interface{ AuthorizeInitialRequest() }); ok {
			first.AuthorizeInitialRequest()
		}
	}
	d.setupFetcher(task.fetcherManager, task.fetcher)
	if task.fetcher.Meta().Req == nil {
		task.fetcher.Meta().Req = task.Meta.Req
	}
	if task.fetcher.Meta().Res == nil {
		task.fetcher.Meta().Res = task.Meta.Res
	}
	if task.fetcher.Meta().Opts == nil {
		task.fetcher.Meta().Opts = task.Meta.Opts
	}
	return nil
}

func (d *Downloader) doCreate(f fetcher.Fetcher, opts *base.Options) (taskId string, err error) {
	if f.Meta().Opts == nil {
		f.Meta().Opts = opts
	}

	fm, err := d.parseRequestFm(f.Meta().Req)
	if err != nil {
		return
	}

	task := NewTask()
	task.fetcherManager = fm
	task.fetcher = f
	task.Protocol = fm.Name()
	task.recoveryInitialEligible.Store(task.Protocol == "http" || parentRecoveryProtocol(task.Protocol))
	task.Meta = util.DeepClone(f.Meta())
	task.Progress = &Progress{}
	_, task.Uploading = f.(fetcher.Uploader)
	initTask(task)
	if err = d.storage.Put(bucketTask, task.ID, task.clone()); err != nil {
		return
	}
	taskId = task.ID

	func() {
		d.lock.Lock()
		defer d.lock.Unlock()

		d.tasks = append(d.tasks, task)

		remainRunningCount := d.remainRunningCount()
		if remainRunningCount == 0 {
			task.updateStatus(base.DownloadStatusWait)
			d.enqueueLocked(task)
			return
		}

		err = d.doStart(task)
	}()

	if task.Protocol != "http" {
		go d.watch(task)
	}
	return
}

func (d *Downloader) initOptions(opts *base.Options) (*base.Options, error) {
	if opts == nil {
		opts = &base.Options{}
	} else {
		// Defaults, protocol parsing and automatic renaming belong to this task.
		opts = opts.Clone()
	}
	if opts.SelectFiles == nil {
		opts.SelectFiles = make([]int, 0)
	}
	if opts.Path == "" {
		storeConfig, err := d.GetConfig()
		if err != nil {
			return nil, err
		}
		opts.Path = storeConfig.DownloadDir
	}
	// Replace placeholders in download path (e.g., %year%, %month%, %day%, %date%)
	opts.Path = util.ReplacePathPlaceholders(opts.Path)

	// if enable white download directory, check if the download directory is in the white list
	if len(d.cfg.WhiteDownloadDirs) > 0 {
		inWhiteList := false
		for _, dir := range d.cfg.WhiteDownloadDirs {
			if match, err := filepath.Match(dir, opts.Path); match && err == nil {
				inWhiteList = true
				break
			}
		}
		if !inWhiteList {
			return nil, errors.New("download directory is not in white list")
		}
	}
	return opts, nil
}

func (d *Downloader) statusMut(task *Task, fn func() (bool, error)) (bool, error) {
	task.statusLock.Lock()
	defer task.statusLock.Unlock()

	return fn()
}

func (d *Downloader) doStart(task *Task) (err error) {
	return d.startTask(task, false)
}

func (d *Downloader) startTask(task *Task, onlyWaiting bool) (err error) {
	var needCreate bool
	var verificationResume bool
	var generation uint64
	var runCtx context.Context
	var runCancel context.CancelFunc
	isReturn, err := d.statusMut(task, func() (isReturn bool, err error) {
		if err := d.checkTaskRecovery(task); err != nil {
			return true, err
		}
		if until, e := d.taskSharedUntil(task); e != nil {
			return true, e
		} else if until.After(time.Now()) {
			return true, &transfer.DeferredError{Until: until}
		}
		if task.creationHeld() {
			return true, ErrOperationConflict
		}
		if task.handoffHeld() {
			return true, ErrHandoffHeld
		}
		if onlyWaiting && task.getStatus() != base.DownloadStatusWait ||
			task.getStatus() == base.DownloadStatusRunning || task.getStatus() == base.DownloadStatusDone {
			isReturn = true
			return
		}

		if task.Completion != nil {
			identity, e := d.DatasetIdentity()
			if e != nil {
				return true, e
			}
			owned, record, e := d.readVerifiedTask(identity.DatasetID, task.ID)
			if e != nil {
				return true, e
			}
			if record.Phase == transfer.CompletionCommitted {
				// Pause may have won just after an unknown C ACK. Explicit
				// Continue reconciles the SAME durable CID, without a run,
				// write, transport restore or reconstructed fresh permission.
				task.dataLock.RLock()
				matching := task.Completion != nil && owned.Completion != nil && *task.Completion == *owned.Completion
				task.dataLock.RUnlock()
				if !matching {
					return true, transfer.ErrRecoveryHeld
				}
				invalidateTaskRecovery(task)
				if task.runCancel != nil {
					task.runCancel()
				}
				return true, d.acceptVerifiedCompletion(task, owned)
			}
			verificationResume = record.Phase != transfer.CompletionPrepared
		}
		if !verificationResume {
			err = d.restoreTask(task)
		}
		if err != nil {
			d.Logger.Error().Stack().Err(err).Msgf("restore fetcher failed, task id: %s", task.ID)
			return
		}
		if task.CreationOperation != nil && !verificationResume {
			_, uploading := task.fetcher.(fetcher.Uploader)
			task.dataLock.Lock()
			task.Uploading = uploading
			task.dataLock.Unlock()
		}
		task.dataLock.RLock()
		needCreate = !task.IsCreated
		task.dataLock.RUnlock()
		// Starting an acknowledged explicit retry consumes its private permit
		// in begin; cancellation/patch/delete still clear it via invalidate.
		if owner := task.recoveryOwner.Load(); owner != nil {
			owner.active.Store(false)
		}
		task.runGeneration = d.recoveryGenerationBase(task) + 1
		task.verificationOnly.Store(false)
		task.dataLock.Lock()
		task.clearReadPublishedLocked()
		task.dataLock.Unlock()
		generation = task.runGeneration
		if task.runCancel != nil {
			task.runCancel()
		}
		runCtx, runCancel = context.WithCancel(context.Background())
		task.runCancel = runCancel
		if task.Protocol == "http" || parentRecoveryProtocol(task.Protocol) || task.PathFreeze != nil {
			cancelSnapshot := runCancel
			task.recoveryRunCancel.Store(&cancelSnapshot)
		}
		task.updateStatus(base.DownloadStatusRunning)

		return
	})
	if err != nil {
		d.Logger.Error().Stack().Err(err).Msgf("start task failed, task id: %s", task.ID)
		return
	}
	if isReturn {
		return
	}

	var startWatch func()
	handler := func() error {
		task.statusLock.Lock()
		previousPause := task.pauseDone
		task.statusLock.Unlock()
		if previousPause != nil {
			select {
			case <-previousPause:
			case <-runCtx.Done():
				return nil
			}
		}
		task.lock.Lock()
		defer task.lock.Unlock()
		task.statusLock.Lock()
		obsolete := task.getStatus() != base.DownloadStatusRunning ||
			task.runGeneration != generation || d.closed.Load()
		task.statusLock.Unlock()
		if obsolete || d.getTask(task.ID) != task {
			return nil
		}
		if verificationResume {
			identity, e := d.DatasetIdentity()
			if e != nil {
				return e
			}
			// Sealed recovery restores only the verified stored binding. There
			// is no new HTTP snapshot/default capture and no network permission.
			_, storedRecovery, e := readRecovery(d.storage, task.ID)
			if e != nil {
				return e
			}
			owner, e := d.ensureTaskRecovery(task, storedRecovery.ExecutionDefaultsHash)
			if e != nil {
				return e
			}
			lease, e := owner.beginVerificationRun(runCtx, identity.DatasetID, generation)
			if e != nil {
				return e
			}
			task.verificationOnly.Store(true)
			task.timer.Start()
			go func() { defer runCancel(); d.watchRun(task, nil, generation, func() error { return nil }, lease) }()
			return nil
		}
		if err := requireTaskPathFreeze(task); err != nil {
			return err
		}
		if task.Completion != nil {
			output, ok := task.fetcher.(interface{ RequireVerifiedOutput() })
			if !ok {
				return transfer.ErrRecoveryHeld
			}
			output.RequireVerifiedOutput()
		}
		if task.MCPWrite != nil {
			if err := d.configureMCPFetcher(task); err != nil {
				return err
			}
		}

		var recovery *taskRecoveryOwner
		var parentSnapshot *transfer.ProtocolExecutionContext
		task.dataLock.RLock()
		sharedEligible := task.Recovery != nil || task.recoveryInitialEligible.Load()
		task.dataLock.RUnlock()
		capture, canCapture := task.fetcher.(interface {
			CaptureRecoveryContext(*base.Request) (*transfer.HTTPExecutionContext, bool, error)
			SetRecoveryContext(*transfer.HTTPExecutionContext)
		})
		if task.Protocol == "http" {
			if !canCapture && task.Recovery != nil {
				return transfer.ErrRecoveryHeld
			}
			if canCapture {
				snapshot, supported, captureErr := capture.CaptureRecoveryContext(task.Meta.Req)
				if captureErr != nil {
					return captureErr
				}
				if !supported {
					if task.Recovery != nil {
						return transfer.ErrRecoveryHeld
					}
				} else {
					var recoveryErr error
					recovery, recoveryErr = d.ensureTaskRecovery(task, snapshot.DefaultsHash())
					if recoveryErr != nil {
						return recoveryErr
					}
				}
			}
		}
		if parentRecoveryProtocol(task.Protocol) {
			_, canBind := task.fetcher.(interface{ SetParentRecoveryContext(context.Context) error })
			if !canBind || !supportsSharedRecoveryDomain(d.storage) {
				if task.Recovery != nil {
					return transfer.ErrRecoveryHeld
				}
			} else {
				var supported bool
				var captureErr error
				parentSnapshot, supported, captureErr = d.captureParentRecoveryContext(task)
				if captureErr != nil {
					return captureErr
				}
				if supported && task.Protocol == "media" {
					bridge, ok := task.fetcher.(interface {
						SupportsParentRecovery(*transfer.ProtocolExecutionContext) (bool, error)
					})
					if !ok {
						supported = false
					} else {
						supported, captureErr = bridge.SupportsParentRecovery(parentSnapshot)
						if captureErr != nil {
							return captureErr
						}
					}
				}
				if !supported {
					if task.Recovery != nil {
						return transfer.ErrRecoveryHeld
					}
				} else {
					recovery, captureErr = d.ensureTaskRecovery(task, parentSnapshot.DefaultsHash())
					if captureErr != nil {
						return captureErr
					}
				}
			}
		}
		if runCtx.Err() != nil {
			invalidateTaskRecovery(task)
			return nil
		}
		startTask := task.clone()
		d.triggerOnStart(startTask)
		task.statusLock.Lock()
		obsoleteAfterStart := runCtx.Err() != nil || task.runGeneration != generation || task.getStatus() != base.DownloadStatusRunning
		if obsoleteAfterStart && (task.Protocol == "http" || parentRecoveryProtocol(task.Protocol)) {
			task.statusLock.Unlock()
			invalidateTaskRecovery(task)
			return nil
		}
		if !obsoleteAfterStart {
			task.dataLock.Lock()
			task.Meta.Req = startTask.Meta.Req
			task.dataLock.Unlock()
		}
		// Legacy protocols retain their preparation cancellation handshake:
		// PrepareContext sees this run's canceled context, and never Start.
		task.statusLock.Unlock()
		if recovery != nil {
			var httpSnapshot *transfer.HTTPExecutionContext
			var defaults string
			if parentRecoveryProtocol(task.Protocol) {
				var supported bool
				var captureErr error
				parentSnapshot, supported, captureErr = d.captureParentRecoveryContext(task)
				if captureErr != nil {
					return captureErr
				}
				if !supported {
					return transfer.ErrRecoveryHeld
				}
				if task.Protocol == "media" {
					bridge, ok := task.fetcher.(interface {
						SupportsParentRecovery(*transfer.ProtocolExecutionContext) (bool, error)
					})
					if !ok {
						return transfer.ErrRecoveryHeld
					}
					supported, captureErr = bridge.SupportsParentRecovery(parentSnapshot)
					if captureErr != nil {
						return captureErr
					}
					if !supported {
						return transfer.ErrRecoveryHeld
					}
				}
				defaults = parentSnapshot.DefaultsHash()
			} else {
				var supported bool
				var captureErr error
				httpSnapshot, supported, captureErr = capture.CaptureRecoveryContext(task.Meta.Req)
				if captureErr != nil {
					return captureErr
				}
				if !supported {
					return transfer.ErrRecoveryHeld
				}
				defaults = httpSnapshot.DefaultsHash()
			}
			if err := recovery.persistSnapshot(task.clone(), nil, false, defaults); err != nil {
				return err
			}
			if (sharedEligible || parentSnapshot != nil) && supportsSharedRecoveryDomain(d.storage) {
				task.lock.Unlock()
				sharedErr := d.prepareSharedRecovery(runCtx, task, recovery, generation)
				task.lock.Lock()
				if sharedErr != nil {
					return sharedErr
				}
			}
			recovery.mu.Lock()
			recovery.parentContext = parentSnapshot
			recovery.mu.Unlock()
			if _, err := recovery.begin(generation, "", runCtx); err != nil {
				return err
			}
			lease, err := recovery.currentLease(runCtx)
			if err != nil {
				return err
			}
			runCtx = transfer.WithRecoveryLease(runCtx, lease)
			if parentSnapshot != nil {
				if err := task.fetcher.(interface{ SetParentRecoveryContext(context.Context) error }).SetParentRecoveryContext(runCtx); err != nil {
					return err
				}
			} else {
				setter, ok := task.fetcher.(interface{ SetRecoveryLease(*transfer.RecoveryLease) })
				if !ok {
					return transfer.ErrRecoveryHeld
				}
				capture.SetRecoveryContext(httpSnapshot)
				setter.SetRecoveryLease(lease)
			}
		}
		if task.Completion != nil && recovery == nil {
			// New recovery-managed output must never resolve through the legacy
			// PAC/unsupported path, including after R's configuration changes.
			return transfer.ErrRecoveryHeld
		}
		if recovery == nil {
			task.recoveryInitialEligible.Store(false)
		}
		var boundWait func() error
		if task.Protocol == "http" || recovery != nil && parentRecoveryProtocol(task.Protocol) || task.PathFreeze != nil {
			if binder, ok := task.fetcher.(interface {
				BindRunWait(context.Context) func() error
			}); ok {
				boundWait = binder.BindRunWait(runCtx)
			}
			if (recovery != nil || task.PathFreeze != nil) && boundWait == nil {
				return transfer.ErrRecoveryHeld
			}
		}
		if recovery != nil {
			if err := d.observeTaskRecoveryCoverage(task, transfer.RecoveryLeaseFromContext(runCtx)); err != nil {
				return err
			}
		}
		pathOutputBound := false
		if task.PathFreeze != nil && task.PathFreeze.Phase == PathFreezeFrozen && task.Protocol == "http" && task.Completion == nil {
			task.lock.Unlock()
			e := d.bindTaskPathOutput(runCtx, task, generation)
			task.lock.Lock()
			if e != nil {
				return e
			}
			pathOutputBound = true
		}
		mcpOutputBound := false
		if task.MCPWrite != nil && task.Meta.Res != nil {
			if err := d.bindMCPOutputForRun(runCtx, task, transfer.RecoveryLeaseFromContext(runCtx)); err != nil {
				return err
			}
			mcpOutputBound = true
			defer task.fetcher.(*httpProtocol.Fetcher).ReleaseVerifiedOutput()
		}
		// Resolve owns its request/options. Keep the published task view detached
		// so Pause can cancel resolution without waiting for a snapshot lock.
		task.dataLock.RLock()
		resolveMeta, cloneErr := task.cloneExecutionMeta()
		task.dataLock.RUnlock()
		if cloneErr != nil {
			return cloneErr
		}
		if resolveMeta.Res == nil {
			var resolveErr error
			if resolver, ok := task.fetcher.(fetcher.TaskResolver); ok {
				// A created task has no resolve deadline; Pause and Delete end the wait.
				resolveErr = resolver.ResolveTask(runCtx, resolveMeta.Req, resolveMeta.Opts)
			} else if resolver, ok := task.fetcher.(fetcher.ContextResolver); ok {
				resolveErr = resolver.ResolveContext(runCtx, resolveMeta.Req, resolveMeta.Opts)
			} else {
				resolveErr = task.fetcher.Resolve(resolveMeta.Req, resolveMeta.Opts)
			}
			for resolveErr != nil {
				var refresh *httpProtocol.RecoveryPublicRefreshRequired
				if !errors.As(resolveErr, &refresh) {
					return resolveErr
				}
				httpFetcher, ok := task.fetcher.(*httpProtocol.Fetcher)
				if !ok {
					return transfer.ErrRecoveryHeld
				}
				// Resolve has returned and its response is closed. Reserve,
				// metadata network and source CAS cannot run under Task.lock.
				task.lock.Unlock()
				lease, refreshErr := httpFetcher.RefreshSharedSource(runCtx, refresh)
				task.lock.Lock()
				if refreshErr != nil {
					return refreshErr
				}
				if runCtx.Err() != nil || lease == nil || d.getTask(task.ID) != task || task.runGeneration != generation {
					return transfer.ErrRecoveryStale
				}
				runCtx = transfer.WithRecoveryLease(runCtx, lease)
				resolveErr = httpFetcher.ResolveContext(runCtx, resolveMeta.Req, resolveMeta.Opts)
			}
		} else if preparer, ok := task.fetcher.(fetcher.StartPreparer); ok {
			if err := preparer.PrepareContext(runCtx, resolveMeta.Req, resolveMeta.Opts); err != nil {
				return err
			}
		}
		task.statusLock.Lock()
		paused := task.getStatus() != base.DownloadStatusRunning || task.runGeneration != generation || d.closed.Load()
		task.statusLock.Unlock()
		if paused {
			return nil
		}
		if task.PathFreeze != nil {
			resolved, e := cloneOperationJSON(task.fetcher.Meta())
			if e != nil {
				return e
			}
			task.lock.Unlock()
			e = d.confirmTaskPath(runCtx, task, resolved, generation)
			task.lock.Lock()
			if e != nil {
				return e
			}
			needCreate = false
		}
		// Keep rename reservation until Start has created the target file.
		if needCreate && task.MCPWrite == nil && task.PathFreeze == nil && task.fetcherManager.AutoRename() {
			d.checkDuplicateLock.Lock()
			defer d.checkDuplicateLock.Unlock()
		}
		if err := func() error {
			task.dataLock.Lock()
			defer task.dataLock.Unlock()
			if task.PathFreeze == nil {
				task.Meta = task.fetcher.Meta()
			}
			if resolveMeta.Res != nil && task.PathFreeze == nil {
				// Apply request changes made by onStart extensions to resolved tasks.
				task.Meta.Req = resolveMeta.Req
			}

			if needCreate {
				if task.MCPWrite == nil && task.PathFreeze == nil && task.fetcherManager.AutoRename() {
					task.Meta.Opts.Name = util.SafeFilename(task.Meta.Opts.Name)
					// check if the download file is duplicated and rename it automatically.
					if task.Meta.Res.Name != "" {
						task.Meta.Res.Name = util.SafeFilename(task.Meta.Res.Name)
						fullDirPath := task.Meta.FolderPath()
						newName, err := util.CheckDuplicateAndRename(fullDirPath)
						if err != nil {
							return err
						}
						task.Meta.Opts.Name = newName
					} else {
						task.Meta.Res.Files[0].Name = util.SafeFilename(task.Meta.Res.Files[0].Name)
						fullFilePath := task.Meta.SingleFilepath()
						newName, err := util.CheckDuplicateAndRename(fullFilePath)
						if err != nil {
							return err
						}
						task.Meta.Opts.Name = newName
					}
				}
				task.IsCreated = true
				task.Meta.Res.CalcSize(task.Meta.Opts.SelectFiles)
			}
			if task.Protocol == "media" || task.Protocol == "netdisk" {
				// Media updates resource sizes from its child process. Publish a
				// detached view until Wait establishes completion synchronization.
				task.Meta = util.DeepClone(task.Meta)
			}
			return nil
		}(); err != nil {
			return err
		}
		if recovery != nil && recovery.coverageInstallAuthorization.Load() != nil {
			// Release the Task operation before Delete/Fence admission. No target
			// writer has been opened yet; the private slot is consumed exactly once.
			task.lock.Unlock()
			coverageErr := d.installTaskRecoveryCoverage(runCtx, task, transfer.RecoveryLeaseFromContext(runCtx))
			task.lock.Lock()
			if coverageErr != nil {
				return coverageErr
			}
		}
		if task.PathFreeze != nil && task.Completion == nil && !pathOutputBound {
			task.lock.Unlock()
			e := d.bindTaskPathOutput(runCtx, task, generation)
			task.lock.Lock()
			if e != nil {
				return e
			}
		}
		if task.PathFreeze != nil && task.Completion != nil {
			httpFetcher, ok := task.fetcher.(*httpProtocol.Fetcher)
			if !ok {
				return ErrPathFreezeHeld
			}
			if e := httpFetcher.BindPathFreezeGuard(task.ID, func() error { return runCtx.Err() }); e != nil {
				return e
			}
		}
		if task.MCPWrite != nil && !mcpOutputBound {
			lease := transfer.RecoveryLeaseFromContext(runCtx)
			if lease == nil {
				return transfer.ErrRecoveryHeld
			}
			if err := d.saveTask(task); err != nil {
				return err
			}
			task.lock.Unlock()
			file, outputErr := d.prepareMCPOutput(runCtx, task, lease)
			task.lock.Lock()
			if outputErr != nil {
				return outputErr
			}
			if runCtx.Err() != nil {
				file.Close()
				return runCtx.Err()
			}
			output, ok := task.fetcher.(*httpProtocol.Fetcher)
			if !ok {
				file.Close()
				return transfer.ErrRecoveryHeld
			}
			if err := output.SetVerifiedOutput(file); err != nil {
				file.Close()
				return err
			}
			defer output.ReleaseVerifiedOutput()
		}
		if task.Completion != nil {
			lease := transfer.RecoveryLeaseFromContext(runCtx)
			if lease == nil {
				return transfer.ErrRecoveryHeld
			}
			// Freeze resolved target metadata before the first stage write.
			if err := d.saveTask(task); err != nil {
				return err
			}
			identity, e := d.DatasetIdentity()
			if e != nil {
				return e
			}
			task.lock.Unlock()
			file, e := d.prepareVerifiedOutput(runCtx, identity.DatasetID, task, lease)
			task.lock.Lock()
			if e != nil {
				return e
			}
			if runCtx.Err() != nil {
				file.Close()
				return runCtx.Err()
			}
			output, ok := task.fetcher.(interface {
				SetVerifiedOutput(*os.File) error
				ReleaseVerifiedOutput()
			})
			if !ok {
				file.Close()
				return transfer.ErrRecoveryHeld
			}
			if e = output.SetVerifiedOutput(file); e != nil {
				file.Close()
				return e
			}
			defer output.ReleaseVerifiedOutput()
		}

		task.statusLock.Lock()
		if task.getStatus() != base.DownloadStatusRunning || task.runGeneration != generation || d.closed.Load() || runCtx.Err() != nil {
			task.statusLock.Unlock()
			return nil
		}
		task.dataLock.Lock()
		task.Progress.Speed = 0
		task.timer.Start()
		startErr := task.fetcher.Start()
		task.dataLock.Unlock()
		task.statusLock.Unlock()
		if startErr != nil {
			return startErr
		}
		var saveErr error
		if task.Protocol == "netdisk" {
			saveErr = d.saveActiveTask(task)
		} else {
			saveErr = d.saveTask(task)
		}
		if saveErr != nil {
			return saveErr
		}
		if d.getTask(task.ID) != task {
			return nil
		}
		f := task.fetcher
		if lease := transfer.RecoveryLeaseFromContext(runCtx); lease != nil && lease.ActionVersion == 4 && boundWait != nil {
			if source, ok := f.(interface {
				CaptureRunQuiescence() func(context.Context) error
			}); ok {
				join := source.CaptureRunQuiescence()
				wait := boundWait
				ctx := runCtx
				boundWait = func() error {
					err := wait()
					if joined := join(ctx); joined != nil {
						return joined
					}
					return err
				}
			} else {
				return transfer.ErrRecoveryHeld
			}
		}
		startWatch = func() {
			go func() {
				defer runCancel()
				d.watchRun(task, f, generation, boundWait, transfer.RecoveryLeaseFromContext(runCtx))
			}()
		}
		return nil
	}
	go func() {
		if err := handler(); err != nil {
			if deferred, ok := transfer.AsDeferred(err); ok {
				if owner := task.recoveryOwner.Load(); owner != nil {
					owner.mu.Lock()
					shared := owner.record.Version == recoverySharedVersion
					owner.mu.Unlock()
					if shared {
						if e := d.deferSharedPreparation(task, generation, runCtx, deferred); e == nil {
							runCancel()
							owner.invalidateToken(ownerTokenForRun(owner, generation))
							return
						} else {
							err = e
						}
					}
				}
			}
			d.failTaskPreparation(task, generation, runCancel, err)
			if task.PathFreeze != nil {
				_ = d.quiesceTaskPath(task)
				_ = d.releasePathOutput(task)
			}
			return
		}
		if startWatch != nil {
			// Preserve Start-before-Done while keeping reentrant listeners out
			// of Task/duplicate-reservation locks needed by lifecycle APIs.
			d.emit(EventKeyStart, task)
			startWatch()
		}
	}()

	return
}

func (d *Downloader) doPause(task *Task) (err error) {
	task.dataLock.RLock()
	mcp := mcpTaskPresence(task)
	task.dataLock.RUnlock()
	if mcp {
		d.operationMu.Lock()
		defer d.operationMu.Unlock()
	}
	var pauseDone chan struct{}
	var previousPauseDone chan struct{}
	isReturn, err := d.statusMut(task, func() (isReturn bool, err error) {
		if task.getStatus() == base.DownloadStatusPause || task.getStatus() == base.DownloadStatusDone ||
			(task.getStatus() == base.DownloadStatusError && d.closed.Load()) {
			pauseDone = task.pauseDone
			isReturn = true
			return
		}

		invalidateTaskRecovery(task)
		previousPauseDone = task.pauseDone
		task.dataLock.Lock()
		task.clearReadPublishedLocked()
		task.dataLock.Unlock()
		pauseDone = make(chan struct{})
		task.pauseDone = pauseDone
		if task.runCancel != nil {
			task.runCancel()
		}
		task.updateStatus(base.DownloadStatusPause)
		task.runGeneration++
		task.timer.Pause()
		return
	})
	if err != nil {
		d.Logger.Error().Stack().Err(err).Msgf("pause task failed, task id: %s", task.ID)
		return
	}
	if isReturn {
		// Pause can already be published while its async persistence is queued.
		// Join it before Close releases the storage backend.
		if d.closed.Load() && pauseDone != nil {
			<-pauseDone
		}
		return
	}
	if resolving, ok := task.fetcher.(interface{ CancelResolve() }); ok {
		resolving.CancelResolve()
	}

	handler := func() error {
		if previousPauseDone != nil {
			<-previousPauseDone
		}
		pauseErr := func() error {
			parent := parentRecoveryProtocol(task.Protocol) || task.PathFreeze != nil
			if parent && task.fetcher != nil {
				if err := task.fetcher.Pause(); err != nil {
					return err
				}
				if err := d.quiesceTaskPath(task); err != nil {
					return err
				}
			}
			task.lock.Lock()
			defer task.lock.Unlock()

			if !parent && task.fetcher != nil {
				if err := task.fetcher.Pause(); err != nil {
					return err
				}
			}
			if err := d.saveActiveTask(task); err != nil {
				return err
			}
			if owner := task.recoveryOwner.Load(); owner != nil {
				if proof, ok := task.fetcher.(interface{ RecoveryCleanPause() bool }); ok && proof.RecoveryCleanPause() {
					return owner.markCleanPaused()
				}
			}
			return nil
		}()
		d.releaseMCPTaskLease(task)
		if pauseErr == nil {
			_ = d.releasePathOutput(task)
		}
		// Callbacks may invoke Close themselves. Publish persistence completion
		// and release operation locks before delivering the event.
		close(pauseDone)
		if pauseErr == nil {
			d.emit(EventKeyPause, task)
		}
		return pauseErr
	}
	// Media continuation must not race a still-running child. Shutdown must
	// persist every paused fetcher before closing its storage.
	if (task.Protocol == "media" || task.Protocol == "netdisk" || task.PathFreeze != nil) || d.closed.Load() {
		return handler()
	}
	go func() {
		if err := handler(); err != nil {
			d.Logger.Error().Stack().Err(err).Msgf("pause task handle failed, task id: %s", task.ID)
		}
	}()
	return
}

// redirect stderr to log file, when panic happened log it
func logPanic(logDir string) {
	if err := util.CreateDirIfNotExist(logDir); err != nil {
		return
	}
	f, err := os.Create(filepath.Join(logDir, "crash.log"))
	if err != nil {
		return
	}
	debug.SetCrashOutput(f, debug.CrashOptions{})
}

func (d *Downloader) assignFetcherManager(task *Task) error {
	fm, err := d.parseRequestFm(task.Meta.Req)
	if err != nil {
		return err
	}
	task.fetcherManager = fm
	return nil
}

func (d *Downloader) buildFetcher(req *base.Request) (fetcher.Fetcher, error) {
	fm, err := d.parseRequestFm(req)
	if err != nil {
		return nil, err
	}
	fetcher := fm.Build()
	d.setupFetcher(fm, fetcher)
	return fetcher, nil
}

// enqueueExtraction adds an extraction job to the global extraction queue
// This ensures only one extraction (or one multi-part archive extraction) runs at a time
// to prevent resource exhaustion
func (d *Downloader) enqueueExtraction(task *Task, downloadFilePath string, opts *http.OptsExtra) {
	partInfo := getArchivePartInfo(downloadFilePath)

	if partInfo.IsMultiPart {
		// For multi-part archives, handle specially
		d.enqueueMultiPartExtraction(task, downloadFilePath, partInfo, opts)
	} else {
		// For single archives, queue immediately
		d.enqueueSingleExtraction(task, downloadFilePath, opts)
	}
}

// enqueueSingleExtraction queues extraction for a single (non-multi-part) archive
func (d *Downloader) enqueueSingleExtraction(task *Task, downloadFilePath string, opts *http.OptsExtra) {
	jobID := "single:" + task.ID

	// Set extraction status to queued
	task.dataLock.Lock()
	task.Progress.ExtractStatus = ExtractStatusQueued
	task.dataLock.Unlock()
	d.emit(EventKeyProgress, task)
	d.storage.Put(bucketTask, task.ID, task.clone())
	d.Logger.Info().Msgf("extraction queued, task id: %s, job id: %s", task.ID, jobID)

	// Create and enqueue the extraction job
	job := NewExtractionJob(jobID, func() {
		d.performExtraction(task, downloadFilePath, task.Meta.Opts.Path, opts)
	})

	go func() {
		GetExtractionQueue().Enqueue(job)
	}()
}

// enqueueMultiPartExtraction handles queueing for multi-part archives
// It ensures only ONE extraction job is queued when ALL parts are ready
func (d *Downloader) enqueueMultiPartExtraction(task *Task, downloadFilePath string, partInfo ArchivePartInfo, opts *http.OptsExtra) {
	// Set multi-part info on the task
	task.dataLock.Lock()
	task.Progress.MultiPartBaseName = partInfo.BaseName
	task.Progress.MultiPartNumber = partInfo.PartNumber
	task.Progress.MultiPartIsFirst = isFirstPart(downloadFilePath)
	task.dataLock.Unlock()

	// Check if all parts are downloaded
	destDir := task.Meta.Opts.Path
	allPartsReady, missingParts := d.checkMultiPartArchiveReady(downloadFilePath, destDir, partInfo)

	if !allPartsReady {
		// Not all parts are ready yet - just set status to waiting, don't queue anything
		task.dataLock.Lock()
		task.Progress.ExtractStatus = ExtractStatusWaitingParts
		task.dataLock.Unlock()
		d.emit(EventKeyProgress, task)
		d.storage.Put(bucketTask, task.ID, task.clone())
		d.Logger.Info().Msgf("multi-part archive waiting for other parts, task id: %s, missing: %v", task.ID, missingParts)
		return
	}

	// All parts are ready! Atomically check if extraction has already been started/queued
	// and if not, mark this task as the one that will handle it
	// Use GetMultiPartArchiveBaseName to get the full path for comparison
	fullBaseName := GetMultiPartArchiveBaseName(downloadFilePath)
	shouldQueue := d.tryClaimMultiPartExtraction(task, fullBaseName)

	if !shouldQueue {
		// Another part already started/queued extraction, mark this task as done
		task.dataLock.Lock()
		task.Progress.ExtractStatus = ExtractStatusDone
		task.Progress.ExtractProgress = 100
		task.dataLock.Unlock()
		d.emit(EventKeyProgress, task)
		d.storage.Put(bucketTask, task.ID, task.clone())
		d.Logger.Info().Msgf("multi-part archive extraction already handled by another part, task id: %s", task.ID)
		return
	}

	// This task claimed the extraction - status already set to queued in tryClaimMultiPartExtraction
	d.emit(EventKeyProgress, task)
	d.storage.Put(bucketTask, task.ID, task.clone())

	jobID := "multipart:" + fullBaseName
	d.Logger.Info().Msgf("multi-part extraction queued, task id: %s, job id: %s", task.ID, jobID)

	// Create and enqueue the extraction job
	job := NewExtractionJob(jobID, func() {
		d.performMultiPartExtraction(task, partInfo.FirstPartPath, destDir, opts)
	})

	go func() {
		GetExtractionQueue().Enqueue(job)
	}()
}

// checkMultiPartArchiveReady checks if all parts of a multi-part archive are downloaded
// by examining task status rather than file existence
func (d *Downloader) checkMultiPartArchiveReady(filePath string, destDir string, partInfo ArchivePartInfo) (bool, []string) {
	// Use task-based checking - find all tasks with the same MultiPartBaseName
	// and verify they are all in Done status
	baseName := GetMultiPartArchiveBaseName(filePath)
	if baseName == "" {
		return true, nil
	}

	return d.checkAllMultiPartTasksDone(baseName)
}

// checkAllMultiPartTasksDone checks if all tasks belonging to a multi-part archive are done
func (d *Downloader) checkAllMultiPartTasksDone(baseName string) (bool, []string) {
	var notDoneParts []string

	// Find all tasks that belong to this multi-part archive
	var relatedTasks []*Task
	for _, task := range d.GetTasks() {
		taskBaseName := ""
		if task.Meta != nil && task.Meta.Res != nil && len(task.Meta.Res.Files) > 0 {
			taskBaseName = GetMultiPartArchiveBaseName(task.Meta.SingleFilepath())
		}
		if taskBaseName == baseName {
			relatedTasks = append(relatedTasks, task)
		}
	}

	// If we found no related tasks, we can't determine readiness
	if len(relatedTasks) == 0 {
		return false, []string{"no related tasks found for " + baseName}
	}

	// Check if all related tasks are done
	for _, task := range relatedTasks {
		if task.getStatus() != base.DownloadStatusDone {
			notDoneParts = append(notDoneParts, task.Meta.SingleFilepath())
		}
	}

	return len(notDoneParts) == 0, notDoneParts
}

// tryClaimMultiPartExtraction atomically checks if extraction can be claimed for a multi-part archive
// and if so, marks the task as queued. Returns true if this task should proceed with queueing.
// This uses sync.Map.LoadOrStore for atomic claim to prevent race conditions.
func (d *Downloader) tryClaimMultiPartExtraction(task *Task, baseName string) bool {
	// Use LoadOrStore for atomic claim - if another goroutine already stored a value, we get that value back
	_, alreadyClaimed := d.claimedExtractions.LoadOrStore(baseName, task.ID)
	if alreadyClaimed {
		return false // Another task already claimed it
	}

	// This task successfully claimed it
	task.dataLock.Lock()
	task.Progress.ExtractStatus = ExtractStatusQueued
	task.dataLock.Unlock()
	return true
}

// releaseMultiPartExtractionClaim releases the extraction claim for a multi-part archive
// This is primarily used for testing purposes
func (d *Downloader) releaseMultiPartExtractionClaim(baseName string) {
	d.claimedExtractions.Delete(baseName)
}

// performExtraction performs extraction for a regular (non-multi-part) archive
func (d *Downloader) performExtraction(task *Task, archivePath string, destDir string, opts *http.OptsExtra) {
	// Set extraction status to extracting
	task.dataLock.Lock()
	task.Progress.ExtractStatus = ExtractStatusExtracting
	task.Progress.ExtractProgress = 0
	task.dataLock.Unlock()
	d.emit(EventKeyProgress, task)
	d.storage.Put(bucketTask, task.ID, task.clone())

	// Extract the archive
	extractErr := extractArchive(archivePath, destDir, opts.ArchivePassword, func(extractedFiles int, totalFiles int, progress int) {
		task.dataLock.Lock()
		task.Progress.ExtractProgress = progress
		task.dataLock.Unlock()
		d.emit(EventKeyProgress, task)
	})

	d.handleExtractionResult(task, extractErr, []string{archivePath}, opts.DeleteAfterExtract)
}

// performMultiPartExtraction performs extraction for a multi-part archive
func (d *Downloader) performMultiPartExtraction(task *Task, firstPartPath string, destDir string, opts *http.OptsExtra) {
	// Get the baseName for releasing the claim later
	fullBaseName := GetMultiPartArchiveBaseName(firstPartPath)

	// Set extraction status to extracting
	task.dataLock.Lock()
	task.Progress.ExtractStatus = ExtractStatusExtracting
	task.Progress.ExtractProgress = 0
	task.dataLock.Unlock()
	d.emit(EventKeyProgress, task)
	d.storage.Put(bucketTask, task.ID, task.clone())

	d.Logger.Info().Msgf("starting multi-part archive extraction, first part: %s, task id: %s", firstPartPath, task.ID)

	// Extract the multi-part archive
	extractErr := extractMultiPartArchive(firstPartPath, destDir, opts.ArchivePassword, func(extractedFiles int, totalFiles int, progress int) {
		task.dataLock.Lock()
		task.Progress.ExtractProgress = progress
		task.dataLock.Unlock()
		d.emit(EventKeyProgress, task)
	})

	// Collect all part files for potential deletion
	partFiles := d.collectMultiPartFiles(firstPartPath)

	d.handleExtractionResult(task, extractErr, partFiles, opts.DeleteAfterExtract)

	// Update status for all related multi-part tasks
	d.updateMultiPartTasksStatus(task, extractErr)

	// Release the claim so future downloads of the same archive can be extracted
	d.releaseMultiPartExtractionClaim(fullBaseName)
}

// collectMultiPartFiles collects all files belonging to a multi-part archive
func (d *Downloader) collectMultiPartFiles(firstPartPath string) []string {
	var files []string
	partInfo := getArchivePartInfo(firstPartPath)
	dir := filepath.Dir(firstPartPath)

	switch {
	case strings.Contains(partInfo.Pattern, ".7z)"):
		// 7z: .7z.001, .7z.002, etc.
		files = d.collectSequentialFiles(dir, partInfo.BaseName, ".%03d")
	case strings.Contains(partInfo.Pattern, ".part"):
		// RAR new style
		files = d.collectRarNewStyleFiles(dir, partInfo.BaseName)
	case partInfo.Pattern == "rar-old-style" || strings.Contains(partInfo.Pattern, ".r("):
		// RAR old style
		files = d.collectRarOldStyleFiles(dir, partInfo.BaseName)
	case strings.Contains(partInfo.Pattern, ".zip)"):
		// ZIP multi-part
		files = d.collectSequentialFiles(dir, partInfo.BaseName, ".%03d")
	case strings.Contains(partInfo.Pattern, ".z("):
		// ZIP split
		files = d.collectZipSplitFiles(dir, partInfo.BaseName)
	}

	return files
}

// collectSequentialFiles collects sequential numbered files
func (d *Downloader) collectSequentialFiles(dir, baseName, format string) []string {
	var files []string
	suffix := filepath.Ext(baseName)
	nameWithoutExt := strings.TrimSuffix(baseName, suffix)
	partNum := 1

	for {
		partPath := filepath.Join(dir, nameWithoutExt+suffix+fmt.Sprintf(format, partNum))
		if _, err := os.Stat(partPath); os.IsNotExist(err) {
			break
		}
		files = append(files, partPath)
		partNum++
	}

	return files
}

// collectRarNewStyleFiles collects RAR new style part files
func (d *Downloader) collectRarNewStyleFiles(dir, baseName string) []string {
	var files []string
	partNum := 1

	for {
		singleDigitPath := filepath.Join(dir, baseName+fmt.Sprintf(".part%d.rar", partNum))
		doubleDigitPath := filepath.Join(dir, baseName+fmt.Sprintf(".part%02d.rar", partNum))

		if _, err := os.Stat(singleDigitPath); err == nil {
			files = append(files, singleDigitPath)
		} else if _, err := os.Stat(doubleDigitPath); err == nil {
			files = append(files, doubleDigitPath)
		} else {
			break
		}
		partNum++
	}

	return files
}

// collectRarOldStyleFiles collects RAR old style part files
func (d *Downloader) collectRarOldStyleFiles(dir, baseName string) []string {
	var files []string

	// .rar file
	rarPath := filepath.Join(dir, baseName+".rar")
	if _, err := os.Stat(rarPath); err == nil {
		files = append(files, rarPath)
	}

	// .r00, .r01, etc.
	partNum := 0
	for {
		partPath := filepath.Join(dir, baseName+fmt.Sprintf(".r%02d", partNum))
		if _, err := os.Stat(partPath); os.IsNotExist(err) {
			break
		}
		files = append(files, partPath)
		partNum++
	}

	return files
}

// collectZipSplitFiles collects ZIP split files
func (d *Downloader) collectZipSplitFiles(dir, baseName string) []string {
	var files []string

	// .z01, .z02, etc.
	partNum := 1
	for {
		partPath := filepath.Join(dir, baseName+fmt.Sprintf(".z%02d", partNum))
		if _, err := os.Stat(partPath); os.IsNotExist(err) {
			break
		}
		files = append(files, partPath)
		partNum++
	}

	// .zip file
	zipPath := filepath.Join(dir, baseName+".zip")
	if _, err := os.Stat(zipPath); err == nil {
		files = append(files, zipPath)
	}

	return files
}

// handleExtractionResult handles the result of an extraction operation
func (d *Downloader) handleExtractionResult(task *Task, extractErr error, archiveFiles []string, deleteAfterExtract bool) {
	if extractErr != nil {
		d.Logger.Error().Err(extractErr).Msgf("auto extract archive failed, task id: %s", task.ID)
		task.dataLock.Lock()
		task.Progress.ExtractStatus = ExtractStatusError
		task.dataLock.Unlock()
		d.emit(EventKeyProgress, task)
		d.storage.Put(bucketTask, task.ID, task.clone())
	} else {
		d.Logger.Info().Msgf("auto extract archive completed, task id: %s", task.ID)
		task.dataLock.Lock()
		task.Progress.ExtractStatus = ExtractStatusDone
		task.Progress.ExtractProgress = 100
		task.dataLock.Unlock()
		d.emit(EventKeyProgress, task)
		d.storage.Put(bucketTask, task.ID, task.clone())

		// Delete archive files after successful extraction if enabled
		if deleteAfterExtract {
			for _, archiveFile := range archiveFiles {
				deleteErr := os.Remove(archiveFile)
				if deleteErr != nil {
					d.Logger.Error().Err(deleteErr).Msgf("delete archive after extraction failed: %s", archiveFile)
				} else {
					d.Logger.Info().Msgf("archive deleted after extraction: %s", archiveFile)
				}
			}
		}
	}
}

// updateMultiPartTasksStatus updates the extraction status for all tasks that belong to the same multi-part archive
func (d *Downloader) updateMultiPartTasksStatus(sourceTask *Task, extractErr error) {
	sourceTask = sourceTask.clone()
	if sourceTask.Progress.MultiPartBaseName == "" {
		return
	}

	status := ExtractStatusDone
	progress := 100
	if extractErr != nil {
		status = ExtractStatusError
		progress = 0
	}

	for _, task := range d.taskList() {
		if task.ID == sourceTask.ID {
			continue
		}
		if task.clone().Progress.MultiPartBaseName == sourceTask.Progress.MultiPartBaseName {
			task.dataLock.Lock()
			task.Progress.ExtractStatus = status
			task.Progress.ExtractProgress = progress
			task.dataLock.Unlock()
			d.emit(EventKeyProgress, task)
			d.storage.Put(bucketTask, task.ID, task.clone())
		}
	}
}

func initTask(task *Task) {
	task.timer = util.NewTimer(task.Progress.Used)

	task.statusLock = &sync.Mutex{}
	task.lock = &sync.Mutex{}
	task.speedArr = make([]int64, 0)
	task.uploadSpeedArr = make([]int64, 0)
}

var defaultDownloader = NewDownloader(nil)

type boot struct {
	url      string
	extra    interface{}
	listener Listener
}

func (b *boot) URL(url string) *boot {
	b.url = url
	return b
}

func (b *boot) Extra(extra interface{}) *boot {
	b.extra = extra
	return b
}

func (b *boot) Listener(listener Listener) *boot {
	b.listener = listener
	return b
}

func (b *boot) Create(opts *base.Options) (string, error) {
	defaultDownloader.Listener(b.listener)
	return defaultDownloader.CreateDirect(&base.Request{
		URL:   b.url,
		Extra: b.extra,
	}, opts)
}

func Boot() *boot {
	err := defaultDownloader.Setup()
	if err != nil {
		panic(err)
	}
	return &boot{}
}

// A preparation can return after releasing task.lock and be preempted before
// cleanup. Only the captured generation may invalidate its owner. The status
// guard is the same admission point that assigns the next run's generation;
// it is released before the normal error/persistence/callback path.
func (d *Downloader) failTaskPreparation(task *Task, generation uint64, cancel context.CancelFunc, err error) {
	cancel()
	task.statusLock.Lock()
	if task.runGeneration == generation {
		invalidateTaskRecovery(task)
	}
	task.statusLock.Unlock()
	d.doOnError(task, err, generation)
}
