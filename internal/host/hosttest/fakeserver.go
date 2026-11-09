// Package hosttest has test doubles for the host daemon: a fake PassStream
// server (CONTRACT §9, the parts the daemon calls), a simulated sender page
// that speaks CONTRACT §11, and a fake "browser" process the daemon can
// supervise. Nothing here is used by the shipped daemon.
package hosttest

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/passstream/passstream/internal/host/proto"
)

// Scenario scripts the simulated sender (the plugin mock's --scenario).
type Scenario struct {
	SignalURL string   `json:"signal_url,omitempty"` // real-server plugin bridge only
	Names     []string `json:"names"`
	Step      float64  `json:"step"`      // seconds between scripted steps
	Phase     string   `json:"phase"`     // report this phase instead of the real one
	FailOp    string   `json:"fail_op"`   // make this command fail…
	FailCode  string   `json:"fail_code"` // …with this code
	PickerMs  int      `json:"picker_ms"` // delay before the "first frame"
	NoLive    bool     `json:"no_live"`   // never pick a screen
}

// Has reports whether the scenario includes name.
func (s Scenario) Has(name string) bool {
	for _, n := range s.Names {
		if n == name {
			return true
		}
	}
	return false
}

// FakeServer is an in-process PassStream server API.
type FakeServer struct {
	real *realBackend
	URL  string
	// PublicURL is the base in view links (default URL); a harness may
	// present another origin and route API calls here (see Transport).
	PublicURL string
	Key       string
	Settings  map[string]any
	// Unauthorized makes every API call 401.
	Unauthorized bool
	Scenario     Scenario

	mu      sync.Mutex
	srv     *http.Server
	ln      net.Listener
	calls   []string
	streams map[string]*fakeStream
	created []map[string]any
	events  map[string][]chan string
	cmdIDs  []string
	silent  bool
	failCmd int
}

// FailCommands makes the next n POST /commands fail with 503
// server_unreachable (after recording their ids).
func (f *FakeServer) FailCommands(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCmd = n
}

// SetSilentEnd makes EndStream skip the live "ended" event, as if the
// daemon's events connection was down at that moment.
func (f *FakeServer) SetSilentEnd(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.silent = v
}

type fakeStream struct {
	id        string
	deleted   bool
	endReason string
	// commands holds ban results by command id: a repeat is a replay.
	commands map[string]map[string]any
	conns    map[string]proto.Connection
	owners   map[string][]string // connection id -> admission ids
}

// NewFakeServer starts a fake server on a free loopback port.
func NewFakeServer(key string) (*FakeServer, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &FakeServer{Key: key, ln: ln, URL: "http://" + ln.Addr().String(), streams: map[string]*fakeStream{},
		events: map[string][]chan string{},
		Settings: map[string]any{"defaults": map[string]any{"pin": map[string]any{"kind": "digits", "length": 4}, "knock": false,
			"max_viewers": 50, "expires_in_s": nil}, "retention": map[string]any{"keep_ip": false}}}
	f.srv = &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: 5 * time.Second}
	go f.srv.Serve(ln)
	return f, nil
}

func (f *FakeServer) public() string {
	if f.PublicURL != "" {
		return f.PublicURL
	}
	return f.URL
}

// Transport sends every request to this server, whatever its URL says.
func (f *FakeServer) Transport() http.RoundTripper {
	return rewrite{addr: strings.TrimPrefix(f.URL, "http://")}
}

type rewrite struct{ addr string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = "http", r.addr
	return http.DefaultTransport.RoundTrip(req)
}

// Close stops the server.
func (f *FakeServer) Close() {
	f.srv.Close()
	if f.real != nil {
		f.real.close()
	}
}

// Calls returns "METHOD path" for every API call so far (never bodies).
func (f *FakeServer) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Created returns the CreateStream bodies received.
func (f *FakeServer) Created() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.created...)
}

// Deleted reports whether the stream was deleted.
func (f *FakeServer) Deleted(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.streams[id]
	return s != nil && s.deleted
}

// CommandIDs lists the command ids of every POST /commands, in order.
func (f *FakeServer) CommandIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.cmdIDs...)
}

// DropEvents closes every open events connection of a stream (a network
// blip); the daemon reconnects.
func (f *FakeServer) DropEvents(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.events[id] {
		select {
		case ch <- "":
		default:
		}
	}
	f.events[id] = nil
}

// EndStream ends a stream server-side (like a dashboard DELETE).
func (f *FakeServer) EndStream(id, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.streams[id]; s != nil && !s.deleted {
		s.deleted, s.endReason = true, reason
	}
	if f.silent {
		return
	}
	for _, ch := range f.events[id] {
		select {
		case ch <- reason:
		default:
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": code}})
}

func (f *FakeServer) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// Test hooks used by the simulated page; no auth (loopback, tests only).
	switch {
	case path == "/test/scenario":
		f.mu.Lock()
		sc := f.Scenario
		f.mu.Unlock()
		writeJSON(w, 200, sc)
		return
	case path == "/test/conn" && r.Method == "POST":
		var body struct {
			StreamID   string           `json:"stream_id"`
			Capability string           `json:"capability"`
			Connection proto.Connection `json:"connection"`
			Admissions []string         `json:"admissions"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body) != nil {
			apiErr(w, 400, "invalid_argument")
			return
		}
		if f.real != nil {
			f.real.mu.Lock()
			f.real.caps[body.StreamID] = body.Capability
			f.real.mu.Unlock()
			id, err := f.real.connection(body.StreamID, body.Connection)
			if err != nil {
				apiErr(w, 500, "internal")
				return
			}
			writeJSON(w, 200, map[string]string{"connection_id": id})
			return
		}
		f.mu.Lock()
		if s := f.streams[body.StreamID]; s != nil {
			s.conns[body.Connection.ConnectionID] = body.Connection
			if body.Admissions != nil {
				s.owners[body.Connection.ConnectionID] = body.Admissions
			}
		}
		f.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+path)
	unauth := f.Unauthorized || r.Header.Get("Authorization") != "Bearer "+f.Key
	f.mu.Unlock()
	if unauth {
		apiErr(w, 401, "unauthorized")
		return
	}
	if f.real != nil {
		f.real.proxy.ServeHTTP(w, r)
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/api/v1/settings" && r.Method == "GET":
		f.mu.Lock()
		s := f.Settings
		f.mu.Unlock()
		writeJSON(w, 200, s)
	case path == "/api/v1/streams" && r.Method == "POST":
		var body map[string]any
		if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body) != nil {
			apiErr(w, 400, "invalid_argument")
			return
		}
		id := proto.NewId22()
		capTok, _ := proto.NewToken43()
		pub, _ := proto.NewToken43()
		f.mu.Lock()
		f.created = append(f.created, body)
		f.streams[id] = &fakeStream{id: id, conns: map[string]proto.Connection{}, owners: map[string][]string{}, commands: map[string]map[string]any{}}
		f.mu.Unlock()
		writeJSON(w, 201, map[string]any{"id": id, "created_at": time.Now().UnixMilli(), "phase": "pending",
			"view_link_base": f.public() + "/v#c=" + capTok, "publish_token": pub, "host_url": f.public() + "/host#id=" + id + "&t=" + pub})
	case len(parts) >= 4 && parts[0] == "api" && parts[2] == "streams":
		f.mu.Lock()
		s := f.streams[parts[3]]
		f.mu.Unlock()
		if s == nil {
			apiErr(w, 404, "not_found")
			return
		}
		f.streamOp(w, r, s, parts[4:])
	default:
		apiErr(w, 404, "not_found")
	}
}

// snapshot is the stream's Stream object (the parts the daemon reads).
func (f *FakeServer) snapshot(s *fakeStream) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := map[string]any{"id": s.id, "phase": "live", "end_reason": nil}
	if s.deleted {
		m["phase"], m["end_reason"] = "ended", s.endReason
	}
	return m
}

func (f *FakeServer) streamOp(w http.ResponseWriter, r *http.Request, s *fakeStream, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == "GET":
		writeJSON(w, 200, f.snapshot(s))
	case len(rest) == 0 && r.Method == "DELETE":
		f.EndStream(s.id, "deleted")
		w.WriteHeader(204)
	case len(rest) == 1 && rest[0] == "connections" && r.Method == "GET":
		f.mu.Lock()
		cs := []proto.Connection{}
		for _, c := range s.conns {
			cs = append(cs, c)
		}
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"connections": cs})
	case len(rest) == 1 && rest[0] == "commands" && r.Method == "POST":
		var cmd struct {
			ID   string         `json:"id"`
			Op   string         `json:"op"`
			Args map[string]any `json:"args"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&cmd) != nil || cmd.Op != "ban" {
			apiErr(w, 400, "invalid_argument")
			return
		}
		cid, _ := cmd.Args["connection_id"].(string)
		f.mu.Lock()
		f.cmdIDs = append(f.cmdIDs, cmd.ID)
		if f.failCmd > 0 {
			f.failCmd--
			f.mu.Unlock()
			apiErr(w, 503, "server_unreachable")
			return
		}
		if res, ok := s.commands[cmd.ID]; ok && cmd.ID != "" {
			f.mu.Unlock()
			w.Header().Set("Idempotent-Replay", "true")
			writeJSON(w, 200, map[string]any{"id": cmd.ID, "ok": true, "result": res})
			return
		}
		_, ok := s.conns[cid]
		ids := append([]string{}, s.owners[cid]...)
		res := map[string]any{"ip_banned": true, "admission_ids": ids}
		if ok && cmd.ID != "" {
			s.commands[cmd.ID] = res
		}
		f.mu.Unlock()
		if !ok {
			apiErr(w, 404, "not_found")
			return
		}
		writeJSON(w, 200, map[string]any{"id": cmd.ID, "ok": true, "result": res})
	case len(rest) == 1 && rest[0] == "events" && r.Method == "GET":
		ch := make(chan string, 1)
		f.mu.Lock()
		deleted := s.deleted
		f.events[s.id] = append(f.events[s.id], ch)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		// Like the server (CONTRACT §9.3): the current stream first, and no
		// replay of an "ended" sent while this client was away.
		snap, _ := json.Marshal(f.snapshot(s))
		fmt.Fprintf(w, ": hello\n\nevent: stream\ndata: %s\n\n", snap)
		if fl != nil {
			fl.Flush()
		}
		if deleted {
			return
		}
		select {
		case reason := <-ch:
			if reason == "" {
				return // DropEvents
			}
			fmt.Fprintf(w, "event: ended\ndata: {\"reason\":%q}\n\n", reason)
			if fl != nil {
				fl.Flush()
			}
		case <-r.Context().Done():
		}
	default:
		apiErr(w, 404, "not_found")
	}
}
