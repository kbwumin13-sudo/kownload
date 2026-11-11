package download

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/logger"
	"github.com/GopeedLab/gopeed/pkg/base"
	gojaerror "github.com/GopeedLab/gopeed/pkg/download/engine/inject/error"
	"github.com/dop251/goja"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	gitfile "github.com/go-git/go-git/v5/plumbing/transport/file"
)

func TestDownloader_InstallExtensionByFolder(t *testing.T) {
	setupDownloader(func(downloader *Downloader) {
		if _, err := downloader.InstallExtensionByFolder("./testdata/extensions/basic", false); err != nil {
			t.Fatal(err)
		}
		rr, err := downloader.Resolve(&base.Request{
			URL: "https://github.com/test",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rr.Res.Files) == 1 {
			t.Fatal("resolve error")
		}
	})
}

func TestDownloader_InstallExtensionByFolderDevMode(t *testing.T) {
	setupDownloader(func(downloader *Downloader) {
		if _, err := downloader.InstallExtensionByFolder("./testdata/extensions/basic", true); err != nil {
			t.Fatal(err)
		}
		rr, err := downloader.Resolve(&base.Request{
			URL: "https://github.com/test",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rr.Res.Files) == 1 {
			t.Fatal("resolve error")
		}
	})
}

func TestDownloader_InstallExtensionByGit(t *testing.T) {
	testInstallExtensionByGit(t, "https://fixture.invalid/extensions#basic")
}

func TestDownloader_InstallExtensionByGitSimple(t *testing.T) {
	testInstallExtensionByGit(t, "fixture.invalid/extensions#basic")
}

func TestDownloader_InstallExtensionByGitFull(t *testing.T) {
	testInstallExtensionByGit(t, "https://fixture.invalid/extensions.git#basic")
}

// These tests verify URL normalization, cloning a subdirectory, installation,
// and script execution without fetching a changing upstream or its release API.
// Do not run them in parallel: go-git's transport registry is process-global.
func testInstallExtensionByGit(t *testing.T, installURL string) {
	t.Helper()
	repoDir := t.TempDir()
	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repoDir, "basic"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "index.js"} {
		data, err := os.ReadFile(filepath.Join("testdata", "extensions", "basic", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, "basic", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("basic"); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Commit("extension test fixture", &git.CommitOptions{
		Author: &object.Signature{Name: "Fixture", Email: "fixture@example.invalid", When: time.Unix(1, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	previous := gitclient.Protocols["https"]
	gitclient.InstallProtocol("https", extensionGitFixtureTransport{Transport: gitfile.DefaultClient, repoDir: repoDir})
	t.Cleanup(func() { gitclient.InstallProtocol("https", previous) })

	downloader := extensionTestDownloader(t)
	ext, err := downloader.InstallExtensionByGit(installURL)
	if err != nil {
		t.Fatal(err)
	}
	if ext.Name != "basic" || ext.DevMode || len(downloader.GetExtensions()) != 1 {
		t.Fatalf("unexpected installed extension: name=%q devMode=%v", ext.Name, ext.DevMode)
	}
	rr, err := downloader.Resolve(&base.Request{URL: "https://github.com/test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Res.Name != "test" || len(rr.Res.Files) != 2 {
		t.Fatalf("unexpected fixture resolution: name=%q files=%d", rr.Res.Name, len(rr.Res.Files))
	}
	for i, file := range rr.Res.Files {
		if file.Size != 1024 || file.Req.Labels["from"] != "basic" {
			t.Fatalf("fixture file %d did not come from installed script", i)
		}
	}
}

// Map only the expected normalized URL to a local repository. The file transport
// uses git-upload-pack, preserving the production shallow-clone behavior.
type extensionGitFixtureTransport struct {
	transport.Transport
	repoDir string
}

func (f extensionGitFixtureTransport) NewUploadPackSession(endpoint *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	if endpoint.String() != "https://fixture.invalid/extensions.git" {
		return nil, transport.ErrRepositoryNotFound
	}
	local, err := transport.NewEndpoint(f.repoDir)
	if err != nil {
		return nil, err
	}
	return f.Transport.NewUploadPackSession(local, auth)
}

func TestDownloader_UpgradeExtension(t *testing.T) {
	getSetting := func(settings []*Setting, name string) *Setting {
		for _, setting := range settings {
			if setting.Name == name {
				return setting
			}
		}
		return nil
	}

	setupDownloader(func(downloader *Downloader) {
		installedExt, err := downloader.InstallExtensionByFolder("./testdata/extensions/update", false)
		if err != nil {
			t.Fatal(err)
		}
		extensions := downloader.GetExtensions()
		if len(extensions) == 0 {
			t.Fatal("extension not installed")
		}
		oldVersion := installedExt.Version
		// fetch new version from git
		newVersion, err := downloader.UpgradeCheckExtension(installedExt.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if newVersion == "" {
			t.Fatal("new version not found")
		}
		// update extension
		if err = downloader.UpgradeExtension(installedExt.Identity); err != nil {
			t.Fatal(err)
		}
		upgradeExt := downloader.getExtension(installedExt.Identity)
		if upgradeExt.Version == oldVersion {
			t.Fatal("extension update fail")
		}

		// check setting update
		s1 := getSetting(upgradeExt.Settings, "s1")
		if s1.Title == "S1 old" {
			t.Fatal("setting update fail")
		}
		// check setting type update
		s2 := getSetting(upgradeExt.Settings, "s2")
		if s2.Type == "number" {
			t.Fatal("setting type update fail")
		}
		// check setting remove
		d1 := getSetting(upgradeExt.Settings, "d1")
		if d1 != nil {
			t.Fatal("setting remove fail")
		}
		// check setting add
		s3 := getSetting(upgradeExt.Settings, "s3")
		if s3 == nil {
			t.Fatal("setting add fail")
		}

		rr, err := downloader.Resolve(&base.Request{
			URL: "https://test.com",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if rr.Res.Name != "test" {
			t.Fatal("script update fail")
		}
	})
}

// The hook tests exercise real HTTP transfers, but never depend on a public host.
// A missing endpoint lets onStart rewrite before any I/O and onError recover
// only after an actual HTTP failure.
type extensionHookFixture struct {
	downloader *Downloader
	url        string
	payload    []byte
	missing    atomic.Bool
	served     atomic.Bool
}

func newExtensionHookFixture(t *testing.T) *extensionHookFixture {
	t.Helper()
	f := &extensionHookFixture{payload: []byte("local extension hook download\n")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing", "/start/missing":
			f.missing.Store(true)
			http.NotFound(w, r)
		case "/file", "/done/file":
			f.served.Store(true)
			http.ServeContent(w, r, "hook.txt", time.Time{}, bytes.NewReader(f.payload))
		default:
			t.Errorf("unexpected hook fixture request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	f.url = server.URL
	f.downloader = extensionTestDownloader(t)
	return f
}

func (f *extensionHookFixture) install(t *testing.T, name string) *Extension {
	t.Helper()
	ext, err := f.downloader.InstallExtensionByFolder("./testdata/extensions/"+name, false)
	if err != nil {
		t.Fatal(err)
	}
	return ext
}

func (f *extensionHookFixture) download(t *testing.T, req *base.Request, wantError bool) *Task {
	t.Helper()
	finished := make(chan *Event, 1)
	f.downloader.Listener(func(event *Event) {
		if event.Key == EventKeyFinally {
			finished <- event
		}
	})
	id, err := f.downloader.CreateDirect(req, &base.Options{Path: t.TempDir(), Name: "hook.txt"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-finished:
		if event.Task.ID != id {
			t.Fatalf("finished task = %s, want %s", event.Task.ID, id)
		}
		if (event.Err != nil) != wantError {
			t.Fatalf("download error = %v, wantError = %v", event.Err, wantError)
		}
		// onDone runs after EventKeyFinally. Return the event's snapshot;
		// its caller awaits the hook's storage acknowledgement before rereading.
		return event.Task
	case <-time.After(5 * time.Second):
		t.Fatal("local hook download did not finish")
	}
	return nil
}

func (f *extensionHookFixture) checkFile(t *testing.T, task *Task) {
	t.Helper()
	if task.Status != base.DownloadStatusDone {
		t.Fatalf("task status = %s, want done", task.Status)
	}
	got, err := os.ReadFile(task.Meta.SingleFilepath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, f.payload) {
		t.Fatalf("downloaded body = %q, want %q", got, f.payload)
	}
}

func TestDownloader_Extension_OnStart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		label   bool
		matches bool
	}{
		{name: "url match", path: "/start/missing", matches: true},
		{name: "label match", path: "/missing", label: true, matches: true},
		{name: "no match", path: "/missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExtensionHookFixture(t)
			f.install(t, "on_start")
			req := &base.Request{URL: f.url + tc.path, Labels: map[string]string{"fixtureUrl": f.url + "/file"}}
			if tc.label {
				req.Labels["test"] = "true"
			}
			task := f.download(t, req, !tc.matches)
			if !tc.matches {
				if task.Status != base.DownloadStatusError || task.Meta.Req.URL != f.url+tc.path || task.Meta.Req.Labels["modified"] != "" {
					t.Fatalf("unmatched onStart changed task: %+v", task)
				}
				if !f.missing.Load() || f.served.Load() {
					t.Fatal("unmatched onStart bypassed the missing resource")
				}
				return
			}
			if task.Meta.Req.URL != f.url+"/file" || task.Meta.Req.Labels["modified"] != "true" {
				t.Fatalf("onStart did not rewrite URL and label: %+v", task.Meta.Req)
			}
			if f.missing.Load() || !f.served.Load() {
				t.Fatal("onStart must rewrite the URL before HTTP I/O")
			}
			f.checkFile(t, task)
		})
	}
}

func TestDownloader_Extension_OnError(t *testing.T) {
	for _, matched := range []bool{true, false} {
		name := "no match"
		if matched {
			name = "label match recovers"
		}
		t.Run(name, func(t *testing.T) {
			f := newExtensionHookFixture(t)
			f.install(t, "on_error")
			req := &base.Request{URL: f.url + "/missing", Labels: map[string]string{"fixtureUrl": f.url + "/file"}}
			if matched {
				req.Labels["test"] = "true"
			}
			task := f.download(t, req, !matched)
			if !f.missing.Load() {
				t.Fatal("onError test did not exercise an HTTP failure")
			}
			if !matched {
				if task.Status != base.DownloadStatusError || task.Meta.Req.URL != f.url+"/missing" || f.served.Load() {
					t.Fatalf("unmatched onError recovered the task: %+v", task)
				}
				return
			}
			if task.Meta.Req.URL != f.url+"/file" || !f.served.Load() {
				t.Fatal("onError did not rewrite and continue the failed download")
			}
			f.checkFile(t, task)
		})
	}
}

func TestDownloader_Extension_OnDone(t *testing.T) {
	f := newExtensionHookFixture(t)
	ext := f.install(t, "on_done")
	task := f.download(t, &base.Request{URL: f.url + "/done/file"}, false)
	storage := &ContextStorage{storage: f.downloader.storage, identity: ext.buildIdentity()}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for storage.Get("hookDone") != "true" {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("onDone hook did not acknowledge completion")
		}
	}
	task = f.downloader.GetTask(task.ID)
	if task.Meta.Req.Labels["modified"] != "true" {
		t.Fatalf("onDone label = %q, want true", task.Meta.Req.Labels["modified"])
	}
	f.checkFile(t, task)
}

func TestDownloader_Extension_Errors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		logMessage string
		messageErr bool
	}{
		{name: "script_error", logMessage: "run script failed"},
		{name: "function_error", logMessage: "call function failed: onResolve"},
		{name: "message_error", logMessage: "call function failed: onResolve", messageErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExtensionHookFixture(t)
			var logs bytes.Buffer
			f.downloader.ExtensionLogger.Logger = f.downloader.ExtensionLogger.Output(&logs)
			ext := f.install(t, tc.name)
			rr, err := f.downloader.Resolve(&base.Request{URL: f.url + "/file"}, &base.Options{Path: t.TempDir()})
			if tc.messageErr {
				me, ok := err.(*gojaerror.MessageError)
				if !ok || me.Error() != "test" {
					t.Fatalf("expected MessageError(test), got %T: %v", err, err)
				}
				if rr != nil || f.served.Load() {
					t.Fatal("MessageError must prevent HTTP fallback")
				}
			} else {
				if err != nil {
					t.Fatalf("script exception should allow HTTP fallback: %v", err)
				}
				if rr == nil || rr.Res == nil || len(rr.Res.Files) != 1 || !f.served.Load() {
					t.Fatalf("expected one real HTTP resource instead of the script's two files: %+v", rr)
				}
				if rr.Res.Size != int64(len(f.payload)) || rr.Res.Files[0].Size != int64(len(f.payload)) {
					t.Fatalf("HTTP fallback resource size = %d/%d, want %d", rr.Res.Size, rr.Res.Files[0].Size, len(f.payload))
				}
			}
			// A successful fallback alone could also mean the script never ran.
			// Require the actual exception at the expected script/callback stage.
			var entry struct {
				Level   string `json:"level"`
				Message string `json:"message"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
				t.Fatalf("expected one structured exception log, got %q: %v", logs.String(), err)
			}
			if entry.Level != "error" || entry.Message != "["+ext.buildIdentity()+"] "+tc.logMessage {
				t.Fatalf("unexpected exception log: %+v", entry)
			}
			if tc.messageErr {
				if entry.Error != "test" {
					t.Fatalf("logged MessageError = %q, want test", entry.Error)
				}
			} else if !strings.Contains(entry.Error, "TypeError:") || !strings.Contains(entry.Error, "Cannot read property 'ccc'") {
				t.Fatalf("expected undefined-property TypeError, got %q", entry.Error)
			}
		})
	}
}

func TestDownloader_Extension_Settings(t *testing.T) {
	setupDownloader(func(downloader *Downloader) {
		if _, err := downloader.InstallExtensionByFolder("./testdata/extensions/settings_empty", false); err != nil {
			t.Fatal(err)
		}
		rr, err := downloader.Resolve(&base.Request{
			URL: "https://github.com/test",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rr.Res.Files) == 1 {
			t.Fatal("settings parse error")
		}
	})

	setupDownloader(func(downloader *Downloader) {
		installedExt, err := downloader.InstallExtensionByFolder("./testdata/extensions/settings_all", false)
		if err != nil {
			t.Fatal(err)
		}
		downloader.UpdateExtensionSettings(installedExt.Identity, map[string]any{
			"stringValued":  "valued",
			"numberValued":  1.1,
			"booleanValued": true,
		})
		rr, err := downloader.Resolve(&base.Request{
			URL: "https://github.com/test",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rr.Res.Files) == 1 {
			t.Fatal("settings parse error")
		}
	})
}

func TestDownloader_ExtensionStorage(t *testing.T) {
	setupDownloader(func(downloader *Downloader) {
		if _, err := downloader.InstallExtensionByFolder("./testdata/extensions/storage", false); err != nil {
			t.Fatal(err)
		}
		rr, err := downloader.Resolve(&base.Request{
			URL: "https://github.com/test",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rr.Res.Files) == 1 {
			t.Fatal("resolve error")
		}
	})
}

func TestDownloader_SwitchExtension(t *testing.T) {
	setupDownloader(func(downloader *Downloader) {
		installedExt, err := downloader.InstallExtensionByFolder("./testdata/extensions/basic", false)
		if err != nil {
			t.Fatal(err)
		}
		if installedExt.Disabled == true {
			t.Fatal("extension disabled")
		}
		if err = downloader.SwitchExtension(installedExt.Identity, false); err != nil {
			t.Fatal(err)
		}
		if installedExt.Disabled == false {
			t.Fatal("extension enabled")
		}
	})
}

func TestDownloader_DeleteExtension(t *testing.T) {
	setupDownloader(func(downloader *Downloader) {
		installedExt, err := downloader.InstallExtensionByFolder("./testdata/extensions/settings_all", false)
		if err != nil {
			t.Fatal(err)
		}
		extensions := downloader.GetExtensions()
		if err := downloader.DeleteExtension(installedExt.Identity); err != nil {
			t.Fatal(err)
		}
		extensions = downloader.GetExtensions()
		if len(extensions) != 0 {
			t.Fatal("extension delete fail")
		}
	})
}

func TestDownloader_Extension_Logger(t *testing.T) {
	logger := logger.NewLogger(false, "")
	il := newInstanceLogger(&Extension{
		Name: "test",
	}, logger)
	il.Debug(goja.NaN(), goja.Undefined())
	il.Info(goja.NaN(), goja.Undefined())
	il.Warn(goja.NaN(), goja.Undefined())
	il.Error(goja.NaN(), goja.Undefined())
}

func setupDownloader(fn func(downloader *Downloader)) {
	defaultDownloader.Setup()
	defaultDownloader.cfg.StorageDir = ".test_storage"
	defaultDownloader.cfg.DownloadDir = ".test_download"
	defer func() {
		defaultDownloader.Clear()
		os.RemoveAll(defaultDownloader.cfg.StorageDir)
		os.RemoveAll(defaultDownloader.cfg.DownloadDir)
	}()
	fn(defaultDownloader)
}
