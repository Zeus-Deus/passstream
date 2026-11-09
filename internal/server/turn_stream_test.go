package server

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/pion/turn/v4"
)

func streamCount(s *turnStreamLimits) int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.conns) }
func awaitStreamCount(t *testing.T, s *turnStreamLimits, n int) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		if streamCount(s) == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stream capacity not released: got %d, want %d", streamCount(s), n)
}
func shortTURNStreams(s *Server) {
	s.turnStreams.mu.Lock()
	s.turnStreams.setup = 150 * time.Millisecond
	s.turnStreams.request = 100 * time.Millisecond
	s.turnStreams.idle = 2 * time.Second
	s.turnStreams.mu.Unlock()
}
func turnTCPAddress(t *testing.T, address string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	return net.JoinHostPort("127.0.0.1", port)
}

func TestTURNStreamSetupStallsReleaseCapacity(t *testing.T) {
	c, _ := publicConfig(t)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	shortTURNStreams(s)
	for _, v := range []struct {
		name, address string
		first         []byte
	}{
		{"silent TCP", c.TURN, nil},
		{"partial TCP header", c.TURN, []byte{0x00}},
		{"silent TLS handshake", c.TURNTLS, nil},
		{"partial TLS handshake", c.TURNTLS, []byte{0x16}},
	} {
		t.Run(v.name, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp4", turnTCPAddress(t, v.address), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			awaitStreamCount(t, s.turnStreams, 1)
			started := time.Now()
			if len(v.first) > 0 {
				if _, err := conn.Write(v.first); err != nil {
					t.Fatal(err)
				}
			}
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := conn.Read(make([]byte, 1)); err == nil {
				t.Fatal("stall was not closed")
			}
			awaitStreamCount(t, s.turnStreams, 0)
			if time.Since(started) > 500*time.Millisecond {
				t.Fatal("setup/frame deadline was extended")
			}
		})
	}
}

func TestTURNStreamCapSharedAndReleased(t *testing.T) {
	c, _ := publicConfig(t)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.turnStreams.mu.Lock()
	s.turnStreams.max = 1
	s.turnStreams.mu.Unlock()
	first, err := net.DialTimeout("tcp4", turnTCPAddress(t, c.TURN), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	awaitStreamCount(t, s.turnStreams, 1)
	// TLS cannot create another Pion goroutine while TCP owns the shared slot.
	second, err := net.DialTimeout("tcp4", turnTCPAddress(t, c.TURNTLS), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("over-cap TLS accepted")
	}
	if streamCount(s.turnStreams) != 1 {
		t.Fatal("cap bypassed")
	}
	first.Close()
	awaitStreamCount(t, s.turnStreams, 0)
	third, err := net.DialTimeout("tcp4", turnTCPAddress(t, c.TURNTLS), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	awaitStreamCount(t, s.turnStreams, 1)
	s.Close()
	awaitStreamCount(t, s.turnStreams, 0)
}

func TestTURNStreamAuthenticationAndPartialRequestDeadline(t *testing.T) {
	c, _ := publicConfig(t)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	shortTURNStreams(s)
	address := turnTCPAddress(t, c.TURN)
	conn, err := net.DialTimeout("tcp4", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	credentials := s.iceServers()[0]
	client, err := turn.NewClient(&turn.ClientConfig{Conn: turn.NewSTUNConn(conn), TURNServerAddr: address, Username: credentials.Username, Password: credentials.Credential.(string), RTO: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Listen(); err != nil {
		t.Fatal(err)
	}
	allocation, err := client.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer allocation.Close()
	// Verified authentication lifts only the absolute setup deadline.
	time.Sleep(200 * time.Millisecond)
	if streamCount(s.turnStreams) != 1 {
		t.Fatal("authenticated allocation hit unauthenticated deadline")
	}
	// A byte drip after authentication cannot reset the request residence.
	started := time.Now()
	conn.Write([]byte{0})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 12 {
			time.Sleep(20 * time.Millisecond)
			if _, err := conn.Write([]byte{0}); err != nil {
				return
			}
		}
	}()
	awaitStreamCount(t, s.turnStreams, 0)
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("absolute setup/request deadline was extended")
	}
	<-done
}

func TestTURNStreamUnauthenticatedRequestsDoNotExtendSetup(t *testing.T) {
	c, _ := publicConfig(t)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	shortTURNStreams(s)
	conn, err := net.DialTimeout("tcp4", turnTCPAddress(t, c.TURN), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	awaitStreamCount(t, s.turnStreams, 1)
	started := time.Now()
	binding, err := stun.Build(stun.BindingRequest, stun.TransactionID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			if _, err := conn.Write(binding.Raw); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	awaitStreamCount(t, s.turnStreams, 0)
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("absolute setup/request deadline was extended")
	}
	<-done
}

func TestTURNStreamWriteDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	limits := newTURNStreamLimits()
	limits.request = 50 * time.Millisecond
	conn := &turnStreamConn{Conn: server, limits: limits, key: "blocked-write", setup: time.NewTimer(time.Minute)}
	limits.conns[conn.key] = conn
	defer conn.Close()
	_, err := conn.Write([]byte("client never reads"))
	netErr, ok := err.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Fatal("unbounded/stalled write", err)
	}
	conn.Close()
	if streamCount(limits) != 0 {
		t.Fatal("closed stream retains capacity")
	}
	if _, err := client.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal(err)
	}
}
