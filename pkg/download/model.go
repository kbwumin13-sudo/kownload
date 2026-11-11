package download

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/protocol/bt"
	"github.com/GopeedLab/gopeed/internal/protocol/ed2k"
	"github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/protocol/media"
	"github.com/GopeedLab/gopeed/internal/protocol/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/util"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

type ResolveResult struct {
	ID  string         `json:"id"`
	Res *base.Resource `json:"res"`
}

// TaskCompletionRef declares required completion state. It carries no phase,
// digest result or execution permission; those require the atomic three-way
// state guard before this optional reference can be acted on.
type TaskCompletionRef struct {
	Version      int    `json:"version"`
	CompletionID string `json:"completionId"`
}

type Task struct {
	Completion        *TaskCompletionRef   `json:"completion,omitempty"`
	Recovery          *TaskRecoveryRef     `json:"recovery,omitempty"`
	Handoff           *BrowserHandoff      `json:"handoff,omitempty"`
	CreationOperation *CreationOperation   `json:"creationOperation,omitempty"`
	ID                string               `json:"id"`
	Protocol          string               `json:"protocol"`
	Meta              *fetcher.FetcherMeta `json:"meta"`
	Status            base.Status          `json:"status"`
	Uploading         bool                 `json:"uploading"`
	Progress          *Progress            `json:"progress"`
	IsCreated         bool                 `json:"isCreated"`
	CreatedAt         time.Time            `json:"createdAt"`
	UpdatedAt         time.Time            `json:"updatedAt"`

	fetcherManager fetcher.FetcherManager
	fetcher        fetcher.Fetcher
	timer          *util.Timer
	// dataLock protects the serialized task view; statusLock serializes lifecycle decisions.
	dataLock sync.RWMutex
	// Ephemeral, copied/published only while holding dataLock. Never serialized.
	mcpPublished MCPReadPublishedSnapshot
	statusLock   *sync.Mutex
	lock         *sync.Mutex
	// Guarded by statusLock; invalidates queued start work after a pause.
	runGeneration  uint64
	runCancel      context.CancelFunc // Guarded by statusLock; cancels preparation for this generation.
	pauseDone      chan struct{}      // Guarded by statusLock; closes after pause persistence.
	speedArr       []int64
	uploadSpeedArr []int64
	// In memory only, guarded by dataLock: a rate-limited task stays queued
	// until notBefore; queuedAt/queueWait measure time spent waiting for a slot.
	notBefore time.Time
	queuedAt  time.Time
	queueWait time.Duration
	// Set on startup from the durable reverse index, even if an old reader
	// discarded CreationOperation while saving the task.
	creationBlocked bool
	recoveryBlocked bool
	recoveryOwner   atomic.Pointer[taskRecoveryOwner]
	// Q receives this one-shot authority only from an acknowledged M in this
	// process. It is intentionally absent from persisted/public Task JSON.
	initialHTTPExecution atomic.Bool
	// One-shot enrollment evidence, absent from JSON. Historical/legacy IO
	// cannot manufacture a free initial attempt when first joining recovery.
	recoveryInitialEligible atomic.Bool
	// Read only atomically by Delete: never wait on statusLock before removing
	// membership while a completion Put may be blocked under that lock.
	recoveryRunCancel atomic.Pointer[context.CancelFunc]
	// Network writers have joined; the same run may still be verifying. No
	// second Task/queue or persisted status is created for the bounded hash slot.
	verificationOnly atomic.Bool
	// Fresh C permission is process-local and survives only this Task membership.
	verifiedSuccess atomic.Pointer[verifiedSuccessPermit]
}

func NewTask() *Task {
	id, err := gonanoid.New()
	if err != nil {
		panic(err)
	}
	return &Task{
		ID:        id,
		Status:    base.DownloadStatusReady,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		IsCreated: false,
	}
}

// Name returns the display name of the task.
func (t *Task) Name() string {
	t.dataLock.RLock()
	defer t.dataLock.RUnlock()
	return t.nameLocked()
}

func (t *Task) nameLocked() string {
	if t.Meta == nil || t.Meta.Opts == nil {
		return "unknown"
	}
	// Custom name first
	if t.Meta.Opts.Name != "" {
		return t.Meta.Opts.Name
	}

	// Task is not resolved, parse the name from the URL
	if t.Meta.Res == nil {
		fallbackName := "unknown"
		if t.fetcherManager == nil {
			return fallbackName
		}
		parseName := t.fetcherManager.ParseName(t.Meta.Req.URL)
		if parseName == "" {
			return fallbackName
		}
		return parseName
	}

	// Task is a folder
	if t.Meta.Res.Name != "" {
		return t.Meta.Res.Name
	}

	// Get the name of the first file
	if len(t.Meta.Res.Files) == 0 {
		return "unknown"
	}
	return t.Meta.Res.Files[0].Name
}

func (t *Task) MarshalJSON() ([]byte, error) {
	t.dataLock.RLock()
	defer t.dataLock.RUnlock()
	// An explicit view avoids copying mutexes and preserves the public JSON shape.
	return json.Marshal(struct {
		ID                string               `json:"id"`
		Protocol          string               `json:"protocol"`
		Meta              *fetcher.FetcherMeta `json:"meta"`
		Status            base.Status          `json:"status"`
		Uploading         bool                 `json:"uploading"`
		Progress          *Progress            `json:"progress"`
		IsCreated         bool                 `json:"isCreated"`
		CreatedAt         time.Time            `json:"createdAt"`
		UpdatedAt         time.Time            `json:"updatedAt"`
		Name              string               `json:"name"`
		Handoff           *BrowserHandoff      `json:"handoff,omitempty"`
		CreationOperation *CreationOperation   `json:"creationOperation,omitempty"`
		Recovery          *TaskRecoveryRef     `json:"recovery,omitempty"`
		Completion        *TaskCompletionRef   `json:"completion,omitempty"`
	}{t.ID, t.Protocol, t.Meta, t.Status, t.Uploading, t.Progress,
		t.IsCreated, t.CreatedAt, t.UpdatedAt, t.nameLocked(), t.Handoff, t.CreationOperation, t.Recovery, t.Completion})
}

func (t *Task) getStatus() base.Status {
	t.dataLock.RLock()
	defer t.dataLock.RUnlock()
	return t.Status
}

func (t *Task) updateStatus(status base.Status) {
	t.dataLock.Lock()
	defer t.dataLock.Unlock()
	t.updateStatusLocked(status)
}

// Caller holds dataLock, allowing status and its published observation to
// change in one snapshot while preserving queue-time bookkeeping.
func (t *Task) updateStatusLocked(status base.Status) {
	now := time.Now()
	if t.Status == base.DownloadStatusWait && status != base.DownloadStatusWait && !t.queuedAt.IsZero() {
		t.queueWait += now.Sub(t.queuedAt)
		t.queuedAt = time.Time{}
	}
	if status == base.DownloadStatusWait && t.Status != base.DownloadStatusWait {
		t.queuedAt = now
	}
	if status != base.DownloadStatusWait {
		t.notBefore = time.Time{}
	}
	t.UpdatedAt = now
	t.Status = status
}

// queueTime is the total time this task waited for a running slot in this process.
func (t *Task) queueTime() time.Duration {
	t.dataLock.RLock()
	defer t.dataLock.RUnlock()
	wait := t.queueWait
	if t.Status == base.DownloadStatusWait && !t.queuedAt.IsZero() {
		wait += time.Since(t.queuedAt)
	}
	return wait
}

func (t *Task) notBeforeTime() time.Time {
	t.dataLock.RLock()
	defer t.dataLock.RUnlock()
	return t.notBefore
}

func (t *Task) clone() *Task {
	if t == nil {
		return nil
	}
	snapshot := util.DeepClone(t)
	snapshot.fetcherManager = t.fetcherManager
	return snapshot
}

func (t *Task) updateSpeed(downloaded int64, usedTime float64) int64 {
	return calcSpeed(&t.speedArr, downloaded, usedTime)
}

func (t *Task) updateUploadSpeed(downloaded int64, usedTime float64) int64 {
	return calcSpeed(&t.uploadSpeedArr, downloaded, usedTime)
}

func calcSpeed(speedArr *[]int64, downloaded int64, usedTime float64) int64 {
	if usedTime <= 0 {
		return 0
	}
	if downloaded < 0 {
		*speedArr = (*speedArr)[:0]
		return 0
	}

	*speedArr = append(*speedArr, downloaded)
	// Record last 5 seconds of download speed to calculate the average speed
	if len(*speedArr) > int(5.0/usedTime) {
		*speedArr = (*speedArr)[1:]
	}

	var total int64
	for _, v := range *speedArr {
		total += v
	}

	return int64(float64(total) / float64(len(*speedArr)) / usedTime)
}

type TaskFilter struct {
	IDs         []string
	Statuses    []base.Status
	NotStatuses []base.Status
}

func (f *TaskFilter) IsEmpty() bool {
	return len(f.IDs) == 0 && len(f.Statuses) == 0 && len(f.NotStatuses) == 0
}

type DownloaderConfig struct {
	Controller    *controller.Controller
	FetchManagers []fetcher.FetcherManager

	RefreshInterval   int // RefreshInterval time duration to refresh task progress(ms)
	Storage           Storage
	StorageDir        string
	WhiteDownloadDirs []string
	// StartupTaskPolicy is launch-scoped, never part of DownloaderStoreConfig.
	StartupTaskPolicy StartupTaskPolicy `json:"startupTaskPolicy,omitempty"`

	ProductionMode bool

	*base.DownloaderStoreConfig
}

func (cfg *DownloaderConfig) Init() *DownloaderConfig {
	if cfg.Controller == nil {
		cfg.Controller = controller.NewController()
	}
	if len(cfg.FetchManagers) == 0 {
		cfg.FetchManagers = []fetcher.FetcherManager{
			new(netdisk.FetcherManager),
			new(media.FetcherManager),
			new(http.FetcherManager),
			new(bt.FetcherManager),
			new(ed2k.FetcherManager),
		}
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 350
	}
	if cfg.Storage == nil {
		cfg.Storage = NewMemStorage()
	}
	return cfg
}
