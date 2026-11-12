package http

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMCPOutputRequiresSameFDConfirmedSync(t *testing.T) {
	f := &Fetcher{}
	f.RequireMCPOutput()
	if err := f.MCPOutputSyncResult(); !errors.Is(err, ErrMCPOutputSync) {
		t.Fatal("unopened output claimed a Sync", err)
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "owned-stage"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	f.file = file
	if _, err := file.Write([]byte("same pinned descriptor")); err != nil {
		t.Fatal(err)
	}
	if err := f.syncMCPOutputBeforeClose(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	f.file = nil
	if err := f.MCPOutputSyncResult(); err != nil {
		t.Fatal("confirmed pre-Close Sync lost", err)
	}
}

func TestMCPOutputSyncFailureCannotBecomeConfirmed(t *testing.T) {
	f := &Fetcher{}
	f.RequireMCPOutput()
	file, err := os.Create(filepath.Join(t.TempDir(), "owned-stage"))
	if err != nil {
		t.Fatal(err)
	}
	f.file = file
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.syncMCPOutputBeforeClose(); !errors.Is(err, ErrMCPOutputSync) {
		t.Fatal("closed-fd Sync was accepted", err)
	}
	f.file = nil
	if err := f.MCPOutputSyncResult(); !errors.Is(err, ErrMCPOutputSync) {
		t.Fatal("Sync failure became success", err)
	}
}
