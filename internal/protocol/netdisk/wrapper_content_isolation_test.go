package netdisk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

type isolationProvider struct{ fixtureProvider }

func (p *isolationProvider) Resolve(_ context.Context, _ *cloud.APIClient, _ *cloud.Session, _ model.Source, file model.File) (*cloud.Download, error) {
	return &cloud.Download{URL: p.url + "/" + file.ID, File: file}, nil
}

type isolationReader struct{ *bytes.Reader }

func (r isolationReader) Read(b []byte) (int, error) {
	if len(b) > 8192 {
		b = b[:8192]
	}
	time.Sleep(time.Millisecond)
	return r.Reader.Read(b)
}

func isolationFixture(t *testing.T, slow bool) (*FetcherManager, []byte, *base.Request) {
	t.Helper()
	body := make([]byte, 2*1024*1024+123)
	for i := range body {
		body[i] = byte((i*31 + i/251) % 251)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			http.Error(w, "unknown fixture", 404)
			return
		}
		data := append([]byte(nil), body...)
		for i := range data {
			data[i] ^= byte(n)
		}
		w.Header().Set("Content-Disposition", `attachment; filename="same-remote-name.bin"`)
		var reader io.ReadSeeker = bytes.NewReader(data)
		if slow {
			reader = isolationReader{bytes.NewReader(data)}
		}
		http.ServeContent(w, r, "same-remote-name.bin", time.Unix(1700000000, 0), reader)
	}))
	t.Cleanup(srv.Close)
	p := &isolationProvider{fixtureProvider{url: srv.URL}}
	s := cloud.NewService(p)
	t.Cleanup(s.Close)
	cap, err := s.Capture("", "", model.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/isolation", InstanceID: "fixture", ContextID: "fixture", AccountID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	req := &base.Request{URL: cap.Source.URL, Protocol: "netdisk", Extra: &model.ReqExtra{Provider: "quark", Source: cap.Source, SessionID: cap.SessionID, AccountID: "fixture"}}
	return &FetcherManager{Service: s}, body, req
}

func isolationRequest(req *base.Request, body []byte, id int) (*base.Request, []byte) {
	req = clone(req)
	_ = base.ParseReqExtra[model.ReqExtra](req)
	data := append([]byte(nil), body...)
	for i := range data {
		data[i] ^= byte(id)
	}
	hash := sha256.Sum256(data)
	req.Extra.(*model.ReqExtra).File = model.File{ID: strconv.Itoa(id), Name: "same-model-name.bin", Path: fmt.Sprintf("folder/%d", id), Size: int64(len(data)), Hash: "sha256:" + hex.EncodeToString(hash[:])}
	return req, data
}

func TestWrapperIsolatesNamedPrefetchAcrossTasks(t *testing.T) {
	m, body, baseReq := isolationFixture(t, false)
	dir := t.TempDir()
	fs := make([]*Fetcher, 4)
	wants := make([][]byte, len(fs))
	for i := range fs {
		req, data := isolationRequest(baseReq, body, i)
		wants[i] = data
		f := m.Build().(*Fetcher)
		f.Setup(controller.NewController())
		t.Cleanup(func() { _ = f.Close() })
		if err := f.Resolve(req, &base.Options{Path: dir, Name: "custom-name.bin"}); err != nil {
			t.Fatal(err)
		}
		fs[i] = f
	}
	// Every resolve can prefetch before any child opens its final path.
	for _, f := range fs {
		if err := f.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, f := range fs {
		if err := f.Wait(); err != nil {
			t.Fatal(err)
		}
		wantPath := filepath.Join(dir, "folder", strconv.Itoa(i), "custom-name.bin")
		if f.Meta().SingleFilepath() != wantPath {
			t.Fatalf("task %d path = %q, want %q", i, f.Meta().SingleFilepath(), wantPath)
		}
		data, err := os.ReadFile(wantPath)
		if err != nil || !bytes.Equal(data, wants[i]) {
			t.Fatalf("task %d content changed: %v", i, err)
		}
	}
}

func TestWrapperStoredRestorePreservesExactContent(t *testing.T) {
	m, body, baseReq := isolationFixture(t, true)
	req, want := isolationRequest(baseReq, body, 7)
	f := m.Build().(*Fetcher)
	f.Setup(controller.NewController())
	t.Cleanup(func() { _ = f.Close() })
	if err := f.Resolve(req, &base.Options{Path: t.TempDir(), Name: "renamed.bin"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.Progress().TotalDownloaded() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := f.Pause(); err != nil {
		t.Fatal(err)
	}
	if got := f.Progress().TotalDownloaded(); got <= 0 || got >= int64(len(want)) {
		t.Fatalf("fixture did not pause mid-file: %d", got)
	}
	data, err := m.Store(f)
	if err != nil {
		t.Fatal(err)
	}
	meta := clone(f.Meta())
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	template, restore := m.Restore()
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, template); err != nil {
		t.Fatal(err)
	}
	resumed := restore(meta, template).(*Fetcher)
	resumed.Setup(controller.NewController())
	t.Cleanup(func() { _ = resumed.Close() })
	if err := resumed.Start(); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Wait(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(resumed.Meta().SingleFilepath())
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("stored restore changed content: %v", err)
	}
}
