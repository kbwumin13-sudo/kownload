package download

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"

	"github.com/GopeedLab/gopeed/pkg/base"
)

// R freezes the original handoff identity alongside the raw path intent. Phase
// changes belong to the same Task, not another operation namespace or ledger.
func reserveBrowserPathOperation(store AtomicStorage, dataset string, request HTTPPathCreateRequest, snapshot PathPolicySnapshot, effectiveOptions *base.Options, handoff BrowserHandoff) (batchOperationRecord, bool, error) {
	if !browserPathIntentValid(request, handoff) {
		return batchOperationRecord{}, false, ErrHandoffInvalid
	}
	return reserveBoundPathTreeOperation(store, dataset, request, snapshot, []*base.Options{effectiveOptions}, &handoff)
}

// Preserve the pre-existing browser prepare fingerprint (raw Req and Opts),
// independently of pathRequestHash, which also binds PathIntent and caps.
func browserPathRequestHash(request HTTPPathCreateRequest) string {
	if request.Request == nil {
		return ""
	}
	raw, err := json.Marshal(struct {
		Req  *base.Request
		Opts *base.Options
	}{request.Request.Req, request.Request.Opts})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func browserPathIntentValid(request HTTPPathCreateRequest, h BrowserHandoff) bool {
	v, ok := pathVariantForCaps(request.RequiredCapabilities)
	if !ok || v.Protocol != "http" || !pathRequestValid(request) || request.Request == nil || h.Version != 1 || h.OperationID != request.OperationID || !validHandoffIdentity(BrowserHandoffRequest{ClientID: h.ClientID, Browser: h.Browser}) || !validDownloadBinding(h.Download) || h.Phase != "prepared" || h.Reason != "" || h.RequestHash == "" || h.RequestHash != browserPathRequestHash(request) {
		return false
	}
	req := request.Request.Req
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	sum := sha256.Sum256([]byte(req.URL))
	if hex.EncodeToString(sum[:]) != h.Download.URLHash {
		return false
	}
	raw, err := json.Marshal(req.Extra)
	if err != nil {
		return false
	}
	var extra struct {
		Method string          `json:"method"`
		Body   json.RawMessage `json:"body"`
	}
	return json.Unmarshal(raw, &extra) == nil && (extra.Method == "" || extra.Method == "GET") && (len(extra.Body) == 0 || string(extra.Body) == "null" || string(extra.Body) == `""`)
}
func browserPathRecordValid(r *batchOperationRecord) bool {
	if r.Path.Handoff == nil {
		return true
	}
	return r.Kind == HTTPPathCreateKind && len(r.Members) == 1 && browserPathIntentValid(r.Path.Request, *r.Path.Handoff)
}
func browserPathIdentityEqual(a, b BrowserHandoff) bool {
	a.Phase, a.Reason, b.Phase, b.Reason = "", "", "", ""
	return a == b
}
func browserPathPhaseValid(h BrowserHandoff) bool {
	switch h.Phase {
	case "prepared", "committed":
		return h.Reason == ""
	case "aborted":
		return h.Reason == "browser_completed" || h.Reason == "user_cancelled" || h.Reason == "browser_retained"
	}
	return false
}
func browserPathTaskMatches(task *Task, r *batchOperationRecord) bool {
	if r == nil || r.Path == nil {
		return false
	}
	expected := r.Path.Handoff
	if expected == nil {
		return task.Handoff == nil
	}
	return task.Handoff != nil && browserPathIdentityEqual(*expected, *task.Handoff) && browserPathPhaseValid(*task.Handoff)
}

// The caller authenticates the original browser cancellation proof and holds
// its lifecycle fence. This kernel performs no IO, live Task mutation or Q.
// Only a fresh confirmed phase ACK can be consumed by that caller; readback,
// replay and unknown ACK never recreate a first-execution RAM qualification.
func transitionBrowserPathHandoff(store AtomicStorage, dataset string, expected BrowserHandoff, phase, reason string) (*Task, bool, error) {
	if store == nil {
		return nil, false, ErrPathFreezeUnavailable
	}
	if expected.Version != 1 || !handoffID.MatchString(expected.OperationID) || !browserPathPhaseValid(expected) || !validHandoffIdentity(BrowserHandoffRequest{ClientID: expected.ClientID, Browser: expected.Browser}) || !validDownloadBinding(expected.Download) {
		return nil, false, ErrHandoffInvalid
	}
	next := expected
	next.Phase, next.Reason = phase, reason
	if !browserPathPhaseValid(next) || (phase != "committed" && phase != "aborted") {
		return nil, false, ErrHandoffInvalid
	}
	var result *Task
	fresh := false
	err := store.UpdateAtomic(func(tx StorageTransaction) error {
		if err := requireDataset(tx, dataset); err != nil {
			return err
		}
		r, err := loadOperationFrom(tx, expected.OperationID)
		if err != nil {
			return err
		}
		if r == nil || r.Version != 2 || r.Kind != HTTPPathCreateKind || r.Path == nil || r.Path.DatasetID != dataset || r.Path.Handoff == nil || len(r.Members) != 1 || r.Members[0].State != "created" || !browserPathIdentityEqual(expected, *r.Path.Handoff) {
			return ErrHandoffHeld
		}
		task, _, _, err := readSharedRecovery(tx, r.Members[0].TaskID)
		if err != nil {
			return ErrHandoffHeld
		}
		if _, err = pathTaskFromRecord(tx, task, r); err != nil {
			return err
		}
		current := *task.Handoff
		if reflect.DeepEqual(current, next) {
			result = task
			return nil
		}
		if current != expected || current.Phase != "prepared" {
			return ErrHandoffConflict
		}
		// This is an owned same-view Task. Preserve every unrelated field, including
		// UpdatedAt, request/options, path state, completion and recovery budgets.
		task.Handoff = &next
		if err = tx.Put(bucketTask, task.ID, task); err != nil {
			return err
		}
		result, fresh = task, true
		return nil
	})
	if err != nil {
		for _, fixed := range []error{ErrHandoffHeld, ErrHandoffConflict, ErrHandoffInvalid} {
			if errors.Is(err, fixed) {
				return nil, false, fixed
			}
		}
		return nil, false, pathStoreError(err)
	}
	return result, fresh, nil
}
