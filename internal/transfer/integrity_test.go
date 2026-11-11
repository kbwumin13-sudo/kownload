package transfer

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	abcSHA256 = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	abcSHA1   = "a9993e364706816aba3e25717850c26c9cd0d89d"
	abcMD5    = "900150983cd24fb0d6963f7d28e17f72"
)

func integrityKind(t *testing.T, err error, want IntegrityErrorKind) {
	t.Helper()
	var typed *IntegrityError
	if !errors.As(err, &typed) || typed.Kind != want {
		t.Fatalf("error %v (%T), want kind %s", err, err, want)
	}
}

type integrityCountingReader struct {
	reader                   io.Reader
	reads, bytes, maxRequest int
}

func (r *integrityCountingReader) Read(p []byte) (int, error) {
	r.reads++
	if len(p) > r.maxRequest {
		r.maxRequest = len(p)
	}
	n, err := r.reader.Read(p)
	r.bytes += n
	return n, err
}
func (r *integrityCountingReader) WriteTo(io.Writer) (int64, error) {
	panic("verification must not bypass per-block cancellation via WriterTo")
}

type integrityReadFunc func([]byte) (int, error)

func (f integrityReadFunc) Read(p []byte) (int, error) { return f(p) }

func TestIntegrityKnownVectors(t *testing.T) {
	checks := []IntegrityCheck{{IntegritySHA256, abcSHA256}, {IntegritySHA1, abcSHA1}, {IntegrityMD5, abcMD5}, {IntegrityAlgorithm("SHA256"), strings.ToUpper(abcSHA256)}}
	original := append([]IntegrityCheck(nil), checks...)
	result, err := VerifyIntegrity(context.Background(), strings.NewReader("abc"), 3, checks)
	if err != nil || result.Outcome != IntegrityMatched || result.BytesRead != 3 {
		t.Fatalf("%+v %v", result, err)
	}
	if !reflect.DeepEqual(result.Actual, map[IntegrityAlgorithm]string{IntegritySHA256: abcSHA256, IntegritySHA1: abcSHA1, IntegrityMD5: abcMD5}) {
		t.Fatal(result.Actual)
	}
	if !reflect.DeepEqual(checks, original) {
		t.Fatal("caller checks were changed")
	}
}

func TestIntegrityMultipleAlgorithmsOnePass(t *testing.T) {
	data := bytes.Repeat([]byte("single-pass-abc\x00"), 200000)
	s256, s1, m5 := sha256.Sum256(data), sha1.Sum(data), md5.Sum(data)
	r := &integrityCountingReader{reader: bytes.NewReader(data)}
	result, err := VerifyIntegrity(context.Background(), r, int64(len(data)), []IntegrityCheck{{IntegritySHA256, hex.EncodeToString(s256[:])}, {IntegrityMD5, hex.EncodeToString(m5[:])}, {IntegritySHA1, hex.EncodeToString(s1[:])}})
	if err != nil || result.Outcome != IntegrityMatched || r.bytes != len(data) || r.reads < 2 || r.maxRequest > 64*1024 {
		t.Fatalf("result=%+v err=%v reader=%+v", result, err, r)
	}
}

func TestIntegrityEmptyAndNoExpectation(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		checks     []IntegrityCheck
		want       IntegrityOutcome
		digest     string
	}{
		{"empty expected", "", []IntegrityCheck{{IntegritySHA256, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}}, IntegrityMatched, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"empty no expectation", "", nil, IntegrityNoExpectation, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"nil checks", "abc", nil, IntegrityNoExpectation, abcSHA256},
		{"empty checks", "abc", []IntegrityCheck{}, IntegrityNoExpectation, abcSHA256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := VerifyIntegrity(context.Background(), strings.NewReader(tc.data), int64(len(tc.data)), tc.checks)
			if err != nil || result.Outcome != tc.want || result.Actual[IntegritySHA256] != tc.digest || result.BytesRead != int64(len(tc.data)) {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}

func TestIntegrityMismatchReportsAlgorithm(t *testing.T) {
	result, err := VerifyIntegrity(context.Background(), strings.NewReader("bad"), 3, []IntegrityCheck{{IntegritySHA256, abcSHA256}, {IntegrityMD5, abcMD5}})
	integrityKind(t, err, IntegrityDigestError)
	if result.Outcome != IntegrityMismatch || !reflect.DeepEqual(result.Mismatched, []IntegrityAlgorithm{IntegritySHA256, IntegrityMD5}) || result.Actual[IntegritySHA256] == abcSHA256 {
		t.Fatalf("%+v", result)
	}
}

func TestIntegrityInvalidChecksReadNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []IntegrityCheck
	}{
		{"unknown", []IntegrityCheck{{"sha512", strings.Repeat("a", 128)}}},
		{"empty algorithm", []IntegrityCheck{{"", abcSHA256}}},
		{"short SHA256", []IntegrityCheck{{IntegritySHA256, abcSHA256[:63]}}},
		{"long SHA256", []IntegrityCheck{{IntegritySHA256, abcSHA256 + "0"}}},
		{"nonhex", []IntegrityCheck{{IntegritySHA256, strings.Repeat("g", 64)}}},
		{"whitespace", []IntegrityCheck{{IntegritySHA256, " " + abcSHA256}}},
		{"short MD5", []IntegrityCheck{{IntegrityMD5, abcMD5[:31]}}},
		{"short SHA1", []IntegrityCheck{{IntegritySHA1, abcSHA1[:39]}}},
		{"conflict", []IntegrityCheck{{IntegritySHA256, abcSHA256}, {IntegritySHA256, strings.Repeat("0", 64)}}},
		{"conflict casefold", []IntegrityCheck{{IntegrityAlgorithm("SHA1"), abcSHA1}, {IntegritySHA1, strings.Repeat("0", 40)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &integrityCountingReader{reader: strings.NewReader("abc")}
			result, err := VerifyIntegrity(context.Background(), r, 3, tc.checks)
			integrityKind(t, err, IntegrityInvalidCheck)
			if r.reads != 0 || result.Outcome != "" || result.Actual != nil {
				t.Fatalf("read on invalid checks: %+v %+v", r, result)
			}
		})
	}
}

func TestIntegrityInvalidInputReadNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		size int64
	}{
		{"nil context", nil, 3}, {"unknown size", context.Background(), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &integrityCountingReader{reader: strings.NewReader("abc")}
			_, err := VerifyIntegrity(tc.ctx, r, tc.size, nil)
			integrityKind(t, err, IntegrityInvalidInput)
			if r.reads != 0 {
				t.Fatal(r.reads)
			}
		})
	}
	_, err := VerifyIntegrity(context.Background(), nil, 0, nil)
	integrityKind(t, err, IntegrityInvalidInput)
}

func TestIntegrityLengthMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		size       int64
	}{
		{"truncated", "ab", 3}, {"excess", "abcd", 3}, {"nonempty expected empty", "a", 0}, {"empty expected nonempty", "", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := VerifyIntegrity(context.Background(), strings.NewReader(tc.data), tc.size, nil)
			integrityKind(t, err, IntegrityLengthError)
			if result.Outcome != "" || result.Actual != nil {
				t.Fatalf("invalid read looked complete: %+v", result)
			}
			if tc.name == "truncated" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal(err)
			}
		})
	}
}

func TestIntegrityShortReadsAndDataWithEOF(t *testing.T) {
	for _, withEOF := range []bool{false, true} {
		t.Run(map[bool]string{false: "short nil reads", true: "last data with EOF"}[withEOF], func(t *testing.T) {
			data := []byte("abc")
			r := integrityReadFunc(func(p []byte) (int, error) {
				if len(data) == 0 {
					return 0, io.EOF
				}
				p[0] = data[0]
				data = data[1:]
				if len(data) == 0 && withEOF {
					return 1, io.EOF
				}
				return 1, nil
			})
			result, err := VerifyIntegrity(context.Background(), r, 3, []IntegrityCheck{{IntegritySHA256, abcSHA256}})
			if err != nil || result.Outcome != IntegrityMatched {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}

func TestIntegrityReadFailureAndNoProgress(t *testing.T) {
	sourceErr := errors.New("private fixture source")
	r := integrityReadFunc(func(p []byte) (int, error) { copy(p, "abc"); return 3, sourceErr })
	result, err := VerifyIntegrity(context.Background(), r, 3, []IntegrityCheck{{IntegritySHA256, abcSHA256}})
	integrityKind(t, err, IntegrityReadFailed)
	if !errors.Is(err, sourceErr) || strings.Contains(err.Error(), sourceErr.Error()) || result.Outcome != "" || result.Actual != nil {
		t.Fatalf("%+v %v", result, err)
	}
	_, err = VerifyIntegrity(context.Background(), integrityReadFunc(func([]byte) (int, error) { return 0, nil }), 0, nil)
	integrityKind(t, err, IntegrityReadFailed)
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatal(err)
	}
}

func TestIntegrityCancellation(t *testing.T) {
	t.Run("deadline preserves cause", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		reader := &integrityCountingReader{reader: strings.NewReader("abc")}
		_, err := VerifyIntegrity(ctx, reader, 3, nil)
		integrityKind(t, err, IntegrityCanceled)
		if !errors.Is(err, context.DeadlineExceeded) || reader.reads != 0 {
			t.Fatalf("%v reads=%d", err, reader.reads)
		}
	})
	t.Run("before first read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := &integrityCountingReader{reader: strings.NewReader("abc")}
		result, err := VerifyIntegrity(ctx, r, 3, nil)
		integrityKind(t, err, IntegrityCanceled)
		if !errors.Is(err, context.Canceled) || r.reads != 0 || result.Outcome != "" {
			t.Fatalf("%+v %v", result, err)
		}
	})
	t.Run("between blocks", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reads := 0
		r := integrityReadFunc(func(p []byte) (int, error) { reads++; copy(p, "abc"); cancel(); return 3, nil })
		result, err := VerifyIntegrity(ctx, r, 100, nil)
		integrityKind(t, err, IntegrityCanceled)
		if !errors.Is(err, context.Canceled) || reads != 1 || result.Outcome != "" || result.Actual != nil {
			t.Fatalf("%+v %v %d", result, err, reads)
		}
	})
	t.Run("blocked Read is not interrupted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		r := integrityReadFunc(func(p []byte) (int, error) { close(entered); <-release; p[0] = 'x'; return 1, io.EOF })
		go func() { _, err := VerifyIntegrity(ctx, r, 1, nil); done <- err }()
		select {
		case <-entered:
		case err := <-done:
			t.Fatalf("returned before reading: %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("did not enter Read")
		}
		cancel()
		select {
		case err := <-done:
			t.Fatalf("returned while Read blocked: %v", err)
		default:
		}
		close(release)
		select {
		case err := <-done:
			integrityKind(t, err, IntegrityCanceled)
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("did not observe cancellation after Read returned")
		}
	})
}

func TestIntegrityFileReadsWholePinnedHandleWithoutChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.bin")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Seek(2, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	result, err := VerifyIntegrityFile(context.Background(), file, 3, []IntegrityCheck{{IntegritySHA256, abcSHA256}, {IntegrityMD5, abcMD5}, {IntegritySHA1, abcSHA1}})
	if err != nil || result.Outcome != IntegrityMatched {
		t.Fatalf("%+v %v", result, err)
	}
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil || offset != 2 {
		t.Fatalf("seek changed: %d %v", offset, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "abc" {
		t.Fatalf("contents changed: %q %v", data, err)
	}
	after, err := file.Stat()
	if err != nil || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("metadata changed: %v", err)
	}
}

func TestIntegrityFilePathReplacementKeepsOpenedObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.bin")
	replacement := path + ".replacement"
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = os.WriteFile(replacement, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	result, err := VerifyIntegrityFile(context.Background(), file, 3, []IntegrityCheck{{IntegritySHA256, abcSHA256}})
	if err != nil || result.Outcome != IntegrityMatched {
		t.Fatalf("reopened replacement: %+v %v", result, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "bad" {
		t.Fatalf("replacement touched: %q %v", data, err)
	}
	newFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer newFile.Close()
	result, err = VerifyIntegrityFile(context.Background(), newFile, 3, []IntegrityCheck{{IntegritySHA256, abcSHA256}})
	integrityKind(t, err, IntegrityDigestError)
	if result.Outcome != IntegrityMismatch {
		t.Fatal(result)
	}
}

func TestIntegrityFileEmptyAndInvalidHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	result, err := VerifyIntegrityFile(context.Background(), file, 0, nil)
	if err != nil || result.Outcome != IntegrityNoExpectation || result.BytesRead != 0 {
		t.Fatalf("%+v %v", result, err)
	}
	_, err = VerifyIntegrityFile(context.Background(), file, 1, nil)
	integrityKind(t, err, IntegrityLengthError)
	file.Close()
	_, err = VerifyIntegrityFile(context.Background(), file, 0, nil)
	integrityKind(t, err, IntegrityReadFailed)
	_, err = VerifyIntegrityFile(context.Background(), nil, 0, nil)
	integrityKind(t, err, IntegrityInvalidInput)
	_, err = VerifyIntegrityFile(context.Background(), file, 0, []IntegrityCheck{{IntegritySHA256, "bad"}})
	integrityKind(t, err, IntegrityInvalidCheck)
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	_, err = VerifyIntegrityFile(context.Background(), dir, 0, nil)
	integrityKind(t, err, IntegrityNotRegular)
	readPipe, writePipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readPipe.Close()
	defer writePipe.Close()
	_, err = VerifyIntegrityFile(context.Background(), readPipe, 0, nil)
	integrityKind(t, err, IntegrityNotRegular)
}

func TestIntegrityConcurrentCallsShareNoMutableState(t *testing.T) {
	checks := []IntegrityCheck{{IntegritySHA256, abcSHA256}, {IntegritySHA1, abcSHA1}}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := VerifyIntegrity(context.Background(), strings.NewReader("abc"), 3, checks)
			if err != nil || result.Outcome != IntegrityMatched {
				t.Errorf("%+v %v", result, err)
			}
		}()
	}
	wg.Wait()
}
