package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/passstream/passstream/internal/cli"
	"github.com/passstream/passstream/internal/host/hostcmd"
	"github.com/passstream/passstream/internal/server"
	"golang.org/x/net/netutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "host" {
		os.Exit(hostcmd.Main(os.Args[2:]))
	}
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		os.Exit(cli.Main(os.Args[1:]))
	}
	f := flag.NewFlagSet("serve", flag.ExitOnError)
	c := server.Config{}
	f.StringVar(&c.Data, "data", "data", "private state directory")
	f.StringVar(&c.Listen, "listen", "127.0.0.1:8080", "HTTP listener")
	f.StringVar(&c.BaseURL, "base-url", "http://127.0.0.1:8080", "browser origin")
	f.StringVar(&c.ICEUDP, "ice-udp", "127.0.0.1:8443", "single ICE UDP mux")
	f.StringVar(&c.ICETCP, "ice-tcp", "127.0.0.1:8443", "passive ICE TCP mux")
	f.StringVar(&c.TURN, "turn", "127.0.0.1:3478", "TURN UDP and TCP bind address")
	f.BoolVar(&c.Public, "public", false, "enable explicit public media transport behind a local HTTPS proxy")
	f.StringVar(&c.MediaIP, "media-ip", "", "advertised IPv4 address for ICE; public mode requires port-preserving routing")
	f.StringVar(&c.TURNHost, "turn-host", "", "advertised TURN hostname (defaults to base-url host)")
	f.StringVar(&c.TURNTLS, "turn-tls", "", "TURN/TLS bind address (separate TCP port or IP)")
	f.StringVar(&c.TURNCert, "turn-cert", "", "TURN/TLS PEM certificate chain path")
	f.StringVar(&c.TURNKey, "turn-key", "", "TURN/TLS PEM private key path")
	f.StringVar(&c.TrustProxy, "trust-proxy", "", "comma-separated trusted proxy IPs or CIDRs for X-Forwarded-For")
	f.StringVar(&c.GeoIPCity, "geoip-city", "", "offline DB-IP Lite City mmdb path")
	f.StringVar(&c.GeoIPASN, "geoip-asn", "", "offline DB-IP Lite ASN mmdb path")
	f.BoolVar(&c.KeepIP, "keep-ip", false, "retain raw viewer IP addresses in history (owner opt-in)")
	f.DurationVar(&c.AuditRetention, "audit-retention", 30*24*time.Hour, "audit retention (minimum 1h)")
	maxConnections := f.Int("max-connections", 2048, "maximum simultaneous HTTP connections")
	f.Parse(os.Args[2:])
	if *maxConnections < 1 {
		log.Fatal("max-connections must be positive")
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	s, e := server.New(c)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	defer s.Close()
	// Bound header residence and total accepted connections, including WebSockets.
	h := &http.Server{Addr: c.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.Shutdown(c)
	}()
	fmt.Fprintln(os.Stderr, "PassStream: sender-verified PIN and end-to-end encryption (beta)")
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		log.Fatal(err)
	}
	if e = h.Serve(netutil.LimitListener(listener, *maxConnections)); e != nil && e != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
