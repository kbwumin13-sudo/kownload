package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/download"
)

func TestResolveRequestCancellation(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(exited)
	}))
	defer source.Close()
	previous := Downloader
	Downloader = download.NewDownloader(&download.DownloaderConfig{StorageDir: t.TempDir()})
	if err := Downloader.Setup(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = Downloader.Close(); Downloader = previous }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/resolve", strings.NewReader(`{"req":{"url":"`+source.URL+`","protocol":"http"}}`)).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { Resolve(response, request); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("source request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("REST resolve ignored request cancellation")
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("source request remained active after cancellation")
	}
	if !strings.Contains(response.Body.String(), "context canceled") {
		t.Fatalf("missing cancellation response: %s", response.Body.String())
	}
}
