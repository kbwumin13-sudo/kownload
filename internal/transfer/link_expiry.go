package transfer

// Link expiry and authentication refusal are not source rate-limit responses.
// Callers refresh only when a source resolver and resource identity are known.
func IsExpiredLinkStatus(status int) bool {
	return status == 401 || status == 403 || status == 410 || status == 412
}
