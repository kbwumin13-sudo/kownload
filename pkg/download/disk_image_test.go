//go:build darwin

package download

import (
	"bytes"
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/pkg/base"
)

func TestDMGDownloadChecksIntegrityBeforeSuccess(t *testing.T) {
	root := t.TempDir()
	content := filepath.Join(root, "source")
	if err := os.Mkdir(content, 0700); err != nil {
		t.Fatal(err)
	}
	sample := make([]byte, 1<<20)
	_, _ = rand.New(rand.NewSource(42)).Read(sample)
	if err := os.WriteFile(filepath.Join(content, "sample.bin"), sample, 0600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(root, "source.dmg")
	if output, err := exec.Command("/usr/bin/hdiutil", "create", "-srcfolder", content, "-format", "UDZO", good).CombinedOutput(); err != nil {
		t.Fatalf("create test image: %v\n%s", err, output)
	}
	body, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.Clone(body)
	const duplicateSize = 32 << 10
	if len(bad) < 3*duplicateSize {
		t.Fatal("test image is too small for the duplicate range")
	}
	start := len(bad) / 2
	copy(bad[start:start+duplicateSize], bad[start-duplicateSize:start])
	if bytes.Equal(bad, body) {
		t.Fatal("the duplicate interval did not corrupt the fixture")
	}
	for _, tt := range []struct {
		name        string
		data        []byte
		status      base.Status
		imageStatus string
		protected   bool
	}{
		{"good.dmg", body, base.DownloadStatusDone, diskImageVerified, false},
		{"bad.dmg", bad, base.DownloadStatusError, diskImageFailed, false},
		{"protected.dmg", body, base.DownloadStatusDone, diskImageVerified, true},
		{"protected-bad.dmg", bad, base.DownloadStatusError, diskImageFailed, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.ServeContent(w, r, tt.name, time.Time{}, bytes.NewReader(tt.data))
			}))
			defer server.Close()
			d := NewDownloader(&DownloaderConfig{StorageDir: t.TempDir()})
			if err := d.Setup(); err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			target, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			req := &base.Request{URL: server.URL + "/" + tt.name}
			opts := &base.Options{Path: target, Name: tt.name}
			var id string
			if tt.protected {
				identity, err := d.DatasetIdentity()
				if err != nil {
					t.Fatal(err)
				}
				result, err := d.CreatePathOperationForDataset(context.Background(), identity.DatasetID, HTTPPathCreateKind, HTTPPathCreateRequest{
					Version: 1, OperationID: "dmg-integrity", RequiredCapabilities: []string{HTTPPathRequiredCapability},
					PathIntent: PathIntent{Version: 1, Path: target, Origin: pathpolicy.UserSelected},
					Request:    &base.CreateTaskBatchItem{Req: req, Opts: opts},
				})
				if err != nil || len(result.Operation.TaskIDs) != 1 {
					t.Fatalf("create protected task: %v", err)
				}
				id = result.Operation.TaskIDs[0]
			} else if id, err = d.CreateDirect(req, opts); err != nil {
				t.Fatal(err)
			}
			outputPath := filepath.Join(target, tt.name)
			for deadline := time.Now().Add(15 * time.Second); ; {
				task := d.GetTask(id)
				if task != nil && (task.Status == base.DownloadStatusDone || task.Status == base.DownloadStatusError) {
					if task.Status != tt.status {
						t.Fatalf("status = %s, want %s", task.Status, tt.status)
					}
					if task.Progress.DiskImageStatus != tt.imageStatus {
						t.Fatalf("image status = %s, want %s", task.Progress.DiskImageStatus, tt.imageStatus)
					}
					if tt.protected && tt.status == base.DownloadStatusError {
						if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
							t.Fatalf("invalid output was published: %v", err)
						}
						outputPath = filepath.Join(target, task.PathFreeze.Output.Binding.StagingName)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("task did not reach a terminal status")
				}
				time.Sleep(10 * time.Millisecond)
			}
			output, err := os.ReadFile(outputPath)
			if err != nil || !bytes.Equal(output, tt.data) {
				t.Fatalf("existing download bytes were changed: %v", err)
			}
		})
	}
}
