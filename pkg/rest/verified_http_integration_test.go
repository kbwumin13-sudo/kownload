//go:build darwin || linux

package rest

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"go.etcd.io/bbolt"
)

const verifiedLiveToken = "synthetic-verified-live-token"

type verifiedLiveServer struct {
	core        *download.Downloader
	client      *http.Client
	domain      string
	createPosts atomic.Int32
	close       func()
}

func verifiedLiveUDS(t *testing.T, root string) *verifiedLiveServer {
	t.Helper()
	previousCore, previousServer, previousAES, previousPort := Downloader, srv, aesKey, runningPort
	t.Cleanup(func() { Downloader, srv, aesKey, runningPort = previousCore, previousServer, previousAES, previousPort })
	socket := filepath.Join(root, "api.sock")
	server, listener, err := BuildServer(&model.StartConfig{Network: "unix", Address: socket, Storage: model.StorageBolt,
		StorageDir: filepath.Join(root, "db"), ApiToken: verifiedLiveToken, StartupTaskPolicy: "imported-review"})
	if err != nil {
		t.Fatal(err)
	}
	core := Downloader
	identity, err := core.DatasetIdentity()
	if err != nil {
		t.Fatal(err)
	}
	f := &verifiedLiveServer{core: core, domain: identity.DatasetID}
	handler := server.Handler
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/verified-http/v1/operations") {
			f.createPosts.Add(1)
		}
		handler.ServeHTTP(w, r)
	})
	served := make(chan struct{})
	go func() { defer close(served); _ = server.Serve(listener) }()
	f.client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var once sync.Once
	f.close = func() {
		once.Do(func() {
			f.client.CloseIdleConnections()
			_ = server.Close()
			_ = listener.Close()
			_ = core.Close()
			<-served
		})
	}
	t.Cleanup(f.close)
	return f
}

func (f *verifiedLiveServer) call(t *testing.T, method, path string, body []byte) (int, datasetReply) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Api-Token", verifiedLiveToken)
	r.Header.Set("Content-Type", "application/json")
	response, err := f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result datasetReply
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, result
}

func (f *verifiedLiveServer) stream(t *testing.T) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+eventRoute(f.domain), nil)
	r.Header.Set("X-Api-Token", verifiedLiveToken)
	response, err := f.client.Do(r)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		cancel()
		response.Body.Close()
		t.Fatal("actual Core stream not ready", response.StatusCode)
	}
	t.Cleanup(func() { cancel(); _ = response.Body.Close() })
	return response, cancel
}

func verifiedLiveBody(t *testing.T, op, url, directory, name, digest string) []byte {
	t.Helper()
	request := map[string]any{"version": 1, "requiredCapability": verifiedHTTPCapability, "operationId": op,
		"request": map[string]any{"req": map[string]any{"url": url, "extra": map[string]any{"opaqueLarge": json.Number("900719925474099312345")}},
			"opts": map[string]any{"path": directory, "name": name, "extra": map[string]any{"autoTorrent": false, "autoExtract": false}}},
		"integrity": map[string]any{"algorithm": "sha256", "expectedSHA256": digest}}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestVerifiedHTTPRealUDSFreshCProjectionAndReopen(t *testing.T) {
	root, err := os.MkdirTemp("/private/tmp", "v14-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f := verifiedLiveUDS(t, root)
	endpoint := "/api/v1/datasets/" + f.domain + "/verified-http/v1"
	status, reply := f.call(t, http.MethodGet, endpoint+"/capability", nil)
	var cap verifiedCapabilityResult
	if err := json.Unmarshal(reply.Data, &cap); err != nil || status != 200 || !cap.Supported || cap.DatasetID != f.domain {
		t.Fatal("complete real Core did not advertise support", status, string(reply.Data), err)
	}
	_, reply = f.call(t, http.MethodGet, endpoint+"/operations/live-operation", nil)
	if !bytes.Contains(reply.Data, []byte(`"state":"unknown"`)) || len(f.core.GetTasks()) != 0 {
		t.Fatal("cap/preflight created a Task")
	}
	stream, cancel := f.stream(t)
	defer cancel()
	payload := bytes.Repeat([]byte("v"), 32768)
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	var requests atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"verified-live-v1"`)
		http.ServeContent(w, r, "payload.bin", time.Unix(1700000000, 0), bytes.NewReader(payload))
	}))
	t.Cleanup(source.Close)
	directory := filepath.Join(root, "files")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	_, before := f.call(t, http.MethodGet, "/api/v1/config", nil)
	body := verifiedLiveBody(t, "live-operation", source.URL+"/payload", directory, "verified.bin", digest)
	status, reply = f.call(t, http.MethodPost, endpoint+"/operations", body)
	var created verifiedOperationReply
	if err := json.Unmarshal(reply.Data, &created); err != nil || status != 200 || reply.Code != 0 || len(created.Operation.TaskIDs) != 1 {
		t.Fatal("actual receiver did not create one Task", status, string(reply.Data), err)
	}
	id := created.Operation.TaskIDs[0]
	reader := bufio.NewReader(stream.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: accepted-done\n" {
		t.Fatal("real fresh C event missing", line, err)
	}
	data, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(data, "data: ") {
		t.Fatal("event payload missing", err)
	}
	var event download.VerifiedHTTPDoneEvent
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &event); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &fields); err != nil || len(fields) != 4 || event.TaskID != id || !event.ValidForDataset(f.domain) {
		t.Fatal("event is not the exact four-field scoped DTO", err)
	}
	if blank, err := reader.ReadString('\n'); err != nil || blank != "\n" {
		t.Fatal("unterminated event")
	}
	cancel()
	_ = stream.Body.Close()
	status, reply = f.call(t, http.MethodGet, "/api/v1/tasks/"+id, nil)
	var task download.Task
	var public struct {
		View completionView `json:"completionView"`
	}
	if err := json.Unmarshal(reply.Data, &task); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reply.Data, &public); err != nil {
		t.Fatal(err)
	}
	if status != 200 || task.Status != "done" || task.Progress.Downloaded != 32768 || task.Completion == nil || task.Completion.CompletionID != event.CompletionID || !public.View.Available || public.View.Phase == nil || *public.View.Phase != "committed" || public.View.RecordedMatch == nil || !*public.View.RecordedMatch || public.View.CompletionID == nil || *public.View.CompletionID != event.CompletionID {
		t.Fatal("Task and projection are not the confirmed C pair", string(reply.Data))
	}
	output, err := os.ReadFile(filepath.Join(directory, "verified.bin"))
	if err != nil || !bytes.Equal(output, payload) {
		t.Fatal("published output differs", err)
	}
	sourceReads := requests.Load()
	// A new subscriber cannot reconstruct the already accepted event. GET,
	// replay and settled cancel all retain history without another C winner.
	events, release, err := f.core.SubscribeVerifiedHTTPDone(context.Background(), f.domain)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, action := range []struct {
		method, path string
		body         []byte
	}{{http.MethodGet, endpoint + "/operations/live-operation", nil}, {http.MethodPost, endpoint + "/operations", body}, {http.MethodPost, endpoint + "/operations/live-operation/cancel", nil}} {
		status, reply = f.call(t, action.method, action.path, action.body)
		if status != 200 || reply.Code != 0 || !bytes.Contains(reply.Data, []byte(id)) {
			t.Fatal("same-op history lost", status, string(reply.Data))
		}
	}
	status, reply = f.call(t, http.MethodPost, endpoint+"/operations", bytes.Replace(body, []byte(digest), []byte(strings.Repeat("0", 64)), 1))
	var conflict verifiedOperationReply
	if err := json.Unmarshal(reply.Data, &conflict); err != nil || status != 409 || reply.Code != 1000 || reply.Msg != "operation_conflict" ||
		conflict.DatasetID != f.domain || conflict.Namespace != scopedBatchNamespace || len(conflict.Operation.TaskIDs) != 1 ||
		conflict.Operation.TaskIDs[0] != id || len(conflict.CompletionViews) != 1 || conflict.CompletionViews[0].CompletionID == nil ||
		*conflict.CompletionViews[0].CompletionID != event.CompletionID {
		t.Fatal("conflict lost its error or original authoritative history", status, string(reply.Data), err)
	}
	// The conflicting payload did not replace frozen integrity. The original
	// request remains a replay of the original Task/CID, with no new IO permit.
	status, reply = f.call(t, http.MethodPost, endpoint+"/operations", body)
	var original verifiedOperationReply
	if err := json.Unmarshal(reply.Data, &original); err != nil || status != 200 || reply.Code != 0 || len(original.Operation.TaskIDs) != 1 || original.Operation.TaskIDs[0] != id ||
		len(original.CompletionViews) != 1 || original.CompletionViews[0].CompletionID == nil || *original.CompletionViews[0].CompletionID != event.CompletionID {
		t.Fatal("conflict replaced original intent or completion", status, string(reply.Data), err)
	}
	select {
	case e := <-events:
		t.Fatal("history/replay emitted", e)
	default:
	}
	if requests.Load() != sourceReads || len(f.core.GetTasks()) != 1 || f.createPosts.Load() != 4 {
		t.Fatal("replay/conflict created IO or Task")
	}
	_, after := f.call(t, http.MethodGet, "/api/v1/config", nil)
	if !bytes.Equal(before.Data, after.Data) {
		t.Fatal("verified receiver changed global config")
	}
	release()
	f.close()
	// Read the stopped fixture's real Bolt records directly: a conflicting
	// request cannot replace either the frozen expected digest or the M link.
	db, err := bbolt.Open(filepath.Join(root, "db", "gopeed.db"), 0600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = db.View(func(tx *bbolt.Tx) error {
		var frozen struct {
			Integrity download.VerifiedHTTPIntegrityIntent `json:"integrity"`
			Members   []struct {
				TaskID       string `json:"taskId"`
				State        string `json:"state"`
				CompletionID string `json:"completionId"`
			} `json:"members"`
		}
		if err := json.Unmarshal(tx.Bucket([]byte("task_operations_v1")).Get([]byte(scopedBatchNamespace+":live-operation")), &frozen); err != nil {
			return err
		}
		if frozen.Integrity.ExpectedSHA256 != digest || len(frozen.Members) != 1 || frozen.Members[0].TaskID != id || frozen.Members[0].State != "created" || frozen.Members[0].CompletionID != event.CompletionID {
			t.Error("conflict changed the frozen expected digest or original member")
		}
		var completion transfer.VerifiedCompletionRecord
		if err := json.Unmarshal(tx.Bucket([]byte("task_completion_v1")).Get([]byte(id)), &completion); err != nil {
			return err
		}
		if completion.ID != event.CompletionID || completion.ExpectedSHA256 != digest || completion.ActualSHA256 != digest || completion.Phase != transfer.CompletionCommitted {
			t.Error("conflict replaced the original committed completion")
		}
		return nil
	})
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatal("cannot inspect frozen records", err, closeErr)
	}
	reopened := verifiedLiveUDS(t, root)
	if reopened.domain != f.domain {
		t.Fatal("logical dataset changed on reopen")
	}
	reopenedEvents, stop, err := reopened.core.SubscribeVerifiedHTTPDone(context.Background(), reopened.domain)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	status, reply = reopened.call(t, http.MethodGet, "/api/v1/tasks/"+id, nil)
	if status != 200 || !bytes.Contains(reply.Data, []byte(`"phase":"committed"`)) || !bytes.Contains(reply.Data, []byte(`"status":"done"`)) {
		t.Fatal("confirmed pair lost after real Bolt reopen", status, string(reply.Data))
	}
	_, reply = reopened.call(t, http.MethodGet, endpoint+"/operations/live-operation", nil)
	if !bytes.Contains(reply.Data, []byte(id)) {
		t.Fatal("operation history lost after reopen")
	}
	select {
	case e := <-reopenedEvents:
		t.Fatal("reopen/query replayed a success event", e)
	default:
	}
	if requests.Load() != sourceReads {
		t.Fatal("reopen resumed a completed network writer")
	}
}

func TestVerifiedHTTPRealUDSMismatchHasNoPublicationOrSuccessEvent(t *testing.T) {
	root, err := os.MkdirTemp("/private/tmp", "v14-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f := verifiedLiveUDS(t, root)
	payload := bytes.Repeat([]byte("bad-digest-fixture"), 1024)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "mismatch.bin", time.Unix(1700000000, 0), bytes.NewReader(payload))
	}))
	t.Cleanup(source.Close)
	directory := filepath.Join(root, "files")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	events, release, err := f.core.SubscribeVerifiedHTTPDone(context.Background(), f.domain)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	endpoint := "/api/v1/datasets/" + f.domain + "/verified-http/v1"
	status, reply := f.call(t, http.MethodPost, endpoint+"/operations", verifiedLiveBody(t, "mismatch-operation", source.URL+"/payload", directory, "mismatch.bin", strings.Repeat("0", 64)))
	var result verifiedOperationReply
	if err := json.Unmarshal(reply.Data, &result); err != nil || status != 200 || len(result.Operation.TaskIDs) != 1 {
		t.Fatal("mismatch fixture was not created", status, string(reply.Data), err)
	}
	id := result.Operation.TaskIDs[0]
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, reply = f.call(t, http.MethodGet, "/api/v1/tasks/"+id, nil)
		var task struct {
			Status string
			View   completionView `json:"completionView"`
		}
		if err := json.Unmarshal(reply.Data, &task); err != nil {
			t.Fatal(err)
		}
		if task.Status == "error" {
			if task.View.RecordedMatch != nil || (task.View.Phase != nil && *task.View.Phase == "committed") {
				t.Fatal("mismatch acquired successful verification")
			}
			break
		}
		if task.Status == "done" || time.Now().After(deadline) {
			t.Fatal("mismatch did not fail closed", string(reply.Data))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Lstat(filepath.Join(directory, "mismatch.bin")); !os.IsNotExist(err) {
		t.Fatal("unverified final file published", err)
	}
	select {
	case event := <-events:
		t.Fatal("mismatch emitted accepted-done", event)
	default:
	}
}
