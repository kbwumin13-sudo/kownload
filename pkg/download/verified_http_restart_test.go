package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type verifiedCrashManifest struct {
	DB, Root, TaskID, FileSHA string
	Budget                    recoveryRecord
}

// This helper deliberately exits without Close, leaving the actual Bolt commit
// and pinned staging files for the parent's new Downloader/explicit Continue.
func TestVerifiedHTTPCrashHelper(t *testing.T) {
	manifestPath := os.Getenv("K15_VERIFIED_CRASH_MANIFEST")
	if manifestPath == "" {
		return
	}
	body := bytes.Repeat([]byte("restart verifies same staging\n"), 2000)
	d, task, _, lease := verifiedWrittenFixture(t, body)
	_, budget, err := readRecovery(d.storage, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	manifest := verifiedCrashManifest{Root: task.Meta.Opts.Path, TaskID: task.ID, FileSHA: hex.EncodeToString(sum[:]), Budget: budget}
	// The fixture freezes the physical output directory but the DB was opened
	// through its original t.TempDir alias; both name the same owned directory.
	manifest.DB = filepath.Join(manifest.Root, "db")
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(manifestPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("K15_VERIFIED_CRASH_PHASE") == "committed" {
		fresh, err := d.completeVerifiedHTTP(task, quiescedVerifiedWriter{}, lease.Token.Generation, lease)
		if err != nil || !fresh {
			t.Fatal(fresh, err)
		}
		os.Exit(0) // Accepted C with no live delivery before process death.
	}
	gated, gate := gateVerifiedLease(lease)
	go func() { _, _ = d.completeVerifiedHTTP(task, quiescedVerifiedWriter{}, lease.Token.Generation, gated) }()
	waitVerifiedGate(t, gate)
	_, record, err := d.readVerifiedTask(scopedDatasetA, task.ID)
	if err != nil || record.Phase != transfer.CompletionVerifying {
		t.Fatal(record.Phase, err)
	}
	os.Exit(0)
}

func TestVerifiedHTTPRealProcessRestartSealedAndCommitted(t *testing.T) {
	for _, phase := range []string{"verifying", "committed"} {
		t.Run(phase, func(t *testing.T) {
			private := t.TempDir()
			manifestPath := filepath.Join(private, "manifest.json")
			child := exec.Command(os.Args[0], "-test.run=^TestVerifiedHTTPCrashHelper$", "-test.timeout=20s")
			child.Dir = private
			child.Env = append(os.Environ(), "K15_VERIFIED_CRASH_MANIFEST="+manifestPath, "K15_VERIFIED_CRASH_PHASE="+phase, "TMPDIR="+private)
			if out, err := child.CombinedOutput(); err != nil {
				t.Fatalf("actual crash child: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var m verifiedCrashManifest
			if err = json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			// os.Exit bypasses testing cleanup, so the parent owns this one generated
			// fixture root and removes it only after Close and preservation checks.
			defer os.RemoveAll(m.Root)
			store := NewBoltStorage(m.DB)
			d := NewDownloader(&DownloaderConfig{Storage: store, StorageDir: m.Root, StartupTaskPolicy: StartupTaskPolicyImportedReview, FetchManagers: []fetcher.FetcherManager{&httpProtocol.FetcherManager{}}})
			if err = d.Setup(); err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			task := d.getTask(m.TaskID)
			if task == nil || task.fetcher != nil || task.verifiedSuccess.Load() != nil {
				t.Fatal("Setup restored IO or success permission")
			}
			events, release, err := d.SubscribeVerifiedHTTPDone(context.Background(), scopedDatasetA)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if err = d.Continue(&TaskFilter{IDs: []string{task.ID}}); err != nil && !(phase == "committed" && errors.Is(err, ErrTaskNotFound)) {
				t.Fatal(err)
			}
			waitStatus(t, d, task.ID, base.DownloadStatusDone, 4*time.Second)
			owned, record, err := d.readVerifiedTask(scopedDatasetA, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if owned.ID != m.TaskID || record.Phase != transfer.CompletionCommitted || task.fetcher != nil {
				t.Fatal("cold sealed path restored fetcher or replaced task")
			}
			contents, err := os.ReadFile(filepath.Join(m.Root, record.FinalName))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(contents)
			if hex.EncodeToString(sum[:]) != m.FileSHA {
				t.Fatal("cold output changed")
			}
			_, budget, err := readRecovery(store, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if budget.Episode != m.Budget.Episode || budget.TotalUsed != m.Budget.TotalUsed || budget.TransportUsed != m.Budget.TransportUsed || budget.SourceRevision != m.Budget.SourceRevision || budget.ExecutionDefaultsHash != m.Budget.ExecutionDefaultsHash || budget.NotBefore != m.Budget.NotBefore {
				t.Fatal("restart reset source/budget")
			}
			if phase == "committed" {
				if task.verifiedSuccess.Load() != nil {
					t.Fatal("committed restart issued fresh")
				}
				select {
				case event := <-events:
					t.Fatal("committed restart replayed live success", event)
				default:
				}
			} else {
				select {
				case event := <-events:
					if event.TaskID != m.TaskID {
						t.Fatal(event)
					}
				case <-time.After(time.Second):
					t.Fatal("new confirmed C missing event")
				}
			}
		})
	}
}
