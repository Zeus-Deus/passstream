package server

import (
	"encoding/binary"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/pion/stun/v4"
)

// Exercise both actual constructors: the queue-unit defect also affected the
// default loopback listener, even though public exposure raised its severity.
func TestICETCPUnauthenticatedResourcesAndServerShutdown(t *testing.T) {
	for _, public := range []bool{false, true} {
		name := "loopback"
		if public {
			name = "public"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := publicConfig(t)
			if !public {
				c = Config{Data: t.TempDir(), Listen: free(t), BaseURL: "http://localhost", ICEUDP: free(t), ICETCP: free(t), TURN: free(t)}
			}
			s, err := New(c)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			address := turnTCPAddress(t, c.ICETCP)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for i := 0; i < 4; i++ {
				conn, err := net.DialTimeout("tcp4", address, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				request, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.NewUsername("unknown-"+string(rune('a'+i))+":remote"))
				if err != nil {
					t.Fatal(err)
				}
				frame := make([]byte, 2+len(request.Raw))
				binary.BigEndian.PutUint16(frame, uint16(len(request.Raw)))
				copy(frame[2:], request.Raw)
				if _, err = conn.Write(frame); err != nil {
					t.Fatal(err)
				}
				conn.SetReadDeadline(time.Now().Add(time.Second))
				var b [1]byte
				_, err = conn.Read(b[:])
				conn.Close()
				if err == nil {
					t.Fatal("unknown ufrag accepted")
				}
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					t.Fatal("unknown ufrag retained")
				}
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			t.Logf("four unknown ufrags retained heap delta=%d bytes", delta)
			if delta > 4<<20 {
				t.Fatalf("unknown ufrags retain excessive heap: %d", delta)
			}
			var sockets []net.Conn
			for _, payload := range [][]byte{nil, {0, 60, 0}} {
				conn, err := net.DialTimeout("tcp4", address, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				sockets = append(sockets, conn)
				defer conn.Close()
				if payload != nil {
					conn.Write(payload)
				}
			}
			// Give the listener time to accept the silent/partial first-STUN sockets.
			time.Sleep(25 * time.Millisecond)
			done := make(chan struct{})
			start := time.Now()
			go func() { s.Close(); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				for _, conn := range sockets {
					conn.Close()
				}
				t.Fatal("server shutdown waits for pre-STUN client")
			}
			t.Logf("Server.Close with silent + partial STUN sockets: %v", time.Since(start))
		})
	}
}
