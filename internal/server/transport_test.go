package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	"github.com/pion/turn/v4"
	"github.com/pion/webrtc/v4"
)

func transportCert(t *testing.T, name string, before, after time.Time, usage x509.ExtKeyUsage) (string, string, *x509.CertPool) {
	t.Helper()
	return transportCertTemplate(t, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: before, NotAfter: after, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, IsCA: true, BasicConstraintsValid: true})
}

func transportCertTemplate(t *testing.T, cert *x509.Certificate) (string, string, *x509.CertPool) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return certPath, keyPath, roots
}

func publicConfig(t *testing.T) (Config, *x509.CertPool) {
	t.Helper()
	cert, key, roots := transportCert(t, "stream.test", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), x509.ExtKeyUsageServerAuth)
	// A free loopback port can be occupied on another host interface. Reserve
	// the actual wildcard scope, keeping all selections distinct until return.
	var reservations []net.Listener
	defer func() {
		for _, l := range reservations {
			l.Close()
		}
	}()
	wildcard := func() string {
		l, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		reservations = append(reservations, l)
		return l.Addr().String()
	}
	return Config{Data: t.TempDir(), Listen: free(t), BaseURL: "https://STREAM.TEST/", Public: true, MediaIP: "198.18.0.1", ICEUDP: wildcard(), ICETCP: wildcard(), TURN: wildcard(), TURNTLS: wildcard(), TURNCert: cert, TURNKey: key}, roots
}

func TestPublicTransportValidation(t *testing.T) {
	valid, _ := publicConfig(t)
	for name, mutate := range map[string]func(*Config){
		"implicit public":            func(c *Config) { c.Public = false },
		"HTTP origin":                func(c *Config) { c.BaseURL = "http://stream.test" },
		"loopback origin":            func(c *Config) { c.BaseURL = "https://localhost" },
		"URL credentials":            func(c *Config) { c.BaseURL = "https://user@stream.test" },
		"URL query":                  func(c *Config) { c.BaseURL = "https://stream.test?" },
		"URL path":                   func(c *Config) { c.BaseURL = "https://stream.test/a" },
		"URL bad port":               func(c *Config) { c.BaseURL = "https://stream.test:65536" },
		"public plaintext HTTP bind": func(c *Config) { c.Listen = "0.0.0.0:8080" },
		"missing advertise":          func(c *Config) { c.MediaIP = "" },
		"wildcard advertise":         func(c *Config) { c.MediaIP = "0.0.0.0" },
		"loopback advertise":         func(c *Config) { c.MediaIP = "127.0.0.1" },
		"multicast advertise":        func(c *Config) { c.MediaIP = "224.0.0.1" },
		"IPv6 advertise":             func(c *Config) { c.MediaIP = "2001:db8::1" },
		"hostname advertise":         func(c *Config) { c.MediaIP = "stream.test" },
		"DNS bind":                   func(c *Config) { c.ICEUDP = "stream.test:8443" },
		"IPv6 bind":                  func(c *Config) { c.ICETCP = "[::]:8443" },
		"ephemeral public bind":      func(c *Config) { c.TURN = "0.0.0.0:0" },
		"link local bind":            func(c *Config) { c.TURN = "169.254.0.1:3478" },
		"missing TLS":                func(c *Config) { c.TURNTLS = "" },
		"missing cert":               func(c *Config) { c.TURNCert = "" },
		"missing key":                func(c *Config) { c.TURNKey = "" },
		"missing cert file":          func(c *Config) { c.TURNCert = filepath.Join(t.TempDir(), "missing") },
		"malformed cert": func(c *Config) {
			c.TURNCert = filepath.Join(t.TempDir(), "invalid.pem")
			if err := os.WriteFile(c.TURNCert, []byte("invalid certificate"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"malformed key": func(c *Config) {
			c.TURNKey = filepath.Join(t.TempDir(), "invalid.pem")
			if err := os.WriteFile(c.TURNKey, []byte("invalid key"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"URL TURN host":          func(c *Config) { c.TURNHost = "https://stream.test" },
		"port in TURN host":      func(c *Config) { c.TURNHost = "stream.test:5349" },
		"loopback TURN host":     func(c *Config) { c.TURNHost = "127.0.0.1" },
		"wrong certificate name": func(c *Config) { c.TURNHost = "other.test" },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid
			mutate(&c)
			if _, err := validateTransport(&c); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
	tlsConfig, err := validateTransport(&valid)
	if err != nil {
		t.Fatal(err)
	}
	if valid.BaseURL != "https://stream.test" || valid.TURNHost != "stream.test" || tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("bad normalization/TLS: %+v", valid)
	}
	for _, v := range []struct {
		name          string
		before, after time.Time
		usage         x509.ExtKeyUsage
	}{
		{"expired", time.Now().Add(-2 * time.Hour), time.Now().Add(-time.Hour), x509.ExtKeyUsageServerAuth},
		{"future", time.Now().Add(time.Hour), time.Now().Add(2 * time.Hour), x509.ExtKeyUsageServerAuth},
		{"client only", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), x509.ExtKeyUsageClientAuth},
	} {
		t.Run(v.name, func(t *testing.T) {
			c := valid
			c.TURNCert, c.TURNKey, _ = transportCert(t, "stream.test", v.before, v.after, v.usage)
			if _, err := validateTransport(&c); err == nil {
				t.Fatal("invalid certificate accepted")
			}
		})
	}
	c := valid
	_, c.TURNKey, _ = transportCert(t, "stream.test", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), x509.ExtKeyUsageServerAuth)
	if _, err := validateTransport(&c); err == nil {
		t.Fatal("mismatched key accepted")
	}
}

func TestLocalTransportDefaults(t *testing.T) {
	c := Config{Listen: "127.0.0.1:0", BaseURL: "http://localhost", ICEUDP: "127.0.0.1:0", ICETCP: "127.0.0.1:0", TURN: "127.0.0.1:0"}
	if tlsConfig, err := validateTransport(&c); err != nil || tlsConfig != nil {
		t.Fatal(tlsConfig, err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.ICEUDP = "0.0.0.0:8443" }, func(c *Config) { c.ICETCP = "192.0.2.1:8443" }, func(c *Config) { c.TURN = "0.0.0.0:3478" }, func(c *Config) { c.MediaIP = "192.0.2.1" }, func(c *Config) { c.BaseURL = "http://stream.test" }} {
		bad := c
		mutate(&bad)
		if _, err := validateTransport(&bad); err == nil {
			t.Fatal("implicit public transport accepted")
		}
	}
}

func TestPublicTransportAdvertisedURLs(t *testing.T) {
	c, _ := publicConfig(t)
	_, err := validateTransport(&c)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: c, secret: "test-only"}
	urls := s.iceServers()[0].URLs
	_, port, _ := net.SplitHostPort(c.TURN)
	_, tlsPort, _ := net.SplitHostPort(c.TURNTLS)
	want := []string{"turn:stream.test:" + port + "?transport=udp", "turn:stream.test:" + port + "?transport=tcp", "turns:stream.test:" + tlsPort + "?transport=tcp"}
	if strings.Join(urls, ",") != strings.Join(want, ",") {
		t.Fatal(urls)
	}
	c.TURNHost = "turn.test"
	s.cfg = c
	if !strings.HasPrefix(s.iceServers()[0].URLs[0], "turn:turn.test:") {
		t.Fatal("TURN host override ignored")
	}
}

func TestPublicTURNTransports(t *testing.T) {
	c, roots := publicConfig(t)
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Register a real SFU ICE transport. All TURN legs must reach its mux,
	// although the advertised address is deliberately absent from this host.
	peer, err := s.Media.Peer("public-transport", true)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if _, err := remote.CreateDataChannel("control", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := remote.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if err := peer.PC.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	gather := webrtc.GatheringCompletePromise(peer.PC)
	answer, err := peer.PC.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.PC.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gather:
	case <-time.After(5 * time.Second):
		t.Fatal("gather timeout")
	}
	params, err := peer.PC.SCTP().Transport().ICETransport().GetLocalParameters()
	if err != nil {
		t.Fatal(err)
	}
	remoteParams, err := remote.SCTP().Transport().ICETransport().GetLocalParameters()
	if err != nil {
		t.Fatal(err)
	}
	request, err := stun.Build(stun.BindingRequest, stun.TransactionID, stun.NewUsername(params.UsernameFragment+":"+remoteParams.UsernameFragment), ice.AttrControlling(1), ice.PriorityAttr(123456), stun.NewShortTermIntegrity(params.Password), stun.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := &net.UDPAddr{IP: net.ParseIP(c.MediaIP), Port: s.Media.UDPAddr().Port}
	credentials := s.iceServers()[0]
	for _, network := range []string{"udp", "tcp", "tls"} {
		t.Run(network, func(t *testing.T) {
			_, port, _ := net.SplitHostPort(s.cfg.TURN)
			if network == "tls" {
				_, port, _ = net.SplitHostPort(c.TURNTLS)
			}
			address := net.JoinHostPort("127.0.0.1", port)
			var conn net.PacketConn
			if network == "udp" {
				conn, err = net.ListenPacket("udp4", "127.0.0.1:0")
			} else {
				var stream net.Conn
				if network == "tls" {
					stream, err = tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp4", address, &tls.Config{RootCAs: roots, ServerName: "stream.test", MinVersion: tls.VersionTLS12})
				} else {
					stream, err = net.DialTimeout("tcp4", address, time.Second)
				}
				if err == nil {
					conn = turn.NewSTUNConn(stream)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			client, err := turn.NewClient(&turn.ClientConfig{Conn: conn, TURNServerAddr: address, Username: credentials.Username, Password: credentials.Credential.(string), RTO: 50 * time.Millisecond})
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
			if err := client.CreatePermission(&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80}); err == nil {
				t.Fatal("internal-service permission granted")
			}
			if err := client.CreatePermission(&net.UDPAddr{IP: net.ParseIP("203.0.113.2"), Port: 80}); err == nil {
				t.Fatal("foreign permission granted")
			}
			if err := allocation.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := allocation.WriteTo(request.Raw, endpoint); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 2048)
			for {
				n, from, err := allocation.ReadFrom(buf)
				if err != nil {
					t.Fatal("advertised -> local SFU route:", err)
				}
				response := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
				if err := response.Decode(); err != nil {
					t.Fatal(err)
				}
				if response.TransactionID != request.TransactionID {
					continue
				}
				if response.Type != stun.BindingSuccess || from.String() != endpoint.String() {
					t.Fatalf("unexpected SFU response: %s %s", response.Type, from)
				}
				if err := stun.NewShortTermIntegrity(params.Password).Check(response); err != nil {
					t.Fatal(err)
				}
				break
			}
			t.Logf("authenticated %s allocation reached advertised %s through wildcard SFU bind; foreign/internal peers refused", network, endpoint)
		})
	}
	_, port, _ := net.SplitHostPort(c.TURNTLS)
	address := net.JoinHostPort("127.0.0.1", port)
	if conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp4", address, &tls.Config{ServerName: "stream.test", MinVersion: tls.VersionTLS12}); err == nil {
		conn.Close()
		t.Fatal("untrusted TURN/TLS certificate accepted")
	}
}

func TestPublicTURNListenerFailureClosesMediaSockets(t *testing.T) {
	c, _ := publicConfig(t)
	occupied, err := net.Listen("tcp4", c.TURNTLS)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(c)
	if s != nil || err == nil {
		t.Fatal("TURN/TLS bind failure silently accepted")
	}
	occupied.Close()
	// Failed construction must close ICE and both earlier TURN listeners.
	s, err = New(c)
	if err != nil {
		t.Fatal("failed construction leaked listeners:", err)
	}
	s.Close()
}

func TestPublicCertificateEKURestrictions(t *testing.T) {
	unknown := []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 55555, 1}}
	for _, test := range []struct {
		name    string
		known   []x509.ExtKeyUsage
		unknown []asn1.ObjectIdentifier
		allowed bool
	}{
		{"unknown-only", nil, unknown, false},
		{"absent-EKU", nil, nil, true},
		{"server-auth-and-unknown", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, unknown, true},
		{"any-and-unknown", []x509.ExtKeyUsage{x509.ExtKeyUsageAny}, unknown, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := publicConfig(t)
			c.Data = filepath.Join(t.TempDir(), "not-created-before-validation")
			template := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"stream.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: test.known, UnknownExtKeyUsage: test.unknown, IsCA: true, BasicConstraintsValid: true}
			var roots *x509.CertPool
			c.TURNCert, c.TURNKey, roots = transportCertTemplate(t, template)
			if _, err := validateTransport(&c); (err == nil) != test.allowed {
				t.Fatalf("validation allowed=%v: %v", test.allowed, err)
			}
			s, err := New(c)
			if !test.allowed {
				if err == nil {
					s.Close()
					t.Fatal("unknown-only EKU allowed")
				}
				if !strings.Contains(err.Error(), "server authentication") {
					t.Fatal("wrong refusal", err)
				}
				if _, err = os.Stat(c.Data); !os.IsNotExist(err) {
					t.Fatal("invalid EKU created state before refusal", err)
				}
				for _, address := range []string{c.Listen, c.ICETCP, c.TURN, c.TURNTLS} {
					listener, err := net.Listen("tcp4", address)
					if err != nil {
						t.Fatal("invalid EKU retained listener", err)
					}
					listener.Close()
				}
				udp, err := net.ListenPacket("udp4", c.ICEUDP)
				if err != nil {
					t.Fatal("invalid EKU retained ICE UDP", err)
				}
				udp.Close()
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp4", turnTCPAddress(t, c.TURNTLS), &tls.Config{RootCAs: roots, ServerName: "stream.test", MinVersion: tls.VersionTLS12})
			if err != nil {
				t.Fatal("trusted TLS client rejected usable EKU", err)
			}
			conn.Close()
		})
	}
}
