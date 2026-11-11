package download

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/rs/zerolog"
)

func TestVerifiedHTTPSuccessPermissionAndReentrantDelete(t *testing.T) {
	for _, removal := range []string{"none", "delete", "close"} {
		t.Run(removal, func(t *testing.T) {
			d, task, owner, lease := verifiedWrittenFixture(t, bytes.Repeat([]byte("fresh success\n"), 100))
			var sent atomic.Int32
			received := make(chan struct{}, 4)
			webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1); received <- struct{}{}; w.WriteHeader(204) }))
			defer webhook.Close()
			marker := filepath.Join(t.TempDir(), "script-count")
			script := filepath.Join(t.TempDir(), "success.sh")
			if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf x >> '"+marker+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			d.cfg.Webhook = &base.WebhookConfig{Enable: true, URLs: []string{webhook.URL}}
			d.cfg.Script = &base.ScriptConfig{Enable: true, Paths: []string{script}}
			var done, finally atomic.Int32
			callbackErr := make(chan error, 2)
			d.Listener(func(event *Event) {
				switch event.Key {
				case EventKeyDone:
					done.Add(1)
					switch removal {
					case "delete":
						callbackErr <- d.Delete(&TaskFilter{IDs: []string{task.ID}}, false)
					case "close":
						callbackErr <- d.Close()
					}
				case EventKeyFinally:
					finally.Add(1)
				}
			})
			fresh, err := d.completeVerifiedHTTP(task, quiescedVerifiedWriter{}, lease.Token.Generation, lease)
			if err != nil || !fresh {
				t.Fatal(fresh, err)
			}
			permit := task.verifiedSuccess.Load()
			if permit == nil {
				t.Fatal("fresh ACK did not issue private success permission")
			}
			// A normal watcher exits and invalidates its transport lease. This must not
			// suppress already accepted completion effects, or recreate a run permit.
			owner.invalidateToken(lease.Token)
			returned := make(chan struct{})
			go func() { d.deliverVerifiedSuccess(task); close(returned) }()
			select {
			case <-returned:
			case <-time.After(3 * time.Second):
				t.Fatal("success callback reentry deadlocked")
			}
			d.deliverVerifiedSuccess(task)
			if done.Load() != 1 {
				t.Fatal("done delivery repeated", done.Load())
			}
			if removal != "none" {
				if err := <-callbackErr; err != nil {
					t.Fatal(err)
				}
				if finally.Load() != 0 || permit.ctx.Err() == nil || d.verifiedSuccessAllowed(permit) {
					t.Fatal("removed task kept fresh permission")
				}
				data := &ScriptData{Event: ScriptEventDownloadDone, Payload: &ScriptPayload{Task: task.clone()}}
				if err := d.executeScriptAtPath(script, data, permit); !errors.Is(err, context.Canceled) {
					t.Fatal("late script accepted", err)
				}
				if _, err := d.sendWebhookToUrl(webhook.URL, &WebhookData{}, permit); !errors.Is(err, context.Canceled) {
					t.Fatal("late webhook accepted", err)
				}
				if sent.Load() != 0 {
					t.Fatal("removed task sent webhook")
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatal("removed task ran script", err)
				}
			} else {
				select {
				case <-received:
				case <-time.After(3 * time.Second):
					t.Fatal("fresh webhook absent")
				}
				deadline := time.Now().Add(3 * time.Second)
				var b []byte
				for time.Now().Before(deadline) {
					b, _ = os.ReadFile(marker)
					if len(b) > 0 {
						break
					}
					time.Sleep(time.Millisecond)
				}
				if string(b) != "x" || finally.Load() != 1 || sent.Load() != 1 {
					t.Fatal("fresh success effects", string(b), finally.Load(), sent.Load())
				}
			}
		})
	}
}

func TestVerifiedHTTPExtensionRevocationStopsNextScript(t *testing.T) {
	d, task, _, lease := verifiedWrittenFixture(t, bytes.Repeat([]byte("extension fresh C\n"), 100))
	fresh, err := d.completeVerifiedHTTP(task, quiescedVerifiedWriter{}, lease.Token.Generation, lease)
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	permit := task.verifiedSuccess.Load()
	root := t.TempDir()
	first := `gopeed.events.onDone(function(){gopeed.logger.info("revoke-success");gopeed.storage.set("first", "true");});`
	second := `gopeed.events.onDone(function(){gopeed.storage.set("second", "true");});`
	if err = os.WriteFile(filepath.Join(root, "first.js"), []byte(first), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "second.js"), []byte(second), 0600); err != nil {
		t.Fatal(err)
	}
	ext := &Extension{Identity: "verified-gate", Name: "verified-gate", DevMode: true, DevPath: root, Scripts: []*Script{{Event: string(EventOnDone), Match: &Match{Urls: []string{task.Meta.Req.URL}}, Entry: "first.js"}, {Event: string(EventOnDone), Match: &Match{Urls: []string{task.Meta.Req.URL}}, Entry: "second.js"}}}
	d.extensionLock.Lock()
	d.extensions = append(d.extensions, ext)
	d.extensionLock.Unlock()
	removed := make(chan error, 1)
	d.ExtensionLogger.Logger = d.ExtensionLogger.Hook(zerolog.HookFunc(func(_ *zerolog.Event, _ zerolog.Level, message string) {
		if message == "[verified-gate] revoke-success" {
			removed <- d.Delete(&TaskFilter{IDs: []string{task.ID}}, false)
		}
	}))
	d.triggerOnDone(task, permit)
	select {
	case err := <-removed:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("first actual JS callback did not revoke membership")
	}
	storage := &ContextStorage{storage: d.storage, identity: ext.buildIdentity()}
	if storage.Get("first") != "true" {
		t.Fatal("first admitted extension did not finish")
	}
	if storage.Get("second") != nil {
		t.Fatal("second extension ran after prior callback deleted Task")
	}
	if d.getTask(task.ID) != nil || d.verifiedSuccessAllowed(permit) {
		t.Fatal("extension deletion retained success permission")
	}
}
