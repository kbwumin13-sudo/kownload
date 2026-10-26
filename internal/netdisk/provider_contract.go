package netdisk

import (
	"context"
	"fmt"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"time"
)

type Session struct {
	ID        string
	Capture   model.Capture
	CreatedAt time.Time
}

type Download struct {
	URL       string
	Header    map[string]string
	Cookies   []model.Cookie
	ExpiresAt time.Time
	File      model.File
}

type Provider interface {
	Info() model.ProviderInfo
	List(context.Context, *APIClient, *Session, model.Source, string) (*model.Listing, error)
	Resolve(context.Context, *APIClient, *Session, model.Source, model.File) (*Download, error)
	Transfer(context.Context, *APIClient, *Session, model.Source, []model.File, string) (*model.TransferReceipt, error)
	Cleanup(context.Context, *APIClient, *Session, *model.TransferReceipt) error
}

type Error struct {
	Code    string
	Message string
	// cause is a typed condition callers may act on, such as a deferral.
	cause error
}

func (e *Error) Error() string        { return e.Message }
func (e *Error) Unwrap() error        { return e.cause }
func Fail(code, message string) error { return &Error{Code: code, Message: message} }
func LoginRequired(name string) error {
	return Fail("login_required", fmt.Sprintf("请在浏览器登录%s，再同步登录状态", name))
}
func TransferRequired() error { return Fail("transfer_required", "此分享需要先转存到网盘") }
