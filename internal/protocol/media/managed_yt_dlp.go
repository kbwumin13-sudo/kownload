package media

import (
	"context"
	"errors"
)

var (
	ErrManagedYtDlpBusy       = errors.New("managed_ytdlp_busy")
	ErrManagedYtDlpLocation   = errors.New("managed_ytdlp_location")
	ErrManagedYtDlpPython     = errors.New("managed_ytdlp_python")
	ErrManagedYtDlpDownload   = errors.New("managed_ytdlp_download")
	ErrManagedYtDlpSignature  = errors.New("managed_ytdlp_signature")
	ErrManagedYtDlpAsset      = errors.New("managed_ytdlp_asset")
	ErrManagedYtDlpNoPrevious = errors.New("managed_ytdlp_no_previous")
	ErrManagedYtDlpOlder      = errors.New("managed_ytdlp_older")
)

type ManagedYtDlpState struct {
	Entry           string `json:"entry"`
	ActiveVersion   string `json:"activeVersion"`
	PreviousVersion string `json:"previousVersion"`
	InUse           bool   `json:"inUse"`
}

type ManagedYtDlp interface {
	State(stored any) (ManagedYtDlpState, error)
	Update(ctx context.Context, stored any) (ManagedYtDlpState, error)
	Rollback(ctx context.Context, stored any) (ManagedYtDlpState, error)
}
