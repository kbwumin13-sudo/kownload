//go:build !windows

package rest

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

func TestUnixSocketRecoveryPreservesNetdiskState(t *testing.T) {
	dir, err := os.MkdirTemp("", "kw-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	address := filepath.Join(dir, "core.sock")
	previous := Downloader
	server, listener, err := BuildServer(&model.StartConfig{
		Network: "unix", Address: address, Storage: model.StorageMem, StorageDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	downloader := Downloader
	defer func() { server.Close(); downloader.Close(); Downloader = previous }()
	go server.Serve(listener)
	client := &http.Client{Timeout: 250 * time.Millisecond, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", address)
		},
	}}
	defer client.CloseIdleConnections()
	providers := func() error {
		resp, err := client.Get("http://localhost/api/v1/netdisk/providers")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var result struct {
			Code int
			Data []json.RawMessage
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return err
		}
		if result.Code != 0 || len(result.Data) != 9 {
			t.Fatalf("unexpected provider response: %d / %d", result.Code, len(result.Data))
		}
		return nil
	}
	if err := providers(); err != nil {
		t.Fatal(err)
	}
	service := downloader.NetdiskService()
	for i := 0; i < 3; i++ {
		if err := os.Remove(address); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			err = providers()
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("providers unavailable after socket deletion #%d: %v", i+1, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if Downloader != downloader || downloader.NetdiskService() != service {
			t.Fatal("socket recovery restarted downloader or replaced login service")
		}
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(address); err != nil {
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)
	if _, err := os.Lstat(address); !os.IsNotExist(err) {
		t.Fatalf("closed socket recreated or retained: %v", err)
	}
}

func TestUnixSocketRecoveryKeepsOpenConnections(t *testing.T) {
	dir, err := os.MkdirTemp("", "kw-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	address := filepath.Join(dir, "core.sock")
	previous := Downloader
	server, listener, err := BuildServer(&model.StartConfig{
		Network: "unix", Address: address, Storage: model.StorageMem, StorageDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	downloader := Downloader
	defer func() { server.Close(); downloader.Close(); Downloader = previous }()
	started, finish := make(chan struct{}), make(chan struct{})
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			w.Write([]byte("before-"))
			w.(http.Flusher).Flush()
			close(started)
			<-finish
			w.Write([]byte("after"))
		} else {
			w.Write([]byte("ready"))
		}
	})
	go server.Serve(listener)
	client := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", address)
		},
	}}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://localhost/stream")
	if err != nil {
		close(finish)
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	if err := os.Remove(address); err != nil {
		close(finish)
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		r, err := client.Get("http://localhost/ready")
		if err == nil {
			r.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			close(finish)
			t.Fatalf("new connection did not recover: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(finish)
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "before-after" {
		t.Fatalf("active response interrupted: %q / %v", body, err)
	}
}

func TestUnixSocketSetupDoesNotModifyReplacement(t *testing.T) {
	dir, err := os.MkdirTemp("", "kw-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	address := filepath.Join(dir, "core.sock")
	listener, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(address); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(address, []byte("unrelated"), 0640); err != nil {
		t.Fatal(err)
	}
	disableUnixUnlink(listener)
	l := &recoveringUnixListener{listener: listener, address: address, done: make(chan struct{})}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(address)
	if err != nil || string(data) != "unrelated" {
		t.Fatalf("replacement lost: %q / %v", data, err)
	}
	info, err := os.Stat(address)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("replacement permissions changed: %v / %v", info, err)
	}
}

func TestUnixSocketMissingDuringSetupRecovers(t *testing.T) {
	dir, err := os.MkdirTemp("", "kw-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	address := filepath.Join(dir, "core.sock")
	raw, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(address); err != nil {
		t.Fatal(err)
	}
	disableUnixUnlink(raw)
	l := &recoveringUnixListener{listener: raw, address: address, done: make(chan struct{})}
	go l.watch()
	defer l.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.Dial("unix", address)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing setup path never recovered: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUnixSocketCloseDoesNotUnlinkAnotherListener(t *testing.T) {
	dir, err := os.MkdirTemp("", "kw-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	address := filepath.Join(dir, "core.sock")
	first, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(address); err != nil {
		t.Fatal(err)
	}
	second, err := net.Listen("unix", address)
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	defer second.Close()
	disableUnixUnlink(first)
	l := &recoveringUnixListener{listener: first, address: address, done: make(chan struct{})}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", address)
	if err != nil {
		t.Fatalf("closing old listener removed replacement: %v", err)
	}
	conn.Close()
}
