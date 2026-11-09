package server

import (
	"errors"
	"github.com/pion/turn/v4"
	"net"
)

// TURN permissions are IP-scoped. Filter the relay socket as well to enforce
// the media port and prevent a credential holder reaching other local services.
type mediaRelay struct {
	turn.RelayAddressGeneratorStatic
	endpoint *net.UDPAddr
	local    *net.UDPAddr
}

func (r *mediaRelay) AllocatePacketConn(network string, port int) (net.PacketConn, net.Addr, error) {
	c, a, e := r.RelayAddressGeneratorStatic.AllocatePacketConn(network, port)
	if e != nil {
		return nil, nil, e
	}
	return &mediaSocket{PacketConn: c, endpoint: r.endpoint, local: r.local}, a, nil
}

type mediaSocket struct {
	net.PacketConn
	endpoint *net.UDPAddr
	local    *net.UDPAddr
}

func (c *mediaSocket) allowed(a net.Addr) bool {
	u, ok := a.(*net.UDPAddr)
	return ok && u.IP.Equal(c.endpoint.IP) && u.Port == c.endpoint.Port
}
func (c *mediaSocket) WriteTo(b []byte, a net.Addr) (int, error) {
	if !c.allowed(a) {
		return 0, errors.New("TURN destination is not a media endpoint")
	}
	// Translate only the advertised SFU tuple, never arbitrary destinations.
	// This also avoids requiring a public-NAT hairpin for the internal leg.
	if c.local != nil {
		a = c.local
	}
	return c.PacketConn.WriteTo(b, a)
}
func (c *mediaSocket) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, a, e := c.PacketConn.ReadFrom(b)
		if e != nil {
			return n, a, e
		}
		u, ok := a.(*net.UDPAddr)
		if c.local == nil && c.allowed(a) || c.local != nil && ok && u.IP.Equal(c.local.IP) && u.Port == c.local.Port {
			return n, c.endpoint, nil
		}
	}
}
