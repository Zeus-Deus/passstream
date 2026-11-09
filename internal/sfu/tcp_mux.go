package sfu

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	"github.com/pion/transport/v5/deadline"
	"github.com/pion/transport/v5/packetio"
)

const (
	iceTCPConnections = 1024
	iceTCPEndpoints   = 2048
	iceTCPReadPackets = 128  // Packet entries, NOT bytes (Pion TCPMux uses the same units).
	iceTCPPacketBytes = 8192 // Pion ICE's receive MTU; a keyframe spans many packets.
	iceTCPWriteBytes  = 64 << 10
	iceTCPQueueBytes  = 256 << 20 // Shared payload ceiling, independent of registered endpoints.
)

type iceTCPTimeouts struct{ first, frame, idle time.Duration }

// Unlike Pion TCPMuxDefault, this mux never creates an endpoint from untrusted
// STUN. Only GetConnByUfrag, called by local ICE gathering, can allocate a queue.
// Registration/removal and socket attachment share mu, avoiding a close/gather
// race that could otherwise re-create an unknown endpoint after removal.
type iceTCPMux struct {
	listener                   net.Listener
	mu                         sync.Mutex
	closed                     bool
	endpoints                  map[iceTCPKey]*iceTCPEndpoint
	conns                      map[*iceTCPConn]struct{}
	wg                         sync.WaitGroup
	limits                     iceTCPTimeouts
	connectionCap, endpointCap int
	queuedBytes, queueCap      int64
	budgetReady                chan struct{}
}
type iceTCPKey struct{ ufrag, local string }
type iceTCPPacket struct {
	data []byte
	addr net.Addr
}
type iceTCPEndpoint struct {
	mux     *iceTCPMux
	key     iceTCPKey
	addr    *net.TCPAddr
	refs    int // protected by mux.mu
	packets chan iceTCPPacket
	done    chan struct{}
}
type iceTCPConn struct {
	net.Conn
	endpoint *iceTCPEndpoint // attached under mux.mu; immutable thereafter
	output   *packetio.Buffer
	done     chan struct{}
	once     sync.Once
}

var _ ice.TCPMux = (*iceTCPMux)(nil)

func newICETCPMux(listener net.Listener) *iceTCPMux {
	m := &iceTCPMux{listener: listener, endpoints: make(map[iceTCPKey]*iceTCPEndpoint), conns: make(map[*iceTCPConn]struct{}), limits: iceTCPTimeouts{10 * time.Second, 10 * time.Second, 60 * time.Second}, connectionCap: iceTCPConnections, endpointCap: iceTCPEndpoints, queueCap: iceTCPQueueBytes, budgetReady: make(chan struct{})}
	m.wg.Add(1)
	go m.accept()
	return m
}
func (m *iceTCPMux) LocalAddr() net.Addr { return m.listener.Addr() }
func (m *iceTCPMux) accept() {
	defer m.wg.Done()
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			return
		}
		m.mu.Lock()
		if m.closed || len(m.conns) >= m.connectionCap {
			m.mu.Unlock()
			conn.Close()
			continue
		}
		c := &iceTCPConn{Conn: conn, output: packetio.NewBuffer(), done: make(chan struct{})}
		c.output.SetLimitSize(iceTCPWriteBytes)
		m.conns[c] = struct{}{}
		limits := m.limits
		c.SetReadDeadline(time.Now().Add(limits.first))
		m.wg.Add(1)
		m.mu.Unlock()
		go m.read(c, limits)
	}
}
func (m *iceTCPMux) read(c *iceTCPConn, limits iceTCPTimeouts) {
	defer m.wg.Done()
	defer func() {
		c.Close()
		m.mu.Lock()
		delete(m.conns, c)
		m.mu.Unlock()
	}()
	// This is RFC4571's TWO-byte length framing, not TURN's STUN/ChannelData
	// framing. Keep Pion's 512-byte first-STUN and 8192-byte subsequent packet limits.
	first, err := readICEFrame(c, 512)
	if err != nil {
		return
	}
	msg := &stun.Message{Raw: first}
	if msg.Decode() != nil || msg.Type != stun.BindingRequest {
		return
	}
	var username stun.Username
	if username.GetFrom(msg) != nil {
		return
	}
	ufrag, remote, ok := strings.Cut(string(username), ":")
	if !ok || ufrag == "" || remote == "" {
		return
	}
	m.mu.Lock()
	endpoint := m.endpoints[iceTCPKey{ufrag, c.LocalAddr().(*net.TCPAddr).IP.String()}]
	if m.closed || endpoint == nil {
		m.mu.Unlock()
		return
	}
	c.endpoint = endpoint
	m.mu.Unlock()
	written := make(chan struct{})
	go func() { defer close(written); c.write(limits.frame) }()
	defer func() { c.Close(); <-written }()
	if !endpoint.deliver(c, first, limits.frame) {
		return
	}
	for {
		// Idle bounds waiting for a new frame; once any byte arrives, the rest
		// of the header and payload share one absolute frame deadline.
		c.SetReadDeadline(time.Now().Add(limits.idle))
		var header [2]byte
		if _, err = io.ReadFull(c, header[:1]); err != nil {
			return
		}
		c.SetReadDeadline(time.Now().Add(limits.frame))
		if _, err = io.ReadFull(c, header[1:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(header[:]))
		if n > iceTCPPacketBytes {
			return
		}
		packet := make([]byte, n)
		if _, err = io.ReadFull(c, packet); err != nil {
			return
		}
		if !endpoint.deliver(c, packet, limits.frame) {
			return
		}
	}
}
func readICEFrame(conn net.Conn, maximum int) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(header[:]))
	if n > maximum {
		return nil, io.ErrShortBuffer
	}
	packet := make([]byte, n)
	_, err := io.ReadFull(conn, packet)
	return packet, err
}
func (e *iceTCPEndpoint) deliver(c *iceTCPConn, packet []byte, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	m := e.mux
	for {
		m.mu.Lock()
		if m.endpoints[e.key] != e {
			m.mu.Unlock()
			return false
		}
		if m.queuedBytes+int64(len(packet)) <= m.queueCap {
			select {
			case e.packets <- iceTCPPacket{packet, c.RemoteAddr()}:
				m.queuedBytes += int64(len(packet))
				m.mu.Unlock()
				return true
			default:
			}
		}
		ready := m.budgetReady
		m.mu.Unlock()
		select {
		case <-ready:
		case <-timer.C:
			return false
		case <-c.done:
			return false
		case <-e.done:
			return false
		}
	}
}

// wake requires mu. A broadcast lets both full endpoint queues and the shared
// byte budget apply backpressure without dropping a large multi-packet frame.
func (m *iceTCPMux) wake() { close(m.budgetReady); m.budgetReady = make(chan struct{}) }
func (m *iceTCPMux) release(bytes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queuedBytes -= int64(bytes)
	m.wake()
}

func (c *iceTCPConn) Close() error {
	var err error
	c.once.Do(func() { close(c.done); err = c.Conn.Close(); c.output.Close() })
	return err
}
func (c *iceTCPConn) write(timeout time.Duration) {
	defer c.Close()
	frame := make([]byte, iceTCPPacketBytes+2)
	for {
		n, _, err := c.output.Read(frame, nil)
		if err != nil {
			return
		}
		if c.SetWriteDeadline(time.Now().Add(timeout)) != nil {
			return
		}
		for sent := 0; sent < n; {
			written, err := c.Conn.Write(frame[sent:n])
			if err != nil || written == 0 {
				return
			}
			sent += written
		}
	}
}
func (m *iceTCPMux) GetConnByUfrag(ufrag string, ipv6 bool, local net.IP) (net.PacketConn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, net.ErrClosed
	}
	if ipv6 || local.To4() == nil {
		return nil, errors.New("ICE TCP supports IPv4 only")
	}
	key := iceTCPKey{ufrag, local.String()}
	e := m.endpoints[key]
	if e == nil {
		if len(m.endpoints) >= m.endpointCap {
			return nil, errors.New("ICE TCP endpoint limit reached")
		}
		e = &iceTCPEndpoint{mux: m, key: key, addr: &net.TCPAddr{IP: append(net.IP(nil), local...), Port: m.LocalAddr().(*net.TCPAddr).Port}, packets: make(chan iceTCPPacket, iceTCPReadPackets), done: make(chan struct{})}
		m.endpoints[key] = e
	}
	e.refs++
	return &iceTCPRef{endpoint: e, done: make(chan struct{}), readDeadline: deadline.New(), writeDeadline: deadline.New()}, nil
}

// remove requires mu. Closing raw sockets and buffers cannot call back into mu.
func (m *iceTCPMux) remove(e *iceTCPEndpoint) {
	if m.endpoints[e.key] != e {
		return
	}
	delete(m.endpoints, e.key)
	close(e.done)
	for {
		select {
		case packet := <-e.packets:
			m.queuedBytes -= int64(len(packet.data))
		default:
			m.wake()
			goto drained
		}
	}
drained:
	for c := range m.conns {
		if c.endpoint == e {
			c.Close()
		}
	}
}
func (m *iceTCPMux) RemoveConnByUfrag(ufrag string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, e := range m.endpoints {
		if key.ufrag == ufrag {
			m.remove(e)
		}
	}
}
func (m *iceTCPMux) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.wg.Wait()
		return nil
	}
	m.closed = true
	err := m.listener.Close()
	for _, e := range m.endpoints {
		m.remove(e)
	}
	for c := range m.conns {
		c.Close()
	}
	m.mu.Unlock()
	m.wg.Wait()
	return err
}

// Each local candidate owns a reference and its own close/deadline signals.
// Closing one alias must not interrupt a sibling candidate's ReadFrom.
type iceTCPRef struct {
	endpoint                    *iceTCPEndpoint
	done                        chan struct{}
	once                        sync.Once
	readDeadline, writeDeadline *deadline.Deadline
}

func (r *iceTCPRef) Close() error {
	r.once.Do(func() {
		close(r.done)
		r.readDeadline.Set(time.Time{})
		r.writeDeadline.Set(time.Time{})
		m := r.endpoint.mux
		m.mu.Lock()
		defer m.mu.Unlock()
		r.endpoint.refs--
		if r.endpoint.refs == 0 {
			m.remove(r.endpoint)
		}
	})
	return nil
}
func (r *iceTCPRef) LocalAddr() net.Addr { return r.endpoint.addr }
func (r *iceTCPRef) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case <-r.done:
		return 0, nil, net.ErrClosed
	case <-r.endpoint.done:
		return 0, nil, net.ErrClosed
	default:
	}
	select {
	case <-r.done:
		return 0, nil, net.ErrClosed
	case <-r.endpoint.done:
		return 0, nil, net.ErrClosed
	case <-r.readDeadline.Done():
		return 0, nil, os.ErrDeadlineExceeded
	case p := <-r.endpoint.packets:
		r.endpoint.mux.release(len(p.data))
		if len(b) < len(p.data) {
			return 0, p.addr, io.ErrShortBuffer
		}
		return copy(b, p.data), p.addr, nil
	}
}
func (r *iceTCPRef) WriteTo(b []byte, addr net.Addr) (int, error) {
	if len(b) > iceTCPPacketBytes {
		return 0, io.ErrShortBuffer
	}
	select {
	case <-r.done:
		return 0, net.ErrClosed
	case <-r.writeDeadline.Done():
		return 0, os.ErrDeadlineExceeded
	default:
	}
	m := r.endpoint.mux
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.endpoints[r.endpoint.key] != r.endpoint {
		return 0, net.ErrClosed
	}
	for c := range m.conns {
		if c.endpoint == r.endpoint && c.RemoteAddr().String() == addr.String() {
			frame := make([]byte, 2+len(b))
			binary.BigEndian.PutUint16(frame, uint16(len(b)))
			copy(frame[2:], b)
			if _, err := c.output.Write(frame, nil); err != nil {
				return 0, err
			}
			return len(b), nil
		}
	}
	return 0, net.ErrClosed
}
func (r *iceTCPRef) SetReadDeadline(t time.Time) error  { r.readDeadline.Set(t); return nil }
func (r *iceTCPRef) SetWriteDeadline(t time.Time) error { r.writeDeadline.Set(t); return nil }
func (r *iceTCPRef) SetDeadline(t time.Time) error {
	r.SetReadDeadline(t)
	return r.SetWriteDeadline(t)
}
