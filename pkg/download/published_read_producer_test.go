package download

import (
	"errors"
	"testing"
	"time"

	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
)

func TestAcceptedHTTPReadErrorUsesSameTypedClassification(t *testing.T) {
	for _, sample := range []struct {
		category, want string
		retry          bool
	}{
		{"network", "network", true}, {"authentication", "authentication", false}, {"truncated", "network", true}, {"unsafe_resume", "integrity", false}, {"local_file", "storage", false}, {"cancelled", "cancelled", false}, {"unknown", "unknown", false},
	} {
		t.Run(sample.category, func(t *testing.T) {
			before := time.Now().UnixMilli()
			got := acceptedHTTPReadError(&httpProtocol.RecoveryFailure{Category: sample.category, Retryable: sample.retry})
			if !got.Known || got.Category != sample.want || got.Retryable != sample.retry || got.OccurredAtMillis < before || got.OccurredAtMillis > time.Now().UnixMilli() {
				t.Fatal("invented classification/time", got)
			}
		})
	}
	if got := acceptedHTTPReadError(transfer.ErrRecoveryExhausted); got.Category != "unknown" || got.Retryable {
		t.Fatal("exhaustion pretended to be retryable network", got)
	}
	if got := acceptedHTTPReadError(transfer.ErrRecoveryUnavailable); got.Category != "storage" || got.Retryable {
		t.Fatal("storage category lost", got)
	}
	if got := acceptedHTTPReadError(errors.New("synthetic private URL ?token=secret")); got.Category != "unknown" || got.Retryable {
		t.Fatal("parsed raw text", got)
	}
}
