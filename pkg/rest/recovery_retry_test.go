package rest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/gorilla/mux"
)

type recoveryRetryFixture struct {
	calls   int
	request download.RecoveryRetryRequest
	result  download.RecoveryTaskState
	err     error
}

func (f *recoveryRetryFixture) QueryRecoveryForDataset(string, string) (download.RecoveryTaskState, error) {
	f.calls++
	return f.result, f.err
}
func (f *recoveryRetryFixture) QueryRecoveryActionForDataset(_ string, _ string, r download.RecoveryRetryRequest) (download.RecoveryTaskState, error) {
	f.calls++
	f.request = r
	return f.result, f.err
}
func (f *recoveryRetryFixture) RetryRecoveryForDataset(_ context.Context, _ string, _ string, r download.RecoveryRetryRequest) (download.RecoveryTaskState, error) {
	f.calls++
	f.request = r
	return f.result, f.err
}
func retryFixtureCall(f *recoveryRetryFixture, method, path, body string) *httptest.ResponseRecorder {
	router := mux.NewRouter()
	registerRecoveryRetryRoutes(router, f)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}
func TestRecoveryRetryRESTStrictRoutingAndIntent(t *testing.T) {
	root := "/api/v1/datasets/" + strings.Repeat("a", 32) + "/tasks/-_task/recovery/v1"
	body := `{"version":1,"actionId":"user-1","expectedEpisode":"old-episode","expectedSourceRevision":1,"decision":"retry"}`
	for _, sample := range []struct {
		method, path, body string
		status, calls      int
	}{
		{"GET", root, "", 200, 1}, {"POST", root + "/retry", body, 200, 1},
		{"GET", root + "?actionId=user-1&expectedEpisode=old-episode&expectedSourceRevision=1", "", 200, 1},
		{"GET", root + "?actionId=user-1", "", 400, 0},
		{"GET", root + "?actionId=user-1&expectedEpisode=old-episode&expectedSourceRevision=01", "", 400, 0},
		{"POST", root + "/retry", body + ` {}`, 400, 0},
		{"POST", root + "/retry", strings.TrimSuffix(body, "}") + `,"continue":true}`, 400, 0},
		{"POST", root + "/retry", strings.Replace(body, `"version":1`, `"version":2`, 1), 400, 0},
		{"POST", root + "/retry?force=true", body, 400, 0},
		{"PUT", root + "/retry", body, 405, 0},
	} {
		t.Run(sample.method+sample.path+sample.body, func(t *testing.T) {
			f := &recoveryRetryFixture{result: download.RecoveryTaskState{Version: 1, TaskID: "-_task", Management: "managed"}}
			w := retryFixtureCall(f, sample.method, sample.path, sample.body)
			if w.Code != sample.status || f.calls != sample.calls {
				t.Fatalf("status/calls=%d/%d want %d/%d body=%s", w.Code, f.calls, sample.status, sample.calls, w.Body.String())
			}
			if w.Code == 200 && !strings.Contains(w.Body.String(), `"scopeVersion":1`) {
				t.Fatal("missing scope wrapper")
			}
		})
	}
}
func TestRecoveryRetryRESTFixedErrorsAndScope(t *testing.T) {
	root := "/api/v1/datasets/" + strings.Repeat("a", 32) + "/tasks/task_1/recovery/v1"
	for _, sample := range []struct {
		err     error
		status  int
		message string
	}{
		{download.ErrDatasetMismatch, 409, "dataset_mismatch"},
		{download.ErrRecoveryRetryStale, 409, "explicit_retry_stale"},
		{download.ErrRecoveryRetryConflict, 409, "explicit_retry_conflict"},
		{transfer.ErrRecoveryHeld, 409, "recovery_held"},
		{errors.New("SYNTHETIC_SECRET https://private/path?token=secret"), 503, "explicit_retry_unavailable_query_same_action"},
	} {
		f := &recoveryRetryFixture{err: sample.err}
		w := retryFixtureCall(f, http.MethodGet, root, "")
		if w.Code != sample.status || !strings.Contains(w.Body.String(), sample.message) || !strings.Contains(w.Body.String(), `"data":null`) || strings.Contains(w.Body.String(), "SYNTHETIC") || strings.Contains(w.Body.String(), "private") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	f := &recoveryRetryFixture{result: download.RecoveryTaskState{Version: 1, TaskID: "another-task"}}
	if w := retryFixtureCall(f, http.MethodGet, root, ""); w.Code != 503 || !strings.Contains(w.Body.String(), `"data":null`) {
		t.Fatal("adopted wrong task", w.Body.String())
	}
}
