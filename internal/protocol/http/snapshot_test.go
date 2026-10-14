package http

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

func TestFetcherManagerStoreSnapshot(t *testing.T) {
	f := &Fetcher{connections: []*connection{{ID: 1, Chunk: newChunk(0, 10000)}}}
	fm := &FetcherManager{}
	saved, err := fm.Store(f)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(1); i <= 1000; i++ {
			f.connMu.Lock()
			f.connections[0].Downloaded = i
			f.connections[0].Chunk.Downloaded = i
			f.connMu.Unlock()
		}
	}()
	for i := 0; i < 1000; i++ {
		data, err := fm.Store(f)
		if err != nil {
			t.Fatal(err)
		}
		// Serialization happens after Store releases connMu, as in real storage.
		encoded, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		var decoded fetcherData
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		c := decoded.Connections[0]
		if c.Downloaded != c.Chunk.Downloaded {
			t.Fatalf("inconsistent resume snapshot: %s", encoded)
		}
		_ = f.Progress()
	}
	wg.Wait()
	c := saved.(*fetcherData).Connections[0]
	if c.Downloaded != 0 || c.Chunk.Downloaded != 0 {
		t.Fatal("Store returned live progress pointers")
	}
}

type blockedPrefetchBody struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *blockedPrefetchBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.closed
	return 0, io.EOF
}
func (b *blockedPrefetchBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestStopPrefetchJoinsBlockedReader(t *testing.T) {
	body := &blockedPrefetchBody{started: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(func() { _ = body.Close() })
	f := &Fetcher{resolveResp: &http.Response{Body: body}, prefetchStopCh: make(chan struct{}), prefetchDoneCh: make(chan struct{})}
	go f.asyncPrefetch()
	<-body.started
	// The read-start signal also publishes the temporary file's creation.
	tempPath := f.prefetchFilePath
	type copyResult struct {
		n   int64
		err error
	}
	copied := make(chan copyResult, 1)
	go func() {
		n, err := f.stopPrefetchAndCopyData()
		copied <- copyResult{n, err}
	}()
	select {
	case result := <-copied:
		if result.n != 0 || result.err != nil {
			t.Fatalf("unexpected prefetch copy result: bytes=%d, err=%v", result.n, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("stopping prefetch did not interrupt the blocked response read")
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("temporary file was not removed: %v", err)
	}
	if f.prefetchFile != nil || f.prefetchFilePath != "" {
		t.Fatal("prefetch file ownership was not released")
	}
}
