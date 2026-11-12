//go:build darwin && cgo

package bt

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestBTPathFreezeMetadataAndCrossFileIO(t *testing.T) {
	UseLoopbackClients(t, time.Second)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{Name: "tree", PieceLength: 8, Pieces: make([]byte, 20), Files: []metainfo.FileInfo{{Path: []string{"a"}, Length: 4}, {Path: []string{"empty"}, Length: 0}, {Path: []string{"b"}, Length: 4}}}
	raw, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: raw})
	if err != nil {
		t.Fatal(err)
	}
	f := buildFetcher().(*Fetcher)
	defer f.Close()
	if err := f.RequirePathFreeze("synthetic-task"); err != nil {
		t.Fatal(err)
	}
	err = f.ResolveTask(context.Background(), &base.Request{URL: "data:application/x-bittorrent;base64," + base64.StdEncoding.EncodeToString(mi)}, &base.Options{Path: root, SelectFiles: []int{0, 2}})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("metadata opened zero/native files")
	}
	if err := f.Start(); !errors.Is(err, transfer.ErrLayoutStorageHeld) {
		t.Fatalf("Start without F %v", err)
	}
	layout, sizes, err := f.PathFreezeLayout()
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Members) != 3 || layout.Members[1].Selected || sizes[1] != 0 {
		t.Fatal("incomplete frozen native tree")
	}
	policy, err := transfer.OpenVerifiedPolicyRoot("synthetic-root", root)
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	lease, err := policy.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	output, binding, err := transfer.CreateVerifiedLayoutStorage(lease, layout, sizes, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if err := output.Activate(binding); err != nil {
		t.Fatal(err)
	}
	if err := f.pathStorage.bind(output, sizes); err != nil {
		t.Fatal(err)
	}
	piece := pathPiece{store: f.pathStorage, index: 0, offset: 0, length: 8}
	if n, err := piece.WriteAt([]byte("abcdefgh"), 0); n != 8 || err != nil {
		t.Fatalf("piece write %d/%v", n, err)
	}
	data := make([]byte, 8)
	if n, err := piece.ReadAt(data, 0); n != 8 || err != nil || !bytes.Equal(data, []byte("abcdefgh")) {
		t.Fatalf("piece bytes %q %d/%v", data, n, err)
	}
	for name, want := range map[string]string{"a": "abcd", "b": "efgh", "empty": ""} {
		data, err := os.ReadFile(filepath.Join(root, "tree", name))
		if err != nil || string(data) != want {
			t.Fatalf("member %s: %q/%v", name, data, err)
		}
	}
}
