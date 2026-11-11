package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestPeerKernelUIDAndSocketAncestors(t *testing.T) {
	path := fixture(t, func(w http.ResponseWriter, r *http.Request) { reply(w, Stats{}) })
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := checkPeer(conn); err != nil {
		t.Fatal(err)
	}
	if err := checkPeerForUID(conn, os.Getuid()+1); err == nil {
		t.Fatal("kernel peer UID mismatch accepted")
	}
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	if err := checkPeer(local); err == nil {
		t.Fatal("non-Unix peer accepted")
	}
	conn.Close()
	if err := checkPeer(conn); err == nil {
		t.Fatal("closed peer accepted")
	}
	root := filepath.Dir(path)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if err := checkSocket(filepath.Join(alias, "api.sock")); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkSocket(regular); err == nil {
		t.Fatal("regular file accepted as socket")
	}
	if err := checkSocket(root + "/./api.sock"); err == nil {
		t.Fatal("unclean path accepted")
	}
	if err := checkSocket("api.sock"); err == nil {
		t.Fatal("relative path accepted")
	}
}
