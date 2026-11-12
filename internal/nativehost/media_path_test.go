package nativehost

import (
	"encoding/json"
	"testing"

	"github.com/GopeedLab/gopeed/internal/appidentity"
)

func TestMediaPathConfirmationTransport(t *testing.T) {
	identity := testIdentity(t)
	scope, err := identity.routeScope()
	if err != nil {
		t.Fatal(err)
	}
	version := IdentityProtocol
	req := Request{Method: "mediaPathCreate", OperationID: "media-path-op",
		IdentityProtocol: &version,
		BridgeBinding: &BridgeBinding{Version: version, HostName: appidentity.HostName, RouteScope: scope},
		Meta: map[string]any{"sessionId": "session-1", "commandId": "media-2", "silent": true},
		Params: json.RawMessage(`{"req":{"url":"https://media.test/watch","protocol":"media","extra":{"format":"v+a","header":{"Referer":"https://media.test"}}},"opts":{"path":"/owned","name":"chosen.mp4"}}`)}
	f := &fixture{available: true, status: 200,
		body: `{"code":0,"operationId":"media-path-op","data":["same-task"],"submission":{"state":"created","taskIds":["same-task"]}}`}
	d := Dispatcher{Client: f, Wakeup: f, Identity: identity}
	if got := dispatchRequest(t, d, req); got.Code != 0 || f.calls != 1 || f.wakes != 1 ||
		f.lastPath != "/mediaPathCreate" || string(f.last.Params) != string(req.Params) {
		t.Fatalf("confirmed route lost original body: %+v calls=%d path=%s", got, f.calls, f.lastPath)
	}
	req.Method, req.Params, req.Meta = "mediaPathQuery", json.RawMessage(`{}`), nil
	if got := dispatchRequest(t, d, req); got.Code != 0 || f.calls != 2 || f.wakes != 1 || f.lastPath != "/mediaPathQuery" {
		t.Fatalf("query mutated or fell back: %+v", got)
	}
	req.IdentityProtocol, req.BridgeBinding = nil, nil
	if got := dispatchRequest(t, d, req); got.Code == 0 || f.calls != 2 || f.wakes != 1 {
		t.Fatalf("legacy host admitted typed path: %+v", got)
	}
	req.IdentityProtocol = &version
	req.BridgeBinding = &BridgeBinding{Version: version, HostName: appidentity.HostName, RouteScope: scope}
	req.Method, req.Params = "mediaPathCreate", json.RawMessage(`{"req":{},"opts":{}}`)
	if got := dispatchRequest(t, d, req); got.Code == 0 || f.calls != 2 || f.wakes != 1 {
		t.Fatalf("missing App confirmation admitted: %+v", got)
	}
}
