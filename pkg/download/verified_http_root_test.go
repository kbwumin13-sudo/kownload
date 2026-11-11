package download

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestVerifiedHTTPRootGlobCannotResolveLiteralStarSymlink(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(allowed, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	literalStar := filepath.Join(allowed, "*")
	if err := os.Symlink(outside, literalStar); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte("owned synthetic bytes")) }))
	defer source.Close()
	d, store, _ := scopedOperationFixture(t, false)
	d.cfg.WhiteDownloadDirs = []string{filepath.Join(allowed, "*")}
	request := operationRequest(source.URL+"/data", literalStar)
	request.integrity = &VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", strings.Repeat("a", 64)}
	if _, err := d.createBatchOperationInDataset(context.Background(), request, scopedDatasetA); !errors.Is(err, ErrVerifiedHTTPUnsupportedOption) {
		t.Fatal("Core accepted glob symlink destination outside the allowed pattern", err)
	}
	record, err := d.loadOperationInDataset(scopedDatasetA, request.OperationID)
	if err != nil || record != nil || hits.Load() != 0 {
		t.Fatal("denied root wrote R or started IO", err, hits.Load())
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("denied root changed outside directory", err)
	}
	var tasks []*Task
	if err := store.List(bucketTask, &tasks); err != nil || len(tasks) != 0 {
		t.Fatal("denied root created Task", err)
	}
}

func TestVerifiedHTTPRootPatternPreservesMatchSyntax(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tail := range []string{"*", "file?", "[ab]*", `literal\*`, "child/*/../other"} {
		pattern := root + string(filepath.Separator) + tail
		got, err := canonicalVerifiedPattern(pattern)
		if err != nil || got != pattern {
			t.Fatal("glob syntax changed", pattern, got, err)
		}
	}
	if _, err := canonicalVerifiedPattern(root + "/["); !errors.Is(err, ErrVerifiedHTTPUnsupportedOption) {
		t.Fatal("malformed pattern became permission", err)
	}
}
