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
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

func TestMediaToolsRouteIsPrivateAuthenticatedAndTakesNoProbeInputs(t *testing.T) {
	dir, err := os.MkdirTemp("", "kw-tools-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	yt := filepath.Join(dir, "custom-yt-dlp")
	if err := os.WriteFile(yt, []byte("#!/bin/sh\necho 2026.10.26"), 0700); err != nil {
		t.Fatal(err)
	}
	ff := filepath.Join(dir, "missing-ffmpeg")
	node := filepath.Join(dir, "missing-node")
	token := "fixture-rest-token"
	previousCore, previousServer := Downloader, srv
	t.Cleanup(func() { Downloader, srv = previousCore, previousServer })

	start := func(network, address string) (*http.Client, string) {
		t.Helper()
		server, listener, err := BuildServer(&model.StartConfig{
			Network: network, Address: address, ApiToken: token,
			Storage: model.StorageMem, StorageDir: dir,
		})
		if err != nil {
			t.Fatal(err)
		}
		core := Downloader
		t.Cleanup(func() { server.Close(); core.Close() })
		config, err := core.GetConfig()
		if err != nil {
			t.Fatal(err)
		}
		config.ProtocolConfig["media"] = map[string]any{
			"ytDlpPath": yt, "ffmpegPath": ff, "jsRuntimePath": node,
		}
		if err := core.PutConfig(config); err != nil {
			t.Fatal(err)
		}
		go server.Serve(listener)
		transport := &http.Transport{DisableKeepAlives: true}
		origin := "http://" + listener.Addr().String()
		if network == "unix" {
			origin = "http://localhost"
			transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
		}
		return &http.Client{Transport: transport}, origin
	}

	request := func(client *http.Client, url, method, token, origin string, body io.Reader, want int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, url, body)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("X-Api-Token", token)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		content, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s %s: %d, want %d, %s (%v)", method, url, resp.StatusCode, want, content, err)
		}
		return content
	}

	client, baseURL := start("unix", filepath.Join(dir, "core.sock"))
	target := baseURL + mediaToolPath
	request(client, target, "GET", "", "", nil, http.StatusUnauthorized)
	request(client, target, "GET", "wrong", "", nil, http.StatusUnauthorized)
	request(client, target, "GET", token, "https://other.invalid", nil, http.StatusForbidden)
	request(client, target+"?path=/bin/sh", "GET", token, "", nil, http.StatusBadRequest)
	request(client, target, "GET", token, "", strings.NewReader(`{"path":"/bin/sh"}`), http.StatusBadRequest)
	request(client, target, "POST", token, "", nil, http.StatusMethodNotAllowed)
	content := request(client, target, "GET", token, "", nil, http.StatusOK)
	var result struct {
		Code int
		Data struct {
			YtDlp  struct{ Path, Version, State string }
			FFmpeg struct{ Path, State string }
			Node   struct{ Path, State string }
			Bridge struct{ State string }
		}
	}
	if err := json.Unmarshal(content, &result); err != nil || result.Code != 0 ||
		result.Data.YtDlp.Path != yt || result.Data.YtDlp.Version != "2026.10.26" ||
		result.Data.FFmpeg.Path != ff || result.Data.FFmpeg.State != "missing" ||
		result.Data.Node.Path != node || result.Data.Node.State != "missing" ||
		result.Data.Bridge.State != "unsupported" {
		t.Fatalf("unexpected daemon response: %s (%v)", content, err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	request(client, target, "GET", token, "", nil, http.StatusForbidden)
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}

	tcp, lan := start("tcp", "127.0.0.1:0")
	content = request(tcp, lan+mediaToolPath, "GET", token, "", nil, http.StatusNotFound)
	if strings.Contains(string(content), dir) {
		t.Fatalf("TCP response exposed local path: %s", content)
	}
}
