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
	Logger          *logger.Logger
	ExtensionLogger *logger.Logger

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
		cfg:          cfg,
		fetcherCache: make(map[string]fetcher.Fetcher),
		waitTasks:    make([]*Task, 0),
		storage:      cfg.Storage,

		lock:               &sync.Mutex{},
		fetcherMapLock:     &sync.RWMutex{},
		checkDuplicateLock: &sync.Mutex{},

		extensions:  make([]*Extension, 0),
		transports:  transfer.NewTransportPool(),
		budget:      transfer.NewBudget(),
		queueSignal: make(chan struct{}),
	}

	loggerSetupOnce.Do(func() { zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack })
	d.Logger = logger.NewLogger(cfg.ProductionMode, filepath.Join(cfg.StorageDir, "logs", "core.log"))
	d.ExtensionLogger = logger.NewLogger(cfg.ProductionMode, filepath.Join(cfg.StorageDir, "logs", "extension.log"))
	if cfg.ProductionMode {
		logPanic(filepath.Join(cfg.StorageDir, "logs"))
	}
	return d
}

func (d *Downloader) Setup() error {
	// setup storage
	if err := d.storage.Setup([]string{bucketTask, bucketSave, bucketProtocolState, bucketConfig, bucketExtension, bucketExtensionStorage}); err != nil {
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
			if task.getStatus() != base.DownloadStatusDone && task.getStatus() != base.DownloadStatusError {
				task.updateStatus(base.DownloadStatusPause)
			}
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
	d.cleanupNonExistingTasks()

	// Restore uploads only from this startup's task snapshot.
	uploadTasks := d.taskList()
	go func() {
		for _, task := range uploadTasks {
			task.lock.Lock()
			task.statusLock.Lock()
			if task.getStatus() == base.DownloadStatusDone && task.Uploading {
				if err := d.restoreTask(task); err != nil {
					d.Logger.Error().Stack().Err(err).Msgf("task upload restore fetcher failed, task id: %s", task.ID)
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
					func() {
						if d.getTask(task.ID) == nil || !task.lock.TryLock() {
							return
						}
						defer task.lock.Unlock()
						task.statusLock.Lock()
						defer task.statusLock.Unlock()
						if task.getStatus() != base.DownloadStatusRunning && !task.Uploading {
							return
						}
						// check if task is deleted
						if task.fetcher == nil {
							return
						}

						current := task.fetcher.Progress().TotalDownloaded()
						task.dataLock.Lock()
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
						d.emit(EventKeyProgress, task)

						// store fetcher progress when download/upload data changed
						if !downloadDataChanged && !uploadDataChanged {
							return
						}
						if task.Protocol == "netdisk" {
							d.saveActiveNetdiskTask(task)
						} else {
							d.saveTask(task)
						}
					}()
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
		if task.Meta == nil || task.Meta.Res == nil {
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
		if t.getStatus() == base.DownloadStatusWait && !t.notBeforeTime().After(now) {
			return true
		}
	}
	return false
}

// Serialize late netdisk progress/start saves with deletion. A removed task
// must never be recreated when asynchronous startup or polling finishes.
func (d *Downloader) saveActiveNetdiskTask(task *Task) error {
	lock := d.errorPersistenceLock(task)
	lock.Lock()
	defer lock.Unlock()
	if d.getTask(task.ID) != task {
		return nil
	}
	return d.saveTask(task)
}

func (d *Downloader) saveTask(task *Task) error {
	data, err := task.fetcherManager.Store(task.fetcher)
	if err != nil {
		d.Logger.Error().Stack().Err(err).Msgf("serialize fetcher failed: %s", task.ID)
		return err
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

	fetcher, err := d.buildFetcher(req)
	if err != nil {
		return
	}
	initOpt, err := d.initOptions(opts)
	if err != nil {
		return
	}
	err = fetcher.Resolve(req, initOpt)
	if err != nil {
		return
	}
	d.fetcherMapLock.Lock()
	d.fetcherCache[rrId] = fetcher
	d.fetcherMapLock.Unlock()
	rr = &ResolveResult{
		ID:  rrId,
		Res: fetcher.Meta().Res,
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
			if wt.getStatus() != base.DownloadStatusWait {
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
			d.startTask(wt, true)
			break
		}
	}()
}

func (d *Downloader) remainRunningCount() int {
	runningCount := 0
	for _, t := range d.tasks {
		if t.getStatus() == base.DownloadStatusRunning {
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
		status := task.getStatus()
		if (task.Protocol == "media" || task.Protocol == "netdisk") && status != base.DownloadStatusPause && status != base.DownloadStatusError {
			return errors.New("media task must be paused or failed before patching")
		}
		err := func() error {
			task.dataLock.Lock()
			defer task.dataLock.Unlock()
			if task.fetcher == nil {
				if err := d.restoreFetcher(task); err != nil {
					return err
				}
			}
			if err := task.fetcher.Patch(req, opts); err != nil {
				return err
			}
			task.Meta = task.fetcher.Meta()
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
	if filter == nil || filter.IsEmpty() {
		return d.continueAll()
	}

	filter.NotStatuses = []base.Status{base.DownloadStatusRunning, base.DownloadStatusDone}
	continueTasks := d.getTasksByFilter(filter)
	if len(continueTasks) == 0 {
		return ErrTaskNotFound
	}

	realContinueTasks := make([]*Task, 0)
	func() {
		d.lock.Lock()
		defer d.lock.Unlock()

		continueCount := len(continueTasks)
		remainRunningCount := d.remainRunningCount()
		needRunningCount := int(math.Min(float64(d.cfg.MaxRunning), float64(continueCount)))
		needPauseCount := needRunningCount - remainRunningCount
		if needPauseCount > 0 {
			pausedCount := 0
			for _, task := range d.tasks {
				if task.getStatus() == base.DownloadStatusRunning {
					if err = d.doPause(task); err != nil {
						return
					}
					task.updateStatus(base.DownloadStatusWait)
					d.enqueueLocked(task)
					pausedCount++
				}
				if pausedCount == needPauseCount {
					break
				}
			}
		}

		for _, task := range continueTasks {
			if len(realContinueTasks) < needRunningCount {
				realContinueTasks = append(realContinueTasks, task)
			} else {
				task.updateStatus(base.DownloadStatusWait)
				d.enqueueLocked(task)
			}
		}
	}()

	for _, task := range realContinueTasks {
		if err = d.doStart(task); err != nil {
			return
		}
	}

	return
}

// continueAll continue all tasks but does not affect tasks already running
func (d *Downloader) continueAll() (err error) {
	continuedTasks := make([]*Task, 0)

	func() {
		d.lock.Lock()
		defer d.lock.Unlock()
		// calculate how many tasks can be continued, can't exceed maxRunning
		remainCount := d.remainRunningCount()
		for _, task := range d.tasks {
			if task.getStatus() != base.DownloadStatusRunning && task.getStatus() != base.DownloadStatusDone {
				if len(continuedTasks) < remainCount {
					continuedTasks = append(continuedTasks, task)
				} else {
					task.updateStatus(base.DownloadStatusWait)
					d.enqueueLocked(task)
				}
			}
		}
	}()

	for _, task := range continuedTasks {
		if err = d.doStart(task); err != nil {
			return
		}
	}

	return
}

func (d *Downloader) ContinueBatch(filter *TaskFilter) (err error) {
	if filter == nil || filter.IsEmpty() {
		return d.continueAll()
	}

	continueTasks := d.getTasksByFilter(filter)
	for _, task := range continueTasks {
		if err = d.doStart(task); err != nil {
			return
		}
	}
	return
}

func (d *Downloader) Delete(filter *TaskFilter, force bool) (err error) {
	if filter == nil || filter.IsEmpty() {
		return d.deleteAll(force)
	}

	deleteTasks := d.getTasksByFilter(filter)
	if len(deleteTasks) == 0 {
		return
	}

	deleteIds := make([]string, 0)
	deleteTasksPtr := make([]*Task, 0)
	for _, task := range deleteTasks {
		deleteIds = append(deleteIds, task.ID)
		deleteTasksPtr = append(deleteTasksPtr, task)
	}
	func() {
		d.lock.Lock()
		defer d.lock.Unlock()

		for _, id := range deleteIds {
			for i, t := range d.tasks {
				if t.ID == id {
					d.tasks = append(d.tasks[:i], d.tasks[i+1:]...)
					break
				}
			}
			for i, t := range d.waitTasks {
				if t.ID == id {
					d.dequeueLocked(i)
					break
				}
			}
		}
	}()

	for _, task := range deleteTasksPtr {
		err = d.doDelete(task, force)
		if err != nil {
			return
		}
	}

	d.notifyRunning()
	return
}

func (d *Downloader) deleteAll(force bool) (err error) {
	var deleteTasksTemp []*Task
	func() {
		d.lock.Lock()
		defer d.lock.Unlock()

		for _, task := range d.tasks {
			deleteTasksTemp = append(deleteTasksTemp, task)
		}
		d.tasks = make([]*Task, 0)
		d.clearQueueLocked()
	}()

	for _, task := range deleteTasksTemp {
		if err = d.doDelete(task, force); err != nil {
			return
		}
	}
	return
}

func (d *Downloader) Stats(id string) (sr any, err error) {
	task := d.getTask(id)
	if task == nil {
		return sr, ErrTaskNotFound
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
			if err := task.fetcher.Close(); err != nil {
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
	d.emit(EventKeyDelete, task)
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
	d.closed.Store(true)

	closeArr := []func() error{
		d.pauseAll,
		// Pause persists resumable state; Close then releases netdisk Wait
		// callers before its shared service and the storage backend are closed.
		d.closeNetdiskFetchers,
	}
	for _, fm := range d.cfg.FetchManagers {
		closeArr = append(closeArr, fm.Close)
	}
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
	return d.cfg.DownloaderStoreConfig, nil
}

func (d *Downloader) PutConfig(v *base.DownloaderStoreConfig) error {
	d.cfg.DownloaderStoreConfig = v
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
	if d.closed.Load() || d.getTask(task.ID) != task {
		return
	}
	if _, loaded := d.watchedTasks.LoadOrStore(task.ID, true); loaded {
		return
	}

	defer func() {
		d.watchedTasks.Delete(task.ID)
	}()

	task.statusLock.Lock()
	f := task.fetcher
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

	err := f.Wait()
	if d.closed.Load() || d.getTask(task.ID) != task {
		return
	}
	if err != nil {
		d.doOnError(task, err)
		return
	}

	task.lock.Lock()
	task.statusLock.Lock()
	if task.getStatus() != base.DownloadStatusRunning || task.Meta.Res == nil {
		task.statusLock.Unlock()
		task.lock.Unlock()
		return
	}
	task.dataLock.Lock()
	if task.Protocol == "media" || task.Protocol == "netdisk" {
		task.Meta = util.DeepClone(f.Meta())
	}
	task.Progress.Used = task.timer.Used()
	if task.Meta.Res.Size == 0 {
		task.Meta.Res.Size = f.Progress().TotalDownloaded()
	}
	used := task.Progress.Used / int64(time.Second)
	if used == 0 {
		used = 1
	}
	totalSize := task.Meta.Res.Size
	task.Progress.Speed = totalSize / used
	task.Progress.Downloaded = totalSize
	task.dataLock.Unlock()
	task.updateStatus(base.DownloadStatusDone)
	d.storage.Put(bucketTask, task.ID, task.clone())
	task.statusLock.Unlock()
	task.lock.Unlock()
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

		if autoTorrentEnabled && strings.HasSuffix(downloadFilePath, ".torrent") {
			// Determine if should delete torrent file after creating BT task
			shouldDelete := false
			if e.DeleteTorrentAfterDownload != nil {
				shouldDelete = *e.DeleteTorrentAfterDownload
			} else if cfg != nil && cfg.AutoTorrent != nil {
				shouldDelete = cfg.AutoTorrent.DeleteAfterDownload
			}

			go func() {
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
		if autoExtractEnabled && isArchiveFile(downloadFilePath) {
			d.enqueueExtraction(task, downloadFilePath, e)
		}
	}
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
		d.Logger.Warn().Err(err).Msgf("task download failed, task id: %s", task.ID)
		task.updateStatus(base.DownloadStatusError)
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
		task.updateStatus(base.DownloadStatusWait)
		task.dataLock.Lock()
		task.notBefore = deferred.Until
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
	if task.fetcher == nil {
		if err := d.restoreFetcher(task); err != nil {
			return err
		}
	}
	go d.watch(task)
	return nil
}

func (d *Downloader) restoreFetcher(task *Task) error {
	v, f := task.fetcherManager.Restore()
	if v != nil {
		// Keep the durable checkpoint until the next successful save replaces
		// it. A crash in OnStart/Start must not erase the last resumable state.
		_, err := d.storage.Get(bucketSave, task.ID, v)
		if err != nil {
			return err
		}
	}
	task.fetcher = f(util.DeepClone(task.Meta), v)
	if task.fetcher == nil {
		task.fetcher = task.fetcherManager.Build()
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

	go d.watch(task)
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
	var generation uint64
	isReturn, err := d.statusMut(task, func() (isReturn bool, err error) {
		if onlyWaiting && task.getStatus() != base.DownloadStatusWait ||
			task.getStatus() == base.DownloadStatusRunning || task.getStatus() == base.DownloadStatusDone {
			isReturn = true
			return
		}

		err = d.restoreTask(task)
		if err != nil {
			d.Logger.Error().Stack().Err(err).Msgf("restore fetcher failed, task id: %s", task.ID)
			return
		}
		task.dataLock.RLock()
		needCreate = !task.IsCreated
		task.dataLock.RUnlock()
		task.runGeneration++
		generation = task.runGeneration
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

	handler := func() error {
		task.lock.Lock()
		defer task.lock.Unlock()
		task.statusLock.Lock()
		obsolete := task.getStatus() != base.DownloadStatusRunning ||
			task.runGeneration != generation || d.closed.Load()
		task.statusLock.Unlock()
		if obsolete || d.getTask(task.ID) != task {
			return nil
		}

		startTask := task.clone()
		d.triggerOnStart(startTask)
		task.dataLock.Lock()
		task.Meta.Req = startTask.Meta.Req
		task.dataLock.Unlock()
		// Resolve owns its request/options. Keep the published task view detached
		// so Pause can cancel resolution without waiting for a snapshot lock.
		task.dataLock.RLock()
		resolveMeta := util.DeepClone(task.Meta)
		task.dataLock.RUnlock()
		if resolveMeta.Res == nil {
			if err := task.fetcher.Resolve(resolveMeta.Req, resolveMeta.Opts); err != nil {
				return err
			}
		}
		task.statusLock.Lock()
		paused := task.getStatus() != base.DownloadStatusRunning || task.runGeneration != generation || d.closed.Load()
		task.statusLock.Unlock()
		if paused {
			return nil
		}
		// Keep rename reservation until Start has created the target file.
		if needCreate && task.fetcherManager.AutoRename() {
			d.checkDuplicateLock.Lock()
			defer d.checkDuplicateLock.Unlock()
		}
		if err := func() error {
			task.dataLock.Lock()
			defer task.dataLock.Unlock()
			task.Meta = task.fetcher.Meta()
			if resolveMeta.Res != nil {
				// Apply request changes made by onStart extensions to resolved tasks.
				task.Meta.Req = resolveMeta.Req
			}

			if needCreate {
				if task.fetcherManager.AutoRename() {
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

		task.statusLock.Lock()
		if task.getStatus() != base.DownloadStatusRunning || task.runGeneration != generation || d.closed.Load() {
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
			saveErr = d.saveActiveNetdiskTask(task)
		} else {
			saveErr = d.saveTask(task)
		}
		if saveErr != nil {
			return saveErr
		}
		if d.getTask(task.ID) != task {
			return nil
		}
		d.emit(EventKeyStart, task)
		return nil
	}
	go func() {
		if err := handler(); err != nil {
			d.doOnError(task, err, generation)
		}
	}()

	return
}

func (d *Downloader) doPause(task *Task) (err error) {
	var pauseDone chan struct{}
	var previousPauseDone chan struct{}
	isReturn, err := d.statusMut(task, func() (isReturn bool, err error) {
		if task.getStatus() == base.DownloadStatusPause || task.getStatus() == base.DownloadStatusDone ||
			(task.getStatus() == base.DownloadStatusError && d.closed.Load()) {
			pauseDone = task.pauseDone
			isReturn = true
			return
		}

		previousPauseDone = task.pauseDone
		pauseDone = make(chan struct{})
		task.pauseDone = pauseDone
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
			task.lock.Lock()
			defer task.lock.Unlock()

			if task.fetcher != nil {
				if err := task.fetcher.Pause(); err != nil {
					return err
				}
			}
			if task.fetcherManager != nil && task.fetcher != nil {
				if err := d.saveTask(task); err != nil {
					return err
				}
			} else {
				if err := d.storage.Put(bucketTask, task.ID, task.clone()); err != nil {
					return err
				}
			}
			return nil
		}()
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
	if (task.Protocol == "media" || task.Protocol == "netdisk") || d.closed.Load() {
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
