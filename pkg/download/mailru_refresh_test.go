package download

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
	fhttp "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

func TestMailruRefreshKeepsOriginalTask(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 2<<20)
	var dispatches, oldRanges atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/dispatcher" {
			dispatches.Add(1)
			fmt.Fprint(w, `{"body":{"weblink_get":[{"url":"https://cloclo1.cloud.mail.ru/public/synthetic/g/no"}]}}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/public/synthetic/g/no/") {
			w.Header().Set("Location", fmt.Sprintf("https://cloclo2.datacloudmail.ru/public/get/synthetic/%d/file", dispatches.Load()))
			w.WriteHeader(301)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/public/get/synthetic/") {
			w.WriteHeader(404)
			return
		}
		begin, end := 0, len(payload)-1
		limit := len(payload)
		ranged := r.Header.Get("Range") != ""
		if ranged {
			if strings.Contains(r.URL.Path, "/synthetic/1/") {
				if oldRanges.Add(1) > 1 {
					w.WriteHeader(403)
					return
				}
				limit = 64 << 10
			}
			fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &begin, &end)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", begin, end, len(payload)))
		}
		w.Header().Set("ETag", `"stable"`)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(end-begin+1))
		if ranged {
			w.WriteHeader(206)
		}
		for n := begin; n <= end; {
			if n-begin >= limit {
				panic(http.ErrAbortHandler)
			}
			last := min(n+4096, end+1, begin+limit)
			if _, err := w.Write(payload[n:last]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			n = last
			time.Sleep(time.Millisecond)
		}
	}))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(400)
			return
		}
		remote, err := net.Dial("tcp", target.Listener.Addr().String())
		if err != nil {
			w.WriteHeader(502)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			remote.Close()
			return
		}
		fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { defer client.Close(); defer remote.Close(); io.Copy(remote, client) }()
		go func() { defer client.Close(); defer remote.Close(); io.Copy(client, remote) }()
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	d := NewDownloader(&DownloaderConfig{Storage: NewMemStorage(), StorageDir: t.TempDir(), RefreshInterval: 50, FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dir := t.TempDir()
	id, err := d.CreateDirect(&base.Request{URL: "https://cloud.mail.ru/public/aaaa/bbbb", SkipVerifyCert: true, Proxy: &base.RequestProxy{Mode: base.RequestProxyModeCustom, Scheme: "http", Host: proxyURL.Host}}, &base.Options{Path: dir, Name: "synthetic.bin", Extra: &fhttp.OptsExtra{Connections: 1}})
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, id, base.DownloadStatusDone, 10*time.Second)
	if tasks := d.GetTasks(); len(tasks) != 1 || tasks[0].ID != id {
		t.Fatal("source refresh replaced or duplicated the task")
	}
	if dispatches.Load() != 2 {
		t.Fatalf("dispatcher requests=%d", dispatches.Load())
	}
	sameContent(t, filepath.Join(dir, "synthetic.bin"), payload)
}
