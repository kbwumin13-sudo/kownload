package download

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/logger"
	"github.com/GopeedLab/gopeed/pkg/base"
	gojaerror "github.com/GopeedLab/gopeed/pkg/download/engine/inject/error"
	"github.com/dop251/goja"
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
	setupDownloader(func(downloader *Downloader) {
		if _, err := downloader.InstallExtensionByGit("https://github.com/GopeedLab/gopeed-extension-samples#github-release-sample"); err != nil {
			t.Fatal(err)
		}
		rr, err := downloader.Resolve(&base.Request{
			URL: "https://github.com/GopeedLab/gopeed/releases",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rr.Res.Files) == 1 {
			t.Fatal("resolve error")
		}
	})
}

func TestDownloader_InstallExtensionByGitSimple(t *testing.T) {
	setupDownloader(func(downloader *Downloader) {
		if _, err := downloader.InstallExtensionByGit("github.com/GopeedLab/gopeed-extension-samples#github-release-sample"); err != nil {
			t.Fatal(err)
		}
		rr, err := downloader.Resolve(&base.Request{
			URL: "https://github.com/GopeedLab/gopeed/releases",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rr.Res.Files) == 1 {
			t.Fatal("resolve error")
		}
	})
}

func TestDownloader_InstallExtensionByGitFull(t *testing.T) {
	setupDownloader(func(downloader *Downloader) {
		if _, err := downloader.InstallExtensionByGit("https://github.com/GopeedLab/gopeed-extension-samples.git#github-release-sample"); err != nil {
			t.Fatal(err)
		}
		rr, err := downloader.Resolve(&base.Request{
			URL: "https://github.com/GopeedLab/gopeed/releases",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rr.Res.Files) == 1 {
			t.Fatal("resolve error")
		}
	})
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
	setupDownloader(func(downloader *Downloader) {
		if _, err := downloader.InstallExtensionByFolder("./testdata/extensions/script_error", false); err != nil {
			t.Fatal(err)
		}
		rr, err := downloader.Resolve(&base.Request{
			URL: "https://github.com/test",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rr.Res.Files) == 2 {
			t.Fatal("script error catch failed")
		}
	})

	setupDownloader(func(downloader *Downloader) {
		if _, err := downloader.InstallExtensionByFolder("./testdata/extensions/function_error", false); err != nil {
			t.Fatal(err)
		}
		rr, err := downloader.Resolve(&base.Request{
			URL: "https://github.com/test",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rr.Res.Files) == 2 {
			t.Fatal("function error catch failed")
		}
	})

	setupDownloader(func(downloader *Downloader) {
		if _, err := downloader.InstallExtensionByFolder("./testdata/extensions/message_error", false); err != nil {
			t.Fatal(err)
		}
		_, err := downloader.Resolve(&base.Request{
			URL: "https://github.com/test",
		}, nil)
		if err == nil {
			t.Fatalf("except error, but got nil")
		}
		me, ok := err.(*gojaerror.MessageError)
		if !ok {
			t.Fatalf("except MessageError type, but got %s", err)
		}
		want := "test"
		if me.Error() != want {
			t.Fatalf("except MessageError message %s, but got %s", want, me.Message)
		}
	})
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
