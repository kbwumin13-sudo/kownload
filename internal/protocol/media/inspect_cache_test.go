package media

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

func fakeInspector(t *testing.T, body string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	program := filepath.Join(dir, "yt-dlp")
	count := filepath.Join(dir, "count")
	script := "#!/bin/sh\nprintf x >> '" + count + "'\n" + body + "\n"
	if err := os.WriteFile(program, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return program, count
}

func configuredInspector(program string) *controller.Controller {
	ctl := controller.NewController()
	ctl.GetConfig = func(v any) { v.(*model.Config).YtDlpPath = program }
	return ctl
}

func invocationCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func TestInspectCoalescesAndResolveReusesResult(t *testing.T) {
	program, count := fakeInspector(t, "sleep 0.2\nprintf '%s\\n' '{\"id\":\"one\",\"title\":\"one\",\"ext\":\"mp4\",\"format_id\":\"best\",\"formats\":[{\"format_id\":\"best\",\"ext\":\"mp4\",\"protocol\":\"m3u8_native\",\"language\":\"en\",\"fps\":30,\"tbr\":1500,\"manifest_url\":\"https://example.test/master.m3u8\"}]}'")
	ctl := configuredInspector(program)
	req := &base.Request{URL: "https://example.test/video?sig=one", Extra: &model.ReqExtra{SourceType: "page"}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := Inspect(context.Background(), req, ctl)
			if err != nil || !result.Supported || result.ID != "one" || result.Formats[0].Protocol != "m3u8_native" || result.Formats[0].TBR != 1500 || result.Formats[0].ManifestURL != "https://example.test/master.m3u8" {
				t.Errorf("inspect: %+v %v", result, err)
			}
		}()
	}
	wg.Wait()
	f := (&FetcherManager{}).Build().(*Fetcher)
	f.Setup(ctl)
	if err := f.Resolve(req, &base.Options{Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if got := invocationCount(t, count); got != 1 {
		t.Fatalf("duplicate extraction: %d", got)
	}
	req.URL = "https://example.test/video?sig=two"
	if _, err := Inspect(context.Background(), req, ctl); err != nil {
		t.Fatal(err)
	}
	if got := invocationCount(t, count); got != 2 {
		t.Fatalf("signed URL reused cache: %d", got)
	}
	inspections.Lock()
	for _, entry := range inspections.items {
		entry.expires = time.Now().Add(-time.Second)
	}
	inspections.Unlock()
	if _, err := Inspect(context.Background(), req, ctl); err != nil {
		t.Fatal(err)
	}
	if got := invocationCount(t, count); got != 3 {
		t.Fatalf("expired inspection reused: %d", got)
	}
	req.Extra = &model.ReqExtra{SourceType: "page", Format: "best"}
	if _, err := Inspect(context.Background(), req, ctl); err != nil {
		t.Fatal(err)
	}
	if got := invocationCount(t, count); got != 3 {
		t.Fatalf("single listed format was re-extracted: %d", got)
	}
}

func TestPopupInspectToSelectedResolve(t *testing.T) {
	program, count := fakeInspector(t, `printf '%s\n' '{"id":"clip","title":"clip","ext":"mp4","format_id":"v+a","formats":[{"format_id":"v","ext":"mp4","vcodec":"avc1","acodec":"none","filesize":100},{"format_id":"a","ext":"m4a","vcodec":"none","acodec":"mp4a","filesize":50},{"format_id":"w","ext":"webm","vcodec":"vp9","acodec":"none","filesize":80},{"format_id":"o","ext":"webm","vcodec":"none","acodec":"opus","filesize":30}]}'`)
	ctl := configuredInspector(program)
	url := "https://example.test/watch?sig=one"
	baseExtra := model.ReqExtra{SourceType: "page", Cookies: []model.Cookie{{Name: "session", Value: "one"}}}
	baseReq := &base.Request{URL: url, Extra: &baseExtra}
	result, err := Inspect(context.Background(), baseReq, ctl)
	if err != nil || !result.Supported || len(result.Formats) < 4 {
		t.Fatalf("popup inspect: %+v %v", result, err)
	}
	resolve := func(format, container string) (*Fetcher, error) {
		extra := baseExtra
		extra.Format, extra.Container = format, container
		f := (&FetcherManager{}).Build().(*Fetcher)
		f.Setup(ctl)
		err := f.Resolve(&base.Request{URL: url, Extra: &extra}, &base.Options{Path: t.TempDir()})
		return f, err
	}
	f, err := resolve("v+a", "mp4")
	if err != nil || f.Meta().Res.Size != 150 || !strings.HasSuffix(f.Meta().Res.Files[0].Name, ".mp4") {
		t.Fatalf("selected pair: %v %+v", err, f.Meta())
	}
	if got := invocationCount(t, count); got != 1 {
		t.Fatalf("popup selection re-extracted: %d", got)
	}
	if _, err := resolve("v+a", "webm"); err == nil {
		t.Fatal("incompatible webm accepted")
	}
	f, err = resolve("v+a", "mkv")
	if err != nil || !strings.HasSuffix(f.Meta().Res.Files[0].Name, ".mkv") {
		t.Fatalf("mkv selection: %v", err)
	}
	if got := invocationCount(t, count); got != 1 {
		t.Fatalf("container choice re-extracted: %d", got)
	}
	if _, err := resolve("bestvideo+bestaudio", ""); err != nil {
		t.Fatal(err)
	}
	if got := invocationCount(t, count); got != 2 {
		t.Fatalf("complex selector did not fall back: %d", got)
	}
	changedCookie := baseExtra
	changedCookie.Cookies = []model.Cookie{{Name: "session", Value: "two"}}
	if _, err := Inspect(context.Background(), &base.Request{URL: url, Extra: &changedCookie}, ctl); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), &base.Request{URL: url + "2", Extra: &baseExtra}, ctl); err != nil {
		t.Fatal(err)
	}
	if got := invocationCount(t, count); got != 4 {
		t.Fatalf("cookie or signed URL collided: %d", got)
	}
}

func TestConcurrentSelectionsDoNotMutateCachedMetadata(t *testing.T) {
	program, count := fakeInspector(t, `printf '%s\n' '{"title":"clip","ext":"mp4","format_id":"v+a","formats":[{"format_id":"v","ext":"mp4","vcodec":"avc1","acodec":"none","filesize":100},{"format_id":"a","ext":"m4a","vcodec":"none","acodec":"mp4a","filesize":50},{"format_id":"w","ext":"webm","vcodec":"vp9","acodec":"none","filesize":80},{"format_id":"o","ext":"webm","vcodec":"none","acodec":"opus","filesize":30}]}'`)
	ctl := configuredInspector(program)
	url := "https://example.test/concurrent?sig=unique"
	if _, err := Inspect(context.Background(), &base.Request{URL: url}, ctl); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, choice := range []struct {
		id, ext string
		size    int64
	}{{"v+a", "mp4", 150}, {"w+o", "webm", 110}} {
		choice := choice
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, _, err := inspectInfo(context.Background(), &base.Request{URL: url, Extra: &model.ReqExtra{Format: choice.id}}, ctl, configFor(ctl))
			if err != nil || info.FormatID != choice.id || info.Ext != choice.ext || info.Filesize != choice.size || len(info.RequestedFormats) != 2 {
				t.Errorf("selected info %s: %+v %v", choice.id, info, err)
			}
			if info != nil {
				info.Title = "mutated"
				info.Formats[0].FormatID = "mutated"
			}
		}()
	}
	wg.Wait()
	baseInfo, _, err := inspectInfo(context.Background(), &base.Request{URL: url}, ctl, configFor(ctl))
	if err != nil || baseInfo.Title != "clip" || baseInfo.Formats[0].FormatID != "v" || baseInfo.FormatID != "v+a" {
		t.Fatalf("cached base mutated: %+v %v", baseInfo, err)
	}
	if got := invocationCount(t, count); got != 1 {
		t.Fatalf("concurrent selection re-extracted: %d", got)
	}
}

func TestInspectCancellationAndFailures(t *testing.T) {
	program, count := fakeInspector(t, "sleep 0.2\nprintf '%s\\n' '{\"title\":\"one\",\"ext\":\"mp4\",\"format_id\":\"best\"}'")
	ctl := configuredInspector(program)
	req := &base.Request{URL: "https://example.test/cancel?unique=one"}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := Inspect(ctx, req, ctl); first <- err }()
	time.Sleep(30 * time.Millisecond)
	second := make(chan error, 1)
	go func() { _, err := Inspect(context.Background(), req, ctl); second <- err }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-first; err == nil {
		t.Fatal("canceled waiter succeeded")
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if got := invocationCount(t, count); got != 1 {
		t.Fatalf("cancel killed shared extraction: %d", got)
	}
	invalidateInspection(req, configFor(ctl))
	if _, err := Inspect(context.Background(), req, ctl); err != nil {
		t.Fatal(err)
	}
	if got := invocationCount(t, count); got != 2 {
		t.Fatalf("invalidated inspection reused: %d", got)
	}
	badProgram, badCount := fakeInspector(t, "echo 'HTTP Error 403: Forbidden https://example.test/?token=secret' >&2\nexit 1")
	badCtl := configuredInspector(badProgram)
	for i := 0; i < 2; i++ {
		_, err := Inspect(context.Background(), &base.Request{URL: "https://example.test/fail"}, badCtl)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("error leaked: %v", err)
		}
	}
	if got := invocationCount(t, badCount); got != 2 {
		t.Fatalf("failure cached: %d", got)
	}
}

func TestInspectRejectsListAndJsRuntimePath(t *testing.T) {
	for _, raw := range []string{
		`{"_type":"playlist","entries":[]}`, `{"_type":"multi_video"}`, `{"entries":[{"id":"one"}]}`,
	} {
		program, _ := fakeInspector(t, "printf '%s\\n' '"+raw+"'")
		result, err := Inspect(context.Background(), &base.Request{URL: "https://example.test/" + filepath.Base(program), Extra: &model.ReqExtra{SourceType: "page"}}, configuredInspector(program))
		if err != nil || result.Supported || !strings.Contains(result.Reason, "playlist") {
			t.Fatalf("list accepted: %+v %v", result, err)
		}
	}
	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	if err := os.WriteFile(node, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	req := &base.Request{URL: "https://example.test/media"}
	args, cleanup, err := requestArgs(req, &model.ReqExtra{}, nil, model.Config{FFmpegPath: "/absolute/ffmpeg", JsRuntimePath: node})
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if !strings.Contains(strings.Join(args, " "), "--js-runtimes node:"+node) {
		t.Fatal("valid node path omitted")
	}
	if err := os.Chmod(node, 0600); err != nil {
		t.Fatal(err)
	}
	args, cleanup, err = requestArgs(req, &model.ReqExtra{}, nil, model.Config{FFmpegPath: "/absolute/ffmpeg", JsRuntimePath: node})
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if strings.Contains(strings.Join(args, " "), "--js-runtimes") {
		t.Fatal("non-executable node path accepted")
	}
	if runtime.GOOS == "darwin" {
		ctl := controller.NewController()
		ctl.GetConfig = func(v any) { v.(*model.Config).FFmpegPath = "ffmpeg" }
		if got := configFor(ctl).FFmpegPath; got != "/opt/homebrew/bin/ffmpeg" {
			t.Fatalf("relative FFmpeg path: %q", got)
		}
	}
}

func TestPageExpectedMediaIDOnlyRejectsVerifiableMismatch(t *testing.T) {
	program, _ := fakeInspector(t, "printf '%s\\n' '{\"id\":\"other\",\"title\":\"clip\",\"ext\":\"mp4\",\"format_id\":\"best\",\"webpage_url\":\"https://www.youtube.com/watch?v=other\",\"url\":\"https://cdn.example.test/stream\"}'")
	ctl := configuredInspector(program)
	for _, tc := range []struct {
		url       string
		supported bool
	}{
		{"https://www.youtube.com/watch?v=expected", false},
		{"https://www.bilibili.com/video/BVexpected", true},
	} {
		result, err := Inspect(context.Background(), &base.Request{URL: tc.url, Extra: &model.ReqExtra{SourceType: "page", ExpectedMediaID: "expected"}}, ctl)
		if err != nil || result.Supported != tc.supported {
			t.Fatalf("%s: %+v %v", tc.url, result, err)
		}
		if result.MediaID != "other" || result.WebpageURL == "" || result.SourceURL == "" {
			t.Fatalf("missing inspection metadata: %+v", result)
		}
	}
}

func TestDownload403InvalidatesInspection(t *testing.T) {
	program, count := fakeInspector(t, "case \" $* \" in *--dump-single-json*) printf '%s\\n' '{\"title\":\"clip\",\"ext\":\"mp4\",\"format_id\":\"best\"}';; *) echo 'HTTP Error 403: Forbidden' >&2; exit 1;; esac")
	ctl := configuredInspector(program)
	req := &base.Request{URL: "https://example.test/expired?sig=one"}
	if _, err := Inspect(context.Background(), req, ctl); err != nil {
		t.Fatal(err)
	}
	req.Extra = &model.ReqExtra{Format: "bestvideo+bestaudio"}
	if _, err := Inspect(context.Background(), req, ctl); err != nil {
		t.Fatal(err)
	}
	f := (&FetcherManager{}).Build().(*Fetcher)
	f.Setup(ctl)
	if err := f.Resolve(req, &base.Options{Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if err := f.Wait(); err == nil || !strings.Contains(err.Error(), "登录态") {
		t.Fatalf("download error: %v", err)
	}
	if _, err := Inspect(context.Background(), req, ctl); err != nil {
		t.Fatal(err)
	}
	defaultReq := &base.Request{URL: req.URL}
	if _, err := Inspect(context.Background(), defaultReq, ctl); err != nil {
		t.Fatal(err)
	}
	if got := invocationCount(t, count); got != 5 {
		t.Fatalf("403 retained base or exact inspection: %d", got)
	}
}

func TestInspect403InvalidatesBase(t *testing.T) {
	program, count := fakeInspector(t, "case \" $* \" in *badselect*) echo 'HTTP Error 403: Forbidden' >&2; exit 1;; *) printf '%s\\n' '{\"title\":\"clip\",\"ext\":\"mp4\",\"format_id\":\"best\"}';; esac")
	ctl := configuredInspector(program)
	url := "https://example.test/inspect403?sig=one"
	if _, err := Inspect(context.Background(), &base.Request{URL: url}, ctl); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), &base.Request{URL: url, Extra: &model.ReqExtra{Format: "badselect"}}, ctl); err == nil {
		t.Fatal("expected 403")
	}
	if _, err := Inspect(context.Background(), &base.Request{URL: url}, ctl); err != nil {
		t.Fatal(err)
	}
	if got := invocationCount(t, count); got != 3 {
		t.Fatalf("inspect 403 retained default metadata: %d", got)
	}
}

func TestInspectionMetadataJSONNames(t *testing.T) {
	value, err := json.Marshal(model.Inspection{MediaID: "id", WebpageURL: "page", Formats: []model.Format{{SourceURL: "track", ManifestURL: "manifest", Protocol: "dash", Language: "zh"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"mediaId"`, `"webpageUrl"`, `"sourceUrl"`, `"manifestUrl"`, `"protocol"`, `"language"`} {
		if !strings.Contains(string(value), key) {
			t.Fatalf("missing JSON key %s: %s", key, value)
		}
	}
}
