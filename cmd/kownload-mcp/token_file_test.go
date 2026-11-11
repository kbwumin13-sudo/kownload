package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

func privateTokenDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAPITokenFileValidation(t *testing.T) {
	for _, sample := range []struct {
		name, text string
		mode       os.FileMode
		good       bool
	}{
		{"private", "SYNTHETIC_TOKEN", 0600, true}, {"readOnly", "SYNTHETIC_TOKEN\n", 0400, true},
		{"max", strings.Repeat("a", maxTokenFile), 0600, true},
		{"empty", "", 0600, false}, {"newline", "\n", 0600, false},
		{"tooLarge", strings.Repeat("a", maxTokenFile+1), 0600, false},
		{"CRLF", "SECRET\r\n", 0600, false}, {"multiline", "SECRET\nSECOND", 0600, false},
		{"twoNewlines", "SECRET\n\n", 0600, false}, {"space", " SECRET ", 0600, false},
		{"NUL", "SECRET\x00", 0600, false}, {"nonASCII", "秘密", 0600, false},
		{"public", "SECRET", 0644, false}, {"group", "SECRET", 0640, false},
		{"executable", "SECRET", 0700, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			path := filepath.Join(privateTokenDir(t), "token")
			if err := os.WriteFile(path, []byte(sample.text), sample.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, sample.mode); err != nil {
				t.Fatal(err)
			}
			got, err := readAPITokenFile(path)
			if sample.good {
				if err != nil || got != strings.TrimSuffix(sample.text, "\n") {
					t.Fatal("valid token rejected")
				}
			} else if err != errTokenFile || got != "" {
				t.Fatal("unsafe token accepted or error leaked")
			}
		})
	}
	for _, kind := range []string{"missing", "symlink", "ancestorSymlink", "hardlink", "directory", "fifo", "publicParent", "writableAncestor", "relative", "unclean", "emptyPath"} {
		t.Run(kind, func(t *testing.T) {
			root := privateTokenDir(t)
			path := filepath.Join(root, "token")
			if err := os.WriteFile(path, []byte("SYNTHETIC_SECRET"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "missing":
				path += "-missing"
			case "symlink":
				err = os.Symlink(path, path+"-link")
				path += "-link"
			case "ancestorSymlink":
				err = os.Symlink(root, filepath.Join(root, "alias"))
				path = filepath.Join(root, "alias", "token")
			case "hardlink":
				err = os.Link(path, path+"-link")
			case "directory":
				path = root
			case "fifo":
				path += "-fifo"
				err = unix.Mkfifo(path, 0600)
			case "publicParent":
				err = os.Chmod(root, 0755)
			case "writableAncestor":
				child := filepath.Join(root, "private")
				err = os.Mkdir(child, 0700)
				if err == nil {
					path = filepath.Join(child, "token")
					err = os.WriteFile(path, []byte("SECRET"), 0600)
				}
				if err == nil {
					err = os.Chmod(root, 0777)
				}
			case "relative":
				path = "token"
			case "unclean":
				path = root + "/./token"
			case "emptyPath":
				path = ""
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := readAPITokenFile(path); got != "" || err != errTokenFile {
				t.Fatal("unsafe path accepted or error leaked")
			}
		})
	}
	stat := unix.Stat_t{Mode: unix.S_IFREG | 0600, Uid: uint32(os.Getuid() + 1), Nlink: 1, Size: 10}
	if privateTokenFile(&stat) {
		t.Fatal("foreign owner accepted")
	}
}

func TestAPITokenReadOnceAndRedaction(t *testing.T) {
	const oldToken = "SYNTHETIC_OLD_SECRET"
	const newToken = "SYNTHETIC_NEW_SECRET"
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != readPrefix || r.Method != "GET" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected request")
		}
		if r.Header.Get("X-Api-Token") != oldToken {
			w.WriteHeader(401)
			w.Write([]byte("LEAK " + oldToken + newToken))
			return
		}
		reply(w, TaskList{Tasks: []Task{}, NextCursor: ""})
	})
	file := filepath.Join(privateTokenDir(t), "token")
	if err := os.WriteFile(file, []byte(oldToken), 0600); err != nil {
		t.Fatal(err)
	}
	token, err := readAPITokenFile(file)
	if err != nil {
		t.Fatal(err)
	}
	b := newBackend(path, token)
	defer b.close()
	if err := os.WriteFile(file, []byte(newToken), 0600); err != nil {
		t.Fatal(err)
	}
	if _, code := b.get(context.Background(), readPrefix+"?limit=1"); code != "" {
		t.Fatal("running backend reloaded token")
	}
	for _, token := range []string{"", newToken} {
		b := newBackend(path, token)
		data, code := b.get(context.Background(), readPrefix+"?limit=1")
		b.close()
		if data != nil || code != "app_authentication_failed" {
			t.Fatal("authentication failure not redacted")
		}
		raw, _ := json.Marshal(toolFailure(code))
		if strings.Contains(string(raw), oldToken) || strings.Contains(string(raw), newToken) {
			t.Fatal("secret escaped")
		}
	}
	token, err = readAPITokenFile(file)
	if err != nil || token != newToken {
		t.Fatal("new process cannot read rotated token")
	}
}

func TestAuthenticatedBackendDoesNotFollowRedirect(t *testing.T) {
	var calls atomic.Int32
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != readPrefix {
			t.Error("followed redirect with credential")
		}
		http.Redirect(w, r, "/private-token-target", http.StatusFound)
	})
	b := newBackend(path, "SYNTHETIC_SECRET")
	defer b.close()
	if _, code := b.get(context.Background(), readPrefix+"?limit=1"); code != "app_unavailable" {
		t.Fatal(code)
	}
	if calls.Load() != 1 {
		t.Fatal("redirect followed")
	}
}
