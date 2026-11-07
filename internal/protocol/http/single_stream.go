package http

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// A declared Range capability without a total length still needs a full stream;
// the placeholder chunk is not a byte range or evidence of completion.
func (f *Fetcher) usesRanges() bool {
	return f.meta.Res.Range && f.meta.Res.Size > 0
}

type localFileError struct{ err error }

func (e *localFileError) Error() string {
	err := e.err
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return fmt.Sprintf("write download file: %v", err)
}
func (e *localFileError) Unwrap() error { return e.err }

func isLocalFileError(err error) bool {
	var local *localFileError
	return errors.As(err, &local)
}

// A new full response starts at byte zero, including retries inside one run.
// Reset under the file lock so Pause cannot close the file during truncation.
func (f *Fetcher) resetSingleStreamAttempt(conn *connection) error {
	f.fileMu.Lock()
	defer f.fileMu.Unlock()
	if f.file == nil {
		return &localFileError{os.ErrClosed}
	}
	if err := f.file.Truncate(0); err != nil {
		return &localFileError{err}
	}
	f.connMu.Lock()
	f.resetConnectionForRestart(conn)
	f.connMu.Unlock()
	return nil
}

func (f *Fetcher) writeDownloadAt(data []byte, offset int64) error {
	f.fileMu.RLock()
	defer f.fileMu.RUnlock()
	if f.file == nil {
		return &localFileError{os.ErrClosed}
	}
	n, err := f.file.WriteAt(data, offset)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return &localFileError{err}
	}
	return nil
}

func (f *Fetcher) failSingleStream(conn *connection, err error) {
	f.connMu.Lock()
	conn.State = connFailed
	conn.failed = true
	conn.lastErr = err
	f.connMu.Unlock()
}
