package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

// Exercise the actual configured yt-dlp CLI parser through a non-Python
// entrypoint, including explicit config loading despite --ignore-config.
func TestStandalonePrivateProxyAndBrowserContext(t *testing.T) {
	cfg := configFor(nil)
	if !executablePath(cfg.YtDlpPath) {
		t.Skip("configured yt-dlp unavailable")
	}
	program, _ := fakeInspector(t, "exec '"+strings.ReplaceAll(cfg.YtDlpPath, "'", "'\"'\"'")+"' \"$@\"")
	cfg.YtDlpPath = program
	for _, browser := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-context", true: "browser"}[browser], func(t *testing.T) {
			var mu sync.Mutex
			var seen []http.Header
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Header.Clone())
				mu.Unlock()
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Length", "4")
				if r.Method != "HEAD" {
					_, _ = w.Write([]byte("clip"))
				}
			}))
			defer proxy.Close()
			proxyURL, _ := url.Parse(proxy.URL)
			proxyURL.User = url.UserPassword("review-user", "private'quoted#password")
			ctl := controller.NewController()
			ctl.GetProxy = func(*base.RequestProxy) func(*http.Request) (*url.URL, error) {
				return func(*http.Request) (*url.URL, error) { return proxyURL, nil }
			}
			extra := &model.ReqExtra{}
			if browser {
				extra.SourcePage = "http://media.example.test/watch?x='quoted'"
				extra.Header = map[string]string{"User-Agent": "Gopeed review's agent"}
				extra.Cookies = []model.Cookie{{Name: "review", Value: "cookie-value", Domain: "media.example.test", HostOnly: true}}
			}
			req := &base.Request{URL: "http://media.example.test/video.mp4", Extra: extra}
			args, cleanup, err := requestArgs(req, extra, ctl, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			args = append(args, "--dump-single-json", "--skip-download", req.URL)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd, clear, err := mediaCommand(ctx, cfg, req, extra, args)
			if err != nil {
				t.Fatal(err)
			}
			defer clear()
			argv := strings.Join(cmd.Args, " ")
			for _, secret := range []string{"review-user", "private", "cookie-value", "Gopeed review's agent"} {
				if strings.Contains(argv, secret) {
					t.Fatal("private context leaked in argv")
				}
			}
			configPath := cmd.Args[3]
			stat, err := os.Stat(configPath)
			if err != nil || stat.Mode().Perm() != 0600 {
				t.Fatal("CLI config not private", err)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("real standalone CLI: %v %s", err, out)
			}
			var info videoInfo
			if json.Unmarshal(out, &info) != nil || info.Ext != "mp4" {
				t.Fatalf("CLI extraction failed: %s", out)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) == 0 {
				t.Fatal("proxy was not used")
			}
			for _, headers := range seen {
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte("review-user:private'quoted#password"))
				if headers.Get("Proxy-Authorization") != want {
					t.Fatal("proxy credentials did not survive config parsing")
				}
				if browser && (headers.Get("Referer") != extra.SourcePage || headers.Get("User-Agent") != extra.Header["User-Agent"] || headers.Get("Cookie") != "review=cookie-value") {
					t.Fatalf("browser context lost: %v", headers)
				}
			}
			clear()
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Fatal("CLI config retained")
			}
		})
	}
}

func TestStandaloneRejectsStrictAuthBeforeExecution(t *testing.T) {
	program, count := fakeInspector(t, "exit 0")
	for _, extra := range []*model.ReqExtra{
		{Header: map[string]string{"X-Token": "secret"}},
		{ScopedHeaders: []model.HeaderScope{{Origin: "https://example.test", Headers: map[string]string{"Authorization": "Bearer secret"}}}},
	} {
		_, _, err := mediaCommand(context.Background(), model.Config{YtDlpPath: program}, &base.Request{URL: "https://example.test/v"}, extra, nil)
		if err == nil || !strings.Contains(err.Error(), "standalone yt-dlp does not support strict") {
			t.Fatalf("unsupported behavior: %v", err)
		}
	}
	if invocationCount(t, count) != 0 {
		t.Fatal("unsupported auth executed")
	}
}

func TestPatchHeaderReplacementAndOriginBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		extra     any
		wantToken string
	}{
		{"signed-refresh", "https://old.test/v?sig=new", nil, "old-secret"},
		{"cookie-refresh", "https://old.test/v?sig=new", &model.ReqExtra{Cookies: []model.Cookie{{Name: "session", Value: "new-cookie"}}}, "old-secret"},
		{"cross-origin", "https://cdn.test/v", nil, ""},
		{"replace", "", map[string]any{"header": map[string]string{"User-Agent": "new-agent"}}, ""},
		{"clear", "", map[string]any{"header": map[string]string{}}, ""},
		{"null", "", map[string]any{"header": nil}, ""},
		{"explicit-new", "https://cdn.test/v", map[string]any{"header": map[string]string{"X-Token": "new-secret"}}, "new-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &Fetcher{identity: "resume", workPrefix: "partial", downloaded: 123, meta: &fetcher.FetcherMeta{Req: &base.Request{URL: "https://old.test/v?sig=old", Extra: &model.ReqExtra{Header: map[string]string{"X-Token": "old-secret"}, Cookies: []model.Cookie{{Name: "session", Value: "old-cookie", HostOnly: true}}, ScopedHeaders: []model.HeaderScope{{Origin: "https://old.test", Headers: map[string]string{"Authorization": "scoped-secret"}}}}}, Opts: &base.Options{}}}
			if err := f.Patch(&base.Request{URL: tc.url, Extra: tc.extra}, nil); err != nil {
				t.Fatal(err)
			}
			extra, _ := extraFor(f.meta.Req)
			if extra.Header["X-Token"] != tc.wantToken {
				t.Fatal("stale header rebound or lost")
			}
			scopes, err := contextScopes(f.meta.Req, extra)
			if err != nil {
				t.Fatal(err)
			}
			if len(scopes) != 3 || scopes[2].Origin != "https://old.test" || scopes[2].Headers["Authorization"] != "scoped-secret" {
				t.Fatal("explicit scope changed")
			}
			if tc.name == "cross-origin" && extra.Cookies[0].Domain != "old.test" {
				t.Fatal("implicit cookie rebound")
			}
			if f.identity != "resume" || f.workPrefix != "partial" || f.downloaded != 123 {
				t.Fatal("patch reset resume state prematurely")
			}
		})
	}
}

func TestSubtitleResolveRejectsUnavailableAndUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name, metadata string
		automatic      bool
		want           string
	}{
		{"absent", `{}`, false, "unavailable"},
		{"automatic-disabled", `{"automatic_captions":{"en":[{"ext":"vtt"}]}}`, false, "unavailable"},
		{"unsupported", `{"subtitles":{"en":[{"ext":"custom"}]}}`, false, "unsupported format"},
		{"automatic-enabled", `{"automatic_captions":{"en":[{"ext":"vtt"}]}}`, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var info map[string]any
			_ = json.Unmarshal([]byte(tc.metadata), &info)
			info["id"] = "one"
			info["title"] = "video"
			info["ext"] = "mp4"
			info["formats"] = []any{map[string]any{"format_id": "a", "ext": "mp4"}}
			encoded, _ := json.Marshal(info)
			program, _ := fakeInspector(t, "printf '%s\\n' '"+string(encoded)+"'")
			f := &Fetcher{}
			f.Setup(configuredInspector(program))
			err := f.Resolve(&base.Request{URL: "https://example.test/v", Extra: &model.ReqExtra{SubtitleLanguages: []string{"en"}, WriteAutomaticSubtitles: tc.automatic}}, &base.Options{Path: t.TempDir()})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("resolve: %v", err)
			}
		})
	}
}

func TestExitZeroDoesNotPublishMissingSelectedSubtitles(t *testing.T) {
	for _, mode := range []string{"missing", "empty", "one-of-two"} {
		t.Run(mode, func(t *testing.T) {
			body := `for arg do
 if [ "$arg" = --dump-single-json ]; then
 printf '%s\n' '{"id":"one","title":"video","ext":"mp4","formats":[{"format_id":"a","ext":"mp4"}],"subtitles":{"en":[{"ext":"vtt"}],"fr":[{"ext":"vtt"}]}}'
 exit 0
 fi
 done
 while [ "$#" -gt 0 ]; do
 if [ "$1" = -o ]; then shift; template=$1; fi
 shift
 done
 prefix=${template%.\%\(ext\)s}
 printf movie > "$prefix.mp4"
 `
			if mode == "empty" {
				body += "\n: > \"$prefix.en.vtt\"\n"
			}
			if mode == "one-of-two" {
				body += "\nprintf subtitle > \"$prefix.en.vtt\"\n"
			}
			body += "\nprintf 'media-final:\"%s.mp4\"\\n' \"$prefix\"\n"
			program, _ := fakeInspector(t, body)
			f := &Fetcher{}
			f.Setup(configuredInspector(program))
			langs := []string{"en"}
			if mode == "one-of-two" {
				langs = append(langs, "fr")
			}
			if err := f.Resolve(&base.Request{URL: "https://example.test/v", Extra: &model.ReqExtra{SubtitleLanguages: langs}}, &base.Options{Path: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			if err := f.Start(); err != nil {
				t.Fatal(err)
			}
			if err := f.Wait(); err == nil || !strings.Contains(err.Error(), "missing or empty") || f.MediaPhase() == "done" {
				t.Fatalf("subtitle gate failed: %v", err)
			}
			if _, err := os.Stat(f.Meta().SingleFilepath()); !os.IsNotExist(err) {
				t.Fatal("incomplete output published")
			}
			sidecars, _ := filepath.Glob(filepath.Join(f.Meta().Opts.Path, "video.en.vtt"))
			if len(sidecars) != 0 {
				t.Fatal("partial sidecar published")
			}
		})
	}
}
