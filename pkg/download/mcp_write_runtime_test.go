//go:build darwin && cgo

package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/mcpwritepolicy"
)

type mcpRuntimeBodyGate struct {
	http.ResponseWriter
	ctx     context.Context
	block   *atomic.Bool
	written int
}

func (w *mcpRuntimeBodyGate) Write(body []byte) (int, error) {
	total := 0
	for len(body) > 0 {
		if w.written >= 65536 && w.block.Load() {
			select {
			case <-w.ctx.Done():
				return total, w.ctx.Err()
			case <-time.After(10 * time.Millisecond):
				continue
			}
		}
		n := len(body)
		if n > 16384 {
			n = 16384
		}
		written, err := w.ResponseWriter.Write(body[:n])
		total += written
		w.written += written
		body = body[written:]
		if flush, ok := w.ResponseWriter.(http.Flusher); ok {
			flush.Flush()
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func TestMCPWriteRealCoreCreatePauseResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rootBase, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(rootBase, "output")
	if err = os.Mkdir(output, 0700); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("same-MCP-Task-parent-byte-integrity\n"), 65536)
	expected := sha256.Sum256(payload)
	var blocked atomic.Bool
	blocked.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"mcp-fixed-object-v1"`)
		http.ServeContent(&mcpRuntimeBodyGate{ResponseWriter: w, ctx: r.Context(), block: &blocked}, r, "owned.bin", time.Unix(1700000000, 0), bytes.NewReader(payload))
	}))
	defer server.Close()
	var webhooks atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { webhooks.Add(1); w.WriteHeader(204) }))
	defer webhook.Close()
	dbPath := filepath.Join(rootBase, "state.db")
	var d *Downloader
	defer func() {
		if d != nil {
			_ = d.Close()
		}
	}()
	open := func(ids []string) *Downloader {
		t.Helper()
		store := NewBoltStorage(dbPath)
		if err := InitializeDatasetIdentity(store); err != nil {
			t.Fatal(err)
		}
		core := NewDownloader(&DownloaderConfig{Storage: store, StorageDir: filepath.Join(rootBase, "core"), FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
		identity, err := core.DatasetIdentity()
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"version": 1, "operations": map[string]bool{"create": true, "pause": true, "resume": true},
			"datasets":    []any{map[string]any{"id": identity.DatasetID, "taskIds": ids, "outputRoots": []string{"owned"}}},
			"outputRoots": map[string]string{"owned": output}, "allowedEffects": []string{}})
		policy, err := mcpwritepolicy.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		network, err := transfer.NewMCPNetworkPolicy(transfer.MCPNetworkPolicyConfig{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		client, err := transfer.NewMCPHTTPClient(network, transfer.MCPHTTPOptions{DialTimeout: time.Second, ResponseHeaderTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if err = core.ConfigureMCPWrites(policy, client); err != nil {
			t.Fatal(err)
		}
		if err = core.Setup(); err != nil {
			t.Fatal(err)
		}
		return core
	}
	d = open([]string{})
	identity, _ := d.DatasetIdentity()
	capability, err := d.MCPWriteCapabilities(ctx, MCPWriteCapabilityRequest{Version: 1, DatasetID: identity.DatasetID})
	if err != nil || capability.PolicyID == nil || !capability.Operations.Create {
		t.Fatal("configured create unavailable", err)
	}
	create := MCPWriteRequest{Version: 1, DatasetID: identity.DatasetID, PolicyID: *capability.PolicyID, OperationID: "mcp-real-create", Kind: MCPWriteCreate,
		Create: &MCPWriteCreateRequest{URL: server.URL, OutputRootID: "owned", FileName: "owned.bin"}}
	receipt, err := d.SubmitMCPWrite(ctx, create)
	if err != nil || receipt.Result == nil || receipt.Result.TaskID == nil {
		t.Fatalf("create failed: %+v %v", receipt, err)
	}
	id := *receipt.Result.TaskID
	waitWritten := func() {
		t.Helper()
		for ctx.Err() == nil {
			task := d.getTask(id)
			if task != nil {
				task.lock.Lock()
				f := task.fetcher
				written := int64(0)
				if f != nil {
					written = f.Progress().TotalDownloaded()
				}
				task.lock.Unlock()
				if task.getStatus() == base.DownloadStatusError {
					t.Fatalf("Task error: %+v", d.GetTask(id))
				}
				if written > 16384 {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("actual writer did not make progress", ctx.Err())
	}
	waitWritten()
	creation := d.GetTask(id).MCPWrite.CreationBinding
	if _, err := d.SubmitMCPWrite(ctx, create); err != nil || len(d.taskList()) != 1 {
		t.Fatal("same op replay changed Task count", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d = nil
	d = open([]string{id}) // Trusted policy reload; same DB and original Task.
	act := func(kind MCPWriteKind, op string) MCPWriteReceipt {
		t.Helper()
		capability, err := d.MCPWriteCapabilities(ctx, MCPWriteCapabilityRequest{Version: 1, DatasetID: identity.DatasetID, TaskID: id})
		if err != nil || capability.PolicyID == nil || capability.Task == nil {
			t.Fatal("trusted action tuple unavailable", err)
		}
		command := MCPWriteRequest{Version: 1, DatasetID: identity.DatasetID, PolicyID: *capability.PolicyID, OperationID: op, Kind: kind, Action: &MCPWriteActionRequest{TaskID: id, Expected: capability.Task.Expected}}
		result, err := d.SubmitMCPWrite(ctx, command)
		if err != nil || result.Result == nil || result.Result.Outcome != MCPWriteApplied {
			t.Fatalf("%s action failed: %+v %v", kind, result, err)
		}
		return result
	}
	act(MCPWriteResume, "mcp-real-resume-1")
	waitWritten()
	act(MCPWritePause, "mcp-real-pause")
	if task := d.GetTask(id); task.Status != base.DownloadStatusPause {
		t.Fatal("pause did not stop same Task", task.Status)
	}
	blocked.Store(false)
	act(MCPWriteResume, "mcp-real-resume-2")
	waitStatus(t, d, id, base.DownloadStatusDone, 8*time.Second)
	final := d.GetTask(id)
	if final.MCPWrite.Output.Phase != MCPWriteOutputPublished || !reflect.DeepEqual(final.MCPWrite.CreationBinding, creation) || final.MCPWrite.CurrentExecutionBinding.PolicyID == creation.PolicyID {
		t.Fatal("output/creation/current binding lost")
	}
	content, err := os.ReadFile(filepath.Join(output, "owned.bin"))
	if err != nil || sha256.Sum256(content) != expected || len(content) != len(payload) {
		t.Fatal("same Task complete bytes mismatch", err)
	}
	history, err := d.QueryMCPWrite(ctx, MCPWriteQuery{1, identity.DatasetID, create.PolicyID, create.OperationID})
	if err != nil || history.Result == nil || *history.Result.TaskID != id {
		t.Fatal("old P historical query changed", err)
	}
	if d.MCPTaskEffectAllowed(final, "script") || d.MCPTaskEffectAllowed(final, "webhook") || d.MCPTaskEffectAllowed(final, "auto_open") || webhooks.Load() != 0 {
		t.Fatal("default MCP effects were enabled")
	}
}
