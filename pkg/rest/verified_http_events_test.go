package rest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

// A transport fixture, deliberately not a Core fresh-C producer.
type completionEventsFixture struct {
	events   chan download.VerifiedHTTPDoneEvent
	err      error
	calls    atomic.Int32
	releases atomic.Int32
}

func (f *completionEventsFixture) SubscribeVerifiedHTTPDone(context.Context, string) (<-chan download.VerifiedHTTPDoneEvent, func(), error) {
	f.calls.Add(1)
	return f.events, func() { f.releases.Add(1) }, f.err
}

func eventRoute(domain string) string {
	return "/api/v1/datasets/" + domain + "/verified-http/v1/completion-events"
}

func eventRouter(source verifiedHTTPDoneSource) *mux.Router {
	router := mux.NewRouter()
	registerVerifiedHTTPEventRoutes(router, source)
	return router
}

func eventFixtureValue() download.VerifiedHTTPDoneEvent {
	return download.VerifiedHTTPDoneEvent{Version: 1, DatasetID: strings.Repeat("a", 32), TaskID: "example-task", CompletionID: "example-completion"}
}

func requireEventsReleased(t *testing.T, fixture *completionEventsFixture) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for fixture.releases.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscription not released")
		}
		time.Sleep(time.Millisecond)
	}
	if fixture.releases.Load() != 1 {
		t.Fatal("release called more than once")
	}
}

func TestCompletionEventsRealHTTPFrameAndCancellation(t *testing.T) {
	fixture := &completionEventsFixture{events: make(chan download.VerifiedHTTPDoneEvent)}
	server := httptest.NewServer(eventRouter(fixture))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+eventRoute(eventFixtureValue().DatasetID), nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("SSE handshake", response.StatusCode, response.Header)
	}
	fixture.events <- eventFixtureValue()
	reader := bufio.NewReader(response.Body)
	name, err := reader.ReadString('\n')
	if err != nil || name != "event: accepted-done\n" {
		t.Fatal("event name", name, err)
	}
	data, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(data, "data: ") {
		t.Fatal("event data", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &decoded); err != nil || len(decoded) != 4 || decoded["version"] != float64(1) || decoded["datasetId"] != eventFixtureValue().DatasetID || decoded["taskId"] != "example-task" || decoded["completionId"] != "example-completion" {
		t.Fatalf("four field DTO: %v %v", decoded, err)
	}
	if blank, err := reader.ReadString('\n'); err != nil || blank != "\n" {
		t.Fatal("frame not terminated", err)
	}
	if len(name)+len(data)+1 > verifiedEventMaxFrame {
		t.Fatal("oversize frame")
	}
	cancel()
	_ = response.Body.Close()
	requireEventsReleased(t, fixture)
}

func TestCompletionEventsRejectResumeAndErrorsBeforeSSE(t *testing.T) {
	domain := eventFixtureValue().DatasetID
	for _, query := range []string{"?since=0", "?cursor=x", "?id=x", "?retry=10", "?unknown="} {
		fixture := &completionEventsFixture{events: make(chan download.VerifiedHTTPDoneEvent)}
		w := httptest.NewRecorder()
		eventRouter(fixture).ServeHTTP(w, httptest.NewRequest(http.MethodGet, eventRoute(domain)+query, nil))
		if w.Code != 400 || fixture.calls.Load() != 0 || !strings.Contains(w.Body.String(), "invalid_completion_events_request") {
			t.Fatal("resume accepted", query, w.Code)
		}
	}
	for _, name := range []string{"Last-Event-ID", "last-event-id"} {
		fixture := &completionEventsFixture{}
		request := httptest.NewRequest(http.MethodGet, eventRoute(domain), nil)
		request.Header[name] = []string{""}
		w := httptest.NewRecorder()
		eventRouter(fixture).ServeHTTP(w, request)
		if w.Code != 400 || fixture.calls.Load() != 0 {
			t.Fatal("empty resume header accepted")
		}
	}
	for _, test := range []struct {
		err     error
		status  int
		message string
	}{
		{errCompletionEventsUnsupported, 501, "verified_http_unsupported"},
		{download.ErrVerifiedHTTPEventsCapacity, 503, "completion_events_capacity"},
		{download.ErrVerifiedHTTPEventsClosed, 503, "completion_events_unavailable"},
		{download.ErrDatasetMismatch, 409, "dataset_mismatch"},
		{download.ErrDatasetInvalid, 503, "dataset_unavailable"},
		{errors.New("private-cookie-path-sha"), 503, "completion_events_unavailable"},
	} {
		fixture := &completionEventsFixture{err: test.err}
		w := httptest.NewRecorder()
		eventRouter(fixture).ServeHTTP(w, httptest.NewRequest(http.MethodGet, eventRoute(domain), nil))
		var reply datasetReply
		if json.Unmarshal(w.Body.Bytes(), &reply) != nil || w.Code != test.status || reply.Msg != test.message || string(reply.Data) != "null" || strings.Contains(w.Body.String(), "private-cookie-path-sha") || w.Header().Get("Content-Type") == "text/event-stream" || fixture.releases.Load() != 1 {
			t.Fatalf("error: %d %s", w.Code, w.Body.String())
		}
	}
	fixture := &completionEventsFixture{events: make(chan download.VerifiedHTTPDoneEvent)}
	w := httptest.NewRecorder()
	eventRouter(fixture).ServeHTTP(w, httptest.NewRequest(http.MethodGet, eventRoute(domain), nil))
	if w.Code != 501 || !strings.Contains(w.Body.String(), "completion_events_stream_unsupported") || fixture.releases.Load() != 1 || w.Header().Get("Content-Type") == "text/event-stream" {
		t.Fatal("unbounded writer accepted")
	}
}

func TestCompletionEventsMalformedValueAndEOF(t *testing.T) {
	for _, action := range []string{"cross-domain", "unknown-version", "closed"} {
		t.Run(action, func(t *testing.T) {
			fixture := &completionEventsFixture{events: make(chan download.VerifiedHTTPDoneEvent)}
			server := httptest.NewServer(eventRouter(fixture))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+eventRoute(eventFixtureValue().DatasetID), nil)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if action == "closed" {
				close(fixture.events)
			} else {
				event := eventFixtureValue()
				if action == "cross-domain" {
					event.DatasetID = strings.Repeat("b", 32)
				} else {
					event.Version = 2
				}
				fixture.events <- event
			}
			body, err := io.ReadAll(response.Body)
			if err != nil || len(body) != 0 {
				t.Fatalf("invalid event escaped: %s %v", body, err)
			}
			requireEventsReleased(t, fixture)
		})
	}
}

func TestCompletionEventsRejectsUnsentBodyWithoutDrain(t *testing.T) {
	for _, header := range []string{"Content-Length: 100", "Transfer-Encoding: chunked"} {
		t.Run(header, func(t *testing.T) {
			for _, rejection := range []string{"plain", "query", "last-event-id", "bad-dataset"} {
				t.Run(rejection, func(t *testing.T) {
					fixture := &completionEventsFixture{}
					server := httptest.NewServer(eventRouter(fixture))
					defer server.Close()
					conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
					if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
						t.Fatal(err)
					}
					path, extra := eventRoute(eventFixtureValue().DatasetID), ""
					switch rejection {
					case "query":
						path += "?since=0"
					case "last-event-id":
						extra = "Last-Event-ID: saved-event\r\n"
					case "bad-dataset":
						path = eventRoute("bad")
					}
					_, err = io.WriteString(conn, "GET "+path+" HTTP/1.1\r\nHost: localhost\r\n"+header+"\r\n"+extra+"\r\n")
					if err != nil {
						t.Fatal(err)
					}
					response, err := http.ReadResponse(bufio.NewReader(conn), nil)
					if err != nil {
						t.Fatal("response blocked on missing request body", err)
					}
					defer response.Body.Close()
					body, err := io.ReadAll(response.Body)
					if err != nil || response.StatusCode != 400 || !response.Close || fixture.calls.Load() != 0 || !bytes.Contains(body, []byte("invalid_completion_events_request")) {
						t.Fatalf("body was drained/subscribed: %d %s %v", response.StatusCode, body, err)
					}
				})
			}
		})
	}
}

// This writer models a full transport buffer and implements the exact public
// response deadline contract. It never spawns a goroutine per blocked write.
type stalledEventWriter struct {
	header               http.Header
	mu                   sync.Mutex
	deadline             time.Time
	changed              chan struct{}
	started              chan struct{}
	ready                chan struct{}
	startOnce, readyOnce sync.Once
	blockFlush           bool
	flushCount           int
}

func newStalledEventWriter() *stalledEventWriter {
	return &stalledEventWriter{header: make(http.Header), changed: make(chan struct{}, 1), started: make(chan struct{}), ready: make(chan struct{})}
}
func (w *stalledEventWriter) Header() http.Header { return w.header }
func (w *stalledEventWriter) WriteHeader(int)     {}
func (w *stalledEventWriter) FlushError() error {
	w.flushCount++
	w.readyOnce.Do(func() { close(w.ready) })
	if w.blockFlush && w.flushCount > 1 {
		_, err := w.waitForDeadline()
		return err
	}
	return nil
}
func (w *stalledEventWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadline = deadline
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return nil
}
func (w *stalledEventWriter) Write(data []byte) (int, error) {
	if w.blockFlush {
		return len(data), nil
	}
	return w.waitForDeadline()
}
func (w *stalledEventWriter) waitForDeadline() (int, error) {
	w.startOnce.Do(func() { close(w.started) })
	for {
		w.mu.Lock()
		deadline := w.deadline
		w.mu.Unlock()
		if deadline.IsZero() {
			<-w.changed
			continue
		}
		left := time.Until(deadline)
		if left <= 0 {
			return 0, os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(left)
		select {
		case <-timer.C:
			return 0, os.ErrDeadlineExceeded
		case <-w.changed:
			if !timer.Stop() {
				<-timer.C
			}
		}
	}
}

func TestCompletionEventsSlowWriterDeadlineAndCancel(t *testing.T) {
	for _, action := range []string{"write-cancel", "write-deadline", "flush-cancel", "flush-deadline"} {
		t.Run(action, func(t *testing.T) {
			fixture := &completionEventsFixture{events: make(chan download.VerifiedHTTPDoneEvent, 1)}
			fixture.events <- eventFixtureValue()
			writer := newStalledEventWriter()
			writer.blockFlush = strings.HasPrefix(action, "flush")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, eventRoute(eventFixtureValue().DatasetID), nil).WithContext(ctx)
			done := make(chan struct{})
			go func() { defer close(done); eventRouter(fixture).ServeHTTP(writer, request) }()
			select {
			case <-writer.started:
			case <-time.After(time.Second):
				t.Fatal("write not reached")
			}
			if strings.HasSuffix(action, "cancel") {
				cancel()
			}
			limit := verifiedEventWriteTimeout + time.Second
			if strings.HasSuffix(action, "cancel") {
				limit = time.Second
			}
			select {
			case <-done:
			case <-time.After(limit):
				cancel()
				<-done
				t.Fatal("writer remained blocked")
			}
			if fixture.releases.Load() != 1 {
				t.Fatal("stalled stream leaked subscription")
			}
			// The joined cancellation watcher cannot touch ResponseController
			// after the handler returns; cancellation here is safe and inert.
			cancel()
		})
	}
}

func TestCompletionEventsIdleClearsWriteDeadline(t *testing.T) {
	fixture := &completionEventsFixture{events: make(chan download.VerifiedHTTPDoneEvent)}
	writer := newStalledEventWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, eventRoute(eventFixtureValue().DatasetID), nil).WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); eventRouter(fixture).ServeHTTP(writer, request) }()
	select {
	case <-writer.ready:
	case <-time.After(time.Second):
		t.Fatal("headers not flushed")
	}
	end := time.Now().Add(time.Second)
	for {
		writer.mu.Lock()
		deadline := writer.deadline
		writer.mu.Unlock()
		if deadline.IsZero() {
			break
		}
		if time.Now().After(end) {
			cancel()
			<-done
			t.Fatal("idle connection retained active write deadline")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle stream did not cancel")
	}
	if fixture.releases.Load() != 1 {
		t.Fatal("idle stream retained subscription")
	}
}

func TestCompletionEventsBuildServerAuthIdleStreamAndNoWrites(t *testing.T) {
	for _, kind := range []model.Storage{model.StorageMem, model.StorageBolt} {
		t.Run(string(kind), func(t *testing.T) {
			server, endpoint, core, dir := datasetServer(t, kind, "synthetic-events-token")
			identity, err := core.DatasetIdentity()
			if err != nil {
				t.Fatal(err)
			}
			capability, err := core.VerifiedHTTPCapabilityForDataset(identity.DatasetID)
			if err != nil {
				t.Fatal(err)
			}
			_, beforeConfig := datasetGET(t, endpoint+"/api/v1/config", "synthetic-events-token")
			var before []byte
			if kind == model.StorageBolt {
				before, err = os.ReadFile(filepath.Join(dir, "gopeed.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, token := range []string{"", "wrong-token"} {
				request := httptest.NewRequest(http.MethodGet, eventRoute(identity.DatasetID), nil)
				if token != "" {
					request.Header.Set("X-Api-Token", token)
				}
				w := httptest.NewRecorder()
				server.Handler.ServeHTTP(w, request)
				if w.Code != 401 || w.Header().Get("Content-Type") == "text/event-stream" {
					t.Fatal("auth", w.Code, w.Body.String())
				}
			}
			// Use a real HTTP writer and cancel an idle successful stream; JSON
			// decoding or waiting for a first event would hang a live-only route.
			finished := make(chan struct{}, 1)
			transport := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { finished <- struct{}{} }()
				server.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(transport.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, transport.URL+eventRoute(identity.DatasetID), nil)
			request.Header.Set("X-Api-Token", "synthetic-events-token")
			response, err := transport.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if capability.Supported {
				if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "no-store" {
					t.Fatal("ready Core did not expose an idle SSE stream", response.StatusCode)
				}
			} else if response.StatusCode != 501 || response.Header.Get("Content-Type") == "text/event-stream" {
				t.Fatal("unsupported platform opened a stream", response.StatusCode)
			}
			cancel()
			_ = response.Body.Close()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("idle subscriber did not release on cancellation")
			}
			wrong := strings.Repeat("0", 32)
			if wrong == identity.DatasetID {
				wrong = strings.Repeat("1", 32)
			}
			request = httptest.NewRequest(http.MethodGet, eventRoute(wrong), nil)
			request.Header.Set("X-Api-Token", "synthetic-events-token")
			w := httptest.NewRecorder()
			server.Handler.ServeHTTP(w, request)
			if w.Code != 409 || !strings.Contains(w.Body.String(), "dataset_mismatch") {
				t.Fatal("wrong domain", w.Code, w.Body.String())
			}
			_, afterConfig := datasetGET(t, endpoint+"/api/v1/config", "synthetic-events-token")
			page, err := core.ListBatchOperationCatalog(download.BatchCatalogQuery{})
			if err != nil || len(core.GetTasks()) != 0 || len(page.Items) != 0 || !bytes.Equal(beforeConfig.Data, afterConfig.Data) {
				t.Fatal("subscription changed Task/config/catalog", err)
			}
			if kind == model.StorageBolt {
				after, err := os.ReadFile(filepath.Join(dir, "gopeed.db"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("subscription wrote database", err)
				}
			}
		})
	}
}
