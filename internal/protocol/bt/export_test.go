package bt

import (
	"testing"
	"time"

	"github.com/anacrolix/torrent"
)

// LoopbackConfig keeps a client on 127.0.0.1 without DHT, trackers, PEX, uTP
// or port mapping, so a test only meets the peers it introduces.
func LoopbackConfig(cfg *torrent.ClientConfig, dataDir string) {
	cfg.DataDir = dataDir
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisablePEX = true
	cfg.NoDefaultPortForwarding = true
	cfg.DisableIPv6 = true
	cfg.DisableUTP = true
	cfg.DisableWebtorrent = true
	cfg.DisableWebseeds = true
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
}

// UseLoopbackClients makes every shared client created during the test
// loopback-only and shortens the interactive resolve deadline.
func UseLoopbackClients(t *testing.T, deadline time.Duration) {
	t.Helper()
	dataDir := t.TempDir()
	lock.Lock()
	_ = closeClientLocked()
	previousDeadline, previousConfigure := resolveTimeout, configureClient
	resolveTimeout = deadline
	configureClient = func(cfg *torrent.ClientConfig) { LoopbackConfig(cfg, dataDir) }
	lock.Unlock()
	t.Cleanup(func() {
		lock.Lock()
		defer lock.Unlock()
		_ = closeClientLocked()
		resolveTimeout, configureClient = previousDeadline, previousConfigure
	})
}

// SharedClient returns the client the fetchers share, or nil.
func SharedClient() *torrent.Client {
	lock.Lock()
	defer lock.Unlock()
	return client
}

// SharedTorrents counts the torrents the shared client holds.
func SharedTorrents() int {
	lock.Lock()
	defer lock.Unlock()
	if client == nil {
		return 0
	}
	return len(client.Torrents())
}
