package rest

import (
	"bytes"
	"encoding/json"
	"github.com/GopeedLab/gopeed/pkg/download"
	"net/http/httptest"
	"testing"
)

func TestNetdiskBrowserRESTContract(t *testing.T) {
	previous := Downloader
	Downloader = download.NewDownloader((&download.DownloaderConfig{}).Init())
	if err := Downloader.Setup(); err != nil {
		t.Fatal(err)
	}
	defer func() { Downloader.Close(); Downloader = previous }()
	call := func(fn func(*httptest.ResponseRecorder), out any) {
		t.Helper()
		w := httptest.NewRecorder()
		fn(w)
		if e := json.Unmarshal(w.Body.Bytes(), out); e != nil {
			t.Fatal(e)
		}
	}
	var providers struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	call(func(w *httptest.ResponseRecorder) {
		NetdiskProviders(w, httptest.NewRequest("GET", "/api/v1/netdisk/providers", nil))
	}, &providers)
	if providers.Code != 0 || len(providers.Data) != 9 {
		t.Fatalf("nine providers missing: %+v", providers)
	}
	source := map[string]any{"provider": "quark", "url": "https://pan.quark.cn/s/contract-share", "shareId": "contract-share"}
	b, _ := json.Marshal(map[string]any{"source": source})
	var request struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	call(func(w *httptest.ResponseRecorder) {
		NetdiskBrowserRequest(w, httptest.NewRequest("POST", "/api/v1/netdisk/browser/request", bytes.NewReader(b)))
	}, &request)
	if request.Code != 0 || request.Data["id"] == nil {
		t.Fatal(request)
	}
	id := request.Data["id"].(string)
	b, _ = json.Marshal(map[string]any{"requestId": id, "sourcePage": "https://pan.quark.cn/s/contract-share?gopeed_request=" + id, "instanceId": "browser-fixture", "contextId": "tab-fixture"})
	var claim struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	call(func(w *httptest.ResponseRecorder) {
		NetdiskClaim(w, httptest.NewRequest("POST", "/api/v1/netdisk/browser/claim", bytes.NewReader(b)))
	}, &claim)
	if claim.Code != 0 || claim.Data["nonce"] == nil {
		t.Fatal(claim)
	}
	b, _ = json.Marshal(map[string]any{"requestId": id, "nonce": claim.Data["nonce"], "capture": map[string]any{"provider": "quark", "sourcePage": "https://pan.quark.cn/s/contract-share", "instanceId": "browser-fixture", "contextId": "tab-fixture", "accountId": "fixture-account", "files": []any{}, "tokens": map[string]string{"token": "secret-contract-value"}}})
	var capture struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	w := httptest.NewRecorder()
	NetdiskCapture(w, httptest.NewRequest("POST", "/api/v1/netdisk/browser/capture", bytes.NewReader(b)))
	if e := json.Unmarshal(w.Body.Bytes(), &capture); e != nil {
		t.Fatal(e)
	}
	if capture.Code != 0 || capture.Data["sessionId"] == nil || bytes.Contains(w.Body.Bytes(), []byte("secret-contract-value")) {
		t.Fatal("capture contract or privacy failed")
	}
	Downloader.NetdiskService().CancelBrowser(id)
}
