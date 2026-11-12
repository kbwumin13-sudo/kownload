//go:build darwin && cgo

package rest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

type mcpLiveUDS struct {
	core   *download.Downloader
	client *http.Client
	close  func() error
}

func mcpLiveStart(t *testing.T, root string) *mcpLiveUDS {
	t.Helper()
	socket := filepath.Join(root, "api.sock")
	s, listener, err := BuildServer(&model.StartConfig{Network: "unix", Address: socket, Storage: model.StorageBolt, StorageDir: filepath.Join(root, "db"), ApiToken: verifiedLiveToken, StartupTaskPolicy: "imported-review"})
	if err != nil {
		t.Fatal(err)
	}
	f := &mcpLiveUDS{core: Downloader}
	f.client = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	served := make(chan struct{})
	go func() { defer close(served); _ = s.Serve(listener) }()
	var once sync.Once
	var closeErr error
	f.close = func() error {
		once.Do(func() {
			f.client.CloseIdleConnections()
			_ = s.Close()
			_ = listener.Close()
			closeErr = f.core.Close()
			<-served
		})
		return closeErr
	}
	t.Cleanup(func() {
		if err := f.close(); err != nil {
			t.Errorf("normal Core close: %v", err)
		}
	})
	return f
}

func (f *mcpLiveUDS) call(t *testing.T, method, path string, body any, out any) int {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := http.NewRequest(method, "http://localhost"+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Api-Token", verifiedLiveToken)
	response, err := f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("%s %s: %d %s", method, path, response.StatusCode, data)
	}
	var wrapper struct {
		Version int             `json:"version"`
		Code    int             `json:"code"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &wrapper) != nil || wrapper.Code != 0 || len(wrapper.Data) == 0 {
		t.Fatalf("invalid actual wrapper: %s", data)
	}
	if strings.HasPrefix(path, mcpWritePrefix) && wrapper.Version != 1 {
		t.Fatal("MCP outer version missing")
	}
	if out != nil && json.Unmarshal(wrapper.Data, out) != nil {
		t.Fatalf("invalid public DTO %s", wrapper.Data)
	}
	return response.StatusCode
}

// One actual receiver chain; no private Core state is changed by the fixture.
// The paired reopen installs explicit Task scope, never create-implied scope.
func TestMCPWriteBuildServerUDSCreatePauseResume(t *testing.T) {
	root, err := os.MkdirTemp("/private/tmp", "mwr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			_ = os.RemoveAll(root)
		} else {
			t.Log("retained", root)
		}
	})
	previousCore, previousServer, previousAES, previousPort := Downloader, srv, aesKey, runningPort
	t.Cleanup(func() { Downloader, srv, aesKey, runningPort = previousCore, previousServer, previousAES, previousPort })
	directory := filepath.Join(root, "out")
	if err = os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store := download.NewBoltStorage(filepath.Join(root, "db"))
	if err = download.InitializeDatasetIdentity(store); err != nil {
		t.Fatal(err)
	}
	var identity download.DatasetIdentity
	if ok, e := store.Get("config", "dataset_identity_v1", &identity); e != nil || !ok {
		t.Fatal("initial domain", e)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(root, "policy.json")
	writePolicy := func(ids []string) {
		policy := map[string]any{"version": 1, "operations": map[string]bool{"create": true, "pause": true, "resume": true}, "datasets": []any{map[string]any{"id": identity.DatasetID, "taskIds": ids, "outputRoots": []string{"owned"}}}, "outputRoots": map[string]string{"owned": directory}}
		data, e := json.Marshal(policy)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(policyPath, data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	writePolicy([]string{})
	payload := bytes.Repeat([]byte("real-uds-mcp-bytes"), 16384)
	var allow atomic.Bool
	var ranges atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"mcp-uds-owned"`)
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			return
		}
		ranges.Add(1)
		begin, end := 0, len(payload)-1
		if _, e := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end); e != nil || begin < 0 || end < begin || end >= len(payload) {
			w.WriteHeader(416)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
		w.Header().Set("Content-Length", strconv.Itoa(end-begin+1))
		w.WriteHeader(206)
		prefix := min(begin+16384, end+1)
		_, _ = w.Write(payload[begin:prefix])
		w.(http.Flusher).Flush()
		for !allow.Load() {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
		_, _ = w.Write(payload[prefix : end+1])
	}))
	t.Cleanup(source.Close)
	t.Setenv("KOWNLOAD_MCP_WRITE_POLICY_FILE", policyPath)
	t.Setenv("KOWNLOAD_MCP_WRITE_ALLOWED_CIDRS", "127.0.0.1/32")
	_, port, _ := net.SplitHostPort(source.Listener.Addr().String())
	t.Setenv("KOWNLOAD_MCP_WRITE_ALLOWED_PORTS", port)
	f := mcpLiveStart(t, root)
	var capability download.MCPWriteCapability
	capPath := mcpWritePrefix + "/capabilities?datasetId=" + identity.DatasetID
	f.call(t, "GET", capPath, nil, &capability)
	if !capability.Operations.Create || capability.PolicyID == nil {
		t.Fatal("real write startup unavailable")
	}
	originalPolicy := *capability.PolicyID
	command := download.MCPWriteRequest{Version: 1, DatasetID: identity.DatasetID, PolicyID: originalPolicy, OperationID: "owned-create", Kind: download.MCPWriteCreate, Create: &download.MCPWriteCreateRequest{URL: source.URL + "/owned", OutputRootID: "owned", FileName: "one.bin"}}
	var receipt download.MCPWriteReceipt
	f.call(t, "POST", mcpWritePrefix+"/operations", command, &receipt)
	if receipt.Result == nil || receipt.Result.TaskID == nil || receipt.Result.Outcome != download.MCPWriteCreated {
		t.Fatalf("create not committed: %+v", receipt)
	}
	id := *receipt.Result.TaskID
	var task download.Task
	wait := func(ready func() bool) {
		t.Helper()
		until := time.Now().Add(8 * time.Second)
		for !ready() {
			if time.Now().After(until) {
				t.Fatalf("actual task boundary not reached: %s", task.Status)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	readTask := func() { f.call(t, "GET", "/api/v1/tasks/"+id, nil, &task) }
	wait(func() bool {
		readTask()
		return task.MCPWrite != nil && task.MCPWrite.Output.Phase == download.MCPWriteOutputStaging && task.Progress != nil && task.Progress.Downloaded >= 16384
	})
	creation := task.MCPWrite.CreationBinding
	config, _ := f.core.GetConfig()
	configBefore, _ := json.Marshal(config)
	f.call(t, "POST", mcpWritePrefix+"/operations", command, &receipt)
	if len(f.core.GetTasks()) != 1 {
		t.Fatal("same op created another task")
	}
	if err = f.close(); err != nil {
		t.Fatal("normal paired close", err)
	}
	writePolicy([]string{id})
	f = mcpLiveStart(t, root)
	f.call(t, "GET", capPath, nil, &capability)
	if capability.PolicyID == nil || *capability.PolicyID == originalPolicy {
		t.Fatal("explicit policy update did not bind")
	}
	currentPolicy := *capability.PolicyID
	f.call(t, "GET", mcpWritePrefix+"/operations/owned-create?datasetId="+identity.DatasetID+"&policyId="+originalPolicy, nil, &receipt)
	if receipt.PolicyID != originalPolicy || receipt.Result == nil || *receipt.Result.TaskID != id {
		t.Fatal("historical query changed binding")
	}
	action := func(kind download.MCPWriteKind, op string) {
		f.call(t, "GET", capPath+"&taskId="+id, nil, &capability)
		if capability.Task == nil {
			t.Fatal("no trusted target tuple")
		}
		cmd := download.MCPWriteRequest{Version: 1, DatasetID: identity.DatasetID, PolicyID: currentPolicy, OperationID: op, Kind: kind, Action: &download.MCPWriteActionRequest{TaskID: id, Expected: capability.Task.Expected}}
		f.call(t, "POST", mcpWritePrefix+"/operations", cmd, &receipt)
		if receipt.Result == nil || receipt.Result.Outcome != download.MCPWriteApplied {
			t.Fatalf("action not applied: %+v", receipt)
		}
	}
	action(download.MCPWriteResume, "resume-one")
	wait(func() bool {
		readTask()
		return task.Status == base.DownloadStatusRunning && task.Progress != nil && task.Progress.Downloaded >= 32768
	})
	action(download.MCPWritePause, "pause-one")
	readTask()
	if task.Status != base.DownloadStatusPause {
		t.Fatal("pause did not stop actual writer")
	}
	action(download.MCPWriteResume, "resume-final")
	allow.Store(true)
	wait(func() bool { readTask(); return task.Status == base.DownloadStatusDone })
	data, err := os.ReadFile(filepath.Join(directory, "one.bin"))
	if err != nil || sha256.Sum256(data) != sha256.Sum256(payload) {
		t.Fatal("complete bytes/SHA mismatch", err)
	}
	if !reflect.DeepEqual(creation, task.MCPWrite.CreationBinding) || task.MCPWrite.CurrentExecutionBinding.PolicyID != currentPolicy {
		t.Fatal("creation ceiling or current binding changed incorrectly")
	}
	for _, effect := range []string{"script", "webhook", "auto_open", "post_process"} {
		if f.core.MCPTaskEffectAllowed(&task, effect) {
			t.Fatal("default effect granted", effect)
		}
	}
	config, _ = f.core.GetConfig()
	after, _ := json.Marshal(config)
	if !bytes.Equal(configBefore, after) || len(f.core.GetTasks()) != 1 {
		t.Fatal("config/task set changed outside the owned operation")
	}
	if err = f.close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual UDS same Task create/pause/resume, full SHA %x, range requests %d", sha256.Sum256(data), ranges.Load())
}
