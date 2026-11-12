package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"

	"errors"

	"github.com/passstream/passstream/internal/sfu"
	"github.com/passstream/passstream/internal/store"
	"github.com/passstream/passstream/web"
	"github.com/pion/turn/v4"
	"github.com/pion/webrtc/v4"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Data, Listen, BaseURL, ICEUDP, ICETCP, TURN   string
	Public                                        bool
	MediaIP, TURNHost, TURNTLS, TURNCert, TURNKey string
	TrustProxy, GeoIPCity, GeoIPASN               string
	KeepIP                                        bool
	AuditRetention                                time.Duration
}
type Server struct {
	cfg              Config
	Store            *store.Store
	Media            *sfu.Engine
	turn             *turn.Server
	turnStreams      *turnStreamLimits
	secret           string
	done             chan struct{}
	closeOnce        sync.Once
	mu               sync.Mutex
	peers            map[string]*sfu.Peer
	live             map[string]*liveStream
	commands         map[string]*commandCall
	inflight         map[string]*commandCall
	subscriberCounts map[string]int
	limits           map[string]*bucket
	facts            *factsDB
	dropped          map[string]uint64
}

func New(c Config) (*Server, error) {
	tlsConfig, e := validateTransport(&c)
	if e != nil {
		return nil, e
	}
	if c.AuditRetention == 0 {
		c.AuditRetention = 30 * 24 * time.Hour
	}
	if c.AuditRetention < time.Hour {
		return nil, errors.New("audit retention must be at least one hour")
	}
	st, e := store.Open(c.Data)
	if e != nil {
		return nil, e
	}
	var m *sfu.Engine
	if c.Public {
		m, e = sfu.NewPublic(c.ICEUDP, c.ICETCP, c.MediaIP)
	} else {
		m, e = sfu.New(c.ICEUDP, c.ICETCP)
	}
	if e != nil {
		st.DB.Close()
		return nil, e
	}
	s := &Server{cfg: c, Store: st, Media: m, secret: store.Token(), done: make(chan struct{}), peers: map[string]*sfu.Peer{}, live: map[string]*liveStream{}, commands: map[string]*commandCall{}, inflight: map[string]*commandCall{}, subscriberCounts: map[string]int{}, limits: map[string]*bucket{}, dropped: map[string]uint64{}}
	s.facts, e = openFacts(c)
	if e != nil {
		s.Close()
		return nil, e
	}
	if e = s.startTURN(tlsConfig); e != nil {
		s.Close()
		return nil, e
	}
	go s.reap()
	return s, nil
}
func (s *Server) turnPassword(user string) string {
	m := hmac.New(sha256.New, []byte(s.secret))
	m.Write([]byte(user))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}
func (s *Server) Close() {
	s.closeOnce.Do(s.close)
}
func (s *Server) close() {
	if s.done != nil {
		close(s.done)
	}
	if s.turn != nil {
		s.turn.Close()
	}
	if s.turnStreams != nil {
		s.turnStreams.close()
	}
	s.mu.Lock()
	live := make([]*liveStream, 0, len(s.live))
	for _, l := range s.live {
		live = append(live, l)
	}
	s.mu.Unlock()
	for _, l := range live {
		l.mu.Lock()
		for _, p := range l.sockets {
			p.close("timeout")
		}
		l.mu.Unlock()
	}
	if s.facts != nil {
		s.facts.close()
	}
	if s.Media != nil {
		s.Media.Close()
	}
	if s.Store != nil {
		s.Store.DB.Close()
	}
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if e := s.Store.DB.PingContext(r.Context()); e != nil {
			writeError(w, 503, "internal", nil)
			return
		}
		w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("GET /llms.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(contract))
	})
	mux.HandleFunc("GET /skill.md", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("# PassStream\nRead /llms.txt for the API contract.\n"))
	})
	s.routes(mux)
	mux.HandleFunc("GET /signal", s.signal)
	dist, _ := fs.Sub(web.Dist, "dist")
	files := http.FileServer(http.FS(dist))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			files.ServeHTTP(w, r)
			return
		}
		b, e := fs.ReadFile(dist, "index.html")
		if e != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(503)
			w.Write([]byte("<h1>PassStream web is not built</h1><p>Run npm --prefix web ci &amp;&amp; npm --prefix web run build, then rebuild Go.</p>"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Safe mode's text recognition in the browser sender (/host) compiles WASM
		// ('wasm-unsafe-eval': not JS eval) and needs cross-origin isolation for
		// threads. Workers take these from their own (asset) responses, so all
		// responses carry them; every page loads same-origin resources only.
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws:; media-src 'self' blob:; img-src 'self' data:; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) reap() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
			s.reapOnce()
		}
	}
}

// newPeerGrace bounds how long a media peer may wait for its first offer.
const newPeerGrace = 2 * time.Minute

func (s *Server) reapOnce() {
	s.mu.Lock()
	for key, p := range s.peers {
		state := p.PC.ConnectionState()
		// A new PC may wait for its client's first offer: E2EE clients offer only
		// once a viewer starts admission or the sender starts capture.
		if state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateNew && p.Age() > newPeerGrace {
			p.Close()
			delete(s.peers, key)
		}
	}
	s.mu.Unlock()
	s.maintain()
	_ = s.Store.Prune(time.Now())
	_ = s.Store.PruneAudit(time.Now(), s.cfg.AuditRetention)
}
