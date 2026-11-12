package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	netdiskProtocol "github.com/GopeedLab/gopeed/internal/protocol/netdisk"
	"github.com/GopeedLab/gopeed/pkg/base"
	httpModel "github.com/GopeedLab/gopeed/pkg/protocol/http"
	netdiskModel "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

// Only the remote provider is controlled. The downloader, controller, parent
// owner, APIClient, HTTP child, dataset and Bolt transactions are real.
type parentBudgetProvider struct {
	cloud.Provider
	api string
	session, account, fileID string
	selected atomic.Int32
}

func (*parentBudgetProvider) Info() netdiskModel.ProviderInfo {
	return netdiskModel.ProviderInfo{ID: "quark", Hosts: []string{"pan.quark.cn"}, APIHosts: []string{"127.0.0.1"}}
}

func (provider *parentBudgetProvider) Resolve(ctx context.Context, client *cloud.APIClient, session *cloud.Session, _ netdiskModel.Source, file netdiskModel.File) (*cloud.Download, error) {
	if session == nil || session.ID != provider.session || session.Capture.AccountID != provider.account || file.ID != provider.fileID {
		return nil, fmt.Errorf("managed parent selected another session/account/file")
	}
	provider.selected.Add(1)
	var result struct { URL string `json:"url"`; File netdiskModel.File `json:"file"` }
	if err := client.JSON(ctx, http.MethodGet, provider.api, nil, nil, &result); err != nil {
		return nil, err
	}
	if result.File.ID != file.ID || result.File.Size != file.Size || result.File.Hash != file.Hash {
		return nil, fmt.Errorf("provider response did not identify the original selected file")
	}
	return &cloud.Download{URL: result.URL, File: result.File}, nil
}

func TestNetdiskActualCoreRefreshPreservesEpisodeAndBytes(t *testing.T) {
	// Above the HTTP child's prefetch-only threshold; an actual Range stream
	// must write a prefix and consume one paid retry before link 1 expires.
	body := make([]byte, (1<<20)+4096)
	for index := range body { body[index] = byte((index*31 + index/251) % 251) }
	digest := sha256.Sum256(body)
	var apiCalls, firstRanges, refreshedRanges atomic.Int32
	refreshEntered, refreshRelease := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseRefresh := func() { releaseOnce.Do(func() { close(refreshRelease) }) }
	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/link" {
			sequence := apiCalls.Add(1)
			if sequence == 2 {
				close(refreshEntered)
				select { case <-refreshRelease: case <-request.Context().Done(): return }
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"url": fmt.Sprintf("%s/file?link=%d", baseURL, sequence),"file":netdiskModel.File{ID:"controlled-file",Size:int64(len(body)),Hash:"sha256:"+hex.EncodeToString(digest[:])}})
			return
		}
		if request.URL.Path != "/file" { http.NotFound(w, request); return }
		link := request.URL.Query().Get("link")
		start, end := 0, len(body)-1
		rawRange := request.Header.Get("Range")
		if rawRange != "" {
			if _, err := fmt.Sscanf(rawRange, "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= len(body) {
				t.Errorf("invalid actual range: %q", rawRange); w.WriteHeader(416); return
			}
			if link == "1" && firstRanges.Add(1) > 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if link != "1" { refreshedRanges.Add(1) }
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"same-controlled-object"`)
		w.Header().Set("Content-Disposition", `attachment; filename="server-name-must-not-move-output.bin"`)
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		if rawRange != "" { w.WriteHeader(http.StatusPartialContent) }
		if link == "1" && rawRange == "" {
			// Metadata is real; prefetch is interrupted by the real Start path.
			w.(http.Flusher).Flush()
			<-request.Context().Done()
			return
		}
		if link == "1" {
			_, _ = w.Write(body[start:min(start+8192,end+1)])
			return // A real truncated body, followed by a paid retry -> 403.
		}
		_, _ = w.Write(body[start:end+1])
	}))
	baseURL = server.URL
	t.Cleanup(server.Close)
	t.Cleanup(releaseRefresh)
	root := t.TempDir()
	if evidence := os.Getenv("K17_NETDISK_CORE_EVIDENCE"); evidence != "" {
		if err := os.MkdirAll(evidence,0700); err != nil { t.Fatal(err) }
		var err error
		root,err = os.MkdirTemp(evidence,"actual-writer-")
		if err != nil { t.Fatal(err) }
	}
	outputRoot := filepath.Join(root,"output")
	provider := &parentBudgetProvider{api:server.URL+"/api/link",account:"synthetic-account",fileID:"controlled-file"}
	service := cloud.NewService(provider)
	manager := &netdiskProtocol.FetcherManager{Service: service}
	captured, err := service.Capture("", "", netdiskModel.Capture{Provider:"quark",SourcePage:"https://pan.quark.cn/s/controlled",InstanceID:"fixture",ContextID:"fixture",AccountID:"synthetic-account"})
	if err != nil { t.Fatal(err) }
	provider.session = captured.SessionID
	store := NewBoltStorage(filepath.Join(root,"db"))
	d := NewDownloader(&DownloaderConfig{Storage:store,StorageDir:root,RefreshInterval:50,FetchManagers:[]fetcher.FetcherManager{manager}})
	if err = d.Setup(); err != nil { store.Close(); t.Fatal(err) }
	closed := false
	t.Cleanup(func(){ releaseRefresh(); if !closed { _ = d.Close() } })
	identity, err := d.DatasetIdentity()
	if err != nil { t.Fatal(err) }
	req := &base.Request{URL:captured.Source.URL,Protocol:"netdisk",Extra:&netdiskModel.ReqExtra{Provider:"quark",Source:captured.Source,SessionID:captured.SessionID,AccountID:"synthetic-account",File:netdiskModel.File{ID:"controlled-file",Name:"artifact.bin",Path:"folder",Size:int64(len(body)),Hash:"sha256:"+hex.EncodeToString(digest[:])}}}
	opts := &base.Options{Path:outputRoot,Name:"chosen.bin",Extra:&httpModel.OptsExtra{Connections:1}}
	id,err := d.CreateDirect(req,opts)
	if err != nil { t.Fatal(err) }
	select {
	case <-refreshEntered:
	case <-time.After(8*time.Second):
		t.Fatalf("real Core never reached charged provider refresh: task=%+v API=%d range=%d",d.GetTask(id),apiCalls.Load(),firstRanges.Load())
	}
	beforeTask,before,err := d.sharedRecoveryExpected(identity.DatasetID,id)
	if err != nil { t.Fatal(err) }
	if before.Record.Version != 4 || before.Record.Episode == "" || before.Record.RefreshUsed != 1 || before.Record.TransportUsed < 1 || firstRanges.Load() < 2 {
		t.Fatalf("refresh bypassed real parent ledger: %+v ranges=%d",before.Record,firstRanges.Load())
	}
	wantedPath := filepath.Join(outputRoot,"folder","chosen.bin")
	if beforeTask.Meta.SingleFilepath() != wantedPath { t.Fatalf("path changed before refresh: %q",beforeTask.Meta.SingleFilepath()) }
	rawParent,_ := json.Marshal(beforeTask.Meta.Req)
	freezeView := func(value *TaskPathFreeze) []byte {
		raw,_ := json.Marshal(value)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw,&fields)
		stable := map[string]json.RawMessage{}
		for _, key := range []string{"version","requiredCapability","decision","layout","layoutHash","treeLayoutHash"} {
			if value,exists := fields[key]; exists { stable[key] = value }
		}
		raw,_ = json.Marshal(stable)
		return raw
	}
	var beforeFreeze json.RawMessage
	if beforeTask.PathFreeze != nil { beforeFreeze = freezeView(beforeTask.PathFreeze) }
	releaseRefresh()
	waitStatus(t,d,id,base.DownloadStatusDone,8*time.Second)
	afterTask,after,err := d.sharedRecoveryExpected(identity.DatasetID,id)
	if err != nil { t.Fatal(err) }
	if afterTask.ID != id || len(d.GetTasks()) != 1 || after.Record.Episode != before.Record.Episode || after.Record.Generation != before.Record.Generation ||
		after.Record.TransportUsed != before.Record.TransportUsed || after.Record.RefreshUsed != before.Record.RefreshUsed || after.Record.DeferralUsed != before.Record.DeferralUsed ||
		after.Record.TotalUsed != before.Record.TotalUsed || after.Record.NextAction != before.Record.NextAction || after.Record.SourceRevision != before.Record.SourceRevision+1 {
		t.Fatalf("source replacement reset/forked the parent budget: before=%+v after=%+v",before.Record,after.Record)
	}
	if after.Record.Coverage == nil {
		if before.Record.Coverage != nil { t.Fatal("source replacement discarded prior coverage") }
	} else if before.Record.Coverage == nil || after.Record.Coverage.Status != "unprovable" || after.Record.Coverage.SettlementSequence != before.Record.Coverage.SettlementSequence {
		t.Fatal("unknown provider UID fabricated or reset coverage rewards")
	}
	afterRaw,_ := json.Marshal(afterTask.Meta.Req)
	if !bytes.Equal(rawParent,afterRaw) || afterTask.Meta.SingleFilepath() != wantedPath || apiCalls.Load() != 2 || provider.selected.Load() != 2 || refreshedRanges.Load() == 0 {
		t.Fatalf("raw request, path or finite plan changed: API=%d newRanges=%d path=%q",apiCalls.Load(),refreshedRanges.Load(),afterTask.Meta.SingleFilepath())
	}
	if beforeTask.PathFreeze != nil {
		afterFreeze := freezeView(afterTask.PathFreeze)
		if !bytes.Equal(beforeFreeze,afterFreeze) { t.Fatal("frozen root/layout changed during source replacement") }
	}
	actual,err := os.ReadFile(wantedPath)
	if err != nil || !bytes.Equal(actual,body) || sha256.Sum256(actual) != digest { t.Fatalf("actual full bytes mismatch: %v bytes=%d",err,len(actual)) }
	if err = d.Close(); err != nil { t.Fatal(err) }
	closed = true
	manifest := struct {
		Source string `json:"source"`
		DatasetID string `json:"datasetId"`
		TaskID string `json:"taskId"`
		StoreDir string `json:"storeDir"`
		Output string `json:"output"`
		OutputSHA string `json:"outputSHA256"`
		Status base.Status `json:"status"`
		Record recoveryRecord `json:"record"`
		Required recoveryRequirement `json:"required"`
		Before,After recoveryRecord
		ActualPathFreeze bool `json:"actualPathFreeze"`
	}{os.Getenv("K17_NETDISK_SOURCE"),identity.DatasetID,id,filepath.Join(root,"db"),wantedPath,hex.EncodeToString(digest[:]),afterTask.Status,after.Record,after.Required,before.Record,after.Record,beforeTask.PathFreeze!=nil}
	raw,err := json.MarshalIndent(manifest,"","  ")
	if err != nil { t.Fatal(err) }
	if err = os.WriteFile(filepath.Join(root,"writer-manifest.json"),append(raw,'\n'),0600); err != nil { t.Fatal(err) }
	t.Logf("actual closed netdisk writer: %s Task=%s E=%s transport=%d refresh=%d total=%d fullSHA=%s pathFreeze=%v",root,id,after.Record.Episode,after.Record.TransportUsed,after.Record.RefreshUsed,after.Record.TotalUsed,hex.EncodeToString(digest[:]),beforeTask.PathFreeze!=nil)
}
