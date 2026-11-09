package sfu

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/pion/stun/v4"
	"github.com/pion/transport/v5/packetio"
)

func tcpTestMux(t *testing.T) *iceTCPMux {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := newICETCPMux(listener)
	t.Cleanup(func() { m.Close() })
	return m
}
func tcpAwait(t *testing.T, predicate func() bool) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for !predicate() {
		if time.Now().After(until) {
			t.Fatal("TCP condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func tcpCount(m *iceTCPMux) int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.conns) }
func tcpDial(t *testing.T, m *iceTCPMux) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp4", m.LocalAddr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func tcpFrame(t *testing.T, c net.Conn, payload []byte) {
	t.Helper()
	frame := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(frame, uint16(len(payload)))
	copy(frame[2:], payload)
	if _, err := io.Copy(c, bytes.NewReader(frame)); err != nil {
		t.Fatal(err)
	}
}
func tcpBinding(t *testing.T, ufrag string) []byte {
	t.Helper()
	request, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.NewUsername(ufrag+":remote"))
	if err != nil {
		t.Fatal(err)
	}
	return request.Raw
}
func tcpClosed(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := c.Read(b[:]); err == nil {
		t.Fatal("socket remained open")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("socket closure timed out")
	}
}
func TestICETCPUnknownUfragChurnAndBudget(t *testing.T) {
	m := tcpTestMux(t)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 64; i++ {
		c := tcpDial(t, m)
		tcpFrame(t, c, tcpBinding(t, fmt.Sprintf("unknown-%d", i)))
		tcpClosed(t, c)
		c.Close()
	}
	tcpAwait(t, func() bool { return tcpCount(m) == 0 })
	m.mu.Lock()
	n := len(m.endpoints)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("remote STUN allocated %d endpoints", n)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("64 unknown/no-integrity ufrags, retained heap delta=%d bytes, endpoints=%d", delta, n)
	if delta > 4<<20 {
		t.Fatalf("untrusted STUN retained excessive heap: %d", delta)
	}
}
func TestICETCPConnectionCapReuseAndShutdown(t *testing.T) {
	for _, mode := range []string{"silent", "partial-STUN"} {
		t.Run(mode, func(t *testing.T) {
			m := tcpTestMux(t)
			m.mu.Lock()
			m.connectionCap = 4
			m.mu.Unlock()
			sockets := make([]net.Conn, 4)
			for i := range sockets {
				sockets[i] = tcpDial(t, m)
				if mode == "partial-STUN" {
					sockets[i].Write([]byte{0, 60, 0})
				}
			}
			tcpAwait(t, func() bool { return tcpCount(m) == 4 })
			extra := tcpDial(t, m)
			tcpClosed(t, extra)
			sockets[0].Close()
			tcpAwait(t, func() bool { return tcpCount(m) == 3 })
			replacement := tcpDial(t, m)
			tcpAwait(t, func() bool { return tcpCount(m) == 4 })
			done := make(chan struct{})
			go func() { m.Close(); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("shutdown blocked on pre-STUN socket")
			}
			for _, c := range append(sockets[1:], replacement) {
				tcpClosed(t, c)
			}
			if tcpCount(m) != 0 {
				t.Fatal("shutdown retained socket slots")
			}
		})
	}
}
func TestICETCPEndpointCapReferenceAndRemoval(t *testing.T) {
	m := tcpTestMux(t)
	m.mu.Lock()
	m.endpointCap = 4
	m.mu.Unlock()
	ip := net.IPv4(127, 0, 0, 1)
	var refs []net.PacketConn
	for i := 0; i < 4; i++ {
		r, err := m.GetConnByUfrag(fmt.Sprint(i), false, ip)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, r)
	}
	if _, err := m.GetConnByUfrag("extra", false, ip); err == nil {
		t.Fatal("endpoint cap bypassed")
	}
	alias, err := m.GetConnByUfrag("0", false, ip)
	if err != nil {
		t.Fatal(err)
	}
	refs[0].Close()
	c := tcpDial(t, m)
	first := tcpBinding(t, "0")
	tcpFrame(t, c, first)
	alias.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, iceTCPPacketBytes)
	n, _, err := alias.ReadFrom(buf)
	if err != nil || !bytes.Equal(buf[:n], first) {
		t.Fatal("closing alias interrupted sibling", n, err)
	}
	m.RemoveConnByUfrag("0")
	tcpClosed(t, c)
	if _, _, err = alias.ReadFrom(buf); err == nil {
		t.Fatal("removed endpoint still readable")
	}
	alias.Close()
	if replacement, err := m.GetConnByUfrag("new", false, ip); err != nil {
		t.Fatal("endpoint capacity not reusable", err)
	} else {
		replacement.Close()
	}
	for _, r := range refs {
		r.Close()
	}
	// A removed ufrag must not be recreated by a late first STUN.
	late := tcpDial(t, m)
	tcpFrame(t, late, tcpBinding(t, "0"))
	tcpClosed(t, late)
	m.mu.Lock()
	count := len(m.endpoints)
	m.mu.Unlock()
	if count != 0 {
		t.Fatal("late STUN recreated endpoint", count)
	}
}
func TestICETCPAbsoluteFrameAndIdleTimeouts(t *testing.T) {
	for _, mode := range []string{"first-drip", "header-drip", "payload-drip", "idle", "full-queue"} {
		t.Run(mode, func(t *testing.T) {
			m := tcpTestMux(t)
			m.mu.Lock()
			m.limits = iceTCPTimeouts{120 * time.Millisecond, 120 * time.Millisecond, 180 * time.Millisecond}
			m.mu.Unlock()
			ref, err := m.GetConnByUfrag("registered", false, net.IPv4(127, 0, 0, 1))
			if err != nil {
				t.Fatal(err)
			}
			defer ref.Close()
			c := tcpDial(t, m)
			if mode != "first-drip" {
				tcpFrame(t, c, tcpBinding(t, "registered"))
				ref.SetReadDeadline(time.Now().Add(time.Second))
				if _, _, err = ref.ReadFrom(make([]byte, 8192)); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "header-drip":
				c.Write([]byte{0})
			case "payload-drip":
				c.Write([]byte{0, 100, 1})
			case "first-drip":
				c.Write([]byte{0, 100, 0})
			case "full-queue":
				for i := 0; i < iceTCPReadPackets+2; i++ {
					tcpFrame(t, c, []byte{1})
				}
			}
			// Additional bytes do not extend a started frame's absolute deadline.
			if mode == "first-drip" || mode == "payload-drip" {
				go func() {
					for i := 0; i < 10; i++ {
						time.Sleep(25 * time.Millisecond)
						if _, err := c.Write([]byte{1}); err != nil {
							return
						}
					}
				}()
			}
			tcpAwait(t, func() bool { return tcpCount(m) == 0 })
			tcpClosed(t, c)
		})
	}
}
func TestICETCPBlockedWriteCloses(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := &iceTCPConn{Conn: a, output: packetio.NewBuffer(), done: make(chan struct{})}
	c.output.SetLimitSize(iceTCPWriteBytes)
	c.output.Write([]byte("blocked write"), nil)
	done := make(chan struct{})
	go func() { c.write(80 * time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("blocked write survived deadline")
	}
	tcpClosed(t, b)
}
func TestICETCPLargeKeyPacketBurst(t *testing.T) {
	m := tcpTestMux(t)
	ref, err := m.GetConnByUfrag("large-key", false, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()
	c := tcpDial(t, m)
	tcpFrame(t, c, tcpBinding(t, "large-key"))
	ref.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, iceTCPPacketBytes)
	if _, _, err = ref.ReadFrom(buf); err != nil {
		t.Fatal(err)
	}
	const packets = 384 // 3 MiB, far larger than either bounded queue.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		payload := bytes.Repeat([]byte{0x5a}, iceTCPPacketBytes)
		for i := 0; i < packets; i++ {
			binary.BigEndian.PutUint32(payload, uint32(i))
			tcpFrame(t, c, payload)
		}
	}()
	for i := 0; i < packets; i++ {
		n, addr, err := ref.ReadFrom(buf)
		if err != nil || n != iceTCPPacketBytes || binary.BigEndian.Uint32(buf) != uint32(i) {
			t.Fatalf("burst packet %d: n=%d err=%v", i, n, err)
		}
		if i%64 == 0 {
			if _, err = ref.WriteTo(buf, addr); err != nil {
				t.Fatal(err)
			}
			reply, err := readICEFrame(c, iceTCPPacketBytes)
			if err != nil || !bytes.Equal(reply, buf) {
				t.Fatal("maximum-size outbound RFC4571 packet corrupted", err)
			}
		}
	}
	wg.Wait()
	t.Log("384 x 8192-byte ordered packets (3 MiB) and maximum-size replies passed; bounded queues backpressure without truncating frames")
}

func TestICETCPRegistrationRemovalRace(t *testing.T) {
	m := tcpTestMux(t)
	for i := 0; i < 64; i++ {
		ufrag := fmt.Sprintf("race-%d", i)
		ref, err := m.GetConnByUfrag(ufrag, false, net.IPv4(127, 0, 0, 1))
		if err != nil {
			t.Fatal(err)
		}
		c := tcpDial(t, m)
		var wg sync.WaitGroup
		wg.Add(2)
		packet := tcpBinding(t, ufrag)
		frame := make([]byte, 2+len(packet))
		binary.BigEndian.PutUint16(frame, uint16(len(packet)))
		copy(frame[2:], packet)
		go func() { defer wg.Done(); c.Write(frame) }()
		go func() { defer wg.Done(); m.RemoveConnByUfrag(ufrag) }()
		wg.Wait()
		ref.Close()
		tcpClosed(t, c)
	}
	tcpAwait(t, func() bool { return tcpCount(m) == 0 })
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.endpoints) != 0 {
		t.Fatal("late attachment recreated removed endpoint")
	}
}

// Gathering reserves TCP endpoints even when UDP wins. Preserve the supported
// 500-viewer product and synthetic 512-viewer cohort, including full overlap
// while one publisher and every viewer replace their peer connection.
func TestICETCPRegistrationCapacity512Peers(t *testing.T) {
	m := tcpTestMux(t)
	if m.connectionCap < 513 {
		t.Fatal("socket budget excludes supported cohort")
	}
	var refs []net.PacketConn
	for generation := 0; generation < 2; generation++ {
		for peer := 0; peer <= 512; peer++ {
			ref, err := m.GetConnByUfrag(fmt.Sprintf("cohort-%d-peer-%d", generation, peer), false, net.IPv4(127, 0, 0, 1))
			if err != nil {
				t.Fatalf("TCP fallback registration lost for generation %d peer %d: %v", generation, peer, err)
			}
			refs = append(refs, ref)
		}
	}
	for i := len(refs); i < iceTCPEndpoints; i++ {
		ref, err := m.GetConnByUfrag(fmt.Sprintf("remaining-%d", i), false, net.IPv4(127, 0, 0, 1))
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	if _, err := m.GetConnByUfrag("over-limit", false, net.IPv4(127, 0, 0, 1)); err == nil {
		t.Fatal("registration budget bypassed")
	}
	for _, ref := range refs {
		ref.Close()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.endpoints) != 0 || m.queuedBytes != 0 {
		t.Fatal("registration cleanup leaked budget")
	}
	t.Log("512 viewers + publisher, twice for replacement overlap, retain TCP registration; 2048 endpoint cap enforced independently of 1024 socket / 256 MiB shared payload budgets")
}
func TestICETCPSharedPayloadBudgetAndReuse(t *testing.T) {
	m := tcpTestMux(t)
	m.mu.Lock()
	m.queueCap = 128
	m.mu.Unlock()
	a, err := m.GetConnByUfrag("a", false, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := m.GetConnByUfrag("b", false, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	raw, other := net.Pipe()
	defer raw.Close()
	defer other.Close()
	c := &iceTCPConn{Conn: raw, done: make(chan struct{})}
	first := a.(*iceTCPRef).endpoint
	second := b.(*iceTCPRef).endpoint
	if !first.deliver(c, make([]byte, 64), time.Second) || !second.deliver(c, make([]byte, 64), time.Second) {
		t.Fatal("initial payload budget refused")
	}
	done := make(chan bool, 1)
	go func() { done <- first.deliver(c, make([]byte, 64), time.Second) }()
	select {
	case <-done:
		t.Fatal("shared byte cap bypassed")
	case <-time.After(25 * time.Millisecond):
	}
	m.mu.Lock()
	used := m.queuedBytes
	m.mu.Unlock()
	if used != 128 {
		t.Fatal("incorrect shared queued byte count", used)
	}
	a.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = a.ReadFrom(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	select {
	case success := <-done:
		if !success {
			t.Fatal("released payload capacity not reused")
		}
	case <-time.After(time.Second):
		t.Fatal("payload release did not unblock writer")
	}
	m.RemoveConnByUfrag("a")
	m.RemoveConnByUfrag("b")
	m.mu.Lock()
	used = m.queuedBytes
	m.mu.Unlock()
	if used != 0 {
		t.Fatal("endpoint removal retained payload reservation", used)
	}
}
