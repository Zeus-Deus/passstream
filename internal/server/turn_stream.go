package server

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"time"
)

// One shared cap covers TCP and TLS. Setup is absolute until verified TURN
// authentication; a complete but unauthenticated STUN drip cannot extend it.
const turnMaxStreams = 2048
const turnSetupTimeout = 10 * time.Second
const turnRequestTimeout = 10 * time.Second
const turnIdleTimeout = time.Minute

type turnStreamLimits struct {
	mu                   sync.Mutex
	conns                map[string]*turnStreamConn
	closed               bool
	max                  int
	setup, request, idle time.Duration
}

func newTURNStreamLimits() *turnStreamLimits {
	return &turnStreamLimits{conns: map[string]*turnStreamConn{}, max: turnMaxStreams, setup: turnSetupTimeout, request: turnRequestTimeout, idle: turnIdleTimeout}
}
func streamTuple(src, dst net.Addr) string {
	return src.Network() + ":" + src.String() + "/" + dst.Network() + ":" + dst.String()
}
func (s *turnStreamLimits) authenticated(src, dst net.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.conns[streamTuple(src, dst)]; c != nil {
		c.setup.Stop()
	}
}
func (s *turnStreamLimits) close() {
	s.mu.Lock()
	s.closed = true
	conns := make([]*turnStreamConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

type turnStreamListener struct {
	net.Listener
	limits *turnStreamLimits
}

func (l *turnStreamListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		s := l.limits
		s.mu.Lock()
		if s.closed || len(s.conns) >= s.max {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		c := &turnStreamConn{Conn: conn, limits: s, key: streamTuple(conn.RemoteAddr(), conn.LocalAddr())}
		c.setup = time.AfterFunc(s.setup, func() { c.Close() })
		s.conns[c.key] = c
		s.mu.Unlock()
		return c, nil
	}
}

type turnStreamConn struct {
	net.Conn
	limits  *turnStreamLimits
	key     string
	setup   *time.Timer
	once    sync.Once
	writeMu sync.Mutex
	// Read is called only by Pion's stream packetizer. Return at most one
	// frame fragment so its partial-frame deadline cannot be refreshed by a
	// slow byte drip or by a following frame in the same socket read.
	header             [4]byte
	headerN, remaining int
	frameDeadline      time.Time
}

func (c *turnStreamConn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if c.headerN == 0 && c.remaining == 0 {
		if err := c.Conn.SetReadDeadline(time.Now().Add(c.limits.idle)); err != nil {
			return 0, err
		}
		n, err := c.Conn.Read(b[:1])
		if n > 0 {
			c.header[0] = b[0]
			c.headerN = 1
			c.frameDeadline = time.Now().Add(c.limits.request)
		}
		return n, err
	}
	if err := c.Conn.SetReadDeadline(c.frameDeadline); err != nil {
		return 0, err
	}
	if c.headerN < 4 {
		n, err := c.Conn.Read(b[:min(len(b), 4-c.headerN)])
		copy(c.header[c.headerN:], b[:n])
		c.headerN += n
		if c.headerN == 4 {
			length := int(binary.BigEndian.Uint16(c.header[2:]))
			switch c.header[0] & 0xc0 {
			case 0:
				c.remaining = 16 + length // STUN's remaining header and body.
			case 0x40:
				c.remaining = (length + 3) &^ 3 // ChannelData stream padding.
			default:
				return 0, errors.New("invalid TURN stream frame")
			}
			if c.remaining == 0 {
				c.headerN = 0
			}
		}
		return n, err
	}
	n, err := c.Conn.Read(b[:min(len(b), c.remaining)])
	c.remaining -= n
	if c.remaining == 0 {
		c.headerN = 0
	}
	return n, err
}
func (c *turnStreamConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.limits.request)); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}
func (c *turnStreamConn) Close() error {
	var err error
	c.once.Do(func() {
		c.limits.mu.Lock()
		c.setup.Stop()
		c.limits.mu.Unlock()
		err = c.Conn.Close()
		c.limits.mu.Lock()
		delete(c.limits.conns, c.key)
		c.limits.mu.Unlock()
	})
	return err
}
