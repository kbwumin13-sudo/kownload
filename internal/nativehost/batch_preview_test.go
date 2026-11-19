package nativehost

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GopeedLab/gopeed/internal/appidentity"
)

func TestBatchPreviewIsOnlyBoundDraftImport(t *testing.T) {
	identity := testIdentity(t)
	scope, err := identity.routeScope()
	if err != nil {
		t.Fatal(err)
	}
	version := IdentityProtocol
	signed := "HTTPS://Files.test/A%2f?Y=2&sig=X%2F&z=1"
	params := json.RawMessage(`{"version":1,"items":[{"url":"` + signed + `","context":"doc-1","header":{"Authorization":"fixture-A","Referer":"https://Files.test/page?a=2"}},{"url":"` + signed + `","context":"doc-2","header":{"Authorization":"fixture-B"}},{"url":"https://elsewhere.test/file"},{"url":""}]}`)
	req := Request{Method: "batchPreview", IdentityProtocol: &version,
		BridgeBinding: &BridgeBinding{Version: version, HostName: appidentity.HostName, RouteScope: scope},
		Params:        params}
	f := &fixture{status: 200, body: `{"code":0,"data":{"private":"fixture-token"}}`}
	d := Dispatcher{Client: f, Wakeup: f, Identity: identity}
	got := dispatchRequest(t, d, req)
	if got.Code != 0 || f.lastPath != "/batchPreview" || f.calls != 1 || f.wakes != 0 {
		t.Fatalf("unexpected import dispatch: %+v, path %s, calls %d, wakes %d", got, f.lastPath, f.calls, f.wakes)
	}
	var sent struct{ Items []batchPreviewItem }
	if json.Unmarshal(f.last.Params, &sent) != nil || len(sent.Items) != 4 ||
		*sent.Items[0].URL != signed || *sent.Items[1].URL != signed ||
		sent.Items[0].Header["Authorization"] != "fixture-A" ||
		sent.Items[1].Header["Authorization"] != "fixture-B" ||
		len(sent.Items[2].Header) != 0 {
		t.Fatal("signed URL or independent request context changed")
	}
	reply, _ := json.Marshal(got)
	if strings.Contains(string(reply), "fixture-token") || got.Submission != nil {
		t.Fatalf("unexpected import reply: %s", reply)
	}
	for name, change := range map[string]func(*Request){
		"unbound": func(r *Request) { r.IdentityProtocol, r.BridgeBinding = nil, nil },
		"legacy host": func(r *Request) {
			b := *r.BridgeBinding
			b.HostName = appidentity.LegacyHostName
			r.BridgeBinding = &b
		},
		"operation ID": func(r *Request) { r.OperationID = "other-op" },
		"metadata":     func(r *Request) { r.Meta = map[string]any{"source": "browser-capture"} },
		"foreign referrer": func(r *Request) {
			r.Params = json.RawMessage(strings.Replace(string(r.Params), "https://Files.test/page", "https://other.test/page", 1))
		},
		"unknown header": func(r *Request) {
			r.Params = json.RawMessage(strings.Replace(string(r.Params), "Authorization", "Proxy-Authorization", 1))
		},
		"missing context": func(r *Request) {
			r.Params = json.RawMessage(strings.Replace(string(r.Params), `"context":"doc-1",`, "", 1))
		},
		"large draft": func(r *Request) {
			r.Params = json.RawMessage(`{"version":1,"items":[{"url":"` + strings.Repeat("x", batchPreviewLimit) + `"}]}`)
		},
		"too many items": func(r *Request) {
			r.Params = json.RawMessage(`{"version":1,"items":[` + strings.Repeat(`{"url":""},`, 200) + `{"url":""}]}`)
		},
		"forged task payload": func(r *Request) {
			r.Params = json.RawMessage(`{"version":1,"items":[{"url":"https://files.test/a","opts":{"path":"/fixture"}}]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := req
			change(&changed)
			if got := dispatchRequest(t, d, changed); got.Code == 0 || f.calls != 1 || f.wakes != 0 {
				t.Fatalf("invalid request dispatched: %+v", got)
			}
		})
	}
}

func TestBatchPreviewAssistantOriginPolicy(t *testing.T) {
	identity := testIdentity(t)
	scope, err := identity.routeScope()
	if err != nil {
		t.Fatal(err)
	}
	version := IdentityProtocol
	req := Request{Method: "batchPreview", IdentityProtocol: &version,
		BridgeBinding: &BridgeBinding{Version: version, HostName: appidentity.HostName, RouteScope: scope},
		Params:        json.RawMessage(`{"version":1,"items":[{"url":"https://sample.test/"}]}`)}
	for _, origin := range appidentity.KownloadAssistantOrigins {
		t.Run("assistant "+origin, func(t *testing.T) {
			identity.CallerOrigin = origin
			f := &fixture{status: 200, body: `{"code":0}`}
			d := Dispatcher{Client: f, Wakeup: f, Identity: identity}
			if !AllowedOrigin(origin) {
				t.Fatal("assistant also needs ordinary native messaging")
			}
			if got := dispatchRequest(t, d, req); got.Code != 0 || f.calls != 1 || f.lastPath != "/batchPreview" || f.wakes != 0 {
				t.Fatalf("assistant import rejected: %+v", got)
			}
		})
	}
	for _, origin := range []string{
		"chrome-extension://mijpgljlfcapndmchhjffkpckknofcnd/",
		"chrome-extension://dkajnckekendchdleoaenoophcobooce/",
		"{c5d69a8f-2ed0-46a7-afa4-b3a00dc58088}",
	} {
		t.Run("compatibility "+origin, func(t *testing.T) {
			identity.CallerOrigin = origin
			f := &fixture{status: 200, body: `{"code":0}`}
			d := Dispatcher{Client: f, Wakeup: f, Identity: identity}
			if !AllowedOrigin(origin) {
				t.Fatal("compatibility extension lost ordinary native messaging")
			}
			if got := dispatchRequest(t, d, req); got.Error != "binding_required" || f.calls != 0 || f.wakes != 0 {
				t.Fatalf("compatibility extension gained an assistant import: %+v", got)
			}
		})
	}
}

func TestBatchPreviewAppRejectionDoesNotFallBack(t *testing.T) {
	identity := testIdentity(t)
	scope, _ := identity.routeScope()
	version := IdentityProtocol
	f := &fixture{status: 409, body: `{"code":1000,"msg":"fixture-private"}`}
	d := Dispatcher{Client: f, Wakeup: f, Identity: identity}
	result := dispatchRequest(t, d, Request{Method: "batchPreview", IdentityProtocol: &version,
		BridgeBinding: &BridgeBinding{Version: version, HostName: appidentity.HostName, RouteScope: scope},
		Params:        json.RawMessage(`{"version":1,"items":[{"url":"https://sample.test/"}]}`)})
	if result.Code == 0 || f.calls != 1 || f.wakes != 0 || f.lastPath != "/batchPreview" ||
		strings.Contains(result.Message, "fixture-private") {
		t.Fatalf("unexpected import rejection: %+v", result)
	}
}
