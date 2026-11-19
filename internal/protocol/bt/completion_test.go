package bt

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func TestBTOversizedPartialIsNotReportedComplete(t *testing.T) {
	UseLoopbackClients(t, time.Second)
	const name = "clip.mp4"
	payload := bytes.Repeat([]byte("video payload"), 4096)
	seed := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(seed, payload, 0600); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(seed); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: infoBytes})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, name)
	stale := append(bytes.Clone(payload), []byte("stale file tail")...)
	if err := os.WriteFile(target+".part", stale, 0600); err != nil {
		t.Fatal(err)
	}

	f := buildFetcher().(*Fetcher)
	defer f.Close()
	if err := f.Resolve(&base.Request{
		URL: "data:application/x-bittorrent;base64," + base64.StdEncoding.EncodeToString(spec),
	}, &base.Options{Path: dir}); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.Wait() }()
	err = waitResolve(t, done)
	if err == nil || !strings.Contains(err.Error(), "大小与种子不符") {
		t.Fatalf("completion error = %v for %d bytes on disk, torrent length is %d", err, len(stale), len(payload))
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, stale) {
		t.Fatalf("existing bytes changed on size mismatch: %v", err)
	}
}

type hashGateStorage struct {
	storage.ClientImpl
	wrote   atomic.Bool
	hashing chan struct{}
	release chan struct{}
	enter   sync.Once
}

func (s *hashGateStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, hash metainfo.Hash) (storage.TorrentImpl, error) {
	output, err := s.ClientImpl.OpenTorrent(ctx, info, hash)
	if err != nil {
		return output, err
	}
	piece := output.Piece
	output.Piece = func(p metainfo.Piece) storage.PieceImpl {
		return &hashGatePiece{PieceImpl: piece(p), gate: s}
	}
	return output, nil
}

type hashGatePiece struct {
	storage.PieceImpl
	gate *hashGateStorage
}

func (p *hashGatePiece) WriteAt(b []byte, off int64) (int, error) {
	n, err := p.PieceImpl.WriteAt(b, off)
	if n > 0 {
		p.gate.wrote.Store(true)
	}
	return n, err
}

func (p *hashGatePiece) WriteTo(w io.Writer) (int64, error) {
	if p.gate.wrote.Load() {
		p.gate.enter.Do(func() { close(p.gate.hashing) })
		<-p.gate.release
	}
	return p.PieceImpl.(io.WriterTo).WriteTo(w)
}

func TestBTDoesNotFinishWhileLastPieceIsHashing(t *testing.T) {
	const name = "clip.mp4"
	seedDir, downloadDir := t.TempDir(), t.TempDir()
	seedPath := filepath.Join(seedDir, name)
	payload := bytes.Repeat([]byte("video-fixture-00"), 1024)
	if err := os.WriteFile(seedPath, payload, 0600); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(seedPath); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}
	seedCfg := torrent.NewDefaultClientConfig()
	LoopbackConfig(seedCfg, seedDir)
	seedCfg.Seed = true
	seeder, err := torrent.NewClient(seedCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer seeder.Close()
	seed, err := seeder.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	cfg := torrent.NewDefaultClientConfig()
	LoopbackConfig(cfg, downloadDir)
	leecher, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer leecher.Close()
	gate := &hashGateStorage{
		ClientImpl: storage.NewFile(downloadDir),
		hashing:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	release := sync.OnceFunc(func() { close(gate.release) })
	defer release()
	spec := torrent.TorrentSpecFromMetaInfo(mi)
	spec.Storage = gate
	tr, _, err := leecher.AddTorrentSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	f := buildFetcher().(*Fetcher)
	defer f.Close()
	f.torrent = tr
	f.meta.Opts = &base.Options{Path: downloadDir, SelectFiles: []int{0}}
	f.updateRes()
	f.torrentReady.Store(true)
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	seed.AddClientPeer(leecher)
	select {
	case <-gate.hashing:
	case <-time.After(5 * time.Second):
		t.Fatal("last piece did not enter hash verification")
	}
	if got := tr.Files()[0].BytesCompleted(); got != int64(len(payload)) {
		t.Fatalf("byte progress = %d, want %d before hash completes", got, len(payload))
	}
	if f.isDone() {
		t.Fatal("100% byte progress was treated as complete while the last piece was still hashing")
	}
	release()
	done := make(chan error, 1)
	go func() { done <- f.Wait() }()
	if err := waitResolve(t, done); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(downloadDir, name))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("download did not match the seed after verification: %v", err)
	}
}
