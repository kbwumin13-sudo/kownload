package pathpolicy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareBeforeLossyFilenameAndFinalizeOnce(t *testing.T) {
	cfg := Config{Enabled: true, Version: "rules-1", Rules: []Rule{{ID: "mp4", Kind: ExtensionRule, Match: Match{Extension: ".mp4"}, Root: "/Video"}}}
	name := strings.Repeat("a", 96) + ".mp4" + strings.Repeat("x", 20)
	p, err := PrepareSingleFile(Input{Path: "/Default", Origin: Default, Name: name}, cfg)
	if err != nil || p.Decision.Root != "/Default" || !strings.HasSuffix(p.Name, ".mp4") {
		t.Fatal(p, err)
	}
	final, err := FinalizeSingleFile(p, p.Name+" (1)")
	if err != nil || final.Name != p.Name+" (1)" || final.Decision.Root != p.Decision.Root || final.LayoutHash == p.LayoutHash {
		t.Fatal(final, err)
	}
	if err := ValidatePreparation(final); err != nil {
		t.Fatal(err)
	}
	unicode, err := PrepareSingleFile(Input{Path: "/Default", Origin: UserSelected, Name: "Cafe\u0301?.txt"}, Config{})
	if err != nil || unicode.Name != "Caf\u00e9_.txt" {
		t.Fatal(unicode, err)
	}
	raw, _ := json.Marshal(unicode.Decision)
	if string(raw) != `{"version":1,"root":"/Default","origin":"user_selected","ruleId":"","ruleVersion":null}` {
		t.Fatal(string(raw))
	}
}

func TestPreparationOwnsValuesAndRejectsChangedLayout(t *testing.T) {
	p, err := PrepareSingleFile(Input{Path: "/D", Origin: Default, Name: "v.mp4"}, Config{Enabled: true, Version: "v1", Rules: []Rule{{ID: "r", Kind: ExtensionRule, Root: "/V", Match: Match{Extension: ".mp4"}}}})
	if err != nil {
		t.Fatal(err)
	}
	next, err := FinalizeSingleFile(p, "v (1).mp4")
	if err != nil {
		t.Fatal(err)
	}
	*next.Decision.RuleVersion = "changed"
	next.Layout.Members[0].RelativePath = "changed"
	if *p.Decision.RuleVersion != "v1" || p.Layout.Members[0].RelativePath != "v.mp4" {
		t.Fatal("output aliases old preparation")
	}
	if ValidatePreparation(next) == nil {
		t.Fatal("accepted changed layout")
	}
	for _, name := range []string{"", ".", "..", "../x", "a/b", "a\\b", "bad\nname"} {
		if got, err := FinalizeSingleFile(p, name); err == nil || got.Layout.Members != nil {
			t.Fatal("unsafe name did not fail with zero output", name, got, err)
		}
	}
	if _, err := PrepareSingleFile(Input{Frozen: &p.Decision, Name: "other"}, Config{}); err == nil {
		t.Fatal("fresh preparation reinterpreted frozen state")
	}
}
