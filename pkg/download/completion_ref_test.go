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
		task.Completion = &TaskCompletionRef{Version: version}
		raw, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		var restored Task
		if err := json.Unmarshal(raw, &restored); err != nil || restored.Completion == nil || restored.Completion.Version != version {
			t.Fatal("lost required version", err)
		}
		copy := task.clone()
		if copy.Completion == nil || copy.Completion.Version != version {
			t.Fatal("clone lost required reference")
		}
		copy.Completion.Version = 0
		if task.Completion.Version != version {
			t.Fatal("clone aliased completion reference")
		}
	}
}
