package http

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

func TestRecoveryErrorPolicyMatrixNeverEchoesRequestContext(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		category string
		retry    bool
	}{
		{"cancel", context.Canceled, "cancelled", false},
		{"deadline", context.DeadlineExceeded, "timeout", true},
		{"dns-missing", &net.DNSError{Name: "synthetic-secret.invalid", IsNotFound: true}, "dns", false},
		{"dns-temporary", &net.DNSError{Name: "synthetic-secret.invalid", IsTimeout: true}, "dns", true},
		{"tcp-reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, "network", true},
		{"proxy-connect", &net.OpError{Op: "proxyconnect", Err: syscall.ECONNREFUSED}, "network", true},
		{"tls-untrusted", x509.UnknownAuthorityError{}, "tls", false},
		{"tls-hostname", x509.HostnameError{}, "tls", false},
		{"disk-permission", &localFileError{&os.PathError{Op: "open", Path: "synthetic-secret", Err: syscall.EACCES}}, "local_file", false},
		{"disk-full", &localFileError{syscall.ENOSPC}, "local_file", false},
		{"eof", io.EOF, "truncated", true},
		{"short-body", io.ErrUnexpectedEOF, "truncated", true},
		{"validated-range-short-body", fmt.Errorf("%w: %w", errInvalidRangeResponse, io.ErrUnexpectedEOF), "truncated", true},
		{"scoped-context", transfer.ErrScopedContext, "authentication_context", false},
		{"credential-context", transfer.ErrCredentialContext, "authentication_context", false},
		{"unsafe-resume", ErrUnsafeResume, "unsafe_resume", false},
		{"invalid-range", errInvalidRangeResponse, "unsafe_resume", false},
		{"unknown", errors.New("synthetic-secret"), "unknown", false},
	}
	for _, code := range []int{401, 403, 407, 404, 410, 408, 429, 503, 416, 400, 500, 502, 504} {
		category := "http_status"
		retry := false
		switch code {
		case 401, 403, 407:
			category = "authentication"
		case 404, 410:
			category = "source_missing"
		case 408:
			category = "timeout"
			retry = true
		case 429, 503:
			category = "source_cooldown"
		case 416:
			category = "unsafe_resume"
		default:
			if code >= 500 {
				category = "server"
				retry = true
			}
		}
		tests = append(tests, struct {
			name     string
			err      error
			category string
			retry    bool
		}{"status-" + strconv.Itoa(code), NewRequestError(code), category, retry})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classified := ClassifyRecoveryError(&url.Error{Op: "GET", URL: "https://synthetic-secret:synthetic-secret@source.invalid/?token=synthetic-secret", Err: test.err})
			if classified.Category != test.category || classified.Retryable != test.retry {
				t.Fatalf("policy: got %v/%v want %v/%v", classified.Category, classified.Retryable, test.category, test.retry)
			}
			if strings.Contains(classified.Error(), "synthetic-secret") || strings.Contains(classified.Error(), "source.invalid") {
				t.Fatal("request context leaked")
			}
		})
	}
}
