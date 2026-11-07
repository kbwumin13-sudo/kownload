package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/GopeedLab/gopeed/pkg/rest/model"
)

const operationHeader = "X-Kownload-Operation-Id"

var validOperationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type submissionCall struct {
	id, domain string
	started    time.Time
}
type submissionContextKey struct{}
type submissionEntry struct {
	path    string
	done    chan struct{}
	status  int
	body    []byte
	expires time.Time
	domain  string
}

// The ledger lives with one REST server, not the database. Restart, TTL expiry
// and capacity eviction end the retry guarantee. In-flight entries are retained.
type submissionLedger struct {
	mu      sync.Mutex
	entries map[string]*submissionEntry
	limit   int
	ttl     time.Duration
	now     func() time.Time
}

func newSubmissionLedger() *submissionLedger {
	return &submissionLedger{entries: map[string]*submissionEntry{}, limit: 512, ttl: 24 * time.Hour, now: time.Now}
}
func (l *submissionLedger) makeRoom(now time.Time) bool {
	for id, e := range l.entries {
		select {
		case <-e.done:
			if !now.Before(e.expires) {
				delete(l.entries, id)
			}
		default:
		}
	}
	if len(l.entries) < l.limit {
		return true
	}
	var oldest string
	var expires time.Time
	for id, e := range l.entries {
		select {
		case <-e.done:
			if oldest == "" || e.expires.Before(expires) {
				oldest = id
				expires = e.expires
			}
		default:
		}
	}
	if oldest != "" {
		delete(l.entries, oldest)
		return true
	}
	return false
}
func submissionFailure(id, kind string, code model.RespCode) map[string]any {
	return map[string]any{"code": code, "msg": "请求未完成（" + kind + "）", "data": nil, "operationId": id, "error": kind}
}
func traceSubmission(call *submissionCall, stage, kind string) {
	if call == nil || call.id == "" || Downloader == nil {
		return
	}
	Downloader.Logger.Info().Str("operationId", call.id).Str("stage", stage).Str("errorCategory", kind).Str("domain", call.domain).Int64("elapsedMs", time.Since(call.started).Milliseconds()).Msg("browser submission")
}
func submissionDomain(r *http.Request, raw string) {
	if call, ok := r.Context().Value(submissionContextKey{}).(*submissionCall); ok {
		if u, e := url.Parse(raw); e == nil {
			call.domain = u.Hostname()
		}
	}
}
func (l *submissionLedger) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(operationHeader)
		if id == "" {
			next(w, r)
			return
		}
		if !validOperationID.MatchString(id) || len(r.Header.Values(operationHeader)) != 1 {
			WriteStatusJson(w, 400, submissionFailure("", "invalid_request", model.CodeInvalidParam))
			return
		}
		call := &submissionCall{id: id, started: time.Now()}
		traceSubmission(call, "rest_received", "")
		l.mu.Lock()
		entry := l.entries[id]
		if entry != nil {
			select {
			case <-entry.done:
				if !l.now().Before(entry.expires) {
					delete(l.entries, id)
					entry = nil
				}
			default:
			}
		}
		if entry != nil {
			l.mu.Unlock()
			if entry.path != r.URL.Path {
				WriteStatusJson(w, 409, submissionFailure(id, "invalid_request", model.CodeInvalidParam))
				return
			}
			select {
			case <-entry.done:
				call.domain = entry.domain
				traceSubmission(call, "rest_replayed", "")
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(entry.status)
				_, _ = w.Write(entry.body)
			case <-r.Context().Done():
				WriteStatusJson(w, 408, submissionFailure(id, "request_failed", model.CodeError))
			}
			return
		}
		if !l.makeRoom(l.now()) {
			l.mu.Unlock()
			WriteStatusJson(w, 503, submissionFailure(id, "request_failed", model.CodeError))
			return
		}
		entry = &submissionEntry{path: r.URL.Path, done: make(chan struct{})}
		l.entries[id] = entry
		l.mu.Unlock()
		captured := &submissionWriter{header: http.Header{}, status: 200}
		defer func() {
			var envelope map[string]any
			kind := ""
			if json.Unmarshal(captured.body.Bytes(), &envelope) != nil || envelope == nil {
				captured.status = 500
				kind = "invalid_response"
			}
			code, ok := envelope["code"].(float64)
			if kind == "" {
				switch {
				case captured.status < 200 || captured.status >= 300:
					kind = "http_error"
				case !ok:
					kind = "invalid_response"
				case code != 0:
					kind = "business_error"
				}
			}
			if kind != "" {
				business := model.CodeError
				if ok && code != 0 {
					business = model.RespCode(code)
				}
				envelope = submissionFailure(id, kind, business)
			} else {
				envelope["operationId"] = id
			}
			encoded, _ := json.Marshal(envelope)
			l.mu.Lock()
			entry.status = captured.status
			entry.body = encoded
			entry.domain = call.domain
			entry.expires = l.now().Add(l.ttl)
			close(entry.done)
			l.mu.Unlock()
			stage := "rest_created"
			if kind != "" {
				stage = "rest_failed"
			}
			traceSubmission(call, stage, kind)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(entry.status)
			_, _ = w.Write(encoded)
		}()
		next(captured, r.WithContext(context.WithValue(r.Context(), submissionContextKey{}, call)))
	}
}

type submissionWriter struct {
	header  http.Header
	status  int
	written bool
	body    bytes.Buffer
}

func (w *submissionWriter) Header() http.Header { return w.header }
func (w *submissionWriter) WriteHeader(status int) {
	if !w.written {
		w.status = status
		w.written = true
	}
}
func (w *submissionWriter) Write(p []byte) (int, error) {
	if !w.written {
		w.WriteHeader(200)
	}
	return w.body.Write(p)
}

func writeCreated[T any](w http.ResponseWriter, r *http.Request, data T, ids []string) {
	result := model.NewOkResult(data)
	result.OperationID = r.Header.Get(operationHeader)
	result.Submission = &model.Submission{State: "created", TaskIDs: ids}
	WriteJson(w, result)
}
