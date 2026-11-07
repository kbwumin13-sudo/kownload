// Package nativehost implements the public browser protocol and kownload's
// message contract. It has no dependency on the download engine.
package nativehost

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const MaxInput = 64 * 1024 * 1024
const MaxOutput = 1024 * 1024

func ReadFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.NativeEndian.Uint32(header[:])
	if n == 0 || n > MaxInput {
		return nil, errors.New("invalid frame length")
	}
	body := make([]byte, int(n))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, errors.New("invalid JSON frame")
	}
	return body, nil
}

func WriteFrame(w io.Writer, value Response) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(body) > MaxOutput {
		body, _ = json.Marshal(failure(value.OperationID, "invalid_response", nil, nil))
	}
	var header [4]byte
	binary.NativeEndian.PutUint32(header[:], uint32(len(body)))
	for _, part := range [][]byte{header[:], body} {
		for len(part) > 0 {
			n, err := w.Write(part)
			if err != nil {
				return err
			}
			if n <= 0 {
				return io.ErrShortWrite
			}
			part = part[n:]
		}
	}
	return nil
}

func Serve(r io.Reader, w io.Writer, dispatch func([]byte) Response) error {
	for {
		body, err := ReadFrame(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			_ = WriteFrame(w, failure("", "invalid_request", nil, nil))
			return err
		}
		if err = WriteFrame(w, dispatch(body)); err != nil {
			return err
		}
	}
}
