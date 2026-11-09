package server

import (
	"github.com/passstream/passstream/internal/sfu"
	"github.com/passstream/passstream/internal/store"
	"net"
	"testing"
	"time"
)

func TestTURNOnlyMediaEndpoint(t *testing.T) {
	c := &mediaSocket{endpoint: &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18443}}
	for _, a := range []net.Addr{&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80}, &net.UDPAddr{IP: net.ParseIP("127.0.0.2"), Port: 18443}, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18443}} {
		if c.allowed(a) {
			t.Fatalf("allowed %v", a)
		}
	}
	if !c.allowed(&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18443}) {
		t.Fatal("media rejected")
	}
}
func TestReaperRemovesClosedPeersAndExpiredSessions(t *testing.T) {
	s, e := New(Config{Data: t.TempDir(), Listen: free(t), ICEUDP: free(t), ICETCP: free(t), TURN: free(t), BaseURL: "http://localhost"})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	v, _, _, e := s.Store.Create()
	if e != nil {
		t.Fatal(e)
	}
	token, e := s.Store.Session(v.ID, "view")
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.Media.Peer(v.ID, false)
	if e != nil {
		t.Fatal(e)
	}
	s.peers = map[string]*sfu.Peer{store.Hash(token): p}
	p.Close()
	_ = p.PC.Close()
	_, e = s.Store.DB.Exec("UPDATE transport_sessions SET expires=?", time.Now().Add(-time.Second).UnixMilli())
	if e != nil {
		t.Fatal(e)
	}
	s.reapOnce()
	if len(s.peers) != 0 {
		t.Fatal("peer retained")
	}
	var n int
	if e = s.Store.DB.QueryRow("SELECT COUNT(*) FROM transport_sessions").Scan(&n); e != nil || n != 0 {
		t.Fatal("sessions retained", n, e)
	}
}

// Exercise actual socket I/O: TURN permissions alone cannot enforce a port.
func TestMediaSocketPinsAdvertisedAndLocalTuples(t *testing.T) {
	listen := func() net.PacketConn {
		c, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	local, relay, stranger := listen(), listen(), listen()
	endpoint := &net.UDPAddr{IP: net.ParseIP("198.18.0.1"), Port: 18443}
	socket := &mediaSocket{PacketConn: relay, endpoint: endpoint, local: local.LocalAddr().(*net.UDPAddr)}
	for _, destination := range []net.Addr{local.LocalAddr(), stranger.LocalAddr(), &net.UDPAddr{IP: endpoint.IP, Port: 80}, &net.UDPAddr{IP: net.ParseIP("203.0.113.1"), Port: endpoint.Port}} {
		if _, err := socket.WriteTo([]byte("forbidden"), destination); err == nil {
			t.Fatalf("allowed %v", destination)
		}
	}
	if _, err := socket.WriteTo([]byte("request"), endpoint); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	local.SetReadDeadline(time.Now().Add(time.Second))
	n, from, err := local.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "request" {
		t.Fatal(n, err)
	}
	// A packet from any other socket must not enter the client's relay path.
	stranger.WriteTo([]byte("injection"), relay.LocalAddr())
	local.WriteTo([]byte("response"), from)
	socket.SetReadDeadline(time.Now().Add(time.Second))
	n, from, err = socket.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "response" || from.String() != endpoint.String() {
		t.Fatal("source filter/translation", n, from, err)
	}
}
