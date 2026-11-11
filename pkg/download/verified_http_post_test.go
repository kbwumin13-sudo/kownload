package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestVerifiedHTTPNewMPostRedirectCompletesThroughSamePinnedOutput(t *testing.T) {
	d, _, root := scopedOperationFixture(t, true)
	body := bytes.Repeat([]byte("verified redirected payload\n"), 4096)
	var posts, gets, ranges atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("target method %s", r.Method)
			w.WriteHeader(400)
			return
		}
		data, _ := io.ReadAll(r.Body)
		if len(data) != 0 {
			t.Errorf("redirected target received original body")
			w.WriteHeader(400)
			return
		}
		gets.Add(1)
		if r.Header.Get("Range") != "" {
			ranges.Add(1)
		}
		w.Header().Set("ETag", `"verified-post-redirect"`)
		http.ServeContent(w, r, "post.bin", time.Time{}, bytes.NewReader(body))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("source method %s", r.Method)
			w.WriteHeader(400)
			return
		}
		data, _ := io.ReadAll(r.Body)
		if string(data) != "explicit original body" {
			t.Errorf("source body changed")
			w.WriteHeader(400)
			return
		}
		posts.Add(1)
		http.Redirect(w, r, target.URL+"/payload", http.StatusSeeOther)
	}))
	defer source.Close()
	sum := sha256.Sum256(body)
	item := &base.CreateTaskBatchItem{Req: &base.Request{URL: source.URL + "/entry", Extra: map[string]any{"method": "POST", "body": "explicit original body"}}, Opts: &base.Options{Path: root, Name: "post.bin", Extra: map[string]any{"connections": 1}}}
	request := VerifiedHTTPRequest{1, "post-verified", item, VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", hex.EncodeToString(sum[:])}}
	result, err := d.CreateVerifiedHTTPOperationForDataset(context.Background(), scopedDatasetA, request)
	if err != nil || len(result.Operation.TaskIDs) != 1 {
		t.Fatal(result, err)
	}
	id := result.Operation.TaskIDs[0]
	waitStatus(t, d, id, base.DownloadStatusDone, 5*time.Second)
	output, err := os.ReadFile(filepath.Join(root, "post.bin"))
	if err != nil || !bytes.Equal(output, body) {
		t.Fatal("same stage completion body changed", err)
	}
	if posts.Load() != 1 || gets.Load() < 2 || ranges.Load() < 1 {
		t.Fatal("did not use first POST plus redirected range continuation", posts.Load(), gets.Load(), ranges.Load())
	}
	query, err := d.QueryVerifiedHTTPOperationForDataset(scopedDatasetA, request.OperationID)
	if err != nil || len(query.CompletionViews) != 1 || query.CompletionViews[0].Phase != "committed" {
		t.Fatal(query, err)
	}
}
