package main

import (
	"encoding/json"
	"flag"
	"github.com/GopeedLab/gopeed/internal/identitymigration"
	"os"
)

func main() {
	source := flag.String("source", "", "inactive source root")
	target := flag.String("target", "", "new identity root")
	ignore := flag.Bool("ignore-legacy-changes", false, "keep the new root and stop checking the legacy root (persistent)")
	flag.Parse()
	if *source == "" || *target == "" {
		os.Stderr.WriteString("source_and_target_required\n")
		os.Exit(2)
	}
	s, err := identitymigration.Import(identitymigration.Options{Source: *source, Target: *target, IgnoreLegacyChanges: *ignore})
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": err.Error()})
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "snapshot": s})
}
