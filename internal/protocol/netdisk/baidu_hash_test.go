package netdisk

import (
	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"os"
	"path/filepath"
	"testing"
)

func TestBaiduObfuscatedMD5StillVerifiesContents(t *testing.T) {
	dir := t.TempDir()
	f := &Fetcher{meta: &fetcher.FetcherMeta{Opts: &base.Options{Path: dir}, Res: &base.Resource{Files: []*base.FileInfo{{Name: "empty"}}}}}
	if err := os.WriteFile(filepath.Join(dir, "empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	req := &model.ReqExtra{Provider: "baidu", File: model.File{Hash: "8e23f7635tb64136eddb0719602bc477"}}
	if err := f.verifyFile(req); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty"), []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.verifyFile(req); err == nil {
		t.Fatal("corrupted contents accepted")
	}
}
