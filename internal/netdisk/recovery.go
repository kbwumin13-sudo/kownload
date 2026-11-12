package netdisk

import (
	"context"
	"io"
	"net/http"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

// Every hop, including a redirected request or one that waited for an API
// slot, checks the same parent lease immediately before network admission.
type recoveryAPITransport struct{ next http.RoundTripper }

func (transport recoveryAPITransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := checkRecoveryContext(request.Context()); err != nil {
		return nil, err
	}
	var scope *transfer.RecoveryWorkScope
	if lease := transfer.RecoveryLeaseFromContext(request.Context()); lease != nil {
		owner, ok := lease.Owner.(transfer.RecoveryParentController)
		if !ok {
			return nil, transfer.ErrRecoveryHeld
		}
		var err error
		scope, err = owner.EnterRecoveryWork(request.Context(), lease.Token, "netdisk/api")
		if err != nil {
			return nil, err
		}
		if err = scope.Check(); err != nil {
			scope.Leave()
			return nil, err
		}
		request = request.Clone(scope.Context())
	}
	response, err := transport.next.RoundTrip(request)
	if currentErr := checkRecoveryContext(request.Context()); currentErr != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		scope.Leave()
		return nil, currentErr
	}
	if err == nil && response != nil && response.Body != nil {
		response.Body = &recoveryAPIBody{ReadCloser: response.Body, ctx: request.Context(), scope: scope}
	} else {
		scope.Leave()
	}
	return response, err
}

type recoveryAPIBody struct {
	io.ReadCloser
	ctx context.Context
	scope *transfer.RecoveryWorkScope
}

func (body *recoveryAPIBody) Read(buffer []byte) (int, error) {
	if err := checkRecoveryContext(body.ctx); err != nil {
		body.Close()
		return 0, err
	}
	n, err := body.ReadCloser.Read(buffer)
	if currentErr := checkRecoveryContext(body.ctx); currentErr != nil {
		body.Close()
		return 0, currentErr
	}
	if err != nil {
		body.scope.Leave()
	}
	return n, err
}

func (body *recoveryAPIBody) Close() error {
	err := body.ReadCloser.Close()
	body.scope.Leave()
	return err
}
