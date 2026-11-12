package download

import (
	"context"
	"net/url"
	"path"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/util"
	gonanoid "github.com/matoous/go-nanoid/v2"
)

// These receivers expose the strict R2 variants. Neither their receipts nor a
// preview authorizes output IO; the runner still requires F and identity ACKs.
func (d *Downloader) PathCapabilityForDataset(dataset, kind string, required []string) (HTTPPathCapability, error) {
	v, ok := pathVariantForCaps(required)
	if !ok || v.Kind != kind {
		return HTTPPathCapability{}, ErrPathFreezeInvalid
	}
	if d.closed.Load() {
		return HTTPPathCapability{}, ErrPathFreezeUnavailable
	}
	if err := withDatasetReadView(d.storage, dataset, func(datasetReader) error { return nil }); err != nil {
		return HTTPPathCapability{}, err
	}
	_, atomic := d.storage.(AtomicStorage)
	supported := atomic && d.pathExecutionAvailable(v.Protocol)
	if len(required) == 2 {
		sha, err := d.VerifiedHTTPCapabilityForDataset(dataset)
		if err != nil {
			return HTTPPathCapability{}, err
		}
		supported = supported && sha.Supported
	}
	methods := HTTPPathMethods{"unsupported", "unsupported", "unsupported", "unsupported"}
	if supported {
		methods = HTTPPathMethods{"POST", "GET", "POST", "POST"}
	}
	return HTTPPathCapability{1, dataset, kind, 1, supported, append([]string{}, required...), methods}, nil
}

func (d *Downloader) QueryPathOperationForDataset(dataset, kind, operationID string) (HTTPPathOperationResult, error) {
	result, _, err := d.readPathOperation(dataset, kind, operationID)
	return result, err
}

// A single owned manifest is shared by all member checks. This read never
// constructs a fetcher, attaches a Task or manufactures first-execution RAM.
func (d *Downloader) readPathOperation(dataset, kind, id string) (HTTPPathOperationResult, *batchOperationRecord, error) {
	if !pathKnownKind(kind) || !operationIDPattern.MatchString(id) {
		return HTTPPathOperationResult{}, nil, ErrPathFreezeInvalid
	}
	var result HTTPPathOperationResult
	var record *batchOperationRecord
	err := withDatasetReadView(d.storage, dataset, func(reader datasetReader) error {
		var err error
		record, err = loadOperationFrom(reader, id)
		if err != nil {
			return err
		}
		if record != nil && (record.Version != 2 || record.Path == nil || record.Kind != kind || record.Path.DatasetID != dataset) {
			return ErrPathFreezeHeld
		}
		result.RequiredCapabilities = []string{}
		result.Operation, err = operationResultFrom(reader, id, record, make(map[string]*Task))
		if err != nil {
			return err
		}
		if record != nil {
			result.RequiredCapabilities = append([]string{}, record.RequiredCapabilities...)
			if record.Integrity != nil {
				result.CompletionViews = []TaskCompletionView{}
				for _, id := range result.Operation.TaskIDs {
					result.CompletionViews = append(result.CompletionViews, completionViewFrom(reader, dataset, id))
				}
			}
		}
		return nil
	})
	if err != nil {
		return HTTPPathOperationResult{}, nil, pathStoreError(err)
	}
	for i, view := range result.CompletionViews {
		result.CompletionViews[i] = d.gateCompletionVisibility(view)
	}
	return result, record, nil
}
func pathKnownKind(kind string) bool {
	for _, v := range pathVariants {
		if v.Kind == kind {
			return true
		}
	}
	return false
}

func (d *Downloader) CancelPathOperationForDataset(dataset, kind, operationID string) (HTTPPathOperationResult, error) {
	if !pathKnownKind(kind) {
		return HTTPPathOperationResult{}, ErrPathFreezeInvalid
	}
	if d.closed.Load() {
		return HTTPPathOperationResult{}, ErrPathFreezeUnavailable
	}
	store, ok := d.storage.(AtomicStorage)
	if !ok {
		return HTTPPathOperationResult{}, ErrPathFreezeUnavailable
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	if err := cancelPathOperation(store, dataset, operationID, kind); err != nil {
		result, _, _ := d.readPathOperation(dataset, kind, operationID)
		return result, err
	}
	return d.QueryPathOperationForDataset(dataset, kind, operationID)
}

// Freeze effective defaults and ordered rules once, before R. Raw caller paths
// and opaque options stay in Request; only the server execution projection is
// canonicalized. Replay bypasses this helper entirely.
func (d *Downloader) preparePathReception(request HTTPPathCreateRequest) (PathPolicySnapshot, []*base.Options, error) {
	if d.cfg == nil {
		return PathPolicySnapshot{}, nil, ErrPathFreezeUnavailable
	}
	d.configSnapshotMu.RLock()
	if d.cfg.DownloaderStoreConfig == nil {
		d.configSnapshotMu.RUnlock()
		return PathPolicySnapshot{}, nil, ErrPathFreezeUnavailable
	}
	cfg, err := pathpolicy.ReadConfig(d.cfg.Extra)
	defaultRoot := d.cfg.DownloadDir
	d.configSnapshotMu.RUnlock()
	if err != nil {
		return PathPolicySnapshot{}, nil, ErrPathFreezeInvalid
	}
	defaultRoot = util.ReplacePathPlaceholders(defaultRoot)
	root := request.PathIntent.Path
	if root == "" {
		root = defaultRoot
	} else {
		root = util.ReplacePathPlaceholders(root)
	}
	root, err = d.freezeVerifiedRoot(root)
	if err != nil {
		return PathPolicySnapshot{}, nil, err
	}
	// The unused default is retained as a frozen value, not granted as an output
	// root. Only the actual selected input/rule root goes through policy checks.
	if defaultRoot == "" {
		defaultRoot = root
	}
	for i := range cfg.Rules {
		cfg.Rules[i].Root, err = d.freezeVerifiedRoot(util.ReplacePathPlaceholders(cfg.Rules[i].Root))
		if err != nil {
			return PathPolicySnapshot{}, nil, err
		}
	}
	snapshot := PathPolicySnapshot{1, cfg, defaultRoot, root}
	if !pathSnapshotValid(snapshot) {
		return PathPolicySnapshot{}, nil, ErrPathFreezeInvalid
	}
	opts := make([]*base.Options, len(pathItems(request)))
	for i, item := range pathItems(request) {
		input, e := cloneOperationJSON(item.Opts)
		if e != nil {
			return PathPolicySnapshot{}, nil, ErrPathFreezeInvalid
		}
		input.Path = root
		opts[i], err = d.initOperationOptions(input)
		if err == nil && request.Integrity != nil {
			opts[i], err = d.freezeVerifiedOptions(opts[i])
		}
		if err != nil {
			return PathPolicySnapshot{}, nil, err
		}
	}
	return snapshot, opts, nil
}

func (d *Downloader) PreviewPathForDataset(ctx context.Context, dataset, kind string, request HTTPPathCreateRequest) (pathpolicy.Preparation, error) {
	if ctx == nil {
		return pathpolicy.Preparation{}, ErrPathFreezeInvalid
	}
	if err := ctx.Err(); err != nil {
		return pathpolicy.Preparation{}, err
	}
	v, ok := pathVariantForCaps(request.RequiredCapabilities)
	if !ok || v.Kind != kind || !pathRequestValid(request) {
		return pathpolicy.Preparation{}, ErrPathFreezeInvalid
	}
	if err := withDatasetReadView(d.storage, dataset, func(datasetReader) error { return nil }); err != nil {
		return pathpolicy.Preparation{}, err
	}
	if d.closed.Load() {
		return pathpolicy.Preparation{}, ErrPathFreezeUnavailable
	}
	snapshot, opts, err := d.preparePathReception(request)
	if err != nil {
		return pathpolicy.Preparation{}, err
	}
	// A directory tree has no truthful single-file hint in this envelope. Its
	// complete resolved layout is prepared by the protocol runner before F.
	if len(opts) != 1 {
		return pathpolicy.Preparation{}, ErrPathFreezeHeld
	}
	if v.Native {
		return d.previewNativePath(ctx, request, snapshot, opts[0])
	}
	name := opts[0].Name
	u, err := url.Parse(pathItems(request)[0].Req.URL)
	if err != nil {
		return pathpolicy.Preparation{}, ErrPathFreezeInvalid
	}
	if name == "" && v.Protocol == "http" {
		name = path.Base(u.Path)
		if name == "." || name == "/" {
			name = ""
		}
	}
	if name == "" {
		return pathpolicy.Preparation{}, ErrPathFreezeHeld
	}
	return pathpolicy.PrepareSingleFile(pathpolicy.Input{Path: snapshot.InputRoot, Origin: request.PathIntent.Origin, Domain: u.Hostname(), Name: name}, snapshot.Config)
}

func (d *Downloader) CreatePathOperationForDataset(ctx context.Context, dataset, kind string, request HTTPPathCreateRequest) (HTTPPathOperationResult, error) {
	if ctx == nil {
		return HTTPPathOperationResult{}, ErrPathFreezeInvalid
	}
	if err := ctx.Err(); err != nil {
		return HTTPPathOperationResult{}, err
	}
	v, ok := pathVariantForCaps(request.RequiredCapabilities)
	if !ok || v.Kind != kind || !pathRequestValid(request) {
		return HTTPPathOperationResult{}, ErrPathFreezeInvalid
	}
	request, err := cloneOperationJSON(request)
	if err != nil {
		return HTTPPathOperationResult{}, ErrPathFreezeInvalid
	}
	store, ok := d.storage.(AtomicStorage)
	if !ok {
		return HTTPPathOperationResult{}, ErrPathFreezeUnavailable
	}
	d.operationMu.Lock()
	defer d.operationMu.Unlock()
	oldResult, old, err := d.readPathOperation(dataset, kind, request.OperationID)
	if err != nil {
		return HTTPPathOperationResult{}, err
	}
	if old != nil {
		if old.RequestHash != pathRequestHash(request) {
			return HTTPPathOperationResult{}, ErrPathFreezeConflict
		}
		return oldResult, nil
	}
	capability, err := d.PathCapabilityForDataset(dataset, kind, request.RequiredCapabilities)
	if err != nil {
		return HTTPPathOperationResult{}, err
	}
	if !capability.Supported {
		return HTTPPathOperationResult{}, ErrPathFreezeHeld
	}
	snapshot, opts, err := d.preparePathReception(request)
	if err != nil {
		return HTTPPathOperationResult{}, err
	}
	// All pre-R captures are local configuration/proxy captures: no Resolve,
	// Restore, protocol Store, file creation or request transmission occurs here.
	candidates := make([]*Task, len(opts))
	defaults := make([]string, len(opts))
	for i, item := range pathItems(request) {
		req, e := cloneOperationJSON(item.Req)
		if e != nil {
			return HTTPPathOperationResult{}, ErrPathFreezeInvalid
		}
		manager, e := d.parseRequestFm(req)
		if e != nil || manager.Name() != v.Protocol {
			return HTTPPathOperationResult{}, ErrPathFreezeInvalid
		}
		task := NewTask()
		task.Protocol = v.Protocol
		task.Meta = &fetcher.FetcherMeta{Req: req, Opts: opts[i]}
		task.Progress = &Progress{}
		task.Status = base.DownloadStatusPause
		if !v.Native {
			defaults[i], e = d.capturePathDefaults(task)
			if e != nil {
				return HTTPPathOperationResult{}, e
			}
			task.Recovery = &TaskRecoveryRef{Version: 4}
		}
		candidates[i] = task
	}
	if err = ctx.Err(); err != nil {
		return HTTPPathOperationResult{}, err
	}
	r, fresh, err := reservePathTreeOperation(store, dataset, request, snapshot, opts)
	if err != nil {
		return d.pathReceptionError(dataset, kind, request.OperationID, err)
	}
	if !fresh {
		return d.QueryPathOperationForDataset(dataset, kind, request.OperationID)
	}
	initial := make([]recoveryRecord, len(candidates))
	for i, task := range candidates {
		task.ID = r.Members[i].TaskID
		task.Meta.Opts = r.Members[i].Options
		if !v.Native {
			episode, e := gonanoid.New()
			if e != nil {
				return d.pathReceptionError(dataset, kind, request.OperationID, ErrPathFreezeUnavailable)
			}
			source, e := recoveryTaskSourceHash(task, defaults[i])
			if e != nil {
				return d.pathReceptionError(dataset, kind, request.OperationID, ErrPathFreezeHeld)
			}
			initial[i] = recoveryRecord{Version: 4, TaskID: task.ID, Episode: episode, SourceRevision: 1, SourceHash: source, ExecutionDefaultsHash: defaults[i], InitialAttempt: recoveryInitialAttempt{State: "ungranted"}, Actions: []recoveryAction{}}
		}
	}
	if err = ctx.Err(); err != nil {
		return d.pathReceptionError(dataset, kind, request.OperationID, err)
	}
	created, freshM, err := createPathOperationTree(store, dataset, r, candidates, initial)
	if err != nil {
		return d.pathReceptionError(dataset, kind, request.OperationID, err)
	}
	if freshM {
		// Only this acknowledged M winner may carry the private new-M RAM evidence
		// into Q. Query/replay/unknown ACK never pass through this branch.
		if err = ctx.Err(); err == nil && !d.closed.Load() {
			d.lock.Lock()
			for _, task := range created {
				if err = d.attachOperationSnapshotLocked(task, true); err != nil {
					break
				}
			}
			d.lock.Unlock()
		} else if err == nil {
			err = ErrPathFreezeUnavailable
		}
		if err != nil {
			return d.pathReceptionError(dataset, kind, request.OperationID, err)
		}
	}
	return d.QueryPathOperationForDataset(dataset, kind, request.OperationID)
}
func (d *Downloader) capturePathDefaults(task *Task) (string, error) {
	if task.Protocol != "http" {
		snapshot, supported, err := d.captureParentRecoveryContext(task)
		if err != nil || !supported || snapshot == nil {
			return "", ErrPathFreezeHeld
		}
		if task.Protocol == "media" {
			// Use the same actual fetcher and frozen interpreter/configuration
			// check as execution. Standalone/no-bridge cannot enter managed R/M.
			f, err := d.buildFetcher(task.Meta.Req)
			if err != nil {
				return "", ErrPathFreezeHeld
			}
			bridge, ok := f.(interface {
				SupportsParentRecovery(*transfer.ProtocolExecutionContext) (bool, error)
			})
			if !ok {
				return "", ErrPathFreezeHeld
			}
			supported, err = bridge.SupportsParentRecovery(snapshot)
			if err != nil || !supported {
				return "", ErrPathFreezeHeld
			}
		}
		return snapshot.DefaultsHash(), nil
	}
	f, err := d.buildFetcher(task.Meta.Req)
	if err != nil {
		return "", ErrPathFreezeHeld
	}
	capture, ok := f.(interface {
		CaptureRecoveryContext(*base.Request) (*transfer.HTTPExecutionContext, bool, error)
	})
	if !ok {
		return "", ErrPathFreezeHeld
	}
	snapshot, supported, err := capture.CaptureRecoveryContext(task.Meta.Req)
	if err != nil || !supported || snapshot == nil {
		return "", ErrPathFreezeHeld
	}
	return snapshot.DefaultsHash(), nil
}
func (d *Downloader) pathReceptionError(dataset, kind, op string, cause error) (HTTPPathOperationResult, error) {
	result, _, _ := d.readPathOperation(dataset, kind, op)
	return result, cause
}

func (d *Downloader) HTTPPathCapabilityForDataset(dataset string, required []string) (HTTPPathCapability, error) {
	return d.PathCapabilityForDataset(dataset, HTTPPathCreateKind, required)
}
func (d *Downloader) PreviewHTTPPathForDataset(ctx context.Context, dataset string, request HTTPPathCreateRequest) (pathpolicy.Preparation, error) {
	return d.PreviewPathForDataset(ctx, dataset, HTTPPathCreateKind, request)
}
func (d *Downloader) CreateHTTPPathOperationForDataset(ctx context.Context, dataset string, request HTTPPathCreateRequest) (HTTPPathOperationResult, error) {
	return d.CreatePathOperationForDataset(ctx, dataset, HTTPPathCreateKind, request)
}
func (d *Downloader) QueryHTTPPathOperationForDataset(dataset, op string) (HTTPPathOperationResult, error) {
	return d.QueryPathOperationForDataset(dataset, HTTPPathCreateKind, op)
}
func (d *Downloader) CancelHTTPPathOperationForDataset(dataset, op string) (HTTPPathOperationResult, error) {
	return d.CancelPathOperationForDataset(dataset, HTTPPathCreateKind, op)
}
