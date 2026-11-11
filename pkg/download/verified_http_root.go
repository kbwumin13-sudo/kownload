package download

import (
	"os"
	"path/filepath"
	"strings"
)

// Resolve existing parent aliases once, before R freezes the effective target.
// No directories are created here. The stage adapter subsequently pins every
// component with NOFOLLOW and rejects namespace replacement after this choice.
func canonicalVerifiedRoot(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", ErrVerifiedHTTPUnsupportedOption
	}
	parent := filepath.Clean(absolute)
	var missing []string
	for {
		info, err := os.Stat(parent)
		if err == nil {
			if !info.IsDir() {
				return "", ErrVerifiedHTTPUnsupportedOption
			}
			resolved, err := filepath.EvalSymlinks(parent)
			if err != nil {
				return "", ErrVerifiedHTTPUnsupportedOption
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", ErrVerifiedHTTPUnsupportedOption
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", ErrVerifiedHTTPUnsupportedOption
		}
		missing = append(missing, filepath.Base(parent))
		parent = next
	}
}

func (d *Downloader) freezeVerifiedRoot(path string) (string, error) {
	root, err := canonicalVerifiedRoot(path)
	if err != nil {
		return "", err
	}
	if len(d.cfg.WhiteDownloadDirs) == 0 {
		return root, nil
	}
	for _, allowed := range d.cfg.WhiteDownloadDirs {
		canonical, err := canonicalVerifiedPattern(allowed)
		if err != nil {
			continue
		}
		if matched, err := filepath.Match(canonical, root); err == nil && matched {
			return root, nil
		}
	}
	return "", ErrVerifiedHTTPUnsupportedOption
}

// A glob is syntax, never an existing filesystem entry. Only its fixed literal
// parent can be resolved. In particular, a file literally named '*' must not
// turn an allow-pattern into its symlink destination. Preserve escapes and
// metacharacters in the remaining pattern exactly; do not Clean across them.
func canonicalVerifiedPattern(pattern string) (string, error) {
	if !filepath.IsAbs(pattern) {
		return "", ErrVerifiedHTTPUnsupportedOption
	}
	if _, err := filepath.Match(pattern, ""); err != nil {
		return "", ErrVerifiedHTTPUnsupportedOption
	}
	parts := strings.Split(pattern, string(filepath.Separator))
	for i, part := range parts {
		if !strings.ContainsAny(part, "*?[\\") {
			continue
		}
		prefix := strings.Join(parts[:i], string(filepath.Separator))
		if prefix == "" {
			prefix = string(filepath.Separator)
		}
		canonical, err := canonicalVerifiedRoot(prefix)
		if err != nil {
			return "", err
		}
		tail := strings.Join(parts[i:], string(filepath.Separator))
		return strings.TrimSuffix(canonical, string(filepath.Separator)) + string(filepath.Separator) + tail, nil
	}
	return canonicalVerifiedRoot(pattern)
}
