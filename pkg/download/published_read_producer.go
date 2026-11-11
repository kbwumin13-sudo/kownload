package download

import (
	"errors"
	"time"

	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
)

// Called only after Core accepts this run's real error. Classification is the
// same HTTP classifier as recovery, never status/UpdatedAt or raw log parsing.
func acceptedHTTPReadError(err error) MCPReadPublishedError {
	var failure *httpProtocol.RecoveryFailure
	if !errors.As(err, &failure) {
		failure = httpProtocol.ClassifyRecoveryError(err)
	}
	category := "unknown"
	switch failure.Category {
	case "dns", "network", "tls", "timeout", "truncated", "server", "source_cooldown", "source_missing", "http_status":
		category = "network"
	case "authentication", "authentication_context":
		category = "authentication"
	case "unsafe_resume":
		category = "integrity"
	case "local_file":
		category = "storage"
	case "cancelled":
		category = "cancelled"
	}
	if errors.Is(err, transfer.ErrRecoveryUnavailable) {
		category = "storage"
	}
	return MCPReadPublishedError{Known: true, Category: category, Retryable: failure.Retryable, OccurredAtMillis: time.Now().UnixMilli()}
}
