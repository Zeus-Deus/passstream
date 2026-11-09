package server

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/passstream/passstream/internal/store"
	"github.com/pion/turn/v4"
	"github.com/pion/webrtc/v4"
)

func bindAddress(address string, public bool) error {
	host, port, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 0 || n > 65535 || ip == nil || ip.To4() == nil {
		return errors.New("media bind must be a literal IPv4 address and port")
	}
	if !public && !ip.IsLoopback() {
		return errors.New("non-loopback media requires --public")
	}
	if public && (n == 0 || (!ip.IsUnspecified() && (!ip.IsGlobalUnicast() || ip.IsLoopback()))) {
		return errors.New("public media bind requires a unicast or wildcard IPv4 address and nonzero port")
	}
	return nil
}

func validHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLoopback()
	}
	if len(host) > 253 || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return host != ""
}

// Signaling is HTTP on loopback only. Public HTTPS is terminated by a local
// reverse proxy; its origin, rather than proxy headers, controls links/cookies.
func validateTransport(c *Config) (*tls.Config, error) {
	host, port, err := net.SplitHostPort(c.Listen)
	p, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || p < 0 || p > 65535 || !net.ParseIP(host).IsLoopback() {
		return nil, errors.New("HTTP signaling requires an explicit loopback listener; use a local HTTPS reverse proxy")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(c.BaseURL, "#") || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid base URL")
	}
	host = strings.ToLower(u.Hostname())
	if u.Port() != "" {
		p, err := strconv.Atoi(u.Port())
		if err != nil || p < 1 || p > 65535 {
			return nil, errors.New("invalid base URL port")
		}
		u.Host = net.JoinHostPort(host, u.Port())
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Path = ""
	u.RawPath = ""
	c.BaseURL = u.String()
	for _, address := range []string{c.ICEUDP, c.ICETCP, c.TURN} {
		if err := bindAddress(address, c.Public); err != nil {
			return nil, err
		}
	}
	if !c.Public {
		if c.MediaIP != "" || c.TURNHost != "" || c.TURNTLS != "" || c.TURNCert != "" || c.TURNKey != "" {
			return nil, errors.New("public transport options require --public")
		}
		if u.Scheme == "http" && host != "localhost" && !net.ParseIP(host).IsLoopback() {
			return nil, errors.New("non-loopback base URL requires HTTPS")
		}
		return nil, nil
	}
	if u.Scheme != "https" || !validHost(host) {
		return nil, errors.New("--public requires a non-loopback HTTPS --base-url")
	}
	ip := net.ParseIP(c.MediaIP)
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() {
		return nil, errors.New("--public requires a unicast IPv4 --media-ip (advertised, never a wildcard)")
	}
	c.MediaIP = ip.String()
	if c.TURNHost == "" {
		c.TURNHost = host
	}
	c.TURNHost = strings.ToLower(c.TURNHost)
	if !validHost(c.TURNHost) {
		return nil, errors.New("--turn-host must be a non-loopback hostname or IPv4 address without scheme or port")
	}
	if c.TURNTLS == "" || c.TURNCert == "" || c.TURNKey == "" {
		return nil, errors.New("--public requires --turn-tls, --turn-cert and --turn-key")
	}
	if err := bindAddress(c.TURNTLS, true); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(c.TURNCert, c.TURNKey)
	if err != nil {
		return nil, fmt.Errorf("load TURN/TLS certificate and key: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse TURN/TLS certificate: %w", err)
	}
	if err := leaf.VerifyHostname(c.TURNHost); err != nil {
		return nil, fmt.Errorf("TURN/TLS certificate must cover --turn-host: %w", err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("TURN/TLS certificate is not currently valid")
	}
	if len(leaf.ExtKeyUsage) != 0 || len(leaf.UnknownExtKeyUsage) != 0 {
		serverAuth := false
		for _, usage := range leaf.ExtKeyUsage {
			serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny
		}
		if !serverAuth {
			return nil, errors.New("TURN/TLS certificate does not permit server authentication")
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, nil
}

func (s *Server) startTURN(tlsConfig *tls.Config) (err error) {
	udp, tcp, err := listenTURNPair(s.cfg.TURN, net.Listen, net.ListenPacket)
	if err != nil {
		return err
	}
	// Use the same port for UDP and TCP even in ephemeral local tests.
	host, _, _ := net.SplitHostPort(s.cfg.TURN)
	_, port, _ := net.SplitHostPort(udp.LocalAddr().String())
	s.cfg.TURN = net.JoinHostPort(host, port)
	listeners := []net.Listener{tcp}
	defer func() {
		if err != nil {
			udp.Close()
			for _, l := range listeners {
				l.Close()
			}
		}
	}()
	if tlsConfig != nil {
		listener, e := tls.Listen("tcp4", s.cfg.TURNTLS, tlsConfig)
		if e != nil {
			return e
		}
		listeners = append(listeners, listener)
	}
	local := s.Media.UDPAddr()
	if local.IP.IsUnspecified() {
		local.IP = net.ParseIP("127.0.0.1")
	}
	endpoint := &net.UDPAddr{IP: local.IP, Port: local.Port}
	if s.cfg.Public {
		endpoint.IP = net.ParseIP(s.cfg.MediaIP)
	}
	gen := func() *mediaRelay {
		return &mediaRelay{RelayAddressGeneratorStatic: turn.RelayAddressGeneratorStatic{RelayAddress: net.ParseIP("127.0.0.2"), Address: "127.0.0.2"}, endpoint: endpoint, local: local}
	}
	permission := func(_ net.Addr, ip net.IP) bool { return ip.Equal(endpoint.IP) }
	s.turnStreams = newTURNStreamLimits()
	configs := make([]turn.ListenerConfig, 0, len(listeners))
	for _, listener := range listeners {
		configs = append(configs, turn.ListenerConfig{Listener: &turnStreamListener{Listener: listener, limits: s.turnStreams}, RelayAddressGenerator: gen(), PermissionHandler: permission})
	}
	s.turn, err = turn.NewServer(turn.ServerConfig{Realm: "passstream", EventHandler: turn.EventHandler{OnAuth: func(src, dst net.Addr, _, _, _, _ string, verified bool) {
		if verified {
			s.turnStreams.authenticated(src, dst)
		}
	}}, AuthHandler: func(user, realm string, _ net.Addr) ([]byte, bool) {
		parts := strings.SplitN(user, ":", 2)
		n, e := strconv.ParseInt(parts[0], 10, 64)
		if e != nil || len(parts) != 2 || n < time.Now().Unix() || n > time.Now().Add(time.Hour).Unix() {
			return nil, false
		}
		return turn.GenerateAuthKey(user, realm, s.turnPassword(user)), true
	}, PacketConnConfigs: []turn.PacketConnConfig{{PacketConn: udp, RelayAddressGenerator: gen(), PermissionHandler: permission}}, ListenerConfigs: configs})
	return err
}

func (s *Server) iceServers() []webrtc.ICEServer {
	host, port, _ := net.SplitHostPort(s.cfg.TURN)
	if s.cfg.Public {
		host = s.cfg.TURNHost
	}
	urls := []string{"turn:" + net.JoinHostPort(host, port) + "?transport=udp", "turn:" + net.JoinHostPort(host, port) + "?transport=tcp"}
	if s.cfg.TURNTLS != "" {
		_, port, _ := net.SplitHostPort(s.cfg.TURNTLS)
		urls = append(urls, "turns:"+net.JoinHostPort(host, port)+"?transport=tcp")
	}
	user := fmt.Sprintf("%d:%s", time.Now().Add(30*time.Minute).Unix(), store.ID())
	return []webrtc.ICEServer{{URLs: urls, Username: user, Credential: s.turnPassword(user)}}
}
