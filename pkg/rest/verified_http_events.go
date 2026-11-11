package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
)

const (
	verifiedEventWriteTimeout = 5 * time.Second
	verifiedEventMaxFrame     = 4096
)

var errCompletionEventsUnsupported = errors.New("verified_http_unsupported")

type verifiedHTTPDoneSource interface {
	SubscribeVerifiedHTTPDone(context.Context, string) (<-chan download.VerifiedHTTPDoneEvent, func(), error)
}

// No fresh-C publisher is connected yet. Capture this server's owner and decline
// after a read-only domain check. A pure broker must not turn the capability on.
type unsupportedVerifiedHTTPEvents struct{ core *download.Downloader }

func (b unsupportedVerifiedHTTPEvents) SubscribeVerifiedHTTPDone(_ context.Context, expected string) (<-chan download.VerifiedHTTPDoneEvent, func(), error) {
	identity, err := b.core.DatasetIdentity()
	if err != nil {
		return nil, nil, err
	}
	if identity.Version != 1 || !scopedDatasetPattern.MatchString(identity.DatasetID) {
		return nil, nil, download.ErrDatasetInvalid
	}
	if identity.DatasetID != expected {
		return nil, nil, download.ErrDatasetMismatch
	}
	return nil, nil, errCompletionEventsUnsupported
}

func registerVerifiedHTTPEventRoutes(router *mux.Router, source verifiedHTTPDoneSource) {
	router.Methods(http.MethodGet).Path("/api/v1/datasets/{datasetId}/verified-http/v1/completion-events").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		domain := mux.Vars(r)["datasetId"]
		if !scopedDatasetPattern.MatchString(domain) {
			WriteStatusJson(w, http.StatusBadRequest, model.NewErrorResult("invalid_dataset_request"))
			return
		}
		hasResumeHeader := false
		for name := range r.Header {
			hasResumeHeader = hasResumeHeader || strings.EqualFold(name, "Last-Event-ID")
		}
		if r.URL.RawQuery != "" || hasResumeHeader {
			WriteStatusJson(w, http.StatusBadRequest, model.NewErrorResult("invalid_completion_events_request"))
			return
		}
		// net/http may otherwise wait for an unread HTTP/1 request body before
		// flushing headers. A write deadline cannot bound that read. This GET
		// has no body; close rejected connections without draining attacker IO.
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			w.Header().Set("Connection", "close")
			WriteStatusJson(w, http.StatusBadRequest, model.NewErrorResult("invalid_completion_events_request"))
			return
		}
		events, release, err := source.SubscribeVerifiedHTTPDone(r.Context(), domain)
		if release != nil {
			defer release()
		}
		if err != nil {
			writeCompletionEventsError(w, err)
			return
		}
		if events == nil || release == nil {
			writeCompletionEventsError(w, download.ErrVerifiedHTTPEventsClosed)
			return
		}
		serveVerifiedHTTPEvents(w, r, domain, events)
	})
}

func writeCompletionEventsError(w http.ResponseWriter, err error) {
	status, message := http.StatusServiceUnavailable, "completion_events_unavailable"
	switch {
	case errors.Is(err, errCompletionEventsUnsupported):
		status, message = http.StatusNotImplemented, "verified_http_unsupported"
	case errors.Is(err, download.ErrVerifiedHTTPEventsCapacity):
		message = "completion_events_capacity"
	case errors.Is(err, download.ErrDatasetMismatch):
		status, message = http.StatusConflict, "dataset_mismatch"
	case errors.Is(err, download.ErrDatasetInvalid), errors.Is(err, download.ErrDatasetMissing), errors.Is(err, download.ErrDatasetViewClosed):
		message = "dataset_unavailable"
	case errors.Is(err, download.ErrDatasetUnsupported):
		status, message = http.StatusNotImplemented, "dataset_identity_storage_unsupported"
	}
	WriteStatusJson(w, status, model.NewErrorResult(message))
}

func verifiedEventsCanFlush(w http.ResponseWriter) bool {
	// ResponseController supports these exact interfaces through Unwrap. Bound
	// traversal so a malformed middleware cannot create an infinite loop.
	for depth := 0; depth < 16; depth++ {
		switch writer := w.(type) {
		case interface{ FlushError() error }:
			return true
		case http.Flusher:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			w = writer.Unwrap()
		default:
			return false
		}
	}
	return false
}

func serveVerifiedHTTPEvents(w http.ResponseWriter, r *http.Request, domain string, events <-chan download.VerifiedHTTPDoneEvent) {
	controller := http.NewResponseController(w)
	if !verifiedEventsCanFlush(w) || controller.SetWriteDeadline(time.Now().Add(verifiedEventWriteTimeout)) != nil {
		WriteStatusJson(w, http.StatusNotImplemented, model.NewErrorResult("completion_events_stream_unsupported"))
		return
	}
	var deadlineMu sync.Mutex
	// Serialize only the deadline setters, never network writes or Core locks.
	// Cancellation cannot be overwritten by a later future/idle deadline.
	setDeadline := func(deadline time.Time) error {
		deadlineMu.Lock()
		defer deadlineMu.Unlock()
		if err := r.Context().Err(); err != nil {
			return err
		}
		return controller.SetWriteDeadline(deadline)
	}
	cancelFinished := make(chan struct{})
	stopCancel := context.AfterFunc(r.Context(), func() {
		defer close(cancelFinished)
		deadlineMu.Lock()
		defer deadlineMu.Unlock()
		_ = controller.SetWriteDeadline(time.Now())
	})
	writeFailed := false
	defer func() {
		if !stopCancel() {
			<-cancelFinished
		}
		// Bound net/http's final response flush too. An expired/failed write
		// must retain its deadline; clearing it could block finalization.
		if !writeFailed {
			_ = setDeadline(time.Now().Add(verifiedEventWriteTimeout))
		}
	}()
	if r.Context().Err() != nil {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := controller.Flush(); err != nil {
		writeFailed = true
		return
	}
	if err := setDeadline(time.Time{}); err != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-events:
			if !open || !event.ValidForDataset(domain) {
				return
			}
			payload, err := json.Marshal(event)
			if err != nil {
				return
			}
			frame := "event: accepted-done\ndata: " + string(payload) + "\n\n"
			if len(frame) > verifiedEventMaxFrame || setDeadline(time.Now().Add(verifiedEventWriteTimeout)) != nil {
				return
			}
			n, err := io.WriteString(w, frame)
			if err != nil || n != len(frame) {
				writeFailed = true
				return
			}
			if err := controller.Flush(); err != nil {
				writeFailed = true
				return
			}
			// Idle streams have no heartbeat and must not expire an outstanding
			// write deadline; a timed-out stream is never revived or retried.
			if setDeadline(time.Time{}) != nil {
				return
			}
		}
	}
}
