package download

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/util"
)

var (
	ErrHandoffInvalid  = errors.New("handoff_invalid")
	ErrHandoffConflict = errors.New("handoff_conflict")
	ErrHandoffHeld     = errors.New("handoff_pending")
	handoffID          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// BrowserHandoff belongs to the existing task record. Missing metadata retains
// the ordinary task lifecycle; no second task store or download worker exists.
type BrowserHandoff struct {
	Version     int                    `json:"version"`
	OperationID string                 `json:"operationId"`
	ClientID    string                 `json:"clientId"`
	Browser     string                 `json:"browser"`
	Download    BrowserDownloadBinding `json:"download"`
	Phase       string                 `json:"phase"`
	RequestHash string                 `json:"requestHash"`
	Reason      string                 `json:"reason,omitempty"`
}
type BrowserDownloadBinding struct {
	ID        int    `json:"id"`
	StartTime string `json:"startTime"`
	URLHash   string `json:"urlHash"`
}
type BrowserDownloadState struct {
	State string `json:"state"`
	Error string `json:"error"`
}
type BrowserHandoffRequest struct {
	PathCreate      *HTTPPathCreateRequest `json:"pathCreate,omitempty"`
	DatasetID       string                 `json:"datasetId,omitempty"`
	Action          string                 `json:"action"`
	OperationID     string                 `json:"operationId"`
	ClientID        string                 `json:"clientId"`
	Browser         string                 `json:"browser"`
	Download        BrowserDownloadBinding `json:"download"`
	Req             *base.Request          `json:"req,omitempty"`
	Opts            *base.Options          `json:"opts,omitempty"`
	TaskID          string                 `json:"taskId,omitempty"`
	BrowserState    BrowserDownloadState   `json:"browserState"`
	CancelConfirmed bool                   `json:"cancelConfirmed"`
	Reason          string                 `json:"reason,omitempty"`
	Pending         bool                   `json:"pending,omitempty"`
}
type BrowserPathHandoffProjection struct {
	Version              int      `json:"version"`
	DatasetID            string   `json:"datasetId"`
	Kind                 string   `json:"kind"`
	RequiredCapabilities []string `json:"requiredCapabilities"`
}

type BrowserHandoffResult struct {
	PathCreate  *BrowserPathHandoffProjection `json:"pathCreate,omitempty"`
	Version     int                           `json:"version"`
	OperationID string                        `json:"operationId"`
	State       string                        `json:"state"`
	TaskID      string                        `json:"taskId,omitempty"`
	TaskIDs     []string                      `json:"taskIds"`
	Download    *BrowserDownloadBinding       `json:"download,omitempty"`
	Reason      string                        `json:"reason,omitempty"`
	TaskStatus  base.Status                   `json:"taskStatus,omitempty"`
}

func (t *Task) handoffHeld() bool {
	t.dataLock.RLock()
	defer t.dataLock.RUnlock()
	return t.Handoff != nil && (t.Handoff.Version != 1 || t.Handoff.Phase != "committed")
}
func handoffEligible(tasks []*Task) []*Task {
	out := make([]*Task, 0, len(tasks))
	for _, t := range tasks {
		if !t.handoffHeld() {
			out = append(out, t)
		}
	}
	return out
}
func validHandoffIdentity(r BrowserHandoffRequest) bool {
	return handoffID.MatchString(r.ClientID) && (r.Browser == "chrome" || r.Browser == "edge")
}
func validDownloadBinding(d BrowserDownloadBinding) bool {
	_, err := time.Parse(time.RFC3339Nano, d.StartTime)
	decoded, e := hex.DecodeString(d.URLHash)
	return d.ID >= 0 && err == nil && e == nil && len(decoded) == sha256.Size && strings.ToLower(d.URLHash) == d.URLHash
}
func handoffResult(task *Task, op string) BrowserHandoffResult {
	result := BrowserHandoffResult{Version: 1, OperationID: op, State: "unknown", TaskIDs: []string{}}
	if task == nil {
		return result
	}
	task.dataLock.RLock()
	defer task.dataLock.RUnlock()
	if task.Handoff == nil {
		return result
	}
	binding := task.Handoff.Download
	result.State = task.Handoff.Phase
	result.TaskID = task.ID
	result.TaskIDs = []string{task.ID}
	result.Download = &binding
	result.Reason = task.Handoff.Reason
	result.TaskStatus = task.Status
	return result
}
func (d *Downloader) findHandoff(op string) *Task {
	for _, t := range d.taskList() {
		t.dataLock.RLock()
		same := t.Handoff != nil && t.Handoff.OperationID == op
		t.dataLock.RUnlock()
		if same {
			return t
		}
	}
	return nil
}
func handoffMatches(task *Task, r BrowserHandoffRequest) bool {
	task.dataLock.RLock()
	defer task.dataLock.RUnlock()
	h := task.Handoff
	return h != nil && h.Version == 1 && h.ClientID == r.ClientID && h.Browser == r.Browser && (r.TaskID == "" || r.TaskID == task.ID)
}

// BrowserHandoff applies one operation to an existing persisted task. Its mutex
// serializes only these short local transitions, never HTTP resolution or IO.
func (d *Downloader) BrowserHandoff(r BrowserHandoffRequest) (BrowserHandoffResult, error) {
	result := handoffResult(nil, r.OperationID)
	if !validHandoffIdentity(r) || !handoffID.MatchString(r.OperationID) {
		return result, ErrHandoffInvalid
	}
	d.handoffMu.Lock()
	defer d.handoffMu.Unlock()
	if d.closed.Load() {
		return result, ErrHandoffConflict
	}
	task := d.findHandoff(r.OperationID)
	if task != nil && !handoffMatches(task, r) {
		return result, ErrHandoffConflict
	}
	if r.PathCreate != nil || r.DatasetID != "" || task != nil && task.PathFreeze != nil {
		return d.browserPathHandoff(r, task)
	}
	switch r.Action {
	case "prepare":
		if r.Req == nil || !validDownloadBinding(r.Download) {
			return result, ErrHandoffInvalid
		}
		u, err := url.Parse(r.Req.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return result, ErrHandoffInvalid
		}
		digest := sha256.Sum256([]byte(r.Req.URL))
		if hex.EncodeToString(digest[:]) != r.Download.URLHash {
			return result, ErrHandoffInvalid
		}
		// Ordinary capture is GET-only. Do not turn a captured POST/body into GET.
		raw, err := json.Marshal(r.Req.Extra)
		if err != nil {
			return result, ErrHandoffInvalid
		}
		var extra struct {
			Method string          `json:"method"`
			Body   json.RawMessage `json:"body"`
		}
		if json.Unmarshal(raw, &extra) != nil || extra.Method != "" && extra.Method != "GET" || len(extra.Body) > 0 && string(extra.Body) != "null" && string(extra.Body) != `""` {
			return result, ErrHandoffInvalid
		}
		fm, err := d.parseRequestFm(r.Req)
		if err != nil || fm.Name() != "http" {
			return result, ErrHandoffInvalid
		}
		encoded, err := json.Marshal(struct {
			Req  *base.Request
			Opts *base.Options
		}{r.Req, r.Opts})
		if err != nil {
			return result, ErrHandoffInvalid
		}
		fingerprint := sha256.Sum256(encoded)
		requestHash := hex.EncodeToString(fingerprint[:])
		if task != nil {
			task.dataLock.RLock()
			same := task.Handoff.Download == r.Download && task.Handoff.RequestHash == requestHash
			task.dataLock.RUnlock()
			if !same {
				return result, ErrHandoffConflict
			}
			return handoffResult(task, r.OperationID), nil
		}
		opts, err := d.initOptions(r.Opts)
		if err != nil {
			return result, ErrHandoffInvalid
		}
		// No Build/Setup/Resolve/Start/watch/queue/output path belongs to prepare.
		task = NewTask()
		task.Protocol = "http"
		task.Meta = &fetcher.FetcherMeta{Req: util.DeepClone(r.Req), Opts: opts}
		task.Progress = &Progress{}
		task.Status = base.DownloadStatusPause
		task.fetcherManager = fm
		task.Handoff = &BrowserHandoff{Version: 1, OperationID: r.OperationID, ClientID: r.ClientID, Browser: r.Browser, Download: r.Download, Phase: "prepared", RequestHash: requestHash}
		initTask(task)
		if err = d.storage.Put(bucketTask, task.ID, task.clone()); err != nil {
			return result, err
		}
		d.lock.Lock()
		d.tasks = append(d.tasks, task)
		d.lock.Unlock()
		return handoffResult(task, r.OperationID), nil
	case "query":
		return handoffResult(task, r.OperationID), nil
	case "abort":
		if r.Reason != "browser_completed" && r.Reason != "user_cancelled" && r.Reason != "browser_retained" {
			return result, ErrHandoffInvalid
		}
		if task == nil {
			return result, nil
		}
		if task.Handoff.Phase == "committed" {
			return handoffResult(task, r.OperationID), ErrHandoffConflict
		}
		if task.Handoff.Phase == "aborted" {
			return handoffResult(task, r.OperationID), nil
		}
		return d.persistHandoff(task, "aborted", r.Reason)
	case "commit":
		if task == nil {
			return result, nil
		}
		if r.TaskID == "" || !r.CancelConfirmed || r.Download != task.Handoff.Download || r.BrowserState.State != "interrupted" || r.BrowserState.Error != "USER_CANCELED" {
			return result, ErrHandoffInvalid
		}
		if task.Handoff.Phase == "aborted" {
			return handoffResult(task, r.OperationID), ErrHandoffConflict
		}
		if task.Handoff.Phase == "committed" {
			return handoffResult(task, r.OperationID), nil
		}
		result, err := d.persistHandoff(task, "committed", "")
		if err != nil || result.State != "committed" {
			return result, err
		}
		// Reuse ordinary creation's queue policy. This does not preempt other tasks.
		d.lock.Lock()
		if d.getTaskLocked(task.ID) == task {
			if d.remainRunningCount() == 0 {
				task.updateStatus(base.DownloadStatusWait)
				d.enqueueLocked(task)
			} else {
				err = d.doStart(task)
			}
		}
		d.lock.Unlock()
		if err != nil {
			d.Logger.Error().Err(err).Str("taskId", task.ID).Msg("handoff committed; task requires recovery")
		}
		// Ownership is already durable even if startup itself reports an error.
		return handoffResult(task, r.OperationID), nil
	default:
		return result, ErrHandoffInvalid
	}
}

// Persist the next phase before exposing it to any start/continue path. A failed
// write leaves the in-memory gate held as well as the durable record unchanged.
func (d *Downloader) persistHandoff(task *Task, phase, reason string) (BrowserHandoffResult, error) {
	persistence := d.errorPersistenceLock(task)
	persistence.Lock()
	defer persistence.Unlock()
	if d.getTask(task.ID) != task {
		return handoffResult(nil, task.Handoff.OperationID), nil
	}
	task.statusLock.Lock()
	defer task.statusLock.Unlock()
	snapshot := task.clone()
	snapshot.Handoff.Phase = phase
	snapshot.Handoff.Reason = reason
	snapshot.UpdatedAt = time.Now()
	if err := d.storage.Put(bucketTask, task.ID, snapshot); err != nil {
		return handoffResult(task, task.Handoff.OperationID), err
	}
	task.dataLock.Lock()
	task.Handoff = snapshot.Handoff
	task.UpdatedAt = snapshot.UpdatedAt
	task.dataLock.Unlock()
	return handoffResult(task, task.Handoff.OperationID), nil
}

func (d *Downloader) PendingBrowserHandoffs(clientID, browser string) ([]BrowserHandoffResult, error) {
	r := BrowserHandoffRequest{ClientID: clientID, Browser: browser}
	if !validHandoffIdentity(r) {
		return nil, ErrHandoffInvalid
	}
	d.handoffMu.Lock()
	defer d.handoffMu.Unlock()
	out := []BrowserHandoffResult{}
	for _, task := range d.taskList() {
		task.dataLock.RLock()
		h := task.Handoff
		match := h != nil && h.ClientID == clientID && h.Browser == browser && h.Phase == "prepared"
		task.dataLock.RUnlock()
		if match {
			if task.PathFreeze != nil {
				stored, _, projection, err := d.readBrowserPathHandoff(BrowserHandoffRequest{Action: "query", ClientID: clientID, Browser: browser, OperationID: h.OperationID})
				if err != nil {
					return nil, err
				}
				out = append(out, browserPathResult(stored, h.OperationID, projection))
			} else {
				out = append(out, handoffResult(task, h.OperationID))
			}
		}
	}
	return out, nil
}
