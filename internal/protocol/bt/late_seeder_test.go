package bt_test

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/protocol/bt"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/GopeedLab/gopeed/pkg/download"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

const resolveDeadline = 100 * time.Millisecond

func taskStatus(d *download.Downloader, id string) base.Status {
	if task := d.GetTask(id); task != nil {
		return task.Status
	}
	return ""
}

func waitStatus(t *testing.T, d *download.Downloader, id string, want base.Status, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for taskStatus(d, id) != want {
		if time.Now().After(deadline) {
			t.Fatalf("task %s is %q, want %q", id, taskStatus(d, id), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitTorrents(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for bt.SharedTorrents() != want {
		if time.Now().After(deadline) {
			t.Fatalf("shared client holds %d torrents, want %d", bt.SharedTorrents(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func promptly(t *testing.T, what string, fn func() error) {
	t.Helper()
	start := time.Now()
	if err := fn(); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("%s took %s", what, elapsed)
	}
}

func createMagnet(t *testing.T, d *download.Downloader, magnet, dir string) string {
	t.Helper()
	var id string
	promptly(t, "create", func() (err error) {
		id, err = d.CreateDirect(&base.Request{URL: magnet}, &base.Options{Path: dir})
		return
	})
	waitStatus(t, d, id, base.DownloadStatusRunning, time.Second)
	return id
}

// stillWaiting checks that a task outlives the interactive deadline without
// failing or dropping its torrent.
func stillWaiting(t *testing.T, d *download.Downloader, id string, torrents int) {
	t.Helper()
	time.Sleep(5 * resolveDeadline)
	if got := taskStatus(d, id); got != base.DownloadStatusRunning {
		t.Fatalf("task is %q after the resolve deadline, want running", got)
	}
	if got := bt.SharedTorrents(); got != torrents {
		t.Fatalf("shared client holds %d torrents, want %d", got, torrents)
	}
}

func newSeeder(t *testing.T, dir string, mi *metainfo.MetaInfo) *torrent.Torrent {
	t.Helper()
	cfg := torrent.NewDefaultClientConfig()
	bt.LoopbackConfig(cfg, dir)
	cfg.Seed = true
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	tr, err := cl.AddTorrent(mi)
	if err != nil {
		t.Fatal(err)
	}
	<-tr.GotInfo()
	if err := tr.VerifyData(); err != nil {
		t.Fatal(err)
	}
	if missing := tr.BytesMissing(); missing != 0 {
		t.Fatalf("seeder misses %d bytes", missing)
	}
	return tr
}

// A magnet added for direct download keeps waiting for a seeder that appears
// only after the interactive resolve deadline; Delete, Pause and Continue act
// at once meanwhile, and the file downloads intact once the seeder connects.
func TestMagnetTaskDownloadsFromLateSeeder(t *testing.T) {
	bt.UseLoopbackClients(t, resolveDeadline)

	seedDir := t.TempDir()
	const name = "late-seed.bin"
	payload := make([]byte, 3<<20+12345)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, name), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 256 << 10}
	if err := info.BuildFromFilePath(filepath.Join(seedDir, name)); err != nil {
		t.Fatal(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}
	magnet := mi.Magnet(nil, &info).String()

	d := download.NewDownloader(&download.DownloaderConfig{
		StorageDir:    t.TempDir(),
		FetchManagers: []fetcher.FetcherManager{new(bt.FetcherManager)},
	})
	if err := d.Setup(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	dir := t.TempDir()

	// Nobody seeds this magnet; Delete still ends its wait at once.
	unseeded := createMagnet(t, d, "magnet:?xt=urn:btih:"+strings.Repeat("ab", 20), dir)
	stillWaiting(t, d, unseeded, 1)
	promptly(t, "delete", func() error {
		return d.Delete(&download.TaskFilter{IDs: []string{unseeded}}, false)
	})
	if d.GetTask(unseeded) != nil {
		t.Fatal("deleted task is still listed")
	}
	waitTorrents(t, 0)

	id := createMagnet(t, d, magnet, dir)
	stillWaiting(t, d, id, 1)

	// Pause ends the wait at once and leaves the task paused, not failed.
	promptly(t, "pause", func() error {
		return d.Pause(&download.TaskFilter{IDs: []string{id}})
	})
	waitStatus(t, d, id, base.DownloadStatusPause, time.Second)
	waitTorrents(t, 0)
	time.Sleep(3 * resolveDeadline)
	if got := taskStatus(d, id); got != base.DownloadStatusPause {
		t.Fatalf("paused task became %q", got)
	}

	promptly(t, "continue", func() error {
		return d.Continue(&download.TaskFilter{IDs: []string{id}})
	})
	waitStatus(t, d, id, base.DownloadStatusRunning, time.Second)
	stillWaiting(t, d, id, 1)

	// The seeder appears long after the deadline and reaches us, as a peer
	// found later through DHT or a tracker would.
	seed := newSeeder(t, seedDir, mi)
	seed.AddClientPeer(bt.SharedClient())
	waitStatus(t, d, id, base.DownloadStatusDone, time.Minute)
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("downloaded file differs from the seeded payload")
	}

	// Let the finished task seed past a counter update while the downloader
	// keeps reading and saving it, then Delete stops seeding and releases the
	// torrent.
	time.Sleep(1500 * time.Millisecond)
	if got := taskStatus(d, id); got != base.DownloadStatusDone {
		t.Fatalf("seeding task became %q", got)
	}
	promptly(t, "delete finished", func() error {
		return d.Delete(&download.TaskFilter{IDs: []string{id}}, false)
	})
	waitTorrents(t, 0)
}
