package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// upgradeRangeResume preserves a legacy sequential prefix when the server
// omitted Accept-Ranges during the original resolve. A saved modification date
// and a matching overlap are required before converting the persisted chunk.
func (f *Fetcher) upgradeRangeResume() error {
	if f.meta.Res.Range || f.meta.Res.Size <= 0 || len(f.meta.Res.Files) != 1 ||
		f.meta.Res.Files[0].Ctime == nil || f.getState() == stateSlowStart || f.getState() == stateSteady {
		return nil
	}
	modified := *f.meta.Res.Files[0].Ctime
	if modified.IsZero() || time.Since(modified) < time.Minute {
		return nil
	}
	f.connMu.Lock()
	if len(f.connections) != 1 {
		f.connMu.Unlock()
		return nil
	}
	conn := f.connections[0]
	prefix := conn.Downloaded
	eligible := !conn.Completed && prefix > 0 && prefix < f.meta.Res.Size &&
		conn.Chunk != nil && conn.Chunk.Begin == 0 && conn.Chunk.Downloaded == prefix
	f.connMu.Unlock()
	if !eligible {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), rangeProbeTimeout)
	defer cancel()
	request, err := f.buildRequestWithOriginalURL(ctx, f.meta.Req)
	if err != nil {
		return err
	}
	if request.Method != http.MethodGet || (request.Body != nil && request.Body != http.NoBody) ||
		request.Header.Get("Range") != "" {
		return nil
	}
	// A failed verification leaves the counted prefix and file untouched. It
	// must not silently turn an upgrade/restart into a byte-zero redownload.
	failure := func() error {
		return fmt.Errorf("cannot verify saved HTTP prefix for range resume; partial file preserved")
	}
	file, err := os.Open(f.meta.SingleFilepath())
	if err != nil {
		return failure()
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || stat.Size() < prefix {
		return failure()
	}
	begin := max(int64(0), prefix-64*1024)
	expected := make([]byte, prefix-begin)
	if _, err = file.ReadAt(expected, begin); err != nil {
		return failure()
	}
	validator := modified.UTC().Format(http.TimeFormat)
	request.Header.Set("If-Range", validator)
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", begin, prefix-1))
	request.Header.Set("Accept-Encoding", "identity")
	response, err := f.buildClient().Do(request)
	if err != nil {
		return failure()
	}
	defer response.Body.Close()
	start, end, size, valid := parseContentRange(response.Header.Get("Content-Range"))
	remoteModified, modifiedErr := http.ParseTime(response.Header.Get("Last-Modified"))
	responseDate, dateErr := http.ParseTime(response.Header.Get("Date"))
	encoding := response.Header.Get("Content-Encoding")
	if response.StatusCode != http.StatusPartialContent || !valid || start != begin || end != prefix-1 ||
		size != f.meta.Res.Size || modifiedErr != nil || !remoteModified.Equal(modified) ||
		dateErr != nil || responseDate.Sub(remoteModified) < time.Minute || response.Uncompressed ||
		(encoding != "" && !strings.EqualFold(encoding, "identity")) {
		return failure()
	}
	actual, err := io.ReadAll(io.LimitReader(response.Body, int64(len(expected))+1))
	if err != nil || !bytes.Equal(actual, expected) {
		return failure()
	}
	f.connMu.Lock()
	defer f.connMu.Unlock()
	if len(f.connections) != 1 || f.connections[0] != conn || conn.Downloaded != prefix {
		return failure()
	}
	conn.Chunk = newChunk(0, f.meta.Res.Size-1)
	conn.Chunk.Downloaded = prefix
	f.meta.Res.Range = true
	f.rangeResumeValidator = validator
	f.diag.setRange(rangeResumed, "")
	return nil
}
