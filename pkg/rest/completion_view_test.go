package rest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompletionViewWhitelistAndUnavailableEvidence(t *testing.T) {
	domain, task, cid := strings.Repeat("a", 32), "example-task", "example-completion"
	for _, phase := range []string{"prepared", "waiting_verify", "verifying", "verified", "published_pending_commit", "committed"} {
		t.Run(phase, func(t *testing.T) {
			view := completionView{Version: 1, ScopeVersion: 1, DatasetID: domain, TaskID: task, CompletionID: &cid, Available: true, Phase: &phase}
			if phase == "verified" || phase == "published_pending_commit" || phase == "committed" {
				match := true
				view.RecordedMatch = &match
			}
			actual := publicCompletionView(view, domain, task)
			if !actual.Available || actual.Phase == nil || *actual.Phase != phase || actual.RecordedMatch != view.RecordedMatch {
				t.Fatal("valid projection lost")
			}
		})
	}
	phase, match, raw := "verified", true, "private-path-cookie-hash"
	valid := completionView{Version: 1, ScopeVersion: 1, DatasetID: domain, TaskID: task, CompletionID: &cid, Available: true, Phase: &phase, RecordedMatch: &match}
	for _, mutate := range []func(*completionView){
		func(v *completionView) { v.Version = 2 },
		func(v *completionView) { v.ScopeVersion = 2 },
		func(v *completionView) { v.DatasetID = strings.Repeat("b", 32) },
		func(v *completionView) { v.TaskID = "other-task" },
		func(v *completionView) { v.CompletionID = nil },
		func(v *completionView) { v.Available = false },
		func(v *completionView) { v.Phase = &raw },
		func(v *completionView) { v.Phase = nil },
		func(v *completionView) { f := false; v.RecordedMatch = &f },
		func(v *completionView) { v.RecordedMatch = nil },
		func(v *completionView) { p := "verifying"; v.Phase = &p },
		func(v *completionView) { v.ErrorCategory = &raw },
	} {
		view := valid
		mutate(&view)
		actual := publicCompletionView(view, domain, task)
		if actual.Available || actual.CompletionID != nil || actual.Phase != nil || actual.RecordedMatch != nil || actual.ErrorCategory == nil || *actual.ErrorCategory != "completion_unavailable" {
			t.Fatalf("invalid evidence: %+v", actual)
		}
		encoded, err := json.Marshal(actual)
		if err != nil || strings.Contains(string(encoded), raw) || strings.Contains(string(encoded), "other-task") || strings.Contains(string(encoded), "falseMatch") {
			t.Fatal("invalid projection leaked data")
		}
	}
	if views := completionViewsForOperation([]completionView{valid}, domain, verifiedCreatedReceipt()); len(views) != 1 || !views[0].Available {
		t.Fatal("associated view omitted")
	}
	if views := completionViewsForOperation([]completionView{valid}, domain, scopeFixtureResult()); len(views) != 0 {
		t.Fatal("unknown operation acquired completion evidence")
	}
	if views := completionViewsForOperation([]completionView{valid, valid}, domain, verifiedCreatedReceipt()); len(views) != 1 || views[0].Available {
		t.Fatal("ambiguous views adopted")
	}
	for _, category := range []string{"completion_unavailable", "completion_state_invalid", "integrity_mismatch", "publish_conflict", "cancelled", "run_changed", "io_error", "unsupported_option"} {
		view := unavailableCompletionView(domain, task)
		view.ErrorCategory = &category
		actual := publicCompletionView(view, domain, task)
		if actual.Available || actual.RecordedMatch != nil || actual.Phase != nil || actual.CompletionID != nil || actual.ErrorCategory == nil || *actual.ErrorCategory != category {
			t.Fatal("confirmed unavailable category lost", category)
		}
	}
}
