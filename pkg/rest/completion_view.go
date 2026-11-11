package rest

import "github.com/GopeedLab/gopeed/pkg/download"

// Public fields only. The future Core adapter must source these from its
// coherent completion snapshot, including confirmed C/API publication. This
// formatter cannot infer evidence from Task.done, its marker or error text.
type completionView struct {
	Version       int     `json:"version"`
	ScopeVersion  int     `json:"scopeVersion"`
	DatasetID     string  `json:"datasetId"`
	TaskID        string  `json:"taskId"`
	CompletionID  *string `json:"completionId"`
	Available     bool    `json:"available"`
	Phase         *string `json:"phase"`
	RecordedMatch *bool   `json:"recordedMatch"`
	ErrorCategory *string `json:"errorCategory"`
}

func unavailableCompletionView(domain, task string) completionView {
	category := "completion_unavailable"
	return completionView{Version: 1, ScopeVersion: 1, DatasetID: domain, TaskID: task, ErrorCategory: &category}
}

func publicCompletionView(view completionView, domain, task string) completionView {
	if view.Version != 1 || view.ScopeVersion != 1 || view.DatasetID != domain || view.TaskID != task ||
		!publicCompletionError(view.ErrorCategory) {
		return unavailableCompletionView(domain, task)
	}
	if !view.Available {
		if view.CompletionID != nil || view.Phase != nil || view.RecordedMatch != nil {
			return unavailableCompletionView(domain, task)
		}
		return view
	}
	if view.CompletionID == nil || *view.CompletionID == "" || view.Phase == nil {
		return unavailableCompletionView(domain, task)
	}
	switch *view.Phase {
	case "prepared", "waiting_verify", "verifying":
		if view.RecordedMatch != nil {
			return unavailableCompletionView(domain, task)
		}
	case "verified", "published_pending_commit", "committed":
		if view.RecordedMatch == nil || !*view.RecordedMatch {
			return unavailableCompletionView(domain, task)
		}
	default:
		return unavailableCompletionView(domain, task)
	}
	return view
}

func publicCompletionError(category *string) bool {
	if category == nil {
		return true
	}
	switch *category {
	case "completion_unavailable", "completion_state_invalid", "integrity_mismatch", "publish_conflict", "cancelled", "run_changed", "io_error", "unsupported_option":
		return true
	}
	return false
}

func completionViewsForOperation(views []completionView, domain string, op download.BatchOperationResult) []completionView {
	if len(views) == 0 || len(op.TaskIDs) != 1 {
		return nil
	}
	task := op.TaskIDs[0]
	if len(views) != 1 {
		return []completionView{unavailableCompletionView(domain, task)}
	}
	return []completionView{publicCompletionView(views[0], domain, task)}
}
