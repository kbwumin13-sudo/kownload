package rest

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/pkg/download"
)

type completionEventsCoreFixture struct {
	completionEventsFixture
	capability             download.VerifiedHTTPCapability
	capabilityErr          error
	capabilityCalls        int
	dataset                string
	seenContext            context.Context
	cancelDuringCapability context.CancelFunc
}

func (f *completionEventsCoreFixture) VerifiedHTTPCapabilityForDataset(dataset string) (download.VerifiedHTTPCapability, error) {
	f.capabilityCalls++
	f.dataset = dataset
	if f.cancelDuringCapability != nil {
		f.cancelDuringCapability()
	}
	return f.capability, f.capabilityErr
}

func (f *completionEventsCoreFixture) SubscribeVerifiedHTTPDone(ctx context.Context, dataset string) (<-chan download.VerifiedHTTPDoneEvent, func(), error) {
	f.seenContext = ctx
	if dataset != f.dataset {
		return nil, nil, errors.New("adapter changed the expected domain")
	}
	return f.completionEventsFixture.SubscribeVerifiedHTTPDone(ctx, dataset)
}

func supportedEventsCapability() download.VerifiedHTTPCapability {
	return download.VerifiedHTTPCapability{Version: 1, Capability: verifiedHTTPCapability, Supported: true}
}

func TestCompletionEventsAdapterUnsupportedAndErrorsNeverSubscribe(t *testing.T) {
	domain := strings.Repeat("a", 32)
	for _, test := range []struct {
		name       string
		capability download.VerifiedHTTPCapability
		err        error
		status     int
		message    string
	}{
		{"inactive", download.VerifiedHTTPCapability{Version: 1, Capability: verifiedHTTPCapability}, nil, 501, "verified_http_unsupported"},
		{"unknown-version", download.VerifiedHTTPCapability{Version: 2, Capability: verifiedHTTPCapability, Supported: true}, nil, 501, "verified_http_unsupported"},
		{"unknown-name", download.VerifiedHTTPCapability{Version: 1, Capability: "other-capability", Supported: true}, nil, 501, "verified_http_unsupported"},
		{"missing-contract", download.VerifiedHTTPCapability{Supported: true}, nil, 501, "verified_http_unsupported"},
		{"mismatch", supportedEventsCapability(), download.ErrDatasetMismatch, 409, "dataset_mismatch"},
		{"unsupported", supportedEventsCapability(), download.ErrVerifiedHTTPUnsupported, 501, "verified_http_unsupported"},
		{"unavailable", supportedEventsCapability(), errors.New("private-path-auth-digest"), 503, "completion_events_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := &completionEventsCoreFixture{capability: test.capability, capabilityErr: test.err}
			w := httptest.NewRecorder()
			eventRouter(verifiedHTTPEventsAdapter{core: fixture}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, eventRoute(domain), nil))
			if w.Code != test.status || !strings.Contains(w.Body.String(), `"msg":"`+test.message+`"`) ||
				w.Header().Get("Content-Type") == "text/event-stream" || strings.Contains(w.Body.String(), "private-path") ||
				fixture.capabilityCalls != 1 || fixture.dataset != domain || fixture.calls.Load() != 0 || fixture.releases.Load() != 0 {
				t.Fatalf("untrusted capability subscribed or streamed: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestCompletionEventsAdapterCancellationPreventsSubscription(t *testing.T) {
	for _, before := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		fixture := &completionEventsCoreFixture{capability: supportedEventsCapability()}
		if before {
			cancel()
		} else {
			fixture.cancelDuringCapability = cancel
		}
		events, release, err := (verifiedHTTPEventsAdapter{core: fixture}).SubscribeVerifiedHTTPDone(ctx, strings.Repeat("a", 32))
		if !errors.Is(err, context.Canceled) || events != nil || release != nil || fixture.calls.Load() != 0 {
			t.Fatal("cancelled adapter allocated subscription")
		}
		want := 1
		if before {
			want = 0
		}
		if fixture.capabilityCalls != want {
			t.Fatal("unexpected capability read after cancellation")
		}
	}
}

func TestCompletionEventsAdapterForwardsSameDomainContextAndRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := &completionEventsCoreFixture{capability: supportedEventsCapability(), completionEventsFixture: completionEventsFixture{events: make(chan download.VerifiedHTTPDoneEvent)}}
	events, release, err := (verifiedHTTPEventsAdapter{core: fixture}).SubscribeVerifiedHTTPDone(ctx, eventFixtureValue().DatasetID)
	if err != nil || events != fixture.events || release == nil || fixture.seenContext != ctx || fixture.calls.Load() != 1 {
		t.Fatal("typed subscription was not forwarded")
	}
	release()
	if fixture.releases.Load() != 1 {
		t.Fatal("release was replaced or lost")
	}
}

func TestCompletionEventsAdapterRechecksSubscriptionErrorsBeforeHeaders(t *testing.T) {
	fixture := &completionEventsCoreFixture{capability: supportedEventsCapability(), completionEventsFixture: completionEventsFixture{err: download.ErrDatasetMismatch}}
	w := httptest.NewRecorder()
	eventRouter(verifiedHTTPEventsAdapter{core: fixture}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, eventRoute(eventFixtureValue().DatasetID), nil))
	if w.Code != 409 || w.Header().Get("Content-Type") == "text/event-stream" || fixture.calls.Load() != 1 || fixture.releases.Load() != 1 {
		t.Fatal("post-capability dataset error exposed SSE or leaked subscriber")
	}
}

// This is a real HTTP transport adapter test with injected events, not proof
// of a Core fresh-C emission. That source-of-authority test needs the final
// activated Core and confirmed C/API publication.
func TestCompletionEventsAdapterRealHTTPFutureFrameAndCancellation(t *testing.T) {
	fixture := &completionEventsCoreFixture{capability: supportedEventsCapability(), completionEventsFixture: completionEventsFixture{events: make(chan download.VerifiedHTTPDoneEvent, 1)}}
	server := httptest.NewServer(eventRouter(verifiedHTTPEventsAdapter{core: fixture}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+eventRoute(eventFixtureValue().DatasetID), nil)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("adapter stream handshake failed")
	}
	fixture.events <- eventFixtureValue()
	reader := bufio.NewReader(response.Body)
	name, err := reader.ReadString('\n')
	if err != nil || name != "event: accepted-done\n" {
		t.Fatal("adapter event frame missing", err)
	}
	data, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(data, `"completionId":"example-completion"`) {
		t.Fatal("adapter changed event payload", err)
	}
	if blank, err := reader.ReadString('\n'); err != nil || blank != "\n" {
		t.Fatal("adapter frame not terminated")
	}
	cancel()
	_ = response.Body.Close()
	requireEventsReleased(t, &fixture.completionEventsFixture)
}
