package ed2k

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
	"github.com/monkeyWie/goed2k"
	gprotocol "github.com/monkeyWie/goed2k/protocol"
)

type pathStateFixture struct{ raw []byte }

func (s *pathStateFixture) Load(value any) (bool, error) {
	if s.raw == nil {
		return false, nil
	}
	return true, json.Unmarshal(s.raw, value)
}
func (s *pathStateFixture) Save(value any) error {
	raw, err := json.Marshal(value)
	if err == nil {
		s.raw = raw
	}
	return err
}
func (s *pathStateFixture) Delete() error { s.raw = nil; return nil }

func TestED2KPathResumeDoesNotEnterLegacyAutoLoad(t *testing.T) {
	link, err := parseLink(testLink)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	managed := goed2k.ClientTransferState{Hash: link.Hash, Size: link.NumberValue, TargetPath: filepath.Join(root, "frozen", "file"), Paused: true}
	legacy := goed2k.ClientTransferState{Hash: gprotocol.MustHashFromString("01010101010101010101010101010101"), Size: 3, TargetPath: filepath.Join(root, "legacy", "file")}
	fixture := &pathStateFixture{}
	if err := fixture.Save(nativePathState{ClientState: goed2k.ClientState{Version: 2, Transfers: []goed2k.ClientTransferState{legacy}}, PathFrozenTransfers: []nativePathTransfer{{OwnerID: "task", Transfer: managed}}}); err != nil {
		t.Fatal(err)
	}
	var oldReader goed2k.ClientState
	if err := json.Unmarshal(fixture.raw, &oldReader); err != nil {
		t.Fatal(err)
	}
	if len(oldReader.Transfers) != 1 || oldReader.Transfers[0].Hash != legacy.Hash {
		t.Fatal("old native reader can auto-open D6 output")
	}
	store := &clientStateStore{store: fixture}
	// Exercise the actual native LoadState without Start, sockets or bootstrap.
	// Only legacy directories may be created; the D6 record stays metadata.
	settings := goed2k.NewSettings()
	settings.EnableDHT, settings.EnableUPnP = false, false
	client := goed2k.NewClient(settings)
	defer client.Close()
	client.SetStateStore(store)
	if err := client.LoadState(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(managed.TargetPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native LoadState opened D6 directory")
	}
	if !client.FindTransfer(legacy.Hash).IsValid() || client.FindTransfer(managed.Hash).IsValid() {
		t.Fatal("native loaded wrong transfer set")
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Transfers) != 1 || state.Transfers[0].Hash != legacy.Hash {
		t.Fatal("changed legacy native recovery")
	}
	fm := &FetcherManager{stateStore: store}
	if err := fm.RegisterPathFreeze("task", &base.Request{URL: testLink}); err != nil {
		t.Fatal(err)
	}
	if err := fm.RegisterPathFreeze("other-task", &base.Request{URL: testLink}); !errors.Is(err, transfer.ErrLayoutStorageHeld) {
		t.Fatal("shared native ownership")
	}
	owned, ok, err := store.pathResume("task", link.Hash)
	if err != nil || !ok || owned.TargetPath != managed.TargetPath {
		t.Fatalf("resume %v/%v", ok, err)
	}
	owned.TargetPath = "mutated"
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	var saved nativePathState
	if err := json.Unmarshal(fixture.raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.PathFrozenTransfers) != 1 || saved.PathFrozenTransfers[0].Transfer.TargetPath != managed.TargetPath {
		t.Fatal("native save lost/aliased held D6 state")
	}
	if err := store.Save(nil); err != nil {
		t.Fatal(err)
	}
	if fixture.raw == nil {
		t.Fatal("delete lost held D6 resume")
	}
	before := string(fixture.raw)
	if err := fm.ForgetPathFreeze("other-task", &base.Request{URL: testLink}); !errors.Is(err, transfer.ErrLayoutStorageHeld) {
		t.Fatal("forgot foreign owner")
	}
	if string(fixture.raw) != before {
		t.Fatal("rejected forget changed snapshot")
	}
	if err := fm.ForgetPathFreeze("task", &base.Request{URL: testLink}); err != nil {
		t.Fatal(err)
	}
	saved = nativePathState{}
	if err := json.Unmarshal(fixture.raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.PathFrozenTransfers) != 0 {
		t.Fatal("deleted Task retained native hash ownership")
	}
	if err := fm.RegisterPathFreeze("new-task", &base.Request{URL: testLink}); err != nil {
		t.Fatal("cannot explicitly recreate deleted hash")
	}
}
