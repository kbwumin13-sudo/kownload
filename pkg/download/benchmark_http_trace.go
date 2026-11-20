//go:build speedbench

package download

import (
	"errors"
	"strconv"

	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
)

// Never record an error string: request errors can contain signed URLs.
func benchmarkHTTPReason(errs []error) string {
	if len(errs) == 0 || errs[0] == nil {
		return ""
	}
	err := errs[0]
	switch {
	case errors.Is(err, transfer.ErrRecoveryExhausted):
		return "recovery_budget_exhausted"
	case errors.Is(err, transfer.ErrRecoveryStale):
		return "recovery_generation_obsolete"
	case errors.Is(err, transfer.ErrRecoveryWork):
		return "recovery_work_not_authorized"
	case errors.Is(err, transfer.ErrRecoveryHeld):
		return "recovery_state_incompatible"
	case errors.Is(err, transfer.ErrRecoveryUnavailable):
		return "recovery_state_unavailable"
	}
	var response *httpProtocol.RequestError
	if errors.As(err, &response) {
		return "http_" + strconv.Itoa(response.Code)
	}
	var classified *httpProtocol.RecoveryFailure
	if !errors.As(err, &classified) {
		classified = httpProtocol.ClassifyRecoveryError(err)
	}
	switch classified.Category {
	case "cancelled", "authentication_context", "truncated", "unsafe_resume", "local_file",
		"authentication", "source_missing", "timeout", "source_cooldown", "server", "http_status", "tls", "dns", "network":
		return classified.Category
	default:
		return "unknown"
	}
}
