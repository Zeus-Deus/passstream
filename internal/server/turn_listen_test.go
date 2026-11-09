package server

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestTURNEphemeralPairRetriesOccupiedUDP(t *testing.T) {
	var listeners []net.Listener
	listen := func(network, address string) (net.Listener, error) {
		l, err := net.Listen(network, address)
		if err == nil {
			listeners = append(listeners, l)
			t.Cleanup(func() { l.Close() })
		}
		return l, err
	}
	// The independently chosen TCP port can already belong to UDP. This
	// injected bind result makes that cross-protocol collision deterministic.
	calls := 0
	packet := func(network, address string) (net.PacketConn, error) {
		calls++
		if calls == 1 {
			return nil, &net.OpError{Op: "listen", Net: network, Err: syscall.EADDRINUSE}
		}
		return net.ListenPacket(network, address)
	}
	udp, tcp, err := listenTURNPair("127.0.0.1:0", listen, packet)
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	defer tcp.Close()
	if calls != 2 || len(listeners) != 2 {
		t.Fatalf("packet calls=%d, TCP reservations=%d", calls, len(listeners))
	}
	listeners[0].(*net.TCPListener).SetDeadline(time.Now())
	if _, err := listeners[0].Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("failed reservation was not closed: %v", err)
	}
	if udp.LocalAddr().String() != tcp.Addr().String() {
		t.Fatalf("different TURN addresses: UDP %s, TCP %s", udp.LocalAddr(), tcp.Addr())
	}
}

func TestTURNPairBindFailureIsBoundedAndClosesReservations(t *testing.T) {
	for _, tc := range []struct {
		name, address string
		bindErr       error
		wantCalls     int
	}{
		{"ephemeral occupied", "127.0.0.1:0", syscall.EADDRINUSE, 16},
		{"other error", "127.0.0.1:0", syscall.EACCES, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var listeners []net.Listener
			listen := func(network, address string) (net.Listener, error) {
				l, err := net.Listen(network, address)
				if err == nil {
					listeners = append(listeners, l)
					t.Cleanup(func() { l.Close() })
				}
				return l, err
			}
			calls := 0
			packet := func(network, _ string) (net.PacketConn, error) {
				calls++
				return nil, &net.OpError{Op: "listen", Net: network, Err: tc.bindErr}
			}
			udp, tcp, err := listenTURNPair(tc.address, listen, packet)
			if !errors.Is(err, tc.bindErr) || udp != nil || tcp != nil || calls != tc.wantCalls || len(listeners) != tc.wantCalls {
				t.Fatalf("UDP=%v TCP=%v err=%v calls=%d reservations=%d", udp, tcp, err, calls, len(listeners))
			}
			for _, l := range listeners {
				l.(*net.TCPListener).SetDeadline(time.Now())
				if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
					t.Fatalf("failed reservation was not closed: %v", err)
				}
			}
		})
	}
}

func TestTURNPairFixedPortDoesNotMoveOnCollision(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp"} {
		t.Run(protocol, func(t *testing.T) {
			var address string
			if protocol == "udp" {
				occupied, err := net.ListenPacket("udp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer occupied.Close()
				address = occupied.LocalAddr().String()
			} else {
				occupied, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer occupied.Close()
				address = occupied.Addr().String()
			}
			calls := 0
			listen := func(network, got string) (net.Listener, error) {
				calls++
				if got != address {
					t.Fatalf("moved configured address: %s -> %s", address, got)
				}
				return net.Listen(network, got)
			}
			udp, tcp, err := listenTURNPair(address, listen, net.ListenPacket)
			if !errors.Is(err, syscall.EADDRINUSE) || udp != nil || tcp != nil || calls != 1 {
				t.Fatalf("UDP=%v TCP=%v err=%v listen calls=%d", udp, tcp, err, calls)
			}
		})
	}
}
