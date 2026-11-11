package download

import (
	"context"
	"encoding/base64"
	"errors"
	"sort"
	"strings"
	"time"
)

const MCPReadMaxPage = 100
const mcpReadMaxInteger int64 = 9007199254740991

var (
	ErrMCPReadInvalid     = errors.New("invalid read arguments")
	ErrMCPReadBusy        = errors.New("read snapshot busy")
	ErrMCPReadUnavailable = errors.New("read snapshot unavailable")
)

// MCPReadProgress contains only cached scalar progress. For a valid done
// snapshot, TotalBytes is the recorded final Downloaded value, not a resource
// size or integrity check. Otherwise it stays nil. No protocol getters are used.
type MCPReadProgress struct {
	DownloadedBytes           int64  `json:"downloadedBytes"`
	SpeedBytesPerSecond       int64  `json:"speedBytesPerSecond"`
	UploadedBytes             int64  `json:"uploadedBytes"`
	UploadSpeedBytesPerSecond int64  `json:"uploadSpeedBytesPerSecond"`
	TotalBytes                *int64 `json:"totalBytes"`
	Phase                     string `json:"phase"`
}
type MCPReadTask struct {
	ID        string          `json:"id"`
	Protocol  string          `json:"protocol"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"createdAt"`
	UpdatedAt string          `json:"updatedAt"`
	Progress  MCPReadProgress `json:"progress"`
}
type MCPReadPage struct {
	Tasks      []MCPReadTask `json:"tasks"`
	NextCursor string        `json:"nextCursor"`
}

// Only explicitly published observations are available. Missing fields are
// unknown, not zero; unavailable errors do not assert error-free history.
type MCPReadStats struct {
	Available     bool   `json:"available"`
	ActiveStreams *int64 `json:"activeStreams,omitempty"`
	TargetStreams *int64 `json:"targetStreams,omitempty"`
	Peers         *int64 `json:"peers,omitempty"`
}
type MCPReadErrors struct {
	Available  bool   `json:"available"`
	Category   string `json:"category,omitempty"`
	Retryable  *bool  `json:"retryable,omitempty"`
	OccurredAt string `json:"occurredAt,omitempty"`
}

type mcpReadReference struct {
	id   string
	task *Task
}

// MCPReadList uses ID keyset pagination, independent of queue order. Membership
// is copied once per request; fields are individual live snapshots, not a
// transaction across pages. New IDs <= the cursor appear on a new traversal;
// deletions and reordering never shift an offset into duplicate/skipped rows.
func (d *Downloader) MCPReadList(ctx context.Context, limit int, cursor string) (MCPReadPage, error) {
	empty := MCPReadPage{Tasks: []MCPReadTask{}}
	after, err := mcpReadCursor(cursor)
	if err != nil || limit < 1 || limit > MCPReadMaxPage {
		return empty, ErrMCPReadInvalid
	}
	refs, err := d.mcpReadMembers(ctx)
	if err != nil {
		return empty, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].id < refs[j].id })
	selected := make([]mcpReadReference, 0, limit+1)
	previous := ""
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		if !MCPReadValidID(ref.id) || ref.id == previous {
			return empty, ErrMCPReadUnavailable
		}
		previous = ref.id
		if ref.id > after && len(selected) <= limit {
			selected = append(selected, ref)
		}
	}
	page := MCPReadPage{Tasks: make([]MCPReadTask, 0, limit)}
	if len(selected) > limit {
		selected = selected[:limit]
		page.NextCursor = "v1_" + base64.RawURLEncoding.EncodeToString([]byte(selected[limit-1].id))
	}
	for _, ref := range selected {
		snapshot, err := mcpReadSnapshot(ctx, ref.task)
		if err != nil {
			return empty, err
		}
		page.Tasks = append(page.Tasks, snapshot)
	}
	return page, ctx.Err()
}

func (d *Downloader) MCPReadGetTask(ctx context.Context, id string) (MCPReadTask, error) {
	task, err := d.mcpReadFind(ctx, id)
	if err != nil {
		return MCPReadTask{}, err
	}
	return mcpReadSnapshot(ctx, task)
}
func (d *Downloader) MCPReadGetProgress(ctx context.Context, id string) (MCPReadProgress, error) {
	task, err := d.MCPReadGetTask(ctx, id)
	return task.Progress, err
}
func (d *Downloader) MCPReadGetStats(ctx context.Context, id string) (MCPReadStats, error) {
	task, err := d.mcpReadFind(ctx, id)
	if err != nil {
		return MCPReadStats{}, err
	}
	value, err := mcpReadPublishedSnapshot(ctx, task)
	if err != nil {
		return MCPReadStats{}, err
	}
	return mcpReadStatsDTO(value.Stats)
}
func (d *Downloader) MCPReadGetErrors(ctx context.Context, id string) (MCPReadErrors, error) {
	task, err := d.mcpReadFind(ctx, id)
	if err != nil {
		return MCPReadErrors{}, err
	}
	value, err := mcpReadPublishedSnapshot(ctx, task)
	if err != nil {
		return MCPReadErrors{}, err
	}
	return mcpReadErrorsDTO(value.Error)
}

// Main lock is held only to copy membership/immutable IDs. Never acquire a
// task/fetcher lock while holding it. TryLock avoids waiting behind Continue's
// synchronous lifecycle work. No retries, goroutines, queue or storage access.
func (d *Downloader) mcpReadMembers(ctx context.Context) ([]mcpReadReference, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d == nil || d.lock == nil || d.closed.Load() {
		return nil, ErrMCPReadUnavailable
	}
	if !d.lock.TryLock() {
		return nil, ErrMCPReadBusy
	}
	refs := make([]mcpReadReference, 0, len(d.tasks))
	for _, task := range d.tasks {
		if task == nil {
			d.lock.Unlock()
			return nil, ErrMCPReadUnavailable
		}
		refs = append(refs, mcpReadReference{id: task.ID, task: task})
	}
	d.lock.Unlock()
	return refs, ctx.Err()
}
func (d *Downloader) mcpReadFind(ctx context.Context, id string) (*Task, error) {
	if !MCPReadValidID(id) {
		return nil, ErrMCPReadInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d == nil || d.lock == nil || d.closed.Load() {
		return nil, ErrMCPReadUnavailable
	}
	if !d.lock.TryLock() {
		return nil, ErrMCPReadBusy
	}
	var found *Task
	for _, task := range d.tasks {
		if task != nil && task.ID == id {
			found = task
			break
		}
	}
	d.lock.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrTaskNotFound
	}
	return found, nil
}

func mcpReadSnapshot(ctx context.Context, task *Task) (MCPReadTask, error) {
	if err := ctx.Err(); err != nil {
		return MCPReadTask{}, err
	}
	if !task.dataLock.TryRLock() {
		return MCPReadTask{}, ErrMCPReadBusy
	}
	value := MCPReadTask{ID: task.ID, Protocol: task.Protocol, Status: string(task.Status)}
	created, updated := task.CreatedAt, task.UpdatedAt
	present := task.Progress != nil
	if present {
		value.Progress = MCPReadProgress{
			DownloadedBytes: task.Progress.Downloaded, SpeedBytesPerSecond: task.Progress.Speed,
			UploadedBytes: task.Progress.Uploaded, UploadSpeedBytesPerSecond: task.Progress.UploadSpeed,
			Phase: task.Progress.MediaPhase,
		}
	}
	task.dataLock.RUnlock()
	// Formatting/validation/serialization happens after releasing the task lock.
	if err := ctx.Err(); err != nil {
		return MCPReadTask{}, err
	}
	if !present || !MCPReadValidID(value.ID) || created.IsZero() || updated.IsZero() || created.Year() < 1 || created.Year() > 9999 || updated.Year() < 1 || updated.Year() > 9999 {
		return MCPReadTask{}, ErrMCPReadUnavailable
	}
	switch value.Status {
	case "ready", "wait", "running", "pause", "done", "error":
	default:
		return MCPReadTask{}, ErrMCPReadUnavailable
	}
	switch value.Protocol {
	case "http", "https", "bt", "ed2k", "media", "netdisk":
	default:
		value.Protocol = "unknown"
	}
	for _, n := range []int64{value.Progress.DownloadedBytes, value.Progress.SpeedBytesPerSecond, value.Progress.UploadedBytes, value.Progress.UploadSpeedBytesPerSecond} {
		if n < 0 || n > mcpReadMaxInteger {
			return MCPReadTask{}, ErrMCPReadUnavailable
		}
	}
	value.CreatedAt = created.UTC().Format(time.RFC3339Nano)
	value.UpdatedAt = updated.UTC().Format(time.RFC3339Nano)
	switch value.Progress.Phase {
	case "queued", "resolving", "downloading", "merging", "verifying", "publishing":
	default:
		value.Progress.Phase = "unknown"
	}
	if value.Status == "done" {
		value.Progress.Phase = "done"
		// Use the validated scalar copied with Status, never task or Meta.Res.
		total := value.Progress.DownloadedBytes
		value.Progress.TotalBytes = &total
	}
	return value, nil
}

func MCPReadValidID(id string) bool { return mcpReadToken(id, 64) }
func mcpReadToken(id string, max int) bool {
	if len(id) < 1 || len(id) > max {
		return false
	}
	for _, c := range id {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func mcpReadCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if !mcpReadToken(cursor, 128) || !strings.HasPrefix(cursor, "v1_") {
		return "", ErrMCPReadInvalid
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(cursor, "v1_"))
	if err != nil || !MCPReadValidID(string(decoded)) {
		return "", ErrMCPReadInvalid
	}
	return string(decoded), nil
}
