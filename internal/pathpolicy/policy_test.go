package pathpolicy

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func root(name string) string { return filepath.Join(string(filepath.Separator), "downloads", name) }

func rule(id string, kind Kind, match Match) Rule {
	return Rule{ID: id, Kind: kind, Match: match, Root: root(id)}
}

func config(rules ...Rule) Config { return Config{Enabled: true, Version: "snapshot-17", Rules: rules} }

func input() Input {
	return Input{Path: root("original"), Origin: Default, Domain: "files.example", MIME: "video/mp4", Name: "电影.MP4"}
}

func evaluate(t *testing.T, in Input, cfg Config) Decision {
	t.Helper()
	d, err := Evaluate(in, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDefaultOff(t *testing.T) {
	in := input()
	cfg := config(rule("videos", ExtensionRule, Match{Extension: ".mp4"}))
	cfg.Enabled = false
	d := evaluate(t, in, cfg)
	if d.Root != in.Path || d.Origin != Default || d.RuleVersion != nil || d.RuleID != "" {
		t.Fatalf("%+v", d)
	}
	// Disabled/absent settings never activate stale malformed rules.
	if got := evaluate(t, in, Config{}); got != d {
		t.Fatalf("zero config: %+v", got)
	}
	if got := evaluate(t, in, Config{Rules: []Rule{{}}}); got != d {
		t.Fatalf("disabled: %+v", got)
	}
}

func TestExplicitSamePathIsDifferentIntent(t *testing.T) {
	cfg := config(rule("videos", ExtensionRule, Match{Extension: ".mp4"}))
	for _, origin := range []Origin{UserSelected, ManualCategory, APIExplicit} {
		t.Run(string(origin), func(t *testing.T) {
			in := input()
			in.Origin = origin
			d := evaluate(t, in, cfg)
			if d.Root != in.Path || d.Origin != origin || d.RuleVersion != nil {
				t.Fatalf("explicit: %+v", d)
			}
			in.Origin = Default // Deliberately keep exactly the same path string.
			if got := evaluate(t, in, cfg); got.RuleID != "videos" || got.Root == d.Root {
				t.Fatalf("default: %+v", got)
			}
		})
	}
}

func TestUnknownIncomingPreservesRoot(t *testing.T) {
	for _, origin := range []Origin{Unknown, ""} {
		in := input()
		in.Origin = origin
		d := evaluate(t, in, config(rule("videos", ExtensionRule, Match{Extension: ".mp4"})))
		if d.Root != in.Path || d.Origin != Unknown || d.RuleID != "" || d.RuleVersion != nil {
			t.Fatalf("%+v", d)
		}
	}
}

func TestSuggestedRootIsFallback(t *testing.T) {
	in := input()
	in.Origin = SourceSuggested
	if got := evaluate(t, in, Config{}); got.Root != in.Path || got.Origin != SourceSuggested {
		t.Fatalf("off: %+v", got)
	}
	if got := evaluate(t, in, config()); got.Root != in.Path || got.Origin != SourceSuggested {
		t.Fatalf("no match: %+v", got)
	}
	if got := evaluate(t, in, config(rule("videos", ExtensionRule, Match{Extension: ".mp4"}))); got.RuleID != "videos" {
		t.Fatalf("rule: %+v", got)
	}
}

func TestPriorityAndStableUserOrder(t *testing.T) {
	all := []Rule{
		rule("extension", ExtensionRule, Match{Extension: ".mp4"}),
		rule("mime", MIMERule, Match{MIME: "video/*"}),
		rule("domain", DomainRule, Match{Domain: "files.example"}),
		rule("z-first-user", UserRule, Match{MIME: "video/mp4", Domain: "files.example"}),
		rule("a-second-user", UserRule, Match{Extension: ".mp4"}),
	}
	for _, tc := range []struct {
		remove Kind
		winner string
	}{
		{"", "z-first-user"}, {UserRule, "domain"}, {DomainRule, "mime"}, {MIMERule, "extension"}, {ExtensionRule, ""},
	} {
		if tc.remove != "" {
			kept := make([]Rule, 0, len(all))
			for _, r := range all {
				if r.Kind != tc.remove {
					kept = append(kept, r)
				}
			}
			all = kept
		}
		for i := 0; i < 30; i++ {
			d := evaluate(t, input(), config(all...))
			if d.RuleID != tc.winner {
				t.Fatalf("remove %s: %+v", tc.remove, d)
			}
			if tc.winner != "" && (d.Origin != RuleSelected || d.RuleVersion == nil || *d.RuleVersion != "snapshot-17") {
				t.Fatalf("missing provenance: %+v", d)
			}
		}
	}
}

func TestMatchNormalizationAndUnknownMIME(t *testing.T) {
	cases := []struct {
		name   string
		hint   Input
		rules  []Rule
		winner string
	}{
		{"domain case and final dot", Input{Domain: "FILES.EXAMPLE."}, []Rule{rule("domain", DomainRule, Match{Domain: "files.example"})}, "domain"},
		{"subdomain is distinct", Input{Domain: "evil.files.example"}, []Rule{rule("domain", DomainRule, Match{Domain: "files.example"})}, ""},
		{"URL not host", Input{Domain: "https://files.example/a"}, []Rule{rule("domain", DomainRule, Match{Domain: "files.example"})}, ""},
		{"MIME parameters", Input{MIME: "Video/MP4; codecs=avc1"}, []Rule{rule("mime", MIMERule, Match{MIME: "video/*"})}, "mime"},
		{"unknown MIME falls to suffix", Input{Name: "电影.MP4"}, []Rule{rule("mime", MIMERule, Match{MIME: "video/*"}), rule("ext", ExtensionRule, Match{Extension: ".mp4"})}, "ext"},
		{"invalid MIME is unknown", Input{MIME: "unknown", Name: "电影.mp4"}, []Rule{rule("mime", MIMERule, Match{MIME: "video/*"}), rule("ext", ExtensionRule, Match{Extension: ".mp4"})}, "ext"},
		{"no suffix inference to MIME", Input{Name: "x.mp4"}, []Rule{rule("mime", MIMERule, Match{MIME: "video/*"})}, ""},
		{"user conditions are AND", Input{Name: "x.mp4", Domain: "other.example"}, []Rule{rule("user", UserRule, Match{Domain: "files.example", Extension: ".mp4"})}, ""},
		{"MIME wildcard is bounded", Input{MIME: "videogame/mp4"}, []Rule{rule("mime", MIMERule, Match{MIME: "video/*"})}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.hint
			in.Path, in.Origin = root("original"), Default
			if got := evaluate(t, in, config(tc.rules...)); got.RuleID != tc.winner {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestFrozenRoundTripDoesNotRerouteOrInventProvenance(t *testing.T) {
	for _, frozen := range []Decision{
		{Root: "legacy/../untouched", Origin: Unknown},
		{Version: 99, Root: root("future"), Origin: "future-origin"},
		evaluate(t, input(), config(rule("old", ExtensionRule, Match{Extension: ".mp4"}))),
	} {
		encoded, err := json.Marshal(frozen)
		if err != nil {
			t.Fatal(err)
		}
		var restarted Decision
		if err := json.Unmarshal(encoded, &restarted); err != nil {
			t.Fatal(err)
		}
		in := Input{Path: "bad new path", Origin: "bad origin", Name: "changed.zip", Domain: "changed.example", Frozen: &restarted}
		got := evaluate(t, in, Config{Enabled: true, Version: "new-rules", Rules: []Rule{{}}})
		if !reflect.DeepEqual(got, frozen) {
			t.Fatalf("frozen changed: %+v -> %+v", frozen, got)
		}
		if got.RuleVersion != nil {
			*got.RuleVersion = "mutated preview"
			if *restarted.RuleVersion == *got.RuleVersion {
				t.Fatal("preview aliases persisted state")
			}
		}
	}
}

func TestOneRootForWholeRelativeTree(t *testing.T) {
	in := input()
	// Directory logical name has no extension or MIME. Member metadata does
	// not enter Input and cannot cause per-member routing.
	in.Name, in.MIME = "网盘目录", ""
	members := []string{"课程/视频.mp4", "课程/字幕.srt", "数据/原始.csv"}
	before := append([]string(nil), members...)
	cfg := config(rule("videos", ExtensionRule, Match{Extension: ".mp4"}), rule("tree", DomainRule, Match{Domain: "files.example"}))
	d := evaluate(t, in, cfg)
	if d.RuleID != "tree" {
		t.Fatalf("%+v", d)
	}
	for _, member := range members {
		if got := filepath.Join(d.Root, member); !strings.HasPrefix(got, d.Root+string(filepath.Separator)) {
			t.Fatalf("%s", got)
		}
	}
	if !reflect.DeepEqual(before, members) {
		t.Fatal("tree changed")
	}
}

func TestBasenameHintsDoNotCreateOrRewritePaths(t *testing.T) {
	cfg := config(rule("unicode", ExtensionRule, Match{Extension: ".é"}), rule("videos", ExtensionRule, Match{Extension: ".mp4"}))
	for _, tc := range []struct{ name, winner string }{
		{"报告.é", "unicode"}, {"报告.e\u0301", "unicode"},
		{strings.Repeat("界", 100) + ".MP4", "videos"}, {"电影.MP4", "videos"},
		{"../escape.mp4", ""}, {"..\\escape.mp4", ""}, {"/absolute.mp4", ""},
		{"%2e%2e%2fescape%2emp4", ""}, {"bad\x00.mp4", ""}, {"bad\xff.mp4", ""},
		{".mp4", ""}, {"..", ""}, {"", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := input()
			in.Name = tc.name
			before := in
			d := evaluate(t, in, cfg)
			if d.RuleID != tc.winner || in != before {
				t.Fatalf("%+v", d)
			}
		})
	}
}

func TestInvalidConfigurationReturnsNoDecision(t *testing.T) {
	valid := rule("one", ExtensionRule, Match{Extension: ".mp4"})
	cases := []Config{
		{Enabled: true}, config(Rule{}), config(valid, valid),
		config(Rule{ID: "id", Kind: ExtensionRule, Root: "relative", Match: Match{Extension: ".mp4"}}),
		config(rule("x", "future", Match{Extension: ".mp4"})),
		config(rule("x", UserRule, Match{})),
		config(rule("x", DomainRule, Match{Domain: "https://files.example"})),
		config(rule("x", MIMERule, Match{MIME: "*/*"})),
		config(rule("x", MIMERule, Match{MIME: "video/mp4; q=1"})),
		config(rule("x", ExtensionRule, Match{Extension: "mp4"})),
		config(rule("x", ExtensionRule, Match{Extension: ".../bad"})),
		config(rule("x", DomainRule, Match{Domain: "files.example", Extension: ".mp4"})),
		config(Rule{ID: "x", Kind: UserRule, Root: root("bad\x00"), Match: Match{Extension: ".mp4"}}),
	}
	for i, cfg := range cases {
		got, err := Evaluate(input(), cfg)
		if !errors.Is(err, ErrConfig) || got != (Decision{}) {
			t.Fatalf("case %d: %+v %v", i, got, err)
		}
	}
}

func TestInvalidNewInputReturnsNoDecision(t *testing.T) {
	for _, in := range []Input{
		{Path: "relative", Origin: Default}, {Path: "", Origin: Default},
		{Path: root("bad\x00"), Origin: Default}, {Path: root("bad\xff"), Origin: Default},
		{Path: root("valid"), Origin: "future-origin"},
	} {
		got, err := Evaluate(in, Config{})
		if !errors.Is(err, ErrInput) || got != (Decision{}) {
			t.Fatalf("%+v %v", got, err)
		}
	}
}

func TestEvaluationDoesNotMutateConfiguration(t *testing.T) {
	cfg := config(rule("UPPER", DomainRule, Match{Domain: "FILES.EXAMPLE."}))
	before, _ := json.Marshal(cfg)
	d := evaluate(t, input(), cfg)
	*d.RuleVersion = "mutated"
	after, _ := json.Marshal(cfg)
	if string(before) != string(after) {
		t.Fatalf("config mutated: %s", after)
	}
}
