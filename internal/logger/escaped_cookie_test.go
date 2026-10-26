package logger

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Quotes and trailing backslashes have different escaped JSON boundaries.
// Check both the text contract and the real logger while retaining diagnostics.
func TestEscapedCookieStringBoundaries(t *testing.T) {
	for _, value := range []string{`sid="SYNTH-QUOTED"; other=SYNTH-OTHER`, `sid=SYNTH-END\`, `sid="SYNTH-QUOTE"; path=C:\SYNTH-DIR\`} {
		raw, _ := json.Marshal(map[string]string{"Cookie": value, "Accept": "public-metadata"})
		want, _ := json.Marshal(map[string]string{"Cookie": redacted, "Accept": "public-metadata"})
		for depth := 0; depth < 2; depth++ {
			input, expected := "headers "+string(raw)+"; task=T1", "headers "+string(want)+"; task=T1"
			if got := Redact(input); got != expected {
				t.Fatalf("redaction changed boundaries: got %q, want %q", got, expected)
			}
			path := filepath.Join(t.TempDir(), "core.log")
			l := NewLogger(true, path)
			l.Error().Err(errors.New(input)).Msg("request failed")
			l.CLose()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "SYNTH-") || !strings.Contains(string(data), "public-metadata") || !strings.Contains(string(data), "task=T1") {
				t.Fatalf("credential or diagnostic boundary failure: %s", data)
			}
			raw, _ = json.Marshal(string(raw))
			raw = raw[1 : len(raw)-1]
			want, _ = json.Marshal(string(want))
			want = want[1 : len(want)-1]
		}
	}
}

func TestEscapedJSONCookieFileLogger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.log")
	l := NewLogger(true, path)
	l.Error().Err(errors.New(`headers {\"Cookie\":\"sid=\\\"SYNTH-QUOTED\\\"; other=SYNTH-OTHER\"}`)).Msg("request failed")
	l.CLose()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SYNTH-") {
		t.Fatalf("synthetic credential survived file logger: %s", data)
	}
}
