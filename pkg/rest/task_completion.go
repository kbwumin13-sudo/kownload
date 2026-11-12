package rest

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

type taskCompletionCore interface {
	DatasetIdentity() (download.DatasetIdentity, error)
	GetTasksByFilter(*download.TaskFilter) []*download.Task
	TaskCompletionSnapshotsForDataset(string, []string) (map[string]download.TaskCompletionSnapshot, error)
}

// Capture the same owner as identity and the guarded operation routes. A later
// BuildServer must not change the dataset or Task source of this router.
func registerTaskReadRoutes(router *mux.Router, core taskCompletionCore) {
	reader := taskCompletionReader{core: core}
	router.Methods(http.MethodGet).Path("/api/v1/tasks/{id}").HandlerFunc(reader.get)
	router.Methods(http.MethodGet).Path("/api/v1/tasks").HandlerFunc(reader.list)
}

type taskCompletionReader struct{ core taskCompletionCore }

type taskCompletionResponse struct {
	task    *download.Task
	view    *completionView
	effects *mcpTaskEffects
}

type mcpTaskEffects struct {
	Version         int  `json:"version"`
	AutoOpenAllowed bool `json:"autoOpenAllowed"`
}

func (response taskCompletionResponse) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(response.task)
	if err != nil || response.view == nil && response.effects == nil {
		return raw, err
	}
	// Task has its own MarshalJSON method. Keep its public fields and number
	// bytes intact instead of embedding it (which would hide the projection),
	// or decoding request/option numbers through float64.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if response.view != nil {
		fields["completionView"], err = json.Marshal(response.view)
		if err != nil {
			return nil, err
		}
	}
	if response.effects != nil {
		fields["mcpEffects"], err = json.Marshal(response.effects)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}

func (reader taskCompletionReader) read(ids []string) (map[string]taskCompletionResponse, error) {
	identity, err := reader.core.DatasetIdentity()
	if err != nil {
		return nil, err
	}
	if identity.Version != 1 || !scopedDatasetPattern.MatchString(identity.DatasetID) {
		return nil, download.ErrDatasetInvalid
	}
	snapshots, err := reader.core.TaskCompletionSnapshotsForDataset(identity.DatasetID, ids)
	if err != nil {
		return nil, err
	}
	responses := make(map[string]taskCompletionResponse, len(snapshots))
	for _, id := range ids {
		snapshot, found := snapshots[id]
		if !found {
			continue
		}
		if snapshot.Task == nil || snapshot.Task.ID != id {
			return nil, &download.TaskCompletionStateError{Kind: download.TaskCompletionUnavailable}
		}
		response := taskCompletionResponse{task: snapshot.Task}
		if snapshot.Task.MCPWrite != nil {
			response.effects = &mcpTaskEffects{Version: 1}
			if policy, ok := reader.core.(interface {
				MCPTaskEffectAllowed(*download.Task, string) bool
			}); ok {
				response.effects.AutoOpenAllowed = policy.MCPTaskEffectAllowed(snapshot.Task, "auto_open")
			}
		}
		if snapshot.CompletionView != nil {
			view := adaptCompletionView(*snapshot.CompletionView)
			view = publicCompletionView(view, identity.DatasetID, id)
			ref := snapshot.Task.Completion
			if view.Available && (ref == nil || ref.Version != 1 || view.CompletionID == nil ||
				ref.CompletionID != *view.CompletionID ||
				(view.Phase != nil && *view.Phase == "committed" && snapshot.Task.Status != base.DownloadStatusDone)) {
				view = unavailableCompletionView(identity.DatasetID, id)
			}
			response.view = &view
		} else if snapshot.Task.Completion != nil {
			// A marker with no authoritative projection is never legacy.
			view := unavailableCompletionView(identity.DatasetID, id)
			response.view = &view
		}
		responses[id] = response
	}
	return responses, nil
}

func (reader taskCompletionReader) get(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		WriteJson(w, model.NewErrorResult("param invalid: id", model.CodeInvalidParam))
		return
	}
	responses, err := reader.read([]string{id})
	if err != nil {
		writeTaskCompletionError(w, err)
		return
	}
	response, found := responses[id]
	if !found {
		WriteJson(w, model.NewErrorResult("task not found", model.CodeTaskNotFound))
		return
	}
	WriteJson(w, model.NewOkResult(response))
}

func (reader taskCompletionReader) list(w http.ResponseWriter, r *http.Request) {
	filter, invalid := parseFilter(r)
	if invalid != nil {
		WriteJson(w, invalid)
		return
	}
	selection := reader.core.GetTasksByFilter(filter)
	ids := make([]string, 0, len(selection))
	for _, task := range selection {
		ids = append(ids, task.ID)
	}
	responses, err := reader.read(ids)
	if err != nil {
		writeTaskCompletionError(w, err)
		return
	}
	// Selection is live, not a cross-request snapshot. Retain its order and
	// filters; an ID removed before the authoritative View is simply omitted.
	result := make([]taskCompletionResponse, 0, len(ids))
	for _, id := range ids {
		if response, found := responses[id]; found {
			result = append(result, response)
		}
	}
	WriteJson(w, model.NewOkResult(result))
}

func writeTaskCompletionError(w http.ResponseWriter, err error) {
	status, message := http.StatusServiceUnavailable, "completion_state_unavailable"
	if errors.Is(err, download.ErrDatasetMismatch) {
		status, message = http.StatusConflict, "dataset_mismatch"
	} else {
		var state *download.TaskCompletionStateError
		if errors.As(err, &state) {
			switch state.Kind {
			case download.TaskCompletionHeld, download.TaskCompletionUnavailable,
				download.TaskCompletionConflict, download.TaskCompletionRunChanged, download.TaskCompletionNotRequired:
				message = string(state.Kind)
			}
		}
	}
	WriteStatusJson(w, status, model.NewErrorResult(message))
}
