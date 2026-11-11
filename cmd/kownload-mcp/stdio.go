package main

import (
	"bufio"
	"bytes"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"time"
)

const maxInput = 64 << 10
const maxOutput = 256 << 10

// Limit each JSON-RPC line before the SDK allocates/decodes it. Closing the
// underlying pipe unblocks a pending read when the SDK ends the session.
type lineReader struct {
	source  io.ReadCloser
	scanner *bufio.Scanner
	pending *bytes.Reader
}

func newLineReader(source io.ReadCloser) *lineReader {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 4096), maxInput)
	return &lineReader{source: source, scanner: scanner, pending: bytes.NewReader(nil)}
}
func (r *lineReader) Read(p []byte) (int, error) {
	if r.pending.Len() == 0 {
		if !r.scanner.Scan() {
			if r.scanner.Err() != nil {
				return 0, errors.New("input limit or read failure")
			}
			return 0, io.EOF
		}
		r.pending = bytes.NewReader(append(append([]byte(nil), r.scanner.Bytes()...), '\n'))
	}
	return r.pending.Read(p)
}
func (r *lineReader) Close() error { return r.source.Close() }

type boundedWriter struct{ file *os.File }

func (w *boundedWriter) Write(p []byte) (int, error) {
	if len(p) > maxOutput {
		return 0, errors.New("output limit")
	}
	// Real stdio pipes support deadlines; refuse unsupported sinks rather than
	// letting a stopped reader retain this bridge indefinitely.
	if err := w.file.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return 0, errors.New("unsupported output")
	}
	return w.file.Write(p)
}
func (w *boundedWriter) Close() error { return w.file.Close() }

// Inherited child-process stdio may be a blocking socket/pipe. Duplicate and
// mark it nonblocking before NewFile so Go registers it with the poller;
// deadlines and Close can then interrupt I/O (including a stopped client).
func pollableFile(source *os.File) (*os.File, error) {
	fd, err := unix.Dup(int(source.Fd()))
	if err != nil {
		return nil, errors.New("stdio setup failed")
	}
	unix.CloseOnExec(fd)
	if err = unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, errors.New("stdio setup failed")
	}
	return os.NewFile(uintptr(fd), "mcp-stdio"), nil
}
