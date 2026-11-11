package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

// These are controlled read-API responses, not evidence of App publishers.
func TestPublishedSnapshotContract(t *testing.T) {
	tests := []struct {
		name, kind, payload string
		valid               bool
	}{
		{"stats_unobserved", "stats", `{"available":false}`, true},
		{"stats_active_zero", "stats", `{"available":true,"activeStreams":0}`, true},
		{"stats_target_only", "stats", `{"available":true,"targetStreams":4}`, true},
		{"stats_pair", "stats", `{"available":true,"activeStreams":2,"targetStreams":4}`, true},
		{"stats_safe_max", "stats", `{"available":true,"activeStreams":9007199254740991}`, true},
		{"stats_future_peer_zero", "stats", `{"available":true,"peers":0}`, true},
		{"stats_empty_known", "stats", `{"available":true}`, false},
		{"stats_unknown_with_zero", "stats", `{"available":false,"activeStreams":0}`, false},
		{"stats_negative", "stats", `{"available":true,"activeStreams":-1}`, false},
		{"stats_fraction", "stats", `{"available":true,"targetStreams":0.5}`, false},
		{"stats_unsafe_integer", "stats", `{"available":true,"targetStreams":9007199254740992}`, false},
		{"stats_null", "stats", `{"available":true,"activeStreams":null}`, false},
		{"stats_private", "stats", `{"available":true,"activeStreams":1,"host":"SENTINEL_SECRET"}`, false},
		{"errors_unobserved", "errors", `{"available":false}`, true},
		{"errors_retry_false", "errors", `{"available":true,"category":"network","retryable":false,"occurredAt":"2026-11-12T00:00:00.123Z"}`, true},
		{"errors_retry_true", "errors", `{"available":true,"category":"authentication","retryable":true,"occurredAt":"2026-11-12T00:00:00Z"}`, true},
		{"errors_missing_category", "errors", `{"available":true,"retryable":false,"occurredAt":"2026-11-12T00:00:00Z"}`, false},
		{"errors_missing_retry", "errors", `{"available":true,"category":"network","occurredAt":"2026-11-12T00:00:00Z"}`, false},
		{"errors_missing_time", "errors", `{"available":true,"category":"network","retryable":false}`, false},
		{"errors_bad_time", "errors", `{"available":true,"category":"network","retryable":false,"occurredAt":"2026-99-12T00:00:00Z"}`, false},
		{"errors_unknown_with_data", "errors", `{"available":false,"category":"network"}`, false},
		{"errors_private_category", "errors", `{"available":true,"category":"SENTINEL_SECRET","retryable":false,"occurredAt":"2026-11-12T00:00:00Z"}`, false},
		{"errors_raw_error", "errors", `{"available":true,"category":"network","retryable":false,"occurredAt":"2026-11-12T00:00:00Z","error":"SENTINEL_SECRET"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != readPrefix+"/task_1/"+tt.kind {
					t.Error("unexpected backend request")
				}
				reply(w, json.RawMessage(tt.payload))
			})
			result := call(t, session(t, path), "kownload_get_"+tt.kind, map[string]any{"id": "task_1"})
			if !tt.valid {
				want, _ := json.Marshal(toolFailure("invalid_app_response").Content)
				got, _ := json.Marshal(result.Content)
				if !result.IsError || result.StructuredContent != nil || string(got) != string(want) {
					t.Fatalf("expected fixed redacted rejection, got %s", got)
				}
				return
			}
			if result.IsError {
				t.Fatalf("valid published snapshot rejected: %+v", result)
			}
			encoded, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if json.Unmarshal(encoded, &got) != nil || json.Unmarshal([]byte(tt.payload), &want) != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("snapshot changed: %s", encoded)
			}
		})
	}
}
