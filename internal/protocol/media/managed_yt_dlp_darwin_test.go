package media

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

func managedFixtureHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func managedFixture(t *testing.T) *managedYtDlp {
	t.Helper()
	manager := newManagedYtDlp(filepath.Join(managedFixtureHome(t), ".local", "share", "fixture", "media"))
	t.Cleanup(func() { discardManagedVersion(manager.root) })
	return manager
}

func fixtureYtDlp(version, targets string) map[string]string {
	return map[string]string{
		"yt_dlp/__init__.py": `class YoutubeDL:
    def build_request_director(self): pass
    def process_video_result(self): pass
def main():
    from .version import __version__
    print(__version__)
`,
		"yt_dlp/version.py":             "__version__ = '" + version + "'\n",
		"yt_dlp/networking/__init__.py": "class Request: pass\n",
		"yt_dlp/networking/_urllib.py": `class UrllibRH:
    def _prepare_headers(self, *args): pass
class HTTPHandler:
    def http_request(self, *args): pass
    def do_open(self, *args): pass
`,
		"yt_dlp/networking/_curlcffi.py": "import curl_cffi\nclass CurlCFFIRH:\n    supported_targets = " + targets + "\n    def _send(self, *args): pass\n",
		"yt_dlp/networking/exceptions.py": `class HTTPError(Exception): pass
class RequestError(Exception): pass
class TransportError(Exception): pass
`,
		"yt_dlp/downloader/external.py": "class ExternalFD: pass\n",
		"yt_dlp/utils.py":               "class DownloadError(Exception): pass\ndef parse_m3u8_attributes(*args): pass\n",
	}
}

func fixtureManagedArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var data bytes.Buffer
	data.WriteString("#!/usr/bin/env python3\n")
	zw := zip.NewWriter(&data)
	for name, content := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func fixtureManagedPython(t *testing.T) (entry, site string) {
	t.Helper()
	base, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3.11+ required")
	}
	venv := filepath.Join(managedFixtureHome(t), "external")
	version, err := exec.Command(base, "-I", "-B", "-c", "import sys; print(sys.version_info.minor) if sys.version_info >= (3, 11) else sys.exit(1)").Output()
	if err != nil {
		t.Skip("Python 3.11+ required")
	}
	ctx := context.Background()
	if _, err := runManagedPython(ctx, 20*time.Second, base, "-I", "-B", "-m", "venv", "--copies", "--without-pip", venv); err != nil {
		t.Fatalf("external fixture venv: %v", err)
	}
	minor := strings.TrimSpace(string(version))
	site = filepath.Join(venv, "lib", "python3."+minor, "site-packages")
	python := filepath.Join(venv, "bin", "python3."+minor)
	for name, code := range fixtureYtDlp("2026.10.01", "('chrome', 'safari')") {
		target := filepath.Join(site, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(code), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(site, "curl_cffi.py"), []byte("from types import SimpleNamespace\nrequests = SimpleNamespace(Session=type('Session', (), {}))\n"), 0600); err != nil {
		t.Fatal(err)
	}
	entry = toolFixture(t, venv, "yt-dlp", "#!"+python+"\nimport sys\nsys.dont_write_bytecode = True\nimport yt_dlp\nyt_dlp.main()\n")
	return entry, site
}

func fixtureSigner(t *testing.T) (key []byte, fingerprint string, sign func([]byte) []byte) {
	t.Helper()
	config := &packet.Config{RSABits: 2048, DefaultHash: crypto.SHA256}
	entity, err := openpgp.NewEntity("Isolated", "", "fixture@example.test", config)
	if err != nil {
		t.Fatal(err)
	}
	var public bytes.Buffer
	armored, err := armor.Encode(&public, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = entity.Serialize(armored); err != nil {
		t.Fatal(err)
	}
	if err = armored.Close(); err != nil {
		t.Fatal(err)
	}
	return public.Bytes(), fmt.Sprintf("%X", entity.PrimaryKey.Fingerprint), func(data []byte) []byte {
		var signature bytes.Buffer
		if err := openpgp.DetachSign(&signature, entity, bytes.NewReader(data), config); err != nil {
			t.Fatal(err)
		}
		return signature.Bytes()
	}
}

func TestManagedYtDlpInstallUpdateRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	manager := managedFixture(t)
	entry, exteriorSite := fixtureManagedPython(t)
	stored := map[string]any{"ytDlpPath": entry, "unknown": true}
	external, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	curlBefore, err := os.ReadFile(filepath.Join(exteriorSite, "curl_cffi.py"))
	if err != nil {
		t.Fatal(err)
	}
	pub, pin, sign := fixtureSigner(t)
	var tag string
	var payload, sums, signature []byte
	var publisher sync.RWMutex
	setRelease := func(version, targets string) {
		publisher.Lock()
		defer publisher.Unlock()
		tag = version
		payload = fixtureManagedArchive(t, fixtureYtDlp(version, targets))
		sums = fmt.Appendf(nil, "%x  yt-dlp\n", sha256.Sum256(payload))
		signature = sign(sums)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publisher.RLock()
		defer publisher.RUnlock()
		switch r.URL.Path {
		case "/latest":
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
		case "/download/" + tag + "/" + managedAsset:
			_, _ = w.Write(payload)
		case "/download/" + tag + "/" + managedSums:
			_, _ = w.Write(sums)
		case "/download/" + tag + "/" + managedSignature:
			_, _ = w.Write(signature)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manager.fetch = func(ctx context.Context) (managedRelease, error) {
		return downloadManagedRelease(ctx, server.Client(), server.URL+"/latest", server.URL+"/download/", pub, pin)
	}

	if state, err := manager.State(stored); state.Entry != "" || err != nil {
		t.Fatalf("empty state: %+v %v", state, err)
	}
	if _, err := os.Stat(manager.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading state created managed directories: %v", err)
	}
	setRelease("2026.10.02", "('chrome', 'safari')")
	first, err := manager.Update(ctx, stored)
	if err != nil || first.ActiveVersion != tag || first.InUse || first.Entry == "" {
		t.Fatalf("first install: %+v %v", first, err)
	}
	assertEntry := func(version string) {
		t.Helper()
		got, state := readToolVersion(ctx, 5*time.Second, first.Entry, ytdlpVersion, "--ignore-config", "--version")
		if got != version || state != toolReady {
			t.Fatalf("entry got %s / %s, want %s", got, state, version)
		}
	}
	pointer := func() string {
		t.Helper()
		slot, err := os.Readlink(filepath.Join(manager.root, "current"))
		if err != nil {
			t.Fatal(err)
		}
		return slot
	}
	firstPointer := pointer()
	assertEntry("2026.10.02")
	if again, err := manager.Update(ctx, stored); err != nil || again != first || pointer() != firstPointer {
		t.Fatalf("same signed release was not idempotent: %+v %v", again, err)
	}
	selected := map[string]any{"ytDlpPath": first.Entry}
	// The selected interpreter remains isolated even under a different daemon
	// launch environment, when normal Python would consume these variables.
	t.Setenv("PYTHONPATH", exteriorSite)
	t.Setenv("PYTHONHOME", "/fixture/invalid-python-home")
	assertEntry("2026.10.02")

	for _, failure := range []struct {
		name string
		want error
		edit func()
	}{
		{"manifest signature", ErrManagedYtDlpSignature, func() {
			publisher.Lock()
			sums[0] ^= 1
			publisher.Unlock()
		}},
		{"asset digest", ErrManagedYtDlpAsset, func() {
			publisher.Lock()
			payload[0] ^= 1
			publisher.Unlock()
		}},
		{"lost curl target", ErrManagedYtDlpPython, func() { setRelease("2026.10.03", "('chrome',)") }},
		{"pointer rename", ErrManagedYtDlpLocation, func() {
			manager.rename = func(string, string) error { return errors.New("injected rename error") }
		}},
	} {
		t.Run(failure.name, func(t *testing.T) {
			setRelease("2026.10.03", "('chrome', 'safari')")
			failure.edit()
			_, err := manager.Update(ctx, selected)
			manager.rename = os.Rename
			if !errors.Is(err, failure.want) || pointer() != firstPointer {
				t.Fatalf("failed update changed pointer or returned wrong error: %v", err)
			}
			assertEntry("2026.10.02")
		})
	}
	setRelease("2026.10.03", "('chrome', 'safari')")
	second, err := manager.Update(ctx, selected)
	if err != nil || second.ActiveVersion != tag || second.PreviousVersion != first.ActiveVersion ||
		!second.InUse || second.Entry != first.Entry || pointer() == firstPointer {
		t.Fatalf("update: %+v %v", second, err)
	}
	assertEntry("2026.10.03")
	rollback, err := manager.Rollback(ctx, selected)
	if err != nil || rollback.ActiveVersion != first.ActiveVersion || rollback.PreviousVersion != second.ActiveVersion ||
		!rollback.InUse || rollback.Entry != first.Entry {
		t.Fatalf("rollback: %+v %v", rollback, err)
	}
	assertEntry("2026.10.02")

	if state, err := manager.State(stored); state.InUse || err != nil {
		t.Fatalf("external configuration was selected over: %+v %v", state, err)
	}
	// The fixture external interpreter does not use ._pth. Remove the test
	// override to verify the original file still runs normally.
	if err := os.Unsetenv("PYTHONHOME"); err != nil {
		t.Fatal(err)
	}
	externalVersion, state := readToolVersion(ctx, 5*time.Second, entry, ytdlpVersion, "--ignore-config", "--version")
	data, _ := os.ReadFile(entry)
	curlAfter, _ := os.ReadFile(filepath.Join(exteriorSite, "curl_cffi.py"))
	if externalVersion != "2026.10.01" || state != toolReady || !bytes.Equal(data, external) || !bytes.Equal(curlBefore, curlAfter) ||
		stored["ytDlpPath"] != entry || stored["unknown"] != true {
		t.Fatal("external tool or its configuration changed")
	}
}

func TestManagedYtDlpRejectsConcurrentOperationsAndUntrustedRoot(t *testing.T) {
	manager := managedFixture(t)
	entry, _ := fixtureManagedPython(t)
	stored := map[string]any{"ytDlpPath": entry}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	locked, release := make(chan struct{}), make(chan struct{})
	manager.fetch = func(context.Context) (managedRelease, error) {
		close(locked)
		<-release
		return managedRelease{}, ErrManagedYtDlpDownload
	}
	result := make(chan error, 1)
	go func() {
		_, err := manager.Update(ctx, stored)
		result <- err
	}()
	select {
	case <-locked:
		defer func() {
			close(release)
			<-result
		}()
	case err := <-result:
		t.Fatalf("first operation did not reach locked state: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	second := newManagedYtDlp(manager.root)
	if _, err := second.Rollback(ctx, stored); !errors.Is(err, ErrManagedYtDlpBusy) {
		t.Fatalf("concurrent operation: %v", err)
	}

	untrusted := managedFixture(t)
	home := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(untrusted.root))))
	if err := os.Symlink(managedFixtureHome(t), filepath.Join(home, ".local")); err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Update(ctx, stored); !errors.Is(err, ErrManagedYtDlpLocation) {
		t.Fatalf("symlink escape from root accepted: %v", err)
	}
}

func TestManagedYtDlpRejectsArchiveEscapeAndMismatchedKey(t *testing.T) {
	files := fixtureYtDlp("2026.10.01", "('chrome',)")
	files["yt_dlp/../../escape"] = "not extractable"
	dst := t.TempDir()
	if err := extractManagedModule(fixtureManagedArchive(t, files), dst); !errors.Is(err, ErrManagedYtDlpAsset) {
		t.Fatalf("unsafe archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("archive wrote outside the module")
	}
	key, pin, sign := fixtureSigner(t)
	data := []byte(strings.Repeat("1", 64) + "  yt-dlp\n")
	if _, err := signedManagedDigest(key, managedKeyFingerprint, data, sign(data)); !errors.Is(err, ErrManagedYtDlpSignature) {
		t.Fatalf("foreign key accepted: %v", err)
	}
	if _, err := signedManagedDigest(key, pin, data, sign(data)); err != nil {
		t.Fatalf("fixture key did not verify: %v", err)
	}
}
