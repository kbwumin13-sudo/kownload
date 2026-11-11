package download

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

type verifiedContextManager struct {
	httpProtocol.FetcherManager
	supported atomic.Bool
}
type verifiedContextFetcher struct {
	*httpProtocol.Fetcher
	manager *verifiedContextManager
}

func (f *verifiedContextFetcher) CaptureRecoveryContext(req *base.Request) (*transfer.HTTPExecutionContext, bool, error) {
	if !f.manager.supported.Load() {
		return nil, false, nil
	}
	return f.Fetcher.CaptureRecoveryContext(req)
}
func (m *verifiedContextManager) Build() fetcher.Fetcher {
	return &verifiedContextFetcher{Fetcher: m.FetcherManager.Build().(*httpProtocol.Fetcher), manager: m}
}
func (m *verifiedContextManager) Restore() (any, func(*fetcher.FetcherMeta, any) fetcher.Fetcher) {
	data, restore := m.FetcherManager.Restore()
	return data, func(meta *fetcher.FetcherMeta, data any) fetcher.Fetcher {
		return &verifiedContextFetcher{Fetcher: restore(meta, data).(*httpProtocol.Fetcher), manager: m}
	}
}
func (m *verifiedContextManager) Store(f fetcher.Fetcher) (any, error) {
	return m.FetcherManager.Store(f.(*verifiedContextFetcher).Fetcher)
}

func TestVerifiedHTTPUnsupportedContextBeforeReservationAndResolve(t *testing.T) {
	for _, bolt := range []bool{false, true} {
		t.Run(map[bool]string{false: "mem", true: "bolt"}[bolt], func(t *testing.T) {
			root := t.TempDir()
			var store AtomicStorage = NewMemStorage()
			if bolt {
				store = NewBoltStorage(filepath.Join(root, "db"))
			}
			manager := &verifiedContextManager{}
			d := NewDownloader(&DownloaderConfig{Storage: store, StorageDir: root, FetchManagers: []fetcher.FetcherManager{manager}})
			if err := d.Setup(); err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := InitializeDatasetIdentity(store); err != nil {
				t.Fatal(err)
			}
			setScopedDataset(t, store, scopedDatasetA)
			var hits atomic.Int32
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte("must not resolve")) }))
			defer source.Close()
			request := operationRequest(source.URL, root)
			request.integrity = &VerifiedHTTPIntegrityIntent{1, verifiedHTTPCapability, "sha256", strings.Repeat("a", 64)}
			items, hash, err := canonicalBatch(request)
			if err != nil {
				t.Fatal(err)
			}
			if err = d.reserveOperationInDataset(store, request, items, hash, scopedDatasetA); !errors.Is(err, ErrVerifiedHTTPUnsupportedOption) {
				t.Fatal("unsupported context created R", err)
			}
			if record, err := d.loadOperationInDataset(scopedDatasetA, request.OperationID); err != nil || record != nil {
				t.Fatal("unsupported R persisted", err)
			}
			if hits.Load() != 0 || len(d.GetTasks()) != 0 {
				t.Fatal("pre-R unsupported performed IO or created Task")
			}
			manager.supported.Store(true)
			if err = d.reserveOperationInDataset(store, request, items, hash, scopedDatasetA); err != nil {
				t.Fatal(err)
			}
			manager.supported.Store(false)
			// A same-R replay retains its raw fingerprint and frozen execution policy.
			if err = d.reserveOperationInDataset(store, request, items, hash, scopedDatasetA); err != nil {
				t.Fatal("same-R policy reinterpreted", err)
			}
			result, err := d.createBatchOperationInDataset(context.Background(), request, scopedDatasetA)
			if err != nil || len(result.TaskIDs) != 1 {
				t.Fatal(err, result)
			}
			waitStatus(t, d, result.TaskIDs[0], base.DownloadStatusError, 3*time.Second)
			if hits.Load() != 0 {
				t.Fatal("unsupported runtime resolved before held", hits.Load())
			}
		})
	}
}
