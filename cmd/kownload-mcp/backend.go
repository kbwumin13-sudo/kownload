package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const readPrefix = "/api/v1/mcp/read/v1/tasks"
const maxBackend = 128 << 10

type backend struct {
	socket    string
	client    *http.Client
	transport *http.Transport
}

func newBackend(socket string) *backend {
	b := &backend{socket: socket}
	b.transport = &http.Transport{DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 8192, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if err := checkSocket(socket); err != nil {
			return nil, err
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, err
		}
		if err = checkPeer(conn); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}}
	b.client = &http.Client{Transport: b.transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return b
}
func (b *backend) close() { b.transport.CloseIdleConnections() }

// Startup flags are trusted configuration, never tool inputs. Validate each
// component again for every connection. Peer UID adds a post-connect check;
// neither UID nor a product name authenticates a different process of this user.
func checkSocket(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("untrusted socket")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("unsupported owner")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink")
		}
		if current == path {
			if info.Mode()&os.ModeSocket == 0 || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0 {
				return errors.New("socket permissions")
			}
		} else {
			if !info.IsDir() || (stat.Uid != 0 && int(stat.Uid) != os.Getuid()) {
				return errors.New("directory owner")
			}
			if current == filepath.Dir(path) && (int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0) {
				return errors.New("private socket parent required")
			}
			if info.Mode().Perm()&0022 != 0 && !(info.Mode()&os.ModeSticky != 0 && current != filepath.Dir(path)) {
				return errors.New("writable ancestor")
			}
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return nil
}

type envelope struct {
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data"`
}

func (b *backend) get(ctx context.Context, path string) (json.RawMessage, string) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://kownload.local"+path, nil)
	if err != nil {
		return nil, "invalid_arguments"
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Kownload-Read-Version", "1")
	response, err := b.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "cancelled"
		}
		return nil, "app_unavailable"
	}
	defer response.Body.Close()
	if response.StatusCode == 404 {
		return nil, "endpoint_or_task_unavailable"
	}
	if response.StatusCode != 200 {
		return nil, "app_unavailable"
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBackend+1))
	if err != nil || len(data) > maxBackend {
		return nil, "invalid_app_response"
	}
	var result envelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || result.Version != 1 || len(result.Data) == 0 {
		return nil, "incompatible_read_api"
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, "invalid_app_response"
	}
	return result.Data, ""
}
