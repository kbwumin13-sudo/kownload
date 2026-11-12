package pathpolicy

import (
	"strings"
	"unicode/utf8"

	"github.com/GopeedLab/gopeed/pkg/util"
	"golang.org/x/text/unicode/norm"
)

// Preparation is a value preview, not an ACK, filesystem binding or IO permit.
// PrepareSingleFile is only for a new decision; existing F must be read as-is.
type Preparation struct {
	Decision   Decision `json:"decision"`
	Name       string   `json:"name"`
	Layout     LayoutV1 `json:"layout"`
	LayoutHash string   `json:"layoutHash"`
}

func PrepareSingleFile(in Input, cfg Config) (Preparation, error) {
	if in.Frozen != nil || !utf8.ValidString(in.Name) || strings.ContainsRune(in.Name, 0) {
		return Preparation{}, ErrInput
	}
	// Classification sees the original NFC basename before lossy truncation.
	decision, err := Evaluate(in, cfg)
	if err != nil {
		return Preparation{}, err
	}
	name := util.SafeFilename(norm.NFC.String(in.Name))
	return prepareName(decision, name)
}

// FinalizeSingleFile accepts the caller's single duplicate-name decision.
// It never evaluates rules again, sanitizes that name into a different name,
// or mutates a caller's old preparation. A name collision after F stays a
// conflict at the anchored IO boundary, not another auto-rename opportunity.
func FinalizeSingleFile(prior Preparation, finalName string) (Preparation, error) {
	if err := ValidatePreparation(prior); err != nil {
		return Preparation{}, err
	}
	return prepareName(prior.Decision, finalName)
}

func prepareName(decision Decision, name string) (Preparation, error) {
	if decision.Version != Version {
		return Preparation{}, ErrUnsupportedVersion
	}
	if !validRoot(decision.Root) || !safeFinalBasename(name) {
		return Preparation{}, ErrInput
	}
	if decision.Origin == RuleSelected {
		if decision.RuleID == "" || decision.RuleVersion == nil || strings.TrimSpace(*decision.RuleVersion) == "" {
			return Preparation{}, ErrInput
		}
	} else {
		switch decision.Origin {
		case Unknown, Default, UserSelected, ManualCategory, APIExplicit, SourceSuggested:
		default:
			return Preparation{}, ErrInput
		}
		if decision.RuleID != "" || decision.RuleVersion != nil {
			return Preparation{}, ErrInput
		}
	}
	if decision.RuleVersion != nil {
		version := *decision.RuleVersion
		decision.RuleVersion = &version
	}
	layout, err := BuildLayoutV1(decision.Root, []Member{{Index: 0, RelativePath: name, Selected: true}})
	if err != nil {
		return Preparation{}, err
	}
	hash, err := LayoutFingerprint(layout)
	if err != nil {
		return Preparation{}, err
	}
	return Preparation{Decision: decision, Name: name, Layout: layout, LayoutHash: hash}, nil
}

func ValidatePreparation(value Preparation) error {
	canonical, err := prepareName(value.Decision, value.Name)
	if err != nil {
		return err
	}
	hash, err := LayoutFingerprint(value.Layout)
	if err != nil {
		return err
	}
	if value.LayoutHash != canonical.LayoutHash || hash != canonical.LayoutHash {
		return ErrLayout
	}
	return nil
}

func safeFinalBasename(name string) bool {
	return name != "" && name != "." && name != ".." && utf8.ValidString(name) &&
		!strings.ContainsAny(name, "/\\\x00<>:\"|?*\r\n")
}
