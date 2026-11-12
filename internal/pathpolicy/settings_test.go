package pathpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestSettingsCanonicalRevisionAndOwnedExtra(t *testing.T) {
	raw := []byte(`{"version":1,"enabled":true,"rules":[{"id":"video","kind":"extension","match":{"extension":".mp4"},"root":"/Video"}]}`)
	cfg, err := DecodeSettings(raw)
	if err != nil || len(cfg.Version) != 64 || cfg.Rules[0].ID != "video" {
		t.Fatal(cfg, err)
	}
	encoded, err := EncodeSettings(cfg)
	if err != nil || !bytes.Equal(encoded, raw) {
		t.Fatal(string(encoded), err)
	}
	reordered, err := DecodeSettings([]byte(`{"rules":[{"root":"/Video","match":{"extension":".mp4"},"kind":"extension","id":"video"}],"enabled":true,"version":1}`))
	if err != nil || reordered.Version != cfg.Version {
		t.Fatal(reordered, err)
	}
	extra := map[string]any{"other": map[string]any{"n": json.Number("9007199254740993")}}
	owned, err := WithConfig(extra, cfg)
	if err != nil {
		t.Fatal(err)
	}
	owned["other"].(map[string]any)["n"] = json.Number("7")
	if extra["other"].(map[string]any)["n"] != json.Number("9007199254740993") {
		t.Fatal("aliased unrelated configuration")
	}
	read, err := ReadConfig(owned)
	if err != nil || read.Version != cfg.Version {
		t.Fatal(read, err)
	}
	read.Rules[0].Root = "/Changed"
	again, _ := ReadConfig(owned)
	if again.Rules[0].Root != "/Video" {
		t.Fatal("aliased stored rules")
	}
	clone := CloneConfig(cfg)
	clone.Rules[0].Root = "/Other"
	if cfg.Rules[0].Root != "/Video" {
		t.Fatal("config clone aliases input")
	}
}

func TestSettingsDefaultAndStrictShape(t *testing.T) {
	cfg, err := ReadConfig(nil)
	if err != nil || cfg.Enabled || cfg.Rules == nil || len(cfg.Rules) != 0 {
		t.Fatal(cfg, err)
	}
	cases := []string{
		`null`, `{"version":1,"enabled":null}`, `{"version":1,"rules":null}`,
		`{"version":1,"version":1}`, `{"version":1,"unknown":false}`,
		`{"version":1,"rules":[{"id":"x","kind":"domain","root":"/X","match":{"domain":"a","domain":"b"}}]}`,
		`{"version":1,"rules":[{"id":"x","kind":"domain","root":"/X","match":{"domain":"a","extra":"x"}}]}`,
		`{"version":1,"rules":[{"id":"x","kind":"domain","root":"/X","match":{"domain":"a"}},{"id":"x","kind":"domain","root":"/Y","match":{"domain":"b"}}]}`,
	}
	for _, raw := range cases {
		if got, err := DecodeSettings([]byte(raw)); !errors.Is(err, ErrConfig) || got.Rules != nil {
			t.Fatalf("accepted invalid settings %s: %#v %v", raw, got, err)
		}
	}
	if _, err := ReadConfig(map[string]any{ConfigKey: map[string]any{"version": 2}}); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatal(err)
	}
}
