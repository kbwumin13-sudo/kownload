package download

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
)

// A failed download is logged to core.log through the Downloader's own
// failure path. The request carries a synthetic signed URL, user info, Cookie
// and Authorization; the log keeps the task ID and host but none of them.
func TestFailureLogRedactsRequestSecrets(t *testing.T) {
	secrets := []string{"SYNTH-USER", "SYNTH-PASS", "SYNTH-CREDENTIAL", "SYNTH-SIGNATURE", "SYNTH-TOKEN", "SYNTH-COOKIE", "SYNTH-BEARER", "private-report"}
	for _, failure := range []string{"refused", "reset"} {
		t.Run(failure, func(t *testing.T) {
			var host string
			switch failure {
			case "refused":
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				host = l.Addr().String()
				l.Close()
			case "reset":
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
				}))
				defer srv.Close()
				host = strings.TrimPrefix(srv.URL, "http://")
			}
			dir := t.TempDir()
			d := NewDownloader(&DownloaderConfig{ProductionMode: true, StorageDir: dir, RefreshInterval: 50,
				FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
			if err := d.Setup(); err != nil {
				t.Fatal(err)
			}
			defer d.Clear()
			signed := "http://SYNTH-USER:SYNTH-PASS@" + host + "/private-report.bin?X-Amz-Credential=SYNTH-CREDENTIAL&X-Amz-Signature=SYNTH-SIGNATURE&token=SYNTH-TOKEN"
			id, err := d.CreateDirect(&base.Request{URL: signed, Extra: &httpModel.ReqExtra{Header: map[string]string{
				"Cookie": "session=SYNTH-COOKIE", "Authorization": "Bearer SYNTH-BEARER",
			}}}, &base.Options{Path: t.TempDir(), Name: "report.bin"})
			if err != nil {
				t.Fatal(err)
			}
			waitStatus(t, d, id, base.DownloadStatusError, 20*time.Second)
			data, err := os.ReadFile(filepath.Join(dir, "logs", "core.log"))
			if err != nil {
				t.Fatal(err)
			}
			var line string
			for _, l := range strings.Split(string(data), "\n") {
				if strings.Contains(l, "task download failed, task id: "+id) {
					line = l
				}
			}
			if line == "" || !strings.Contains(line, host) {
				t.Fatalf("no failure line with the task ID and host in core.log:\n%s", data)
			}
			for _, s := range secrets {
				if strings.Contains(string(data), s) {
					t.Fatalf("core.log contains %s:\n%s", s, data)
				}
			}
		})
	}
}
