package download

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompletionReferenceOptionalRoundTrip(t *testing.T) {
	task := NewTask()
	task.Progress = &Progress{}
	initTask(task)
	legacy, err := json.Marshal(task)
	if err != nil || strings.Contains(string(legacy), `"completion"`) {
		t.Fatal("legacy JSON acquired a completion reference", err)
	}
	for _, version := range []int{1, 99} {
		task.Completion = &TaskCompletionRef{Version: version, CompletionID: "slot-completion-id"}
		raw, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		var restored Task
		if err := json.Unmarshal(raw, &restored); err != nil || restored.Completion == nil || restored.Completion.Version != version || restored.Completion.CompletionID != "slot-completion-id" {
			t.Fatal("lost required version", err)
		}
		copy := task.clone()
		if copy.Completion == nil || copy.Completion.Version != version || copy.Completion.CompletionID != "slot-completion-id" {
			t.Fatal("clone lost required reference")
		}
		copy.Completion.Version = 0
		copy.Completion.CompletionID = "another-completion"
		if task.Completion.Version != version || task.Completion.CompletionID != "slot-completion-id" {
			t.Fatal("clone aliased completion reference")
		}
	}
}
