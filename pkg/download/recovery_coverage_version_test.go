package download

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// These are reader-only variants. No enrollment or v3 writer is installed.
func TestRecoveryCoverageStrictVersionVariants(t *testing.T) {
	_, owner := recoveryFixtureStore(t, NewMemStorage(), t.TempDir())
	if _, err := owner.begin(1, ""); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(owner.record)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := transfer.NewHTTPCoverageState(strings.Repeat("a", 64), 32)
	if err != nil {
		t.Fatal(err)
	}
	last := recoveryExplicitRetry{Version: 1, ActionID: "test-action", FromEpisode: "before", ToEpisode: owner.record.Episode, SourceRevision: 1}
	cases := []struct {
		name    string
		version int
		mutate  func(*recoveryRecord)
		valid   bool
	}{
		{"v1", 1, func(*recoveryRecord) {}, true},
		{"v1-coverage", 1, func(r *recoveryRecord) { r.Coverage = &coverage }, false},
		{"v2-last", 2, func(r *recoveryRecord) { r.LastExplicitRetry = &last }, true},
		{"v2-no-last", 2, func(*recoveryRecord) {}, false},
		{"v2-coverage", 2, func(r *recoveryRecord) { r.LastExplicitRetry = &last; r.Coverage = &coverage }, false},
		{"v2-historical-last", 2, func(r *recoveryRecord) { v := last; v.ToEpisode = "historical"; r.LastExplicitRetry = &v }, false},
		{"v3-no-last", 3, func(r *recoveryRecord) { r.Coverage = &coverage }, true},
		{"v3-current-last", 3, func(r *recoveryRecord) { r.Coverage = &coverage; r.LastExplicitRetry = &last }, true},
		{"v3-historical-last", 3, func(r *recoveryRecord) {
			r.Coverage = &coverage
			v := last
			v.ToEpisode = "historical"
			r.LastExplicitRetry = &v
		}, true},
		{"v3-missing-coverage", 3, func(*recoveryRecord) {}, false},
		{"v3-nil-ranges", 3, func(r *recoveryRecord) { v := coverage; v.DurableRanges = nil; r.Coverage = &v }, false},
		{"v3-unknown-coverage", 3, func(r *recoveryRecord) { v := coverage; v.Version = 9; r.Coverage = &v }, false},
		{"v3-unprovable", 3, func(r *recoveryRecord) { v := coverage; v.Status = "unprovable"; r.Coverage = &v }, true},
		{"v3-initial-uncertain", 3, func(r *recoveryRecord) {
			r.Coverage = &coverage
			r.Generation = 0
			r.InitialAttempt = recoveryInitialAttempt{State: "uncertain"}
		}, false},
		{"v3-future-last", 3, func(r *recoveryRecord) {
			r.Coverage = &coverage
			v := last
			v.SourceRevision = 2
			r.LastExplicitRetry = &v
		}, false},
		{"unknown-version", 5, func(r *recoveryRecord) { r.Coverage = &coverage }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var r recoveryRecord
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			r.Version = c.version
			c.mutate(&r)
			if got := r.valid(r.TaskID); got != c.valid {
				t.Fatalf("valid=%v want %v", got, c.valid)
			}
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var round recoveryRecord
			if err = json.Unmarshal(b, &round); err != nil {
				t.Fatal(err)
			}
			if got := round.valid(round.TaskID); got != c.valid {
				t.Fatalf("round trip valid=%v want %v", got, c.valid)
			}
		})
	}
}
