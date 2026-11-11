package identitymigration

import "errors"

// StartupPolicy records provenance/admission, not the user's autoStart setting.
// It does not grant per-task execution rights or certify referenced output files.
type StartupPolicy struct {
	Version int    `json:"version"`
	Mode    string `json:"mode"`
	Reason  string `json:"reason"`
}

func startupPolicy(r record) (StartupPolicy, error) {
	if r.StartupPolicy == nil {
		// Old markers prove an import completed, not execution compatibility.
		return StartupPolicy{Version: 1, Mode: "imported-review", Reason: "legacy_marker"}, nil
	}
	p := *r.StartupPolicy
	if p.Version == 1 {
		switch p.Mode {
		case "normal":
			if (p.Reason == "fresh" && !r.Imported) || p.Reason == "confirmed_compatible" {
				return p, nil
			}
		case "imported-review":
			if p.Reason == "imported_data" || p.Reason == "legacy_marker" {
				return p, nil
			}
		}
	}
	return StartupPolicy{}, errors.New("unsupported_startup_policy")
}
