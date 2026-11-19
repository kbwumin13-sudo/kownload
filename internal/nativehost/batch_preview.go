package nativehost

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/GopeedLab/gopeed/internal/appidentity"
	"github.com/GopeedLab/gopeed/internal/transfer"
)

const batchPreviewLimit = 64 * 1024
const kownloadAssistantOrigin = "chrome-extension://jcalkpjhhaachliihlkojhmafniepnoj/"

type batchPreviewItem struct {
	URL     *string           `json:"url"`
	Header  map[string]string `json:"header,omitempty"`
	Context string            `json:"context,omitempty"`
}

func parseBatchPreview(params json.RawMessage) ([]byte, bool) {
	var draft struct {
		Version int                `json:"version"`
		Items   []batchPreviewItem `json:"items"`
	}
	if len(params) > batchPreviewLimit || !json.Valid(params) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(params))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&draft) != nil || draft.Version != 1 || len(draft.Items) == 0 || len(draft.Items) > 200 {
		return nil, false
	}
	for _, item := range draft.Items {
		if item.URL == nil || len(*item.URL) > 8192 || (item.Context == "") != (len(item.Header) == 0) {
			return nil, false
		}
		if len(item.Header) == 0 {
			continue
		}
		origin, err := previewCredentialOrigin(*item.URL)
		if err != nil || !operationID.MatchString(item.Context) || len(item.Header) > 9 {
			return nil, false
		}
		names := make(map[string]bool, len(item.Header))
		for name, value := range item.Header {
			name = strings.ToLower(name)
			if names[name] || value == "" || len(value) > 8192 ||
				strings.ContainsFunc(value, func(r rune) bool { return r < 32 || r == 127 }) {
				return nil, false
			}
			names[name] = true
			switch name {
			case "authorization", "x-api-key", "x-auth-token", "x-playback-token", "cookie", "user-agent", "accept-encoding":
			case "referer", "origin":
				if from, err := previewCredentialOrigin(value); err != nil || from != origin {
					return nil, false
				}
			default:
				return nil, false
			}
		}
	}
	clean, err := json.Marshal(draft)
	return clean, err == nil && len(clean) <= batchPreviewLimit
}

func previewCredentialOrigin(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil {
		return "", transfer.ErrCredentialContext
	}
	return transfer.CredentialOrigin(raw)
}

func (d Dispatcher) batchPreview(ctx context.Context, req Request) Response {
	if req.IdentityProtocol == nil || req.BridgeBinding == nil ||
		req.BridgeBinding.HostName != appidentity.HostName || d.Identity == nil ||
		d.Identity.CallerOrigin != kownloadAssistantOrigin {
		return failure("", "binding_required", nil, nil)
	}
	if req.OperationID != "" || req.Meta != nil {
		return failure("", "invalid_request", nil, nil)
	}
	params, valid := parseBatchPreview(req.Params)
	if !valid {
		return failure("", "invalid_request", nil, nil)
	}
	req.Params = params
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, reply, err := d.Client.Post(requestCtx, "/batchPreview", req)
	if err != nil {
		return failure("", "request_failed", nil, nil)
	}
	result := interpret(req, status, reply, false)
	// The browser only needs a fixed acknowledgement.
	if result.Code == 0 {
		return Response{Data: map[string]int{"version": 1}}
	}
	return failure("", "request_failed", nil, nil)
}
