//go:build darwin || linux

package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	bolt "go.etcd.io/bbolt"
)

type completionFixture struct {
	path   string
	dir    *CompletionDirectory
	file   *os.File
	record VerifiedCompletionRecord
	body   []byte
}

func newCompletionFixture(t *testing.T, body []byte) *completionFixture {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := OpenCompletionDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	w, err := os.OpenFile(filepath.Join(path, ".owned-stage"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := CompletionIdentityOf(w)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	r := VerifiedCompletionRecord{Version: 1, ID: "fixture-completion", Phase: CompletionWaiting,
		DirectoryIdentity: dir.Identity(), StagingIdentity: identity, StagingName: ".owned-stage",
		FinalName: "result.bin", Size: int64(len(body)), ExpectedSHA256: hex.EncodeToString(digest[:])}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := dir.OpenStaging(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return &completionFixture{path, dir, file, r, body}
}

// This is an adapter fixture, not a substitute Core or persistent generation.
// The production Core owner must implement these guards with its existing token.
type completionStoreFixture struct {
	gate     sync.Mutex
	active   atomic.Bool
	record   VerifiedCompletionRecord // All access below is under gate.
	done     bool
	commits  int
	phases   []VerifiedCompletionPhase
	failSave VerifiedCompletionPhase
	unknown  bool
}

func newCompletionStoreFixture(r VerifiedCompletionRecord) *completionStoreFixture {
	s := &completionStoreFixture{record: r}
	s.active.Store(true)
	return s
}

func (s *completionStoreFixture) hooks() VerifiedCompletionHooks {
	return VerifiedCompletionHooks{
		CheckRun: func() error {
			if !s.active.Load() {
				return errors.New("inactive fixture run")
			}
			return nil
		},
		Fence: func(ctx context.Context, body func() error) error {
			s.gate.Lock()
			defer s.gate.Unlock()
			return body() // Complete checks ctx/token while still admitted.
		},
		Save: func(r VerifiedCompletionRecord) error {
			if s.done || r.ID != s.record.ID {
				return errors.New("cannot regress committed or change intent")
			}
			if r.Phase == s.failSave {
				if s.unknown {
					s.record = r
				}
				return errors.New("unconfirmed fixture save")
			}
			s.record = r
			s.phases = append(s.phases, r.Phase)
			return nil
		},
		Commit: func(r VerifiedCompletionRecord) (bool, error) {
			if s.done {
				return false, nil
			}
			if s.failSave == CompletionCommitted && !s.unknown {
				return false, errors.New("commit failed before write")
			}
			s.record, s.done = r, true
			s.commits++
			if s.failSave == CompletionCommitted {
				return false, errors.New("commit ACK unknown")
			}
			return true, nil
		},
	}
}

func (s *completionStoreFixture) invalidate() {
	s.gate.Lock()
	defer s.gate.Unlock()
	s.active.Store(false)
}

func completerForTest(t *testing.T) *VerifiedCompleter {
	t.Helper()
	v, err := NewVerifiedCompleter(1)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func assertCompletionAbsent(t *testing.T, f *completionFixture) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(f.path, f.record.FinalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected published entry: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(f.path, f.record.StagingName))
	if err != nil || !bytes.Equal(data, f.body) {
		t.Fatal("staging content was not preserved", err)
	}
}

func TestVerifyHTTPCompletion(t *testing.T) {
	f := newCompletionFixture(t, bytes.Repeat([]byte("streamed fixture"), 20000))
	check := func() error { return nil }
	result, err := VerifyHTTPCompletion(context.Background(), f.file, f.record.Size, strings.ToUpper(f.record.ExpectedSHA256), check)
	if err != nil || result.Outcome != IntegrityMatched || result.BytesRead != f.record.Size {
		t.Fatalf("matching file: %v %v", result, err)
	}
	result, err = VerifyHTTPCompletion(context.Background(), f.file, f.record.Size, strings.Repeat("0", 64), check)
	var hashErr *IntegrityError
	if !errors.As(err, &hashErr) || hashErr.Kind != IntegrityDigestError || result.Outcome != IntegrityMismatch {
		t.Fatalf("mismatch: %v %v", result, err)
	}
	for _, expected := range []string{"", "bad", strings.Repeat("g", 64), " " + f.record.ExpectedSHA256} {
		// A closed handle establishes that malformed input fails before descriptor IO.
		closed, err := os.Open(filepath.Join(f.path, f.record.StagingName))
		if err != nil {
			t.Fatal(err)
		}
		_ = closed.Close()
		_, err = VerifyHTTPCompletion(context.Background(), closed, f.record.Size, expected, check)
		var e *VerifiedCompletionError
		if !errors.As(err, &e) || e.Kind != CompletionInvalid {
			t.Fatalf("invalid expected digest did not fail before IO: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VerifyHTTPCompletion(ctx, f.file, f.record.Size, f.record.ExpectedSHA256, check); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel was not preserved: %v", err)
	}
	checks := 0
	result, err = VerifyHTTPCompletion(context.Background(), f.file, f.record.Size, f.record.ExpectedSHA256, func() error {
		checks++
		if checks == 2 {
			return errors.New("source changed after hash")
		}
		return nil
	})
	var completionErr *VerifiedCompletionError
	if !errors.As(err, &completionErr) || completionErr.Kind != CompletionStale || result.Actual != nil || result.Outcome != "" {
		t.Fatalf("late hash result escaped: %v %v", result, err)
	}
	writer, err := os.OpenFile(filepath.Join(f.path, f.record.StagingName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := VerifyHTTPCompletion(context.Background(), writer, f.record.Size, f.record.ExpectedSHA256, check); err == nil {
		t.Fatal("writable hash handle accepted")
	}
	assertCompletionAbsent(t, f)
}

func TestVerifiedCompletionSuccessAndNoReplay(t *testing.T) {
	for _, body := range [][]byte{nil, []byte("complete file"), bytes.Repeat([]byte{0x1b, 0x31, 0xf2}, 100000)} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			f := newCompletionFixture(t, body)
			s := newCompletionStoreFixture(f.record)
			v := completerForTest(t)
			permit, err := v.Complete(context.Background(), f.dir, f.file, f.record, s.hooks())
			if err != nil || !permit.Fresh || !s.done || s.commits != 1 || s.record.Phase != CompletionCommitted {
				t.Fatalf("commit: %+v %v", permit, err)
			}
			data, err := os.ReadFile(filepath.Join(f.path, f.record.FinalName))
			if err != nil || !bytes.Equal(data, body) {
				t.Fatal("final file differs", err)
			}
			if err := f.dir.checkFinal(f.file, s.record); err != nil {
				t.Fatal("final does not belong to staging anchor", err)
			}
			// Simulate restart from the committed record. No file read or replay.
			permit, err = v.Complete(context.Background(), nil, nil, s.record, s.hooks())
			if err != nil || permit.Fresh || s.commits != 1 {
				t.Fatalf("recovered commit granted side effects: %+v %v", permit, err)
			}
		})
	}
}

func TestVerifiedCompletionMismatchAndInvalidRecord(t *testing.T) {
	for _, mutate := range []func(*VerifiedCompletionRecord){
		func(r *VerifiedCompletionRecord) { r.ExpectedSHA256 = strings.Repeat("0", 64) },
		func(r *VerifiedCompletionRecord) { r.ExpectedSHA256 = "" },
		func(r *VerifiedCompletionRecord) { r.Version = 2 },
		func(r *VerifiedCompletionRecord) { r.Phase = CompletionPrepared },
		func(r *VerifiedCompletionRecord) { r.Phase = "unknown" },
		func(r *VerifiedCompletionRecord) { r.Size++ },
		func(r *VerifiedCompletionRecord) { r.StagingIdentity.Inode++ },
		func(r *VerifiedCompletionRecord) { r.DirectoryIdentity.Inode++ },
		func(r *VerifiedCompletionRecord) { r.FinalName = "../escape" },
		func(r *VerifiedCompletionRecord) { r.FinalName = r.StagingName },
	} {
		f := newCompletionFixture(t, []byte("negative fixture"))
		r := f.record
		mutate(&r)
		s := newCompletionStoreFixture(r)
		permit, err := completerForTest(t).Complete(context.Background(), f.dir, f.file, r, s.hooks())
		if err == nil || permit.Fresh || s.done || s.commits != 0 {
			t.Fatalf("invalid or mismatched completion was admitted: %+v %v", permit, err)
		}
		assertCompletionAbsent(t, f)
	}
}

func TestVerifiedCompletionExistingFinalNeverReplaced(t *testing.T) {
	for _, kind := range []string{"different", "same_bytes", "symlink", "dangling_symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			f := newCompletionFixture(t, []byte("verified fixture"))
			final := filepath.Join(f.path, f.record.FinalName)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(f.record.StagingName, final)
			case "dangling_symlink":
				err = os.Symlink("absent-file", final)
			case "directory":
				err = os.Mkdir(final, 0700)
			case "same_bytes":
				err = os.WriteFile(final, f.body, 0600)
			default:
				err = os.WriteFile(final, []byte("external data"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(final)
			if err != nil {
				t.Fatal(err)
			}
			s := newCompletionStoreFixture(f.record)
			permit, err := completerForTest(t).Complete(context.Background(), f.dir, f.file, f.record, s.hooks())
			after, statErr := os.Lstat(final)
			if statErr != nil || !os.SameFile(before, after) {
				t.Fatal("external destination entry was replaced", statErr)
			}
			var e *VerifiedCompletionError
			if !errors.As(err, &e) || e.Kind != CompletionConflict || permit.Fresh || s.done {
				t.Fatalf("external final was accepted: %+v %v", permit, err)
			}
			if kind == "different" {
				data, err := os.ReadFile(final)
				if err != nil || string(data) != "external data" {
					t.Fatal("external contents changed", err)
				}
			}
		})
	}
}

func TestVerifiedCompletionStoreErrorsAndUnknownACK(t *testing.T) {
	for _, phase := range []VerifiedCompletionPhase{CompletionVerifying, CompletionVerified, CompletionPublished, CompletionCommitted} {
		for _, unknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/unknown=%v", phase, unknown), func(t *testing.T) {
				f := newCompletionFixture(t, []byte("durable fixture"))
				s := newCompletionStoreFixture(f.record)
				s.failSave, s.unknown = phase, unknown
				v := completerForTest(t)
				permit, err := v.Complete(context.Background(), f.dir, f.file, f.record, s.hooks())
				if err == nil || permit.Fresh {
					t.Fatalf("unconfirmed store granted success: %+v %v", permit, err)
				}
				if phase == CompletionVerifying || phase == CompletionVerified {
					assertCompletionAbsent(t, f)
				}
				// Reload the same authoritative record; never create another Task.
				s.failSave = ""
				permit, err = v.Complete(context.Background(), f.dir, f.file, s.record, s.hooks())
				if err != nil || !s.done || s.commits != 1 {
					t.Fatalf("same-ID recovery failed: %+v %v", permit, err)
				}
				if permit.Fresh == (phase == CompletionCommitted && unknown) {
					t.Fatal("fresh-vs-recovered permission was incorrect")
				}
			})
		}
	}
}

func TestVerifiedCompletionCancelAtAdmission(t *testing.T) {
	for _, at := range []int{3, 5} { // publication and commit admission respectively
		t.Run(fmt.Sprint(at), func(t *testing.T) {
			f := newCompletionFixture(t, bytes.Repeat([]byte("cancellable"), 10000))
			s := newCompletionStoreFixture(f.record)
			hooks := s.hooks()
			base := hooks.Fence
			ready, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			count := 0
			hooks.Fence = func(ctx context.Context, body func() error) error {
				count++
				if count == at {
					close(ready)
					<-release // Before admission, outside the lifecycle gate.
				}
				return base(ctx, body)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			v := completerForTest(t)
			go func() {
				permit, err := v.Complete(ctx, f.dir, f.file, f.record, hooks)
				if permit.Fresh {
					err = errors.New("canceled run granted success")
				}
				done <- err
			}()
			select {
			case <-ready:
			case err := <-done:
				t.Fatal("completion failed before the admission fixture", err)
			}
			s.invalidate() // Same gate decides invalidation before the short operation.
			cancel()
			releaseOnce.Do(func() { close(release) })
			if err := <-done; err == nil || s.done || s.commits != 0 {
				t.Fatal("invalidated run committed", err)
			}
			if at == 3 {
				assertCompletionAbsent(t, f)
			} else {
				if err := f.dir.checkFinal(f.file, f.record); err != nil {
					t.Fatal("pre-cancel publication lost its recovery anchor", err)
				}
			}
		})
	}
}

func TestVerifiedCompletionHashSlotCancellation(t *testing.T) {
	f := newCompletionFixture(t, []byte("bounded hash"))
	v := completerForTest(t)
	v.slots <- struct{}{}
	s := newCompletionStoreFixture(f.record)
	hooks := s.hooks()
	saved := make(chan struct{})
	base := hooks.Save
	hooks.Save = func(r VerifiedCompletionRecord) error {
		err := base(r)
		close(saved)
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		permit, err := v.Complete(ctx, f.dir, f.file, f.record, hooks)
		if permit.Fresh {
			err = errors.New("hash waiter granted success")
		}
		done <- err
	}()
	select {
	case <-saved:
	case err := <-done:
		t.Fatal("completion failed before the hash-slot fixture", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || len(v.slots) != 1 {
		t.Fatal("hash waiter did not cancel independently", err)
	}
	<-v.slots
	assertCompletionAbsent(t, f)
}

func TestVerifiedCompletionAnchorAndPathMutations(t *testing.T) {
	t.Run("staging_entry_replaced_after_hash", func(t *testing.T) {
		f := newCompletionFixture(t, []byte("immutable staged bytes"))
		s := newCompletionStoreFixture(f.record)
		hooks := s.hooks()
		base := hooks.Save
		hooks.Save = func(r VerifiedCompletionRecord) error {
			if r.Phase == CompletionVerified {
				if err := os.Rename(filepath.Join(f.path, r.StagingName), filepath.Join(f.path, "old-stage")); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(f.path, r.StagingName), f.body, 0600); err != nil {
					return err
				}
			}
			return base(r)
		}
		permit, err := completerForTest(t).Complete(context.Background(), f.dir, f.file, f.record, hooks)
		if err == nil || permit.Fresh || s.done {
			t.Fatal("changed staging name was accepted", err)
		}
		assertCompletionAbsent(t, f)
	})
	t.Run("root_replaced_does_not_redirect_pinned_handle", func(t *testing.T) {
		f := newCompletionFixture(t, []byte("pinned directory"))
		moved := f.path + "-moved"
		if err := os.Rename(f.path, moved); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(moved) })
		if err := os.Mkdir(f.path, 0700); err != nil {
			t.Fatal(err)
		}
		other, err := OpenCompletionDirectory(f.path)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		if _, err := other.OpenStaging(f.record); err == nil {
			t.Fatal("reopened replacement root was accepted")
		}
		s := newCompletionStoreFixture(f.record)
		if _, err := completerForTest(t).Complete(context.Background(), f.dir, f.file, f.record, s.hooks()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(f.path, f.record.FinalName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("publication was redirected into replacement root", err)
		}
	})
	t.Run("symlink_parent_rejected", func(t *testing.T) {
		f := newCompletionFixture(t, []byte("root"))
		link := filepath.Join(f.path, "alias")
		if err := os.Symlink(f.path, link); err != nil {
			t.Fatal(err)
		}
		if d, err := OpenCompletionDirectory(link); err == nil {
			_ = d.Close()
			t.Fatal("symlink directory was followed")
		}
	})
}

// Real local HTTP bytes cross a managed file boundary; this deliberately tests
// the module adapter, not production Task/HTTP/Core integration owned elsewhere.
func TestVerifiedCompletionRealHTTPBytes(t *testing.T) {
	body := bytes.Repeat([]byte("local HTTP representation\n"), 4096)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer source.Close()
	for _, matches := range []bool{true, false} {
		t.Run(fmt.Sprint(matches), func(t *testing.T) {
			f := newCompletionFixture(t, nil)
			digest := sha256.Sum256(body)
			f.record.ExpectedSHA256 = hex.EncodeToString(digest[:])
			if !matches {
				f.record.ExpectedSHA256 = strings.Repeat("0", 64)
			}
			f.record.Phase, f.record.Size = CompletionPrepared, -1
			db, err := bolt.Open(filepath.Join(f.path, "fixture.db"), 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := writeCompletionTestDB(db, f.record, false); err != nil {
				t.Fatal(err)
			}
			// Persist the pinned intent before receiving any HTTP representation.
			w, err := os.OpenFile(filepath.Join(f.path, f.record.StagingName), os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.Get(source.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			n, err := io.Copy(w, response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Sync(); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			f.record.Size, f.record.Phase, f.body = n, CompletionWaiting, body
			if err := writeCompletionTestDB(db, f.record, false); err != nil {
				t.Fatal(err)
			}
			permit, err := completerForTest(t).Complete(context.Background(), f.dir, f.file, f.record, completionBoltHooks(db, ""))
			if matches {
				if err != nil || !permit.Fresh || n != int64(len(body)) {
					t.Fatal("HTTP byte verification/commit failed", err)
				}
				actual, err := os.ReadFile(filepath.Join(f.path, f.record.FinalName))
				if err != nil || !bytes.Equal(actual, body) {
					t.Fatal("published HTTP bytes differ", err)
				}
			} else {
				if err == nil || permit.Fresh {
					t.Fatal("mismatched HTTP representation committed", err)
				}
				assertCompletionAbsent(t, f)
			}
			_, done, err := readCompletionTestDB(db)
			if err != nil || done != matches {
				t.Fatal("durable completion differs from expectation", err)
			}
		})
	}
}

func TestVerifiedCompletionPreparedSchema(t *testing.T) {
	f := newCompletionFixture(t, nil)
	r := f.record
	r.Phase, r.Size, r.DirectoryIdentity, r.StagingIdentity = CompletionPrepared, -1, CompletionFileIdentity{}, CompletionFileIdentity{}
	for _, names := range [][2]string{{"", ""}, {".stage", ""}, {"", "final"}, {".stage", "final"}} {
		r.StagingName, r.FinalName = names[0], names[1]
		if err := ValidateVerifiedCompletionRecord(r); err != nil {
			t.Fatal("prepared intent was not representable", err)
		}
		s := newCompletionStoreFixture(r)
		permit, err := completerForTest(t).Complete(context.Background(), f.dir, f.file, r, s.hooks())
		if err == nil || permit.Fresh || len(s.phases) != 0 {
			t.Fatal("prepared intent was treated as quiescent", err)
		}
	}
	for _, names := range [][2]string{{"../stage", ""}, {"", "../final"}, {".stage", ".STAGE"}} {
		r.StagingName, r.FinalName = names[0], names[1]
		if err := ValidateVerifiedCompletionRecord(r); err == nil {
			t.Fatal("invalid prepared names accepted")
		}
	}
	assertCompletionAbsent(t, f)
}

func TestVerifiedCompletionLateTokenWithoutContextCancellation(t *testing.T) {
	f := newCompletionFixture(t, []byte("late run"))
	s := newCompletionStoreFixture(f.record)
	hooks := s.hooks()
	base := hooks.Fence
	calls := 0
	hooks.Fence = func(ctx context.Context, body func() error) error {
		calls++
		if calls == 3 {
			s.invalidate()
		} // Invalidation wins before publication.
		return base(ctx, body)
	}
	permit, err := completerForTest(t).Complete(context.Background(), f.dir, f.file, f.record, hooks)
	var e *VerifiedCompletionError
	if !errors.As(err, &e) || e.Kind != CompletionStale || permit.Fresh || s.done {
		t.Fatal("old token was accepted", err)
	}
	assertCompletionAbsent(t, f)
}

func TestVerifiedCompletionDestinationChangedBeforeCommit(t *testing.T) {
	f := newCompletionFixture(t, []byte("verified bytes"))
	s := newCompletionStoreFixture(f.record)
	hooks := s.hooks()
	base := hooks.Save
	hooks.Save = func(r VerifiedCompletionRecord) error {
		if r.Phase == CompletionPublished {
			if err := os.Rename(filepath.Join(f.path, r.FinalName), filepath.Join(f.path, "published-original")); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(f.path, r.FinalName), []byte("external replacement"), 0600); err != nil {
				return err
			}
		}
		return base(r)
	}
	permit, err := completerForTest(t).Complete(context.Background(), f.dir, f.file, f.record, hooks)
	if err == nil || permit.Fresh || s.done {
		t.Fatal("replaced final committed", err)
	}
	data, err := os.ReadFile(filepath.Join(f.path, f.record.FinalName))
	if err != nil || string(data) != "external replacement" {
		t.Fatal("external replacement was modified", err)
	}
}

func TestVerifiedCompletionDuplicateCallsHaveOneCommitWinner(t *testing.T) {
	f := newCompletionFixture(t, bytes.Repeat([]byte("same immutable file"), 4096))
	s := newCompletionStoreFixture(f.record)
	v := completerForTest(t)
	results := make(chan VerifiedCompletionPermit, 2)
	for i := 0; i < 2; i++ {
		go func() {
			permit, _ := v.Complete(context.Background(), f.dir, f.file, f.record, s.hooks())
			results <- permit // A rejected late duplicate must never gain permission.
		}()
	}
	winners := 0
	for i := 0; i < 2; i++ {
		if (<-results).Fresh {
			winners++
		}
	}
	if winners != 1 || s.commits != 1 || !s.done {
		t.Fatalf("duplicate success admission: winners=%d commits=%d", winners, s.commits)
	}
}
