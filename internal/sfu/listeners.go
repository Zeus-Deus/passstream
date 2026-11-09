package sfu

import (
	"errors"
	"net"

	"github.com/pion/webrtc/v4"
)

// New keeps the local laboratory's explicit loopback listeners.
func New(udpBind, tcpBind string) (*Engine, error) {
	for _, address := range []string{udpBind, tcpBind} {
		host, _, err := net.SplitHostPort(address)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return nil, errors.New("local ICE requires explicit loopback listeners")
		}
	}
	return newEngine(udpBind, tcpBind, "")
}

// NewPublic publishes only advertiseIP, with the bound ports unchanged. A
// port-preserving NAT may map this address to the local/wildcard listeners.
func NewPublic(udpBind, tcpBind, advertiseIP string) (*Engine, error) {
	ip := net.ParseIP(advertiseIP)
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() {
		return nil, errors.New("public ICE requires a unicast IPv4 advertise address")
	}
	for _, address := range []string{udpBind, tcpBind} {
		host, _, err := net.SplitHostPort(address)
		bind := net.ParseIP(host)
		if err != nil || bind == nil || bind.To4() == nil || (!bind.IsUnspecified() && !bind.IsGlobalUnicast()) {
			return nil, errors.New("public ICE requires IPv4 bind addresses")
		}
	}
	return newEngine(udpBind, tcpBind, ip.String())
}

// UDPAddr is the actual local mux address, including an allocated ephemeral port.
func (e *Engine) UDPAddr() *net.UDPAddr {
	a := e.udp.LocalAddr().(*net.UDPAddr)
	return &net.UDPAddr{IP: append(net.IP(nil), a.IP...), Port: a.Port}
}

// One public address represents one TCP candidate. Pion keys TCP mux sockets by
// local interface IP; mapping every interface to the same public candidate can
// discard the socket for the interface on which the connection actually arrives.
// Use one internal mux identity for gathering and accepted sockets. The real
// listener/connection still binds and carries traffic on the configured address.
func publicTCPListener(settings *webrtc.SettingEngine, listener net.Listener, advertiseIP string) net.Listener {
	identity := net.IPv4(127, 0, 0, 1)
	settings.SetIPFilter(func(ip net.IP) bool { return ip.Equal(identity) })
	settings.SetNAT1To1IPs([]string{advertiseIP}, webrtc.ICECandidateTypeHost)
	return &singleAddressListener{Listener: listener, address: &net.TCPAddr{IP: identity, Port: listener.Addr().(*net.TCPAddr).Port}}
}

type singleAddressListener struct {
	net.Listener
	address *net.TCPAddr
}

func (l *singleAddressListener) Addr() net.Addr { return l.address }
func (l *singleAddressListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &singleAddressConn{Conn: conn, address: l.address}, nil
}

type singleAddressConn struct {
	net.Conn
	address *net.TCPAddr
}

func (c *singleAddressConn) LocalAddr() net.Addr { return c.address }
