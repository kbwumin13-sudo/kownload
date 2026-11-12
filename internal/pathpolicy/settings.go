package pathpolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

const ConfigKey = "pathPolicyV1"
const SettingsVersion = 1

// Settings is the configuration wire value. Config.Version is derived from
// its canonical contents, never supplied by the editor or recomputed at F.
type Settings struct {
	Version int    `json:"version"`
	Enabled bool   `json:"enabled"`
	Rules   []Rule `json:"rules"`
}

func CloneConfig(cfg Config) Config {
	cfg.Rules = append([]Rule{}, cfg.Rules...)
	return cfg
}

// EncodeSettings returns an owned compact value for the existing Extra map.
// It does not write configuration or assign a Task any execution permission.
func EncodeSettings(cfg Config) (json.RawMessage, error) {
	cfg = CloneConfig(cfg)
	if err := validateSettings(cfg); err != nil {
		return nil, err
	}
	return json.Marshal(Settings{Version: SettingsVersion, Enabled: cfg.Enabled, Rules: cfg.Rules})
}

// DecodeSettings accepts absence as default OFF. An explicit null, unknown
// version, unknown field or duplicate key is an error, never a default reset.
func DecodeSettings(raw []byte) (Config, error) {
	if len(raw) == 0 {
		raw = []byte(`{"version":1,"enabled":false,"rules":[]}`)
	}
	fields, err := settingsObject(raw, "version", "enabled", "rules")
	if err != nil {
		return Config{}, err
	}
	var version int
	if json.Unmarshal(fields["version"], &version) != nil {
		return Config{}, ErrConfig
	}
	if version != SettingsVersion {
		return Config{}, ErrUnsupportedVersion
	}
	var enabled bool
	if value, ok := fields["enabled"]; ok {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &enabled) != nil {
			return Config{}, ErrConfig
		}
	}
	rules := []Rule{}
	if value, ok := fields["rules"]; ok {
		var entries []json.RawMessage
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &entries) != nil {
			return Config{}, ErrConfig
		}
		for _, entry := range entries {
			row, err := settingsObject(entry, "id", "kind", "match", "root")
			if err != nil {
				return Config{}, err
			}
			if _, err = settingsObject(row["match"], "domain", "mime", "extension"); err != nil {
				return Config{}, err
			}
			var rule Rule
			if json.Unmarshal(entry, &rule) != nil {
				return Config{}, ErrConfig
			}
			rules = append(rules, rule)
		}
	}
	cfg := Config{Enabled: enabled, Rules: rules}
	canonical, err := EncodeSettings(cfg)
	if err != nil {
		return Config{}, err
	}
	sum := sha256.Sum256(canonical)
	cfg.Version = hex.EncodeToString(sum[:])
	return cfg, nil
}

func validateSettings(cfg Config) error {
	if err := validate(cfg); err != nil {
		return err
	}
	ids := make(map[string]bool, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		if strings.TrimSpace(rule.ID) == "" || ids[rule.ID] {
			return ErrConfig
		}
		ids[rule.ID] = true
	}
	return nil
}

func settingsObject(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrConfig
	}
	out := make(map[string]json.RawMessage)
	for d.More() {
		token, err = d.Token()
		key, ok := token.(string)
		known := false
		for _, field := range allowed {
			known = known || key == field
		}
		if err != nil || !ok || !known {
			return nil, ErrConfig
		}
		if _, duplicate := out[key]; duplicate {
			return nil, ErrConfig
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, ErrConfig
		}
		out[key] = value
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return nil, ErrConfig
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrConfig
	}
	return out, nil
}

func ReadConfig(extra map[string]any) (Config, error) {
	value, present := extra[ConfigKey]
	if !present {
		return DecodeSettings(nil)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return Config{}, ErrConfig
	}
	return DecodeSettings(raw)
}

// WithConfig deep-copies unrelated Extra values using UseNumber. The caller
// owns the result and persists it through the existing configuration writer.
func WithConfig(extra map[string]any, cfg Config) (map[string]any, error) {
	value, err := EncodeSettings(cfg)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(extra)
	if err != nil {
		return nil, ErrConfig
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var owned map[string]any
	if d.Decode(&owned) != nil {
		return nil, ErrConfig
	}
	if owned == nil {
		owned = make(map[string]any)
	}
	owned[ConfigKey] = value
	return owned, nil
}
