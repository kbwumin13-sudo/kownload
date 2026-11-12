package media

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

func TestMediaPathFreezeRequiresBinding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "untouched")
	f := &Fetcher{done: make(chan error, 1)}
	if err := f.RequirePathFreeze("owner"); err != nil {
		t.Fatal(err)
	}
	f.meta = &fetcher.FetcherMeta{Req: &base.Request{URL: "http://127.0.0.1:1/test.wav", Protocol: "media"}, Opts: &base.Options{Path: root}, Res: &base.Resource{Files: []*base.FileInfo{{Name: "test.wav"}}}}
	var err error
	f.pathLayout, err = resolvedMediaLayout("owner", filepath.Join(root, "test.wav"), &model.ReqExtra{}, &videoInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-f.done:
		if !errors.Is(err, transfer.ErrRecoveryHeld) {
			t.Fatalf("unbound start: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = f.Pause()
		t.Fatal("unbound start did not terminate")
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("unbound start wrote target: %v", err)
	}
	if err = f.BindPathFreezeExecution(nil); !errors.Is(err, transfer.ErrRecoveryHeld) {
		t.Fatal(err)
	}
}

func TestMediaPathFreezeExactSidecars(t *testing.T) {
	extra := &model.ReqExtra{SubtitleLanguages: []string{"en", "zh-Hans"}, WriteAutomaticSubtitles: true}
	info := &videoInfo{Subtitles: map[string][]subtitleFormat{"en": {{Ext: "vtt"}, {Ext: "srt"}}}, AutomaticCaptions: map[string][]subtitleFormat{"zh-Hans": {{Ext: "vtt"}}}}
	expected, err := resolvedMediaLayout("owner", filepath.Join(t.TempDir(), "exact.mp4"), extra, info)
	if err != nil {
		t.Fatal(err)
	}
	if got := expected.Layout.Members; len(got) != 3 || got[0].RelativePath != "exact.mp4" || got[1].RelativePath != "exact.en.srt" || got[2].RelativePath != "exact.zh-Hans.vtt" {
		t.Fatalf("layout: %+v", got)
	}
	f := &Fetcher{pathOwner: "owner", pathLayout: expected}
	copy, work, err := f.MediaOutputLayout()
	if err != nil {
		t.Fatal(err)
	}
	copy.Members[1].RelativePath = "changed"
	again, workAgain, err := f.MediaOutputLayout()
	if err != nil || again.Members[1].RelativePath != "exact.en.srt" || work != workAgain || filepath.Base(work) != work {
		t.Fatal("layout alias or unstable workspace")
	}
	extra.SubtitleLanguages = append(extra.SubtitleLanguages, "en")
	if _, err = resolvedMediaLayout("owner", filepath.Join(t.TempDir(), "exact.mp4"), extra, info); err == nil {
		t.Fatal("duplicate subtitle accepted")
	}
	// F may select another root, but Core must apply it first and preserve the
	// metadata-resolved relative final set. No filesystem IO is issued here.
	f.meta = &fetcher.FetcherMeta{Opts: &base.Options{Path: again.ParentRoot}}
	oldRoot := again.ParentRoot
	again.ParentRoot = filepath.Join(oldRoot, "rule-root")
	execution, err := transfer.IssuePathFreezeExecution("owner", again, filepath.Join(again.ParentRoot, work), func() error { return nil }, func(int) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = f.BindPathFreezeExecution(execution); !errors.Is(err, transfer.ErrRecoveryHeld) || f.pathLayout.Layout.ParentRoot != oldRoot {
		t.Fatal("binding changed root before Core applied F", err)
	}
	f.meta.Opts.Path = again.ParentRoot
	if err = f.BindPathFreezeExecution(execution); err != nil {
		t.Fatal("F root binding rejected", err)
	}
	bound, boundWork, err := f.MediaOutputLayout()
	if err != nil || bound.ParentRoot != again.ParentRoot || boundWork != work || bound.Members[1] != again.Members[1] {
		t.Fatal("F root binding changed relative layout", err)
	}
	again.Members[1].RelativePath = "other.en.srt"
	changed, err := transfer.IssuePathFreezeExecution("owner", again, filepath.Join(again.ParentRoot, work), func() error { return nil }, func(int) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = f.BindPathFreezeExecution(changed); !errors.Is(err, transfer.ErrRecoveryHeld) || f.pathExecution != execution {
		t.Fatal("root binding also rewrote final member", err)
	}
}
