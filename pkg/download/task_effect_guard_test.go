package download

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestTaskEffectsKeepMCPGuardForTaskPayloads(t *testing.T) {
	store := NewMemStorage()
	if err := store.Setup([]string{bucketTask, bucketOperationTasks}); err != nil {
		t.Fatal(err)
	}
	d := &Downloader{storage: store}
	managed := NewTask()
	managed.MCPWrite = &MCPTaskState{Version: 2}
	if err := store.Put(bucketTask, managed.ID, managed); err != nil {
		t.Fatal(err)
	}
	droppedSlot := NewTask()
	droppedSlot.ID = managed.ID
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	marker := filepath.Join(t.TempDir(), "executed")
	script := marker + ".sh"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf unexpected > \"$0.executed\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, task := range []*Task{nil, managed, droppedSlot} {
		if _, err := d.sendWebhookToUrl(server.URL, &WebhookData{Payload: &WebhookPayload{Task: task}}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Task webhook guard: %v", err)
		}
		if err := d.executeScriptAtPath(script, &ScriptData{Payload: &ScriptPayload{Task: task}}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Task script guard: %v", err)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("rejected Task payload sent a webhook")
	}
	if _, err := os.Stat(script + ".executed"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected Task payload executed a script: %v", err)
	}
}
