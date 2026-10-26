// Command btseed seeds a deterministic file on loopback so a BT download can
// be checked end to end without public trackers or peers. It prints a magnet
// link whose x.pe parameter points at this seeder.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"
)

func main() {
	size := flag.Int64("size", 32<<20, "payload size in bytes")
	dir := flag.String("dir", "", "directory for the payload")
	ready := flag.String("ready-file", "", "write the magnet and digest here as JSON")
	rateLimit := flag.Int("rate", 0, "upload limit in bytes/s (0 = unlimited)")
	flag.Parse()
	if *dir == "" {
		panic("-dir required")
	}
	path := filepath.Join(*dir, "bt-payload.bin")
	data := make([]byte, *size)
	for i := range data {
		data[i] = byte(uint32(i)*131 + uint32(i/509))
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)

	info := metainfo.Info{PieceLength: 256 << 10}
	if err := info.BuildFromFilePath(path); err != nil {
		panic(err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		panic(err)
	}
	mi := &metainfo.MetaInfo{InfoBytes: infoBytes}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = *dir
	cfg.Seed = true
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisableIPv6 = true
	cfg.NoDefaultPortForwarding = true
	cfg.SetListenAddr("127.0.0.1:0")
	if *rateLimit > 0 {
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(*rateLimit), 256<<10)
	}
	client, err := torrent.NewClient(cfg)
	if err != nil {
		panic(err)
	}
	t, err := client.AddTorrent(mi)
	if err != nil {
		panic(err)
	}
	<-t.GotInfo()
	t.VerifyData()
	port := client.LocalPort()
	hash := mi.HashInfoBytes()
	magnet := mi.Magnet(&hash, &info).String() + "&x.pe=" + net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	out, _ := json.Marshal(map[string]any{"magnet": magnet, "sha256": hex.EncodeToString(sum[:]),
		"size": *size, "name": info.Name, "complete": t.BytesMissing() == 0})
	if *ready != "" {
		os.WriteFile(*ready+".tmp", out, 0o600)
		os.Rename(*ready+".tmp", *ready)
	}
	fmt.Println(string(out))
	select {}
}
