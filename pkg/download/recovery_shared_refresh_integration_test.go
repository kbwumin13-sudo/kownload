package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// Named Mail.ru/redirect hosts terminate only at this private TLS server.
// This is the real Core/Bolt/HTTP producer, not an injected owner or permit.
func TestSharedRecoveryHTTPMailruAndOriginalFallback(t *testing.T) {
	for _,public := range []bool{true,false} {
		name := "original-body-handoff"
		if public { name = "mailru-initial-and-expired-CDN" }
		t.Run(name,func(t *testing.T){
			payload := bytes.Repeat([]byte("same-task-refresh-bytes\n"),32768)
			var dispatch,oldRanges,newRanges,origin atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
				if r.URL.Path == "/api/v2/dispatcher" {
					dispatch.Add(1)
					fmt.Fprint(w,`{"body":{"weblink_get":[{"url":"https://cloclo1.cloud.mail.ru/public/synthetic/g/no"}]}}`)
					return
				}
				if strings.HasPrefix(r.URL.Path,"/public/synthetic/g/no/") {
					w.Header().Set("Location",fmt.Sprintf("https://cloclo2.datacloudmail.ru/public/get/synthetic/%d/file",dispatch.Load()))
					w.WriteHeader(302);return
				}
				if r.URL.Path == "/origin" {
					origin.Add(1)
					if r.Header.Get("Range") == "" {
						w.Header().Set("Location","https://cdn.example.test/old")
						w.WriteHeader(302);return
					}
				}
				old := r.URL.Path == "/old" || strings.Contains(r.URL.Path,"/synthetic/1/")
				begin,end := 0,len(payload)-1
				ranged := r.Header.Get("Range") != ""
				partial := false
				if ranged {
					if old {
						n := oldRanges.Add(1)
						if !public || n > 1 { w.WriteHeader(403);return }
						partial = true
					} else { newRanges.Add(1) }
					if _,err := fmt.Sscanf(r.Header.Get("Range"),"bytes=%d-%d",&begin,&end);err != nil || begin < 0 || end < begin || end >= len(payload) {
						t.Error("invalid actual Range");w.WriteHeader(400);return
					}
					if r.Header.Get("If-Range") != `"same-object"` { t.Error("refresh lost representation validator") }
					w.Header().Set("Content-Range",fmt.Sprintf("bytes %d-%d/%d",begin,end,len(payload)))
				}
				w.Header().Set("ETag",`"same-object"`)
				w.Header().Set("Accept-Ranges","bytes")
				w.Header().Set("Content-Length",fmt.Sprint(end-begin+1))
				if ranged { w.WriteHeader(206) }
				w.(http.Flusher).Flush()
				if !ranged { return } // Header metadata only; actual bytes use Range.
				if partial {
					_,_ = w.Write(payload[begin:min(begin+32768,end+1)])
					w.(http.Flusher).Flush()
					panic(http.ErrAbortHandler)
				}
				_,_ = w.Write(payload[begin:end+1])
			}))
			defer server.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
				if r.Method != http.MethodConnect { w.WriteHeader(400);return }
				up,err := net.Dial("tcp",server.Listener.Addr().String())
				if err != nil { w.WriteHeader(502);return }
				client,_,err := w.(http.Hijacker).Hijack()
				if err != nil { up.Close();return }
				_,_ = fmt.Fprint(client,"HTTP/1.1 200 Connection Established\r\n\r\n")
				go func(){ defer client.Close();defer up.Close();_,_ = io.Copy(up,client) }()
				go func(){ defer client.Close();defer up.Close();_,_ = io.Copy(client,up) }()
			}))
			defer proxy.Close()
			proxyURL,_ := url.Parse(proxy.URL)
			dir := t.TempDir()
			d := coverageRuntimeDownloader(t,true,dir)
			defer d.Close()
			link := "https://origin.example.test/origin"
			if public { link = "https://cloud.mail.ru/public/aaaa/bbbb" }
			request := operationRequest(link,dir)
			request.OperationID = "refresh-"+name
			request.Reqs[0].Req.Proxy = &base.RequestProxy{Mode:base.RequestProxyModeCustom,Scheme:"http",Host:proxyURL.Host}
			request.Reqs[0].Req.SkipVerifyCert = true
			request.Reqs[0].Opts.Extra = &httpModel.OptsExtra{Connections:1}
			created,err := d.CreateBatchOperation(context.Background(),request)
			if err != nil || len(created.TaskIDs)!=1 { t.Fatalf("new M: %+v %v",created,err) }
			id := created.TaskIDs[0]
			deadline := time.Now().Add(12*time.Second)
			for time.Now().Before(deadline) {
				task := d.GetTask(id)
				if task.Status == base.DownloadStatusError { t.Fatalf("actual refresh failed: %s",task.Status) }
				if task.Status == base.DownloadStatusDone { break }
				time.Sleep(5*time.Millisecond)
			}
			task := d.GetTask(id)
			if task.Status != base.DownloadStatusDone { t.Fatalf("refresh not terminal: %s",task.Status) }
			got,err := os.ReadFile(task.Meta.SingleFilepath())
			if err != nil || len(got)!=len(payload) || sha256.Sum256(got)!=sha256.Sum256(payload) { t.Fatalf("same Task bytes changed: len=%d err=%v",len(got),err) }
			record := coverageRuntimeRecord(t,d,id,func(r recoveryRecord)bool{return r.Version==4})
			wantRefresh,wantTransport := 1,0
			if public { wantRefresh,wantTransport = 2,1 }
			if record.RefreshUsed!=wantRefresh || record.TransportUsed!=wantTransport || record.TotalUsed!=wantRefresh+wantTransport || record.SourceRevision!=uint64(wantRefresh+1) {
				t.Fatalf("wrong shared debit: total=%d refresh=%d transport=%d source=%d",record.TotalUsed,record.RefreshUsed,record.TransportUsed,record.SourceRevision)
			}
			last := record.Actions[len(record.Actions)-1]
			if last.Kind!="refresh" || last.State!="finished" { t.Fatal("refresh parent not atomically finished") }
			if public && (dispatch.Load()!=2 || oldRanges.Load()!=2) { t.Fatalf("unbounded public plan: dispatcher=%d old=%d",dispatch.Load(),oldRanges.Load()) }
			if !public && (origin.Load()!=2 || newRanges.Load()!=1) { t.Fatalf("fallback body resent: origin=%d body=%d",origin.Load(),newRanges.Load()) }
			t.Logf("same Task completed %d bytes SHA=%x refresh=%d transport=%d",len(got),sha256.Sum256(got),record.RefreshUsed,record.TransportUsed)
		})
	}
}
