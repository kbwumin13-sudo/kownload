// Package pathpolicy previews one destination root for a logical download.
// It performs no IO. A decision is not permission to write: the owner must
// persist it with the task and receive a successful ACK before any file IO.
// Existing frozen decisions must come from trusted task state, never request
// parameters. Filesystem anchoring and no-replace publication belong to S4.
package pathpolicy

import (
	"errors"
	"mime"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/GopeedLab/gopeed/pkg/util"
	"golang.org/x/text/unicode/norm"
)

const Version = 1

type Origin string

const (
	Unknown         Origin = "unknown"
	Default         Origin = "default"
	UserSelected    Origin = "user_selected"
	ManualCategory  Origin = "manual_category"
	APIExplicit     Origin = "api_explicit"
	SourceSuggested Origin = "source_suggested"
	RuleSelected    Origin = "rule"
)

type Kind string

const (
	UserRule      Kind = "user"
	DomainRule    Kind = "domain"
	MIMERule      Kind = "mime"
	ExtensionRule Kind = "extension"
)

// Match uses conjunction for user rules. Empty fields impose no condition;
// at least one field is required. Domains match exact hosts, not subdomains.
// Domain matching folds case and ignores one final dot, with no IDNA or
// subdomain expansion. MIME folds case, ignores parameters in hints, and
// accepts exact type or type/* rules (rule parameters are rejected).
// Extensions require one leading dot and match the final suffix, folded to
// lowercase and NFC; compound suffixes and dot-only hidden names are excluded.
type Match struct {
	Domain    string
	MIME      string
	Extension string
}

type Rule struct {
	ID    string
	Kind  Kind
	Match Match
	Root  string
}

// Config.Rules is ordered by the user. Within each kind, the first matching
// rule wins. A selected rule's ID must be nonempty and unique in the snapshot.
// Version identifies that immutable rule snapshot, not this package's format;
// it is required only when selecting a rule. The zero config is disabled.
type Config struct {
	Enabled bool
	Version string
	Rules   []Rule
}

// Input represents a whole task/tree, never individual members. Path is the
// original fallback root after existing default/placeholder resolution.
// Domain is a parsed hostname; MIME and Name are optional logical root hints.
// Callers must not aggregate member hints to classify a multi-file tree.
type Input struct {
	Path   string
	Origin Origin
	Frozen *Decision
	Domain string
	MIME   string
	Name   string
}

// Decision is a preview value suitable for the owner's persistence contract.
// RuleID is empty and RuleVersion nil unless a rule selected the root.
// Version is the evaluator format; legacy frozen values remain unchanged.
// No member names or relative paths are modified by this package.
type Decision struct {
	Version     int
	Root        string
	Origin      Origin
	RuleID      string
	RuleVersion *string
}

var (
	ErrInput              = errors.New("invalid path policy input")
	ErrConfig             = errors.New("invalid path policy configuration")
	ErrUnsupportedVersion = errors.New("unsupported path policy version")
)

// Evaluate applies frozen > explicit > ordered user > domain > MIME >
// extension > original root. Unknown incoming provenance preserves its root
// even when rules are enabled; path equality never establishes explicitness.
// Frozen state is copied verbatim without reinterpreting legacy provenance.
// An unknown frozen version returns that copy AND ErrUnsupportedVersion so
// preservation/preview cannot silently authorize execution of unknown state.
func Evaluate(in Input, cfg Config) (Decision, error) {
	if in.Frozen != nil {
		d := *in.Frozen
		if d.RuleVersion != nil {
			v := *d.RuleVersion
			d.RuleVersion = &v
		}
		if d.Version != Version {
			return d, ErrUnsupportedVersion
		}
		return d, nil
	}
	if !validRoot(in.Path) {
		return Decision{}, ErrInput
	}
	origin := in.Origin
	if origin == "" {
		origin = Unknown
	}
	d := Decision{Version: Version, Root: in.Path, Origin: origin}
	switch origin {
	case UserSelected, ManualCategory, APIExplicit, Unknown:
		return d, nil
	case Default, SourceSuggested:
	default:
		return Decision{}, ErrInput
	}
	if !cfg.Enabled {
		return d, nil
	}
	if err := validate(cfg); err != nil {
		return Decision{}, err
	}
	hints := Match{Domain: domain(in.Domain), MIME: mediaType(in.MIME), Extension: extension(in.Name)}
	for _, kind := range []Kind{UserRule, DomainRule, MIMERule, ExtensionRule} {
		for _, rule := range cfg.Rules {
			if rule.Kind == kind && matches(rule.Match, hints) {
				if !validSelectedRule(rule, cfg) {
					return Decision{}, ErrConfig
				}
				v := cfg.Version
				return Decision{Version: Version, Root: rule.Root, Origin: RuleSelected, RuleID: rule.ID, RuleVersion: &v}, nil
			}
		}
	}
	return d, nil
}

func validRoot(root string) bool {
	return utf8.ValidString(root) && filepath.IsAbs(root) && !strings.ContainsRune(root, 0)
}

func validate(cfg Config) error {
	for _, r := range cfg.Rules {
		if !validRoot(r.Root) {
			return ErrConfig
		}
		m := r.Match
		if m == (Match{}) || (m.Domain != "" && domain(m.Domain) == "") ||
			(m.MIME != "" && !validMIMEPattern(m.MIME)) ||
			(m.Extension != "" && !validExtension(m.Extension)) {
			return ErrConfig
		}
		switch r.Kind {
		case UserRule:
		case DomainRule:
			if m.Domain == "" || m.MIME != "" || m.Extension != "" {
				return ErrConfig
			}
		case MIMERule:
			if m.MIME == "" || m.Domain != "" || m.Extension != "" {
				return ErrConfig
			}
		case ExtensionRule:
			if m.Extension == "" || m.Domain != "" || m.MIME != "" {
				return ErrConfig
			}
		default:
			return ErrConfig
		}
	}
	return nil
}

func validSelectedRule(rule Rule, cfg Config) bool {
	if strings.TrimSpace(cfg.Version) == "" || strings.TrimSpace(rule.ID) == "" {
		return false
	}
	count := 0
	for _, r := range cfg.Rules {
		if r.ID == rule.ID {
			count++
		}
	}
	return count == 1
}

func matches(want, have Match) bool {
	if want.Domain != "" && (have.Domain == "" || domain(want.Domain) != have.Domain) {
		return false
	}
	if want.Extension != "" && (have.Extension == "" || strings.ToLower(norm.NFC.String(want.Extension)) != have.Extension) {
		return false
	}
	if want.MIME != "" {
		pattern := strings.ToLower(want.MIME)
		if have.MIME == "" {
			return false
		}
		if strings.HasSuffix(pattern, "/*") {
			if !strings.HasPrefix(have.MIME, strings.TrimSuffix(pattern, "*")) {
				return false
			}
		} else if pattern != have.MIME {
			return false
		}
	}
	return true
}

// Hints are not URLs or paths: never extract a filename/host from arbitrary
// text, decode escaped separators, sniff content, or infer MIME from suffix.
func domain(value string) string {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if value == "" || strings.ContainsAny(value, "/\\:@?#% \t\r\n\x00") {
		return ""
	}
	return value
}

func mediaType(value string) string {
	v, _, err := mime.ParseMediaType(value)
	if err != nil || !validMIMEPattern(v) || strings.Contains(v, "*") {
		return ""
	}
	return strings.ToLower(v)
}

func validMIMEPattern(value string) bool {
	if value != strings.TrimSpace(value) || strings.Contains(value, ";") {
		return false
	}
	v, _, err := mime.ParseMediaType(value)
	parts := strings.Split(v, "/")
	return err == nil && len(parts) == 2 && parts[0] != "" && parts[0] != "*" &&
		parts[1] != "" && !strings.Contains(parts[0], "*") && (parts[1] == "*" || !strings.Contains(parts[1], "*"))
}

func validExtension(value string) bool {
	return utf8.ValidString(value) && strings.HasPrefix(value, ".") && len(value) > 1 &&
		!strings.ContainsAny(value[1:], ". /\\\t\r\n\x00%")
}

func extension(name string) string {
	if !utf8.ValidString(name) || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		return ""
	}
	// Reuse existing UTF-8 safe filename handling for the matching hint only.
	// The original filename/tree remains the fetcher's responsibility.
	name = util.SafeFilename(norm.NFC.String(name))
	ext := filepath.Ext(name)
	if len(name) == len(ext) || !validExtension(ext) {
		return ""
	}
	return strings.ToLower(ext)
}
