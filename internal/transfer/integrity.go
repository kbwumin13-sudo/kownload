package transfer

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"strings"
)

type IntegrityAlgorithm string

const (
	IntegritySHA256 IntegrityAlgorithm = "sha256"
	IntegritySHA1   IntegrityAlgorithm = "sha1"
	IntegrityMD5    IntegrityAlgorithm = "md5"
)

// IntegrityCheck describes an expectation, not its trust or resource identity.
// The caller selects checks using its own policy (including provider-specific
// normalization). Expected must be a complete hex digest without whitespace.
// Algorithm names and hex digits are case insensitive; conflicting checks for
// the same algorithm are rejected before reading.
type IntegrityCheck struct {
	Algorithm IntegrityAlgorithm
	Expected  string
}

type IntegrityOutcome string

const (
	IntegrityMatched       IntegrityOutcome = "matched"
	IntegrityMismatch      IntegrityOutcome = "mismatch"
	IntegrityNoExpectation IntegrityOutcome = "no_expectation"
)

type IntegrityResult struct {
	Outcome    IntegrityOutcome
	BytesRead  int64
	Actual     map[IntegrityAlgorithm]string
	Mismatched []IntegrityAlgorithm
}

type IntegrityErrorKind string

const (
	IntegrityInvalidInput IntegrityErrorKind = "invalid_input"
	IntegrityInvalidCheck IntegrityErrorKind = "invalid_check"
	IntegrityCanceled     IntegrityErrorKind = "canceled"
	IntegrityReadFailed   IntegrityErrorKind = "read_failed"
	IntegrityLengthError  IntegrityErrorKind = "length_mismatch"
	IntegrityNotRegular   IntegrityErrorKind = "not_regular_file"
	IntegrityFileChanged  IntegrityErrorKind = "file_changed"
	IntegrityDigestError  IntegrityErrorKind = "digest_mismatch"
)

// IntegrityError deliberately omits paths, digest values and the underlying
// error's text. Unwrap preserves errors.Is for cancellation and I/O failures.
type IntegrityError struct {
	Kind IntegrityErrorKind
	Err  error
}

func (e *IntegrityError) Error() string { return "integrity: " + string(e.Kind) }
func (e *IntegrityError) Unwrap() error { return e.Err }

func integrityError(kind IntegrityErrorKind, err error) error {
	return &IntegrityError{Kind: kind, Err: err}
}

func integrityChecks(ctx context.Context, size int64, checks []IntegrityCheck) (map[IntegrityAlgorithm]string, error) {
	if ctx == nil || size < 0 {
		return nil, integrityError(IntegrityInvalidInput, nil)
	}
	expected := make(map[IntegrityAlgorithm]string, len(checks))
	for _, check := range checks {
		algorithm := IntegrityAlgorithm(strings.ToLower(string(check.Algorithm)))
		var length int
		switch algorithm {
		case IntegritySHA256:
			length = sha256.Size * 2
		case IntegritySHA1:
			length = sha1.Size * 2
		case IntegrityMD5:
			length = md5.Size * 2
		default:
			return nil, integrityError(IntegrityInvalidCheck, nil)
		}
		if len(check.Expected) != length {
			return nil, integrityError(IntegrityInvalidCheck, nil)
		}
		decoded, err := hex.DecodeString(check.Expected)
		if err != nil {
			return nil, integrityError(IntegrityInvalidCheck, nil)
		}
		value := hex.EncodeToString(decoded)
		if previous, exists := expected[algorithm]; exists && previous != value {
			return nil, integrityError(IntegrityInvalidCheck, nil)
		}
		expected[algorithm] = value
	}
	return expected, nil
}

// VerifyIntegrity reads from the reader's current position to EOF and requires
// exactly expectedSize bytes (zero is a known empty stream, not unknown length).
// It always computes SHA256 as a local fingerprint, plus MD5/SHA1 if requested,
// in one pass. No checks means no_expectation, never matched. A mismatch returns
// both its complete result and an IntegrityDigestError; other errors leave
// Outcome empty and Actual nil so incomplete reads cannot look verified.
//
// Cancellation is checked before and after each bounded Read and before the
// result is returned. It cannot interrupt an underlying blocked Read: the caller
// must provide an interruptible reader or wait for that Read to finish. This
// function never closes the reader, starts goroutines, logs or writes data.
func VerifyIntegrity(ctx context.Context, reader io.Reader, expectedSize int64, checks []IntegrityCheck) (IntegrityResult, error) {
	expected, err := integrityChecks(ctx, expectedSize, checks)
	if err != nil {
		return IntegrityResult{}, err
	}
	if reader == nil {
		return IntegrityResult{}, integrityError(IntegrityInvalidInput, nil)
	}
	return verifyIntegrity(ctx, reader, expectedSize, expected)
}

func verifyIntegrity(ctx context.Context, reader io.Reader, size int64, expected map[IntegrityAlgorithm]string) (IntegrityResult, error) {
	result := IntegrityResult{}
	hashes := map[IntegrityAlgorithm]hash.Hash{IntegritySHA256: sha256.New()}
	if _, ok := expected[IntegrityMD5]; ok {
		hashes[IntegrityMD5] = md5.New()
	}
	if _, ok := expected[IntegritySHA1]; ok {
		hashes[IntegritySHA1] = sha1.New()
	}
	buffer := make([]byte, 64*1024)
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return result, integrityError(IntegrityCanceled, err)
		}
		want := len(buffer)
		// Read at most one excess byte, without overflowing at MaxInt64.
		if remaining := size - result.BytesRead; remaining < int64(want) {
			want = int(remaining) + 1
		}
		n, readErr := reader.Read(buffer[:want])
		if n < 0 || n > want {
			return result, integrityError(IntegrityReadFailed, errors.New("invalid read count"))
		}
		if result.BytesRead > (1<<63-1)-int64(n) {
			return result, integrityError(IntegrityLengthError, nil)
		}
		result.BytesRead += int64(n)
		if err := ctx.Err(); err != nil {
			return result, integrityError(IntegrityCanceled, err)
		}
		if result.BytesRead > size {
			return result, integrityError(IntegrityLengthError, nil)
		}
		if n > 0 {
			emptyReads = 0
			for _, h := range hashes {
				_, _ = h.Write(buffer[:n]) // Standard-library hashes never fail.
			}
		} else {
			emptyReads++
		}
		if readErr != nil {
			if readErr != io.EOF {
				return result, integrityError(IntegrityReadFailed, readErr)
			}
			if result.BytesRead != size {
				return result, integrityError(IntegrityLengthError, io.ErrUnexpectedEOF)
			}
			break
		}
		if emptyReads >= 100 {
			return result, integrityError(IntegrityReadFailed, io.ErrNoProgress)
		}
	}
	if err := ctx.Err(); err != nil {
		return result, integrityError(IntegrityCanceled, err)
	}
	result.Actual = make(map[IntegrityAlgorithm]string, len(hashes))
	for _, algorithm := range []IntegrityAlgorithm{IntegritySHA256, IntegritySHA1, IntegrityMD5} {
		if h := hashes[algorithm]; h != nil {
			actual := hex.EncodeToString(h.Sum(nil))
			result.Actual[algorithm] = actual
			if wanted, ok := expected[algorithm]; ok && wanted != actual {
				result.Mismatched = append(result.Mismatched, algorithm)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return IntegrityResult{BytesRead: result.BytesRead}, integrityError(IntegrityCanceled, err)
	}
	switch {
	case len(result.Mismatched) > 0:
		result.Outcome = IntegrityMismatch
		return result, integrityError(IntegrityDigestError, nil)
	case len(expected) == 0:
		result.Outcome = IntegrityNoExpectation
	default:
		result.Outcome = IntegrityMatched
	}
	return result, nil
}

// VerifyIntegrityFile borrows one ordinary file handle opened O_RDONLY by the
// caller. It reads from offset zero with ReadAt, regardless of the handle's seek
// position, and leaves that position and the file's contents unchanged. It does
// not reopen a path, close the handle or inspect its OS access-mode flags.
// expectedSize must be known and equal the handle's size, including for empty
// files. Path replacement after opening cannot switch the file being hashed.
//
// Before/after Stat checks detect length/mtime changes; they are not proof of
// resource identity or absence of concurrent writers. The caller must keep the
// file quiescent and own its identity/publication/generation policy. A later
// write or cancellation can still occur after this function returns.
func VerifyIntegrityFile(ctx context.Context, file *os.File, expectedSize int64, checks []IntegrityCheck) (IntegrityResult, error) {
	expected, err := integrityChecks(ctx, expectedSize, checks)
	if err != nil {
		return IntegrityResult{}, err
	}
	if file == nil {
		return IntegrityResult{}, integrityError(IntegrityInvalidInput, nil)
	}
	if err := ctx.Err(); err != nil {
		return IntegrityResult{}, integrityError(IntegrityCanceled, err)
	}
	before, err := file.Stat()
	if err != nil {
		return IntegrityResult{}, integrityError(IntegrityReadFailed, err)
	}
	if !before.Mode().IsRegular() {
		return IntegrityResult{}, integrityError(IntegrityNotRegular, nil)
	}
	if before.Size() != expectedSize {
		return IntegrityResult{}, integrityError(IntegrityLengthError, nil)
	}
	result, err := verifyIntegrity(ctx, io.NewSectionReader(file, 0, expectedSize), expectedSize, expected)
	if err != nil {
		return result, err
	}
	after, err := file.Stat()
	if err != nil {
		return IntegrityResult{BytesRead: result.BytesRead}, integrityError(IntegrityReadFailed, err)
	}
	if !os.SameFile(before, after) || after.Size() != expectedSize || !before.ModTime().Equal(after.ModTime()) {
		return IntegrityResult{BytesRead: result.BytesRead}, integrityError(IntegrityFileChanged, nil)
	}
	if err := ctx.Err(); err != nil {
		return IntegrityResult{BytesRead: result.BytesRead}, integrityError(IntegrityCanceled, err)
	}
	return result, nil
}
