package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

const managedPythonProbe = `
import json
import os
import sys
import sysconfig
import yt_dlp
from yt_dlp.networking._curlcffi import CurlCFFIRH, curl_cffi

if sys.version_info < (3, 11) or sys.version_info.major != 3:
    raise SystemExit(1)
targets = CurlCFFIRH.supported_targets
if not targets:
    raise SystemExit(1)
print(json.dumps({
    "minor": sys.version_info.minor,
    "stdlib": os.path.realpath(sysconfig.get_path("stdlib")),
    "dynload": os.path.realpath(sysconfig.get_config_var("DESTSHARED")),
    "purelib": os.path.realpath(sysconfig.get_path("purelib")),
    "platlib": os.path.realpath(sysconfig.get_path("platlib")),
    "curl": os.path.realpath(curl_cffi.__file__),
    "module": os.path.realpath(yt_dlp.__file__),
    "isolated": bool(sys.flags.isolated),
    "targets": sorted(map(str, targets)),
}))
`

type pythonMediaEnvironment struct {
	Minor    int
	Stdlib   string
	Dynload  string
	Purelib  string
	Platlib  string
	Curl     string
	Module   string
	Isolated bool
	Targets  []string
}

func managedPythonInfo(ctx context.Context, python string, external bool) (pythonMediaEnvironment, error) {
	var result pythonMediaEnvironment
	args := []string{"-B", "-c", managedPythonProbe}
	if external {
		args = append([]string{"-I"}, args...)
	}
	output, err := runManagedPython(ctx, 5*time.Second, python, args...)
	if err != nil || json.Unmarshal(output, &result) != nil || !result.Isolated ||
		result.Minor < 11 || result.Minor > 99 || len(result.Targets) == 0 {
		return result, ErrManagedYtDlpPython
	}
	if external {
		for _, directory := range []string{result.Stdlib, result.Dynload, result.Purelib, result.Platlib} {
			if !filepath.IsAbs(directory) || strings.ContainsAny(directory, "\x00\r\n") ||
				checkOwnedPath(directory, true, true) != nil {
				return result, ErrManagedYtDlpPython
			}
		}
		if !pathUnder(result.Purelib, result.Curl) && !pathUnder(result.Platlib, result.Curl) {
			return result, ErrManagedYtDlpPython
		}
	}
	return result, nil
}

func runManagedPython(ctx context.Context, timeout time.Duration, python string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var output boundedToolOutput
	cmd := exec.CommandContext(ctx, python, args...)
	cmd.Env = cleanEnvironment()
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil || output.overflow {
		return nil, ErrManagedYtDlpPython
	}
	return output.buffer.Bytes(), nil
}

func prepareManagedPython(ctx context.Context, dir string, release managedRelease, source string, env pythonMediaEnvironment) (managedVersion, error) {
	var version managedVersion
	venv := filepath.Join(dir, "venv")
	python := filepath.Join(venv, "bin", fmt.Sprintf("python3.%d", env.Minor))
	if !validShebangPath(python) {
		return version, ErrManagedYtDlpLocation
	}
	if _, err := runManagedPython(ctx, 25*time.Second, source, "-I", "-B", "-m", "venv",
		"--copies", "--without-pip", venv); err != nil {
		return version, err
	}
	site := filepath.Join(venv, "lib", fmt.Sprintf("python3.%d", env.Minor), "site-packages")
	if physical, err := filepath.EvalSymlinks(site); err != nil || physical != site {
		return version, ErrManagedYtDlpLocation
	}
	if err := extractManagedModule(release.asset, site); err != nil {
		return version, err
	}

	// A private ._pth enables CPython isolated mode for both the CLI's
	// shebang and the existing bridge's direct -c call. Avoid "import site":
	// inherited .pth hooks and PYTHONPATH cannot replace the signed module.
	paths := []string{env.Stdlib, env.Dynload, site, env.Purelib}
	if env.Platlib != env.Purelib {
		paths = append(paths, env.Platlib)
	}
	if err := os.WriteFile(python+"._pth", []byte(strings.Join(paths, "\n")+"\n"), 0600); err != nil {
		return version, ErrManagedYtDlpLocation
	}
	launcher := "#!" + python + "\nimport sys\nsys.dont_write_bytecode = True\nimport yt_dlp\nyt_dlp.main()\n"
	if err := os.WriteFile(filepath.Join(dir, "yt-dlp"), []byte(launcher), 0700); err != nil {
		return version, ErrManagedYtDlpLocation
	}
	return managedVersion{Tag: release.version, Digest: release.digest, Source: source,
		Curl: env.Curl, Targets: env.Targets}, nil
}

func verifyManagedPython(ctx context.Context, dir string, version managedVersion) error {
	entry := filepath.Join(dir, "yt-dlp")
	cfg := model.Config{YtDlpPath: entry}
	interpreter, err := mediaBridgeInterpreter(cfg)
	if err != nil || interpreter == "" || !pathUnder(filepath.Join(dir, "venv", "bin"), interpreter) {
		return ErrManagedYtDlpPython
	}
	env, err := managedPythonInfo(ctx, interpreter, false)
	if err != nil || env.Curl != version.Curl || !includesTargets(env.Targets, version.Targets) {
		return ErrManagedYtDlpPython
	}
	modulePath := filepath.Join(dir, "venv", "lib", fmt.Sprintf("python3.%d", env.Minor),
		"site-packages", "yt_dlp", "__init__.py")
	if env.Module != modulePath {
		return ErrManagedYtDlpPython
	}
	cliVersion, state := readToolVersion(ctx, 5*time.Second, entry, ytdlpVersion, "--ignore-config", "--version")
	if state != toolReady || cliVersion != version.Tag {
		return ErrManagedYtDlpPython
	}
	bridge := checkBridge(ctx, 5*time.Second, cfg)
	if bridge.State != toolReady || bridge.Version != version.Tag {
		return ErrManagedYtDlpPython
	}
	return nil
}

func includesTargets(have, needed []string) bool {
	for _, target := range needed {
		if !slices.Contains(have, target) {
			return false
		}
	}
	return true
}
