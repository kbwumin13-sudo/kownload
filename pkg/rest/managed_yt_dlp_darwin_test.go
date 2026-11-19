package rest

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

func TestManagedYtDlpRoutesRequireOwnedLocalSocketAndNoArguments(t *testing.T) {
	root, err := os.MkdirTemp("", "kw-my-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("HOME", root)
	socket := filepath.Join(root, "ipc", "api.sock")
	if err := os.Mkdir(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	token := "fixture-only"
	previousCore, previousServer := Downloader, srv
	t.Cleanup(func() { Downloader, srv = previousCore, previousServer })
	start := func(network, address string) (*http.Client, string) {
		t.Helper()
		server, listener, err := BuildServer(&model.StartConfig{
			Network: network, Address: address, ApiToken: token,
			Storage: model.StorageMem, StorageDir: root,
		})
		if err != nil {
			t.Fatal(err)
		}
		core := Downloader
		t.Cleanup(func() { _ = server.Close(); _ = core.Close() })
		cfg, err := core.GetConfig()
		if err != nil {
			t.Fatal(err)
		}
		cfg.ProtocolConfig["media"] = map[string]any{"ytDlpPath": "/fixture/external-missing"}
		if err := core.PutConfig(cfg); err != nil {
			t.Fatal(err)
		}
		go server.Serve(listener)
		transport := &http.Transport{DisableKeepAlives: true}
		host := "http://" + listener.Addr().String()
		if network == "unix" {
			host = "http://localhost"
			transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
		}
		return &http.Client{Transport: transport, Timeout: 5 * time.Second}, host
	}

	request := func(client *http.Client, method, target, auth, origin string, body io.Reader, code int, expected string) {
		t.Helper()
		req, err := http.NewRequest(method, target, body)
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			req.Header.Set("X-Api-Token", auth)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != code || !strings.Contains(string(raw), expected) {
			t.Fatalf("%s %s: http %d body %q err %v", method, target, resp.StatusCode, raw, err)
		}
	}
	client, local := start("unix", socket)
	target := local + managedYtDlpPath
	request(client, "GET", target, "", "", nil, 401, "")
	request(client, "GET", target, token, "", nil, 200, `"activeVersion":""`)
	request(client, "POST", target+"/update?path=/bin/sh", token, "", nil, 400, "invalid_arguments")
	request(client, "POST", target+"/update", token, "", strings.NewReader(`{"python":"/bin/sh"}`), 400, "invalid_arguments")
	request(client, "POST", target+"/update", token, "https://other.invalid", nil, 403, "local_media_tools_required")
	request(client, "POST", target+"/update", token, "", nil, 409, "managed_ytdlp_python")
	request(client, "POST", target+"/rollback", token, "", nil, 409, "managed_ytdlp_no_previous")
	if config, _ := Downloader.GetConfig(); config.ProtocolConfig["media"].(map[string]any)["ytDlpPath"] != "/fixture/external-missing" {
		t.Fatal("managed operations wrote general configuration")
	}
	client, host := start("tcp", "127.0.0.1:0")
	request(client, "GET", host+managedYtDlpPath, token, "", nil, 404, "")
}
