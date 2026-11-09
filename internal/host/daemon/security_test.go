package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Check the kernel credentials, independently of the socket's mode or hello.
func TestUnixPeerCredentialsRequireDaemonUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	peer, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	d := &Daemon{uid: os.Geteuid()}
	if !d.peerIsUs(peer) {
		t.Fatal("own kernel UID rejected")
	}
	d.uid++
	if d.peerIsUs(peer) {
		t.Fatal("foreign kernel UID accepted")
	}
	peer.Close()
	if d.peerIsUs(peer) {
		t.Fatal("credential inspection failed open")
	}
}
