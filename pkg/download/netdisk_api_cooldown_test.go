package download

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	cloud "github.com/GopeedLab/gopeed/internal/netdisk"
	httpProtocol "github.com/GopeedLab/gopeed/internal/protocol/http"
	netdiskProtocol "github.com/GopeedLab/gopeed/internal/protocol/netdisk"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	netdiskModel "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

const cooldownAccountAPI = transfer.SourceKey("api|quark|fixture-account")

// cooldownProvider resolves every file with one provider API call. The API
// host is a fake round tripper and files come from a local server, so nothing
// leaves the machine and no account is involved.
type cooldownProvider struct {
	cloud.Provider
	files    string
	mu       sync.Mutex
	resolves map[string]int
	apiCalls []time.Time
}

func (*cooldownProvider) Info() netdiskModel.ProviderInfo {
	return netdiskModel.ProviderInfo{ID: "quark", Hosts: []string{"pan.quark.cn"}}
}

func (p *cooldownProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.apiCalls = append(p.apiCalls, time.Now())
	p.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
}

func (p *cooldownProvider) Resolve(ctx context.Context, c *cloud.APIClient, _ *cloud.Session, _ netdiskModel.Source, file netdiskModel.File) (*cloud.Download, error) {
	p.mu.Lock()
	p.resolves[file.ID]++
	p.mu.Unlock()
	c.Client.Transport = p
	res, err := c.Do(ctx, http.MethodGet, "https://pan.quark.cn/api/link?fid="+file.ID, nil, nil)
	if err != nil {
		return nil, err
	}
	res.Body.Close()
	return &cloud.Download{URL: p.files + "/" + file.Name, File: file}, nil
}

func (p *cooldownProvider) resolveCount(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resolves[id]
}

func (p *cooldownProvider) calls() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.apiCalls...)
}

type cooldownFixture struct {
	d        *Downloader
	provider *cooldownProvider
	source   netdiskModel.Source
	session  string
	data     []byte
	files    *httptest.Server
	dir      string
	// blocker.bin is served only after unblock, so a task can hold a slot.
	release     chan struct{}
	releaseOnce sync.Once
}

func newCooldownFixture(t *testing.T, maxRunning int) *cooldownFixture {
	t.Helper()
	return newCooldownFixtureWith(t, maxRunning, func(p *cooldownProvider) cloud.Provider { return p })
}

// newCooldownFixtureWith lets a test wrap the provider the service uses.
func newCooldownFixtureWith(t *testing.T, maxRunning int, wrap func(*cooldownProvider) cloud.Provider) *cooldownFixture {
	t.Helper()
	f := &cooldownFixture{data: rateLimitPayload(64 << 10), dir: t.TempDir(), release: make(chan struct{})}
	f.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/blocker.bin" {
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			}
		}
		http.ServeContent(w, r, "f.bin", time.Unix(1700000000, 0), bytes.NewReader(f.data))
	}))
	t.Cleanup(f.files.Close)
	f.provider = &cooldownProvider{files: f.files.URL, resolves: map[string]int{}}
	service := cloud.NewService(wrap(f.provider))
	captured, err := service.Capture("", "", netdiskModel.Capture{Provider: "quark", SourcePage: "https://pan.quark.cn/s/fixture",
		InstanceID: "fixture-browser", ContextID: "fixture-tab", AccountID: "fixture-account"})
	if err != nil {
		t.Fatal(err)
	}
	f.source, f.session = captured.Source, captured.SessionID
	f.d = NewDownloader(&DownloaderConfig{Storage: NewMemStorage(), StorageDir: t.TempDir(), RefreshInterval: 50,
		FetchManagers: []fetcher.FetcherManager{&netdiskProtocol.FetcherManager{Service: service}, &httpProtocol.FetcherManager{}}})
	if err := f.d.Setup(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := f.d.GetConfig()
	cfg.MaxRunning = maxRunning
	if err := f.d.PutConfig(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.unblock()
		_ = f.d.Close()
	})
	return f
}

func (f *cooldownFixture) unblock() { f.releaseOnce.Do(func() { close(f.release) }) }

func (f *cooldownFixture) netdisk(t *testing.T, name string) string {
	t.Helper()
	req := &base.Request{URL: f.source.URL, Protocol: "netdisk", Extra: &netdiskModel.ReqExtra{
		Provider: "quark", Source: f.source, SessionID: f.session, AccountID: "fixture-account",
		File: netdiskModel.File{ID: name, Name: name + ".bin", Size: int64(len(f.data))}}}
	id, err := f.d.CreateDirect(req, &base.Options{Path: f.dir})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *cooldownFixture) http(t *testing.T, name string) string {
	t.Helper()
	id, err := f.d.CreateDirect(&base.Request{URL: f.files.URL + "/" + name + ".bin"}, &base.Options{Path: f.dir, Name: name + ".bin"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func waitFor(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitDeferred(t *testing.T, d *Downloader, id string, until time.Time) {
	t.Helper()
	waitFor(t, "task "+id+" is deferred to the cooldown end", 3*time.Second, func() bool {
		task := d.getTask(id) // GetTask snapshots drop the unexported notBefore
		return task != nil && task.getStatus() == base.DownloadStatusWait && task.notBeforeTime().Equal(until)
	})
}

// Adopted from the independent acceptance (F03): with the only slot taken by a
// netdisk task facing an API cooldown, another source still downloads, and the
// netdisk task waits for the cooldown rather than failing.
func TestAPICooldownReleasesGlobalTaskSlot(t *testing.T) {
	f := newCooldownFixture(t, 1)
	f.d.budget.APIRateLimited(cooldownAccountAPI, time.Minute, true)
	first := f.netdisk(t, "netdisk")
	waitFor(t, "the provider is entered", 2*time.Second, func() bool { return f.provider.resolveCount("netdisk") == 1 })
	second := f.http(t, "other")
	time.Sleep(1500 * time.Millisecond)
	one, two := f.d.GetTask(first).getStatus(), f.d.GetTask(second).getStatus()
	if two != base.DownloadStatusDone || one != base.DownloadStatusWait {
		t.Fatalf("API cooldown occupied the sole global slot or failed the task: netdisk=%s other=%s after 1.5s", one, two)
	}
}

// A netdisk task facing a 60 s cooldown on its account API hands its slot back
// before sending anything, so a local HTTP task starts and completes, with one
// slot and with the default five all taken by same-account netdisk tasks.
func TestAPICooldownFreesSlotsForOtherSources(t *testing.T) {
	for _, slots := range []int{1, 5} {
		t.Run(fmt.Sprintf("MaxRunning=%d", slots), func(t *testing.T) {
			f := newCooldownFixture(t, slots)
			until := f.d.budget.APIRateLimited(cooldownAccountAPI, time.Minute, true)
			var deferred []string
			for i := 0; i < slots; i++ {
				deferred = append(deferred, f.netdisk(t, fmt.Sprintf("netdisk-%d", i)))
			}
			other := f.http(t, "other")
			waitStatus(t, f.d, other, base.DownloadStatusDone, 3*time.Second)
			sameContent(t, filepath.Join(f.dir, "other.bin"), f.data)
			for i, id := range deferred {
				waitDeferred(t, f.d, id, until)
				if n := f.provider.resolveCount(fmt.Sprintf("netdisk-%d", i)); n != 1 {
					t.Fatalf("netdisk task %d resolved %d times while deferred", i, n)
				}
			}
			if n := len(f.provider.calls()); n != 0 {
				t.Fatalf("%d API calls were sent during the cooldown", n)
			}
		})
	}
}

// Tasks deferred by an API cooldown resume after it and complete; one paused
// and one deleted meanwhile stay stopped. The four deferrals hold no API slot:
// a leak would leave all four account slots taken and block the resumes.
func TestAPICooldownDeferredTasksResumeOrStayStopped(t *testing.T) {
	f := newCooldownFixture(t, 1)
	blocker := f.http(t, "blocker")
	waitStatus(t, f.d, blocker, base.DownloadStatusRunning, 3*time.Second)
	names := []string{"resume-a", "resume-b", "paused", "deleted"}
	ids := map[string]string{}
	for _, name := range names {
		ids[name] = f.netdisk(t, name)
	}
	other := f.http(t, "other")
	// With tasks queued, a cooldown of QueuedDeferThreshold or more is handed
	// back instead of waited out in the only slot.
	until := f.d.budget.APIRateLimited(cooldownAccountAPI, 4*time.Second, true)
	f.unblock()
	waitStatus(t, f.d, other, base.DownloadStatusDone, 3*time.Second)
	for _, name := range names {
		waitDeferred(t, f.d, ids[name], until)
	}
	if err := f.d.Pause(&TaskFilter{IDs: []string{ids["paused"]}}); err != nil {
		t.Fatal(err)
	}
	if err := f.d.Delete(&TaskFilter{IDs: []string{ids["deleted"]}}, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"resume-a", "resume-b"} {
		waitStatus(t, f.d, ids[name], base.DownloadStatusDone, time.Until(until)+5*time.Second)
		sameContent(t, filepath.Join(f.dir, name+".bin"), f.data)
	}
	time.Sleep(300 * time.Millisecond)
	calls := f.provider.calls()
	if len(calls) != 2 {
		t.Fatalf("%d API calls, want one per resumed task", len(calls))
	}
	for _, at := range calls {
		if at.Before(until) {
			t.Fatalf("an API call went out %v before the cooldown ended", until.Sub(at))
		}
	}
	if task := f.d.GetTask(ids["paused"]); task == nil || task.getStatus() != base.DownloadStatusPause {
		t.Fatal("the paused deferred task did not stay paused")
	}
	if f.d.GetTask(ids["deleted"]) != nil {
		t.Fatal("the deleted deferred task came back")
	}
	if f.provider.resolveCount("paused") != 1 || f.provider.resolveCount("deleted") != 1 {
		t.Fatal("a paused or deleted deferred task was resolved again")
	}
}

// Close ends a task waiting out the API cooldown in its slot and one deferred
// by it: Close does not wait for the cooldown, neither task starts again when
// the cooldown ends, and both stay paused.
func TestAPICooldownCloseLeavesNothingWaiting(t *testing.T) {
	f := newCooldownFixture(t, 1)
	blocker := f.http(t, "blocker")
	waitStatus(t, f.d, blocker, base.DownloadStatusRunning, 3*time.Second)
	deferredID := f.netdisk(t, "deferred")
	other := f.http(t, "other")
	until := f.d.budget.APIRateLimited(cooldownAccountAPI, 4*time.Second, true)
	f.unblock()
	waitStatus(t, f.d, other, base.DownloadStatusDone, 3*time.Second)
	waitDeferred(t, f.d, deferredID, until)
	// Nothing is queued now, so this task waits out the cooldown in its slot.
	waitingID := f.netdisk(t, "waiting")
	waitFor(t, "the waiting task reaches the API", 2*time.Second, func() bool { return f.provider.resolveCount("waiting") == 1 })
	time.Sleep(50 * time.Millisecond)
	if st := f.d.GetTask(waitingID).getStatus(); st != base.DownloadStatusRunning {
		t.Fatalf("the task with nothing queued behind it should wait in its slot, status %s", st)
	}
	closed := make(chan error, 1)
	go func() { closed <- f.d.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close waited for the API cooldown")
	}
	time.Sleep(time.Until(until) + time.Second)
	if n := len(f.provider.calls()); n != 0 {
		t.Fatalf("%d API calls after Close", n)
	}
	if f.provider.resolveCount("deferred") != 1 || f.provider.resolveCount("waiting") != 1 {
		t.Fatal("a task started again after Close")
	}
	for _, id := range []string{deferredID, waitingID} {
		if st := f.d.GetTask(id).getStatus(); st != base.DownloadStatusPause {
			t.Fatalf("task %s is %s after Close, want paused", id, st)
		}
	}
}

// Taken from the build 17 re-acceptance (R02): a netdisk task already waiting
// out an 8 s account cooldown with nothing queued gives its only slot up when
// another source's task joins the queue afterwards.
func TestAPICooldownLaterQueuedTaskGetsSlot(t *testing.T) {
	f := newCooldownFixture(t, 1)
	until := f.d.budget.APIRateLimited(cooldownAccountAPI, 8*time.Second, true)
	first := f.netdisk(t, "first")
	waitFor(t, "the first provider resolve entered", 2*time.Second, func() bool { return f.provider.resolveCount("first") == 1 })
	time.Sleep(50 * time.Millisecond)
	other := f.http(t, "other")
	time.Sleep(1500 * time.Millisecond)
	one, two := f.d.GetTask(first).getStatus(), f.d.GetTask(other).getStatus()
	if two != base.DownloadStatusDone || one != base.DownloadStatusWait {
		t.Fatalf("later queued source starved: first=%s other=%s API calls=%d cooldown left=%v", one, two, len(f.provider.calls()), time.Until(until))
	}
}

// With the default five slots all held by same-account netdisk tasks waiting
// out a 4 s cooldown, a task queued later starts and completes; the netdisk
// tasks give their slots up, resume after the cooldown and complete, and no
// API call goes out before it ends.
func TestAPICooldownLaterQueueWithFiveSlots(t *testing.T) {
	f := newCooldownFixture(t, 5)
	until := f.d.budget.APIRateLimited(cooldownAccountAPI, 4*time.Second, true)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, f.netdisk(t, fmt.Sprintf("held-%d", i)))
	}
	waitFor(t, "all five resolves entered", 2*time.Second, func() bool {
		for i := 0; i < 5; i++ {
			if f.provider.resolveCount(fmt.Sprintf("held-%d", i)) == 0 {
				return false
			}
		}
		return true
	})
	other := f.http(t, "other")
	waitStatus(t, f.d, other, base.DownloadStatusDone, 2*time.Second)
	sameContent(t, filepath.Join(f.dir, "other.bin"), f.data)
	for i, id := range ids {
		waitStatus(t, f.d, id, base.DownloadStatusDone, time.Until(until)+5*time.Second)
		sameContent(t, filepath.Join(f.dir, fmt.Sprintf("held-%d.bin", i)), f.data)
	}
	for _, at := range f.provider.calls() {
		if at.Before(until) {
			t.Fatalf("an API call went out %v before the cooldown ended", until.Sub(at))
		}
	}
}

// gatedProvider holds the first resolve between its two API calls, so a test
// can queue another task and start a cooldown before the second call.
type gatedProvider struct {
	*cooldownProvider
	entered, release chan struct{}
	once             sync.Once
}

func (p *gatedProvider) Resolve(ctx context.Context, c *cloud.APIClient, ss *cloud.Session, src netdiskModel.Source, file netdiskModel.File) (*cloud.Download, error) {
	c.Client.Transport = p.cooldownProvider
	res, err := c.Do(ctx, http.MethodGet, "https://pan.quark.cn/api/share?fid="+file.ID, nil, nil)
	if err != nil {
		return nil, err
	}
	res.Body.Close()
	p.once.Do(func() {
		close(p.entered)
		<-p.release
	})
	return p.cooldownProvider.Resolve(ctx, c, ss, src, file)
}

// The other order: a task is already queued when the account cooldown starts
// in the middle of a netdisk resolve. The next API call hands the slot back at
// once, the queued task completes, and the netdisk task completes after the
// cooldown without any API call during it.
func TestAPICooldownStartingAfterTaskQueued(t *testing.T) {
	gate := &gatedProvider{entered: make(chan struct{}), release: make(chan struct{})}
	f := newCooldownFixtureWith(t, 1, func(p *cooldownProvider) cloud.Provider {
		gate.cooldownProvider = p
		return gate
	})
	first := f.netdisk(t, "first")
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the resolve did not reach its first API call")
	}
	other := f.http(t, "other")
	waitStatus(t, f.d, other, base.DownloadStatusWait, time.Second)
	until := f.d.budget.APIRateLimited(cooldownAccountAPI, 4*time.Second, true)
	close(gate.release)
	waitStatus(t, f.d, other, base.DownloadStatusDone, 2*time.Second)
	waitStatus(t, f.d, first, base.DownloadStatusDone, time.Until(until)+5*time.Second)
	sameContent(t, filepath.Join(f.dir, "first.bin"), f.data)
	calls := f.provider.calls()
	for _, at := range calls[1:] {
		if at.Before(until) {
			t.Fatalf("an API call went out %v before the cooldown ended", until.Sub(at))
		}
	}
}
