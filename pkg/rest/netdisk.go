package rest

import (
	"context"
	"errors"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	restmodel "github.com/GopeedLab/gopeed/pkg/rest/model"
	"github.com/gorilla/mux"
	"net/http"
	"time"
)

func netdiskResult(w http.ResponseWriter, data any, err error) {
	if err == nil {
		WriteJson(w, restmodel.NewOkResult(data))
		return
	}
	reason := "netdisk_error"
	var e *cloud.Error
	if errors.As(err, &e) {
		reason = e.Code
	}
	WriteJson(w, map[string]any{"code": 1, "msg": err.Error(), "data": map[string]string{"reasonCode": reason}})
}
func NetdiskProviders(w http.ResponseWriter, r *http.Request) {
	netdiskResult(w, Downloader.NetdiskService().Infos(), nil)
}
func NetdiskBrowserRequest(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Source     model.Source `json:"source"`
		InstanceID string       `json:"instanceId"`
	}
	if !ReadJson(r, w, &q) {
		return
	}
	v, e := Downloader.NetdiskService().RequestBrowser(q.Source, q.InstanceID)
	netdiskResult(w, v, e)
}
func NetdiskBrowserStatus(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if r.Method == http.MethodDelete {
		Downloader.NetdiskService().CancelBrowser(id)
		netdiskResult(w, nil, nil)
		return
	}
	v, e := Downloader.NetdiskService().BrowserStatus(id)
	netdiskResult(w, v, e)
}
func NetdiskClaim(w http.ResponseWriter, r *http.Request) {
	var q struct {
		RequestID  string `json:"requestId"`
		SourcePage string `json:"sourcePage"`
		InstanceID string `json:"instanceId"`
		Browser    string `json:"browser"`
		ContextID  string `json:"contextId"`
	}
	if !ReadJson(r, w, &q) {
		return
	}
	v, e := Downloader.NetdiskService().Claim(q.SourcePage, q.InstanceID, q.Browser, q.ContextID, q.RequestID)
	netdiskResult(w, v, e)
}
func NetdiskCapture(w http.ResponseWriter, r *http.Request) {
	var q struct {
		RequestID string        `json:"requestId"`
		Nonce     string        `json:"nonce"`
		Capture   model.Capture `json:"capture"`
	}
	if !ReadJson(r, w, &q) {
		return
	}
	v, e := Downloader.NetdiskService().CaptureContext(r.Context(), Downloader.NetdiskController(), q.RequestID, q.Nonce, q.Capture)
	netdiskResult(w, v, e)
}

type netdiskQuery struct {
	Source    model.Source       `json:"source"`
	SessionID string             `json:"sessionId"`
	Cursor    string             `json:"cursor"`
	Files     []model.File       `json:"files"`
	Opts      *base.Options      `json:"opts"`
	Proxy     *base.RequestProxy `json:"proxy"`
}

func NetdiskList(w http.ResponseWriter, r *http.Request) {
	var q netdiskQuery
	if !ReadJson(r, w, &q) {
		return
	}
	ctx, c := context.WithTimeout(r.Context(), 45*time.Second)
	defer c()
	v, e := Downloader.NetdiskService().List(ctx, Downloader.NetdiskController(), &base.Request{URL: q.Source.URL, Proxy: q.Proxy}, q.Source, q.SessionID, q.Cursor)
	netdiskResult(w, v, e)
}
func NetdiskPrepare(w http.ResponseWriter, r *http.Request) {
	var q netdiskQuery
	if !ReadJson(r, w, &q) {
		return
	}
	ctx, c := context.WithTimeout(r.Context(), 5*time.Minute)
	defer c()
	v, e := Downloader.NetdiskService().Prepare(ctx, Downloader.NetdiskController(), &base.Request{URL: q.Source.URL, Proxy: q.Proxy}, q.Source, q.SessionID, q.Files, q.Opts)
	netdiskResult(w, v, e)
}
func NetdiskTransfers(w http.ResponseWriter, r *http.Request) {
	netdiskResult(w, Downloader.NetdiskService().Transfers(), nil)
}
func NetdiskCleanup(w http.ResponseWriter, r *http.Request) {
	var q struct {
		BatchID string `json:"batchId"`
	}
	if !ReadJson(r, w, &q) {
		return
	}
	ctx, c := context.WithTimeout(r.Context(), 45*time.Second)
	defer c()
	e := Downloader.NetdiskService().Cleanup(ctx, Downloader.NetdiskController(), &base.Request{}, q.BatchID)
	netdiskResult(w, nil, e)
}
