package netdisk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

func captureFailureClaim(t *testing.T, s *Service) (*ClaimResult, model.Capture) {
	t.Helper()
	r, err := s.RequestBrowser(model.Source{URL: "https://pan.quark.cn/s/share"}, "browser")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(r.Source.URL, "browser", "Chrome", "1:0:doc:0:store", r.ID)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v, %v", claim, err)
	}
	return claim, model.Capture{Provider: "quark", SourcePage: r.Source.URL, InstanceID: "browser", ContextID: "1:0:doc:0:store"}
}

func captureFailureStatus(t *testing.T, s *Service, id, want string) *model.BrowserRequest {
	t.Helper()
	status, err := s.BrowserStatus(id)
	if err != nil || status.State != want {
		t.Fatalf("status: %+v, %v; want %s", status, err, want)
	}
	if want == "failed" && (status.Message != "网盘页面同步失败，请重新同步" || status.SessionID != "") {
		t.Fatalf("unsafe failure: %+v", status)
	}
	return status
}

func TestCaptureFailureValidationClosesOwnedRequest(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Service, *model.Capture)
	}{
		{"source_page", func(_ *Service, c *model.Capture) {
			c.SourcePage = "https://private-user:private-password@example.invalid/"
		}},
		{"provider", func(_ *Service, c *model.Capture) { c.Provider = "baidu" }},
		{"file_limit", func(_ *Service, c *model.Capture) { c.Files = make([]model.File, 10001) }},
		{"cookie_limit", func(_ *Service, c *model.Capture) { c.Cookies = make([]model.Cookie, 513) }},
		{"token_limit", func(_ *Service, c *model.Capture) {
			c.Tokens = map[string]string{}
			for i := 0; i < 65; i++ {
				c.Tokens[fmt.Sprint(i)] = "private-token"
			}
		}},
		{"file", func(_ *Service, c *model.Capture) { c.Files = []model.File{{ID: "id", Name: "../private-file"}} }},
		{"path", func(_ *Service, c *model.Capture) {
			c.Files = []model.File{{ID: "id", Name: "file", Path: "../private-path"}}
		}},
		{"cookie_domain", func(_ *Service, c *model.Capture) {
			c.Cookies = []model.Cookie{{Name: "private-cookie", Value: "private-secret", Domain: "private.example"}}
		}},
		{"actual_share", func(_ *Service, c *model.Capture) {
			c.SourcePage = "https://pan.quark.cn/s/other-private-share"
			c.Share = map[string]string{"shareId": "share"}
		}},
		{"payload_share", func(_ *Service, c *model.Capture) { c.Share = map[string]string{"shareId": "other-private-share"} }},
		{"session_capacity", func(s *Service, _ *model.Capture) {
			s.mu.Lock()
			defer s.mu.Unlock()
			for i := 0; i < 128; i++ {
				s.sessions[fmt.Sprint(i)] = &Session{CreatedAt: time.Now()}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService(&testProvider{})
			claim, c := captureFailureClaim(t, s)
			tc.mutate(s, &c)
			if _, err := s.Capture(claim.Request.ID, claim.Nonce, c); err == nil {
				t.Fatal("invalid capture accepted")
			}
			status := captureFailureStatus(t, s, claim.Request.ID, "failed")
			encoded, _ := json.Marshal(status)
			if strings.Contains(string(encoded), "private-") {
				t.Fatal("private payload leaked into browser status")
			}
			if again, err := s.Claim(claim.Request.Source.URL, "browser", "Chrome", c.ContextID, claim.Request.ID); err != nil || again != nil {
				t.Fatalf("failed validation request reclaimed: %v, %v", again, err)
			}
			c = model.Capture{Provider: "quark", SourcePage: claim.Request.Source.URL, InstanceID: "browser", ContextID: c.ContextID}
			if _, err := s.Capture(claim.Request.ID, claim.Nonce, c); err == nil {
				t.Fatal("corrected payload revived failed request")
			}
		})
	}
}

func TestCaptureFailureContextPreflightAndCaptureError(t *testing.T) {
	for _, source := range []string{"https://user:private-password@example.invalid/", "https://pan.quark.cn/s/share"} {
		t.Run(source, func(t *testing.T) {
			s := NewService(&testProvider{})
			claim, c := captureFailureClaim(t, s)
			c.SourcePage = source
			c.Files = []model.File{{Name: "invalid"}}
			if _, err := s.CaptureContext(context.Background(), nil, claim.Request.ID, claim.Nonce, c); err == nil {
				t.Fatal("invalid context capture accepted")
			}
			captureFailureStatus(t, s, claim.Request.ID, "failed")
		})
	}
}

func TestCaptureFailureForeignOwnershipCannotPoisonRequest(t *testing.T) {
	for _, name := range []string{"nonce", "empty_nonce", "tab", "instance", "empty_context", "empty_instance", "request"} {
		t.Run(name, func(t *testing.T) {
			s := NewService(&testProvider{})
			claim, valid := captureFailureClaim(t, s)
			c := valid
			id, nonce := claim.Request.ID, claim.Nonce
			switch name {
			case "nonce":
				nonce = "wrong"
			case "empty_nonce":
				nonce = ""
			case "tab":
				c.ContextID = "2:0:doc:0:store"
			case "instance":
				c.InstanceID = "other"
			case "empty_context":
				c.ContextID = ""
			case "empty_instance":
				c.InstanceID = ""
			case "request":
				other, _ := captureFailureClaim(t, s)
				id = other.Request.ID
			}
			c.SourcePage = "invalid-private-source"
			for _, capture := range []func() error{
				func() error { _, err := s.Capture(id, nonce, c); return err },
				func() error { _, err := s.CaptureContext(context.Background(), nil, id, nonce, c); return err },
			} {
				if err := capture(); err == nil {
					t.Fatal("foreign capture accepted")
				}
			}
			captureFailureStatus(t, s, claim.Request.ID, "capturing")
			if name == "request" {
				captureFailureStatus(t, s, id, "capturing")
			}
			if _, err := s.Capture(claim.Request.ID, claim.Nonce, valid); err != nil {
				t.Fatal(err)
			}
			captureFailureStatus(t, s, claim.Request.ID, "ready")
		})
	}
}

func TestCaptureFailureDeadlineSharedByStatusClaimAndCapture(t *testing.T) {
	for _, trigger := range []string{"status", "claim", "capture", "capture_context"} {
		t.Run(trigger, func(t *testing.T) {
			s := NewService(&testProvider{})
			claim, c := captureFailureClaim(t, s)
			s.mu.Lock()
			s.requests[claim.Request.ID].CaptureStartedAt = time.Now().Add(-60 * time.Second)
			s.mu.Unlock()
			switch trigger {
			case "status":
				captureFailureStatus(t, s, claim.Request.ID, "failed")
			case "claim":
				repeat, err := s.Claim(c.SourcePage, c.InstanceID, "Chrome", c.ContextID, claim.Request.ID)
				if err != nil || repeat != nil {
					t.Fatalf("timed out claim revived: %v, %v", repeat, err)
				}
			case "capture":
				if _, err := s.Capture(claim.Request.ID, claim.Nonce, c); err == nil {
					t.Fatal("late capture accepted")
				}
			case "capture_context":
				if _, err := s.CaptureContext(context.Background(), nil, claim.Request.ID, claim.Nonce, c); err == nil {
					t.Fatal("late context capture accepted")
				}
			}
			captureFailureStatus(t, s, claim.Request.ID, "failed")
			if _, err := s.Capture(claim.Request.ID, claim.Nonce, c); err == nil {
				t.Fatal("failed request revived by capture")
			}
			for _, tab := range []string{c.ContextID, "1:0:reload:1:store", "2:0:doc:0:store"} {
				if next, err := s.Claim(c.SourcePage, c.InstanceID, "Chrome", tab, claim.Request.ID); err != nil || next != nil {
					t.Fatalf("failed request revived by claim: %v, %v", next, err)
				}
			}
			s.mu.Lock()
			s.requests[claim.Request.ID].Request.ExpiresAt = time.Now().Add(-time.Minute)
			s.mu.Unlock()
			captureFailureStatus(t, s, claim.Request.ID, "failed")
		})
	}
}

func TestCaptureFailureRepeatAndReloadCannotExtendDeadline(t *testing.T) {
	s := NewService(&testProvider{})
	claim, c := captureFailureClaim(t, s)
	started := time.Now().Add(-59 * time.Second)
	s.mu.Lock()
	s.requests[claim.Request.ID].CaptureStartedAt = started
	s.mu.Unlock()
	captureFailureStatus(t, s, claim.Request.ID, "capturing")
	for i := 0; i < 5; i++ {
		repeat, err := s.Claim(c.SourcePage, c.InstanceID, "Chrome", c.ContextID, claim.Request.ID)
		if err != nil || repeat == nil || repeat.Nonce != claim.Nonce {
			t.Fatalf("repeat claim: %v, %v", repeat, err)
		}
	}
	c.ContextID = "1:0:reload:1:store"
	reload, err := s.Claim(c.SourcePage, c.InstanceID, "Chrome", c.ContextID, claim.Request.ID)
	if err != nil || reload == nil || reload.Nonce == claim.Nonce {
		t.Fatalf("reload: %v, %v", reload, err)
	}
	s.mu.Lock()
	gotStarted := s.requests[claim.Request.ID].CaptureStartedAt
	s.requests[claim.Request.ID].CaptureStartedAt = started.Add(-time.Second)
	s.mu.Unlock()
	if !gotStarted.Equal(started) {
		t.Fatal("claim extended first capture deadline")
	}
	captureFailureStatus(t, s, claim.Request.ID, "failed")
	if _, err := s.Capture(claim.Request.ID, reload.Nonce, c); err == nil {
		t.Fatal("reload revived timed out capture")
	}
}

func TestCaptureFailureWaitingKeepsTenMinuteExpiry(t *testing.T) {
	s := NewService(&testProvider{})
	before := time.Now()
	r, err := s.RequestBrowser(model.Source{URL: "https://pan.quark.cn/s/share"}, "browser")
	if err != nil {
		t.Fatal(err)
	}
	if r.ExpiresAt.Before(before.Add(10*time.Minute)) || r.ExpiresAt.After(time.Now().Add(10*time.Minute)) {
		t.Fatal("waiting expiry changed")
	}
	captureFailureStatus(t, s, r.ID, "waiting")
	s.mu.Lock()
	s.requests[r.ID].Request.ExpiresAt = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if claim, err := s.Claim(r.Source.URL, "browser", "Chrome", "tab", r.ID); err != nil || claim != nil {
		t.Fatalf("expired waiting request claimed: %v, %v", claim, err)
	}
	captureFailureStatus(t, s, r.ID, "expired")
}

// Pause one payload validation after ownership was checked. Subsequent calls
// can proceed, making cancellation, reload and competing capture deterministic.
type captureFailureBarrierProvider struct {
	testProvider
	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
}

func (p *captureFailureBarrierProvider) Info() model.ProviderInfo {
	p.mu.Lock()
	entered, release := p.entered, p.release
	p.entered = nil
	p.mu.Unlock()
	if entered != nil {
		close(entered)
		<-release
	}
	return p.testProvider.Info()
}

func (p *captureFailureBarrierProvider) pause(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	p.mu.Lock()
	entered, release := make(chan struct{}), make(chan struct{})
	p.entered, p.release = entered, release
	p.mu.Unlock()
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	return entered, resume
}

func captureFailureAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("capture blocked; possible failure-defer deadlock")
		var zero T
		return zero
	}
}

func TestCaptureFailureInFlightCannotOverwriteReadyReloadRetryOrTimeout(t *testing.T) {
	for _, action := range []string{"ready", "reload", "cancel_retry", "timeout", "failure"} {
		t.Run(action, func(t *testing.T) {
			p := &captureFailureBarrierProvider{}
			s := NewService(p)
			claim, c := captureFailureClaim(t, s)
			stale := c
			if action != "timeout" && action != "failure" {
				stale.Files = []model.File{{Name: "invalid"}}
			}
			entered, resume := p.pause(t)
			done := make(chan error, 1)
			go func() { _, err := s.Capture(claim.Request.ID, claim.Nonce, stale); done <- err }()
			captureFailureAwait(t, entered)
			var sessionID string
			switch action {
			case "ready":
				result, err := s.Capture(claim.Request.ID, claim.Nonce, c)
				if err != nil {
					t.Fatal(err)
				}
				sessionID = result.SessionID
			case "reload":
				c.ContextID = "1:0:reload:1:store"
				reloaded, err := s.Claim(c.SourcePage, c.InstanceID, "Chrome", c.ContextID, claim.Request.ID)
				if err != nil || reloaded == nil {
					t.Fatal("reload failed", err)
				}
				claim = reloaded
			case "cancel_retry":
				s.CancelBrowser(claim.Request.ID)
				if _, err := s.BrowserStatus(claim.Request.ID); err == nil {
					t.Fatal("cancelled request still exists")
				}
				claim, c = captureFailureClaim(t, s)
			case "timeout":
				s.mu.Lock()
				s.requests[claim.Request.ID].CaptureStartedAt = time.Now().Add(-time.Minute)
				s.mu.Unlock()
				captureFailureStatus(t, s, claim.Request.ID, "failed")
			case "failure":
				invalid := c
				invalid.Files = []model.File{{Name: "invalid"}}
				if _, err := s.Capture(claim.Request.ID, claim.Nonce, invalid); err == nil {
					t.Fatal("invalid competing capture accepted")
				}
				captureFailureStatus(t, s, claim.Request.ID, "failed")
			}
			resume()
			if err := captureFailureAwait(t, done); err == nil {
				t.Fatal("stale or invalid capture accepted")
			}
			switch action {
			case "ready":
				status := captureFailureStatus(t, s, claim.Request.ID, "ready")
				if status.SessionID != sessionID || status.Message != "" {
					t.Fatal("ready session overwritten")
				}
				s.mu.Lock()
				s.requests[claim.Request.ID].CaptureStartedAt = time.Now().Add(-time.Minute)
				s.requests[claim.Request.ID].Request.ExpiresAt = time.Now().Add(-time.Minute)
				s.mu.Unlock()
				captureFailureStatus(t, s, claim.Request.ID, "ready")
			case "timeout", "failure":
				captureFailureStatus(t, s, claim.Request.ID, "failed")
			default:
				captureFailureStatus(t, s, claim.Request.ID, "capturing")
				if _, err := s.Capture(claim.Request.ID, claim.Nonce, c); err != nil {
					t.Fatal(err)
				}
				captureFailureStatus(t, s, claim.Request.ID, "ready")
			}
		})
	}
}
