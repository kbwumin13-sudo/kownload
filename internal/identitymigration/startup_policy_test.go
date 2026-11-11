package identitymigration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestImportRecordsStartupExecutionPolicy(t *testing.T) {
	for _, copied := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "imported"}[copied], func(t *testing.T) {
			source, target := fixture(t)
			if !copied {
				source = filepath.Join(t.TempDir(), "absent")
			}
			if _, err := Import(Options{Source: source, Target: target}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(target, Marker))
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err = json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			policy, ok := value["startupPolicy"].(map[string]any)
			want := "normal"
			if copied {
				want = "imported-review"
			}
			if !ok || policy["version"] != float64(1) || policy["mode"] != want {
				t.Fatalf("startup execution admission missing: %#v", policy)
			}
		})
	}
}

func TestUnknownStartupPolicyDoesNotChangeEitherRoot(t *testing.T) {
	source, target := fixture(t)
	before, err := summarize(source, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	marker := []byte(`{"version":1,"imported":true,"startupPolicy":{"version":2,"mode":"normal"}}`)
	if err = os.WriteFile(filepath.Join(target, Marker), marker, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Import(Options{Source: source, Target: target, IgnoreLegacyChanges: true}); err == nil {
		t.Fatal("unknown admission must be rejected before rewriting the marker")
	}
	after, err := summarize(source, true)
	if err != nil || before != after {
		t.Fatal("legacy data changed", err)
	}
	got, err := os.ReadFile(filepath.Join(target, Marker))
	if err != nil || string(got) != string(marker) {
		t.Fatal("marker changed", err)
	}
}

func TestLegacyMarkerReviewDoesNotAutomaticallyBecomeNormal(t *testing.T) {
	for _, imported := range []bool{false, true} {
		r := record{Summary: Summary{Version: 1, Imported: imported}}
		got, err := startupPolicy(r)
		if err != nil || got.Mode != "imported-review" || got.Reason != "legacy_marker" {
			t.Fatal("unknown old marker admitted execution", got, err)
		}
	}
	for _, p := range []StartupPolicy{
		{Version: 2, Mode: "normal", Reason: "fresh"},
		{Version: 1, Mode: "future", Reason: "fresh"},
		{Version: 1, Mode: "normal", Reason: "unknown"},
		{Version: 1, Mode: "normal", Reason: "fresh"},
	} {
		if _, err := startupPolicy(record{Summary: Summary{Imported: true}, StartupPolicy: &p}); err == nil {
			t.Fatal("unsupported/imported-fresh policy admitted", p)
		}
	}
}
