package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/store"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

type fixture struct {
	t   *testing.T
	s   *Server
	h   *httptest.Server
	key string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	s, e := New(Config{Data: dir, Listen: "127.0.0.1:0", ICEUDP: "127.0.0.1:0", ICETCP: "127.0.0.1:0", TURN: "127.0.0.1:0", BaseURL: "http://localhost", TrustProxy: "127.0.0.1/32"})
	if e != nil {
		t.Fatal(e)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(func() { s.Close(); h.Close() })
	key, e := os.ReadFile(filepath.Join(dir, "owner-key"))
	if e != nil {
		t.Fatal(e)
	}
	return &fixture{t, s, h, string(key)}
}
func (f *fixture) request(method, path, key, body string) (int, map[string]json.RawMessage, http.Header) {
	f.t.Helper()
	r, _ := http.NewRequest(method, f.h.URL+path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	resp, e := http.DefaultClient.Do(r)
	if e != nil {
		f.t.Fatal(e)
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	if e != nil {
		f.t.Fatal(e)
	}
	out := map[string]json.RawMessage{}
	if len(b) > 0 && json.Unmarshal(b, &out) != nil {
		f.t.Fatalf("not JSON: %s", b)
	}
	return resp.StatusCode, out, resp.Header
}
func (f *fixture) create(body string) (string, string, string) {
	f.t.Helper()
	status, v, _ := f.request("POST", "/api/v1/streams", f.key, body)
	if status != 201 {
		f.t.Fatalf("create %d %s", status, v)
	}
	var id, pub, view string
	_ = json.Unmarshal(v["id"], &id)
	_ = json.Unmarshal(v["publish_token"], &pub)
	_ = json.Unmarshal(v["view_link_base"], &view)
	if !id22.MatchString(id) || len(pub) != 43 || !strings.HasPrefix(view, "http://localhost/v#c=") {
		f.t.Fatal(v)
	}
	if _, ok := v["url"]; ok {
		f.t.Fatal("old field")
	}
	return id, pub, strings.TrimPrefix(view, "http://localhost/v#c=")
}
func (f *fixture) join(id, role, credential, ip string) (*websocket.Conn, map[string]json.RawMessage) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, _, e := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.h.URL, "http")+"/signal", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"http://127.0.0.1:47823"}, "X-Forwarded-For": []string{ip}}})
	if e != nil {
		f.t.Fatal(e)
	}
	f.t.Cleanup(func() { c.CloseNow() })
	hello := message{Type: "join", Protocol: 1, Role: role, ID: id, Credential: credential}
	if role == "publish" {
		hello.Instance = store.ID()
		hello.SenderKind = "local"
	}
	if e = wsjson.Write(ctx, c, hello); e != nil {
		f.t.Fatal(e)
	}
	v := readWS(f.t, c)
	return c, v
}
func readWS(t *testing.T, c *websocket.Conn) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	var v map[string]json.RawMessage
	if e := wsjson.Read(ctx, c, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func readType(t *testing.T, c *websocket.Conn, kind string) map[string]json.RawMessage {
	t.Helper()
	for i := 0; i < 20; i++ {
		v := readWS(t, c)
		if string(v["type"]) == fmt.Sprintf("%q", kind) {
			return v
		}
	}
	t.Fatal("message not found", kind)
	return nil
}
func sendWS(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := wsjson.Write(ctx, c, v); e != nil {
		t.Fatal(e)
	}
}
func errCode(v map[string]json.RawMessage) string {
	var e apiError
	_ = json.Unmarshal(v["error"], &e)
	return e.Code
}
func TestEndpointsAndAuthentication(t *testing.T) {
	f := setup(t)
	id, _, _ := f.create(`{"label":"Test"}`)
	for _, path := range []string{"/api/v1/streams", "/api/v1/streams/" + id, "/api/v1/streams/" + id + "/viewers", "/api/v1/streams/" + id + "/connections", "/api/v1/streams/" + id + "/history", "/api/v1/audit", "/api/v1/settings", "/api/v1/keys"} {
		t.Run(path, func(t *testing.T) {
			status, _, _ := f.request("GET", path, f.key, "")
			if status != 200 {
				t.Fatal(status)
			}
			status, v, _ := f.request("GET", path, "", "")
			if status != 401 || errCode(v) != "unauthorized" {
				t.Fatal(status, v)
			}
		})
	}
	for _, path := range []string{"/healthz", "/readyz", "/llms.txt", "/skill.md"} {
		r, e := http.Get(f.h.URL + path)
		if e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatal(path, r.StatusCode)
		}
	}
	status, v, _ := f.request("GET", "/api/v1/streams/missing", f.key, "")
	if status != 404 || errCode(v) != "not_found" {
		t.Fatal(status, v)
	}
	status, v, _ = f.request("POST", "/api/v1/streams", f.key, strings.Repeat("x", 65537))
	if status != 413 || errCode(v) != "payload_too_large" {
		t.Fatal(status, v)
	}
	for _, b := range []string{`null`, `[]`, `{"pin":{"kind":"future"}}`, `{"pin_explicit":true}`, `{"sender_kind":"evil"}`, `{"pin":{"kind":"digits","length":3}}`} {
		status, v, _ = f.request("POST", "/api/v1/streams", f.key, b)
		if status != 400 {
			t.Fatal(b, status, v)
		}
	}
	status, _, _ = f.request("POST", "/api/v1/keys", f.key, `{"label":"read","scope":"read"}`)
	if status != 201 {
		t.Fatal(status)
	}
	keys, _ := f.s.Store.Keys()
	for _, k := range keys {
		if k.Label == "Initial owner" {
			status, v, _ = f.request("DELETE", "/api/v1/keys/"+k.ID, f.key, "")
			if status != 409 || errCode(v) != "conflict" {
				t.Fatal(status, v)
			}
		}
	}
	r, _ := http.NewRequest("POST", f.h.URL+"/api/v1/streams", nil)
	r.Header.Set("Origin", "https://attacker")
	r.Header.Set("Authorization", "Bearer "+f.key)
	resp, _ := http.DefaultClient.Do(r)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal(resp.StatusCode)
	}
	status, _, _ = f.request("DELETE", "/api/v1/streams/"+id, f.key, "")
	if status != 204 {
		t.Fatal(status)
	}
	status, _, _ = f.request("DELETE", "/api/v1/streams/"+id, f.key, "")
	if status != 204 {
		t.Fatal(status)
	}
}
func TestSettingsCreationOnlyAndKeyScopes(t *testing.T) {
	f := setup(t)
	old, _, _ := f.create(`{}`)
	settings := `{"defaults":{"pin":{"kind":"digits","length":6},"knock":true,"max_viewers":3,"expires_in_s":60},"retention":{"keep_ip":false}}`
	status, _, _ := f.request("PUT", "/api/v1/settings", f.key, settings)
	if status != 200 {
		t.Fatal(status)
	}
	id, _, _ := f.create(`{"pin":{"kind":"none"},"pin_explicit":true,"knock":false}`)
	v, _ := f.s.Store.Get(id)
	if v.Declared.Pin.Kind != "none" || !v.Declared.Knock || v.MaxViewers != 3 || v.ExpiresAt == nil {
		t.Fatal(v)
	}
	v, _ = f.s.Store.Get(old)
	if *v.Declared.Pin.Length != 4 || v.Declared.Knock {
		t.Fatal(v)
	}
	_, key, e := f.s.Store.NewKey("read", "read", nil)
	if e != nil {
		t.Fatal(e)
	}
	status, vv, _ := f.request("POST", "/api/v1/streams", key, `{}`)
	if status != 403 || errCode(vv) != "forbidden_scope" {
		t.Fatal(status, vv)
	}
	status, _, _ = f.request("GET", "/api/v1/streams", key, "")
	if status != 200 {
		t.Fatal(status)
	}
	status, k, _ := f.request("POST", "/api/v1/keys", f.key, `{"label":"agent","scope":"streams"}`)
	if status != 201 {
		t.Fatal(status)
	}
	var token, kid string
	_ = json.Unmarshal(k["key"], &token)
	_ = json.Unmarshal(k["id"], &kid)
	status, _, _ = f.request("GET", "/api/v1/keys", token, "")
	if status != 403 {
		t.Fatal(status)
	}
	status, _, _ = f.request("DELETE", "/api/v1/keys/"+kid, f.key, "")
	if status != 204 {
		t.Fatal(status)
	}
	status, _, _ = f.request("GET", "/api/v1/streams", token, "")
	if status != 401 {
		t.Fatal(status)
	}
}
func TestSessionBearerOnly(t *testing.T) {
	f := setup(t)
	status, _, h := f.request("POST", "/api/v1/session", f.key, "")
	if status != 204 {
		t.Fatal(status)
	}
	cookie := h.Get("Set-Cookie")
	if !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Strict") {
		t.Fatal(cookie)
	}
	call := func(method, path, origin string) int {
		r, _ := http.NewRequest(method, f.h.URL+path, nil)
		r.Header.Set("Cookie", strings.Split(cookie, ";")[0])
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		resp, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if call("GET", "/api/v1/session", "") != 200 || call("GET", "/api/v1/keys", "") != 403 || call("DELETE", "/api/v1/session", "") != 403 || call("DELETE", "/api/v1/session", "http://localhost") != 204 || call("GET", "/api/v1/session", "") != 401 {
		t.Fatal("session rules")
	}
	status, v, _ := f.request("POST", "/api/v1/session", "", `{"key":"`+f.key+`"}`)
	if status != 401 || errCode(v) != "unauthorized" {
		t.Fatal(status, v)
	}
}
func TestEveryRemoteLooseningIsRejected(t *testing.T) {
	f := setup(t)
	id, _, _ := f.create(`{}`)
	for _, body := range []string{`{"op":"resume"}`, `{"op":"unfreeze"}`, `{"op":"pin.rotate"}`, `{"op":"pin.set","args":{"policy":{"kind":"none"}}}`, `{"op":"knock.approve"}`, `{"op":"lock","args":{"locked":false}}`, `{"op":"kick","args":{"rotate_pin":true}}`, `{"op":"kick","args":{"pin":"secret"}}`, `{"op":"kick","args":{"keep_pin":false}}`, `{"op":"pause","args":{"row_code":"K7P"}}`} {
		status, v, _ := f.request("POST", "/api/v1/streams/"+id+"/commands", f.key, body)
		if status != 403 || errCode(v) != "local_owner_required" {
			t.Fatal(body, status, v)
		}
	}
	for _, body := range []string{`{"op":"pause","args":{"typo":true}}`, `{"op":"kick"}`, `{"op":"mode","args":{"mode":"future"}}`, `{"op":"lock","args":{}}`, `{"op":"kick","args":{"admission_id":"bad"}}`} {
		status, v, _ := f.request("POST", "/api/v1/streams/"+id+"/commands", f.key, body)
		if status != 400 || errCode(v) != "invalid_argument" {
			t.Fatal(body, status, v)
		}
	}
}
func TestCommandStateMachine(t *testing.T) {
	f := setup(t)
	id, pub, _ := f.create(`{}`)
	path := "/api/v1/streams/" + id + "/commands"
	status, v, _ := f.request("POST", path, f.key, `{"id":"offline","op":"pause"}`)
	if status != 409 || errCode(v) != "sender_offline" {
		t.Fatal(status, v)
	}
	p, _ := f.join(id, "publish", pub, "10.0.0.1")
	readType(t, p, "e2ee.peers")
	body := `{"id":"same","op":"kick","args":{"lock":true,"admission_id":"` + store.ID() + `"}}`
	results := make(chan int, 2)
	go func() { s, _, _ := f.request("POST", path, f.key, body); results <- s }()
	control := readType(t, p, "control")
	go func() { s, _, _ := f.request("POST", path, f.key, body); results <- s }()
	time.Sleep(20 * time.Millisecond)
	sendWS(t, p, map[string]any{"type": "control.result", "id": json.RawMessage(control["id"]), "ok": false, "error": apiError{Code: "unknown_admission", Message: "gone", Details: map[string]any{"locked": true}}})
	for i := 0; i < 2; i++ {
		if s := <-results; s != 404 {
			t.Fatal(s)
		}
	}
	status, v, h := f.request("POST", path, f.key, body)
	if status != 404 || h.Get("Idempotent-Replay") != "true" {
		t.Fatal(status, v, h)
	}
	status, v, _ = f.request("POST", path, f.key, `{"id":"same","op":"pause"}`)
	if status != 409 || errCode(v) != "idempotency_conflict" {
		t.Fatal(status, v)
	}
	go func() { s, _, _ := f.request("POST", path, f.key, `{"id":"timeout","op":"pause"}`); results <- s }()
	first := readType(t, p, "control")
	if s := <-results; s != 504 {
		t.Fatal(s)
	}
	go func() { s, _, _ := f.request("POST", path, f.key, `{"id":"timeout","op":"pause"}`); results <- s }()
	second := readType(t, p, "control")
	if !bytes.Equal(first["id"], second["id"]) {
		t.Fatal("forward id changed")
	}
	sendWS(t, p, map[string]any{"type": "control.result", "id": json.RawMessage(second["id"]), "ok": true, "result": map[string]any{}})
	if s := <-results; s != 200 {
		t.Fatal(s)
	}
	for _, op := range []string{"mode", "lock", "knock.deny"} {
		args := map[string]any{}
		switch op {
		case "mode":
			args["mode"] = "text"
		case "lock":
			args["locked"] = true
		case "knock.deny":
			args["knock_id"] = store.ID()
		}
		body, _ := json.Marshal(map[string]any{"op": op, "args": args})
		go func() { s, _, _ := f.request("POST", path, f.key, string(body)); results <- s }()
		c := readType(t, p, "control")
		sendWS(t, p, map[string]any{"type": "control.result", "id": json.RawMessage(c["id"]), "ok": true, "result": map[string]any{}})
		if s := <-results; s != 200 {
			t.Fatal(s)
		}
	}
	status, _, _ = f.request("POST", path, f.key, `{"op":"stop"}`)
	if status != 200 {
		t.Fatal(status)
	}
}
func TestRelayLifecycleBoundsAndBans(t *testing.T) {
	f := setup(t)
	id, pub, cap := f.create(`{}`)
	p, _ := f.join(id, "publish", pub, "10.0.0.1")
	readType(t, p, "e2ee.peers")
	v, joined := f.join(id, "view", cap, "10.0.0.2")
	var conn string
	_ = json.Unmarshal(joined["connection_id"], &conn)
	b := base64.RawURLEncoding.EncodeToString([]byte("opaque"))
	sendWS(t, v, map[string]string{"type": "e2ee", "b": b})
	relay := readType(t, p, "e2ee")
	if string(relay["peer"]) != fmt.Sprintf("%q", conn) || string(relay["b"]) != fmt.Sprintf("%q", b) {
		t.Fatal(relay)
	}
	sendWS(t, p, map[string]string{"type": "e2ee", "to": conn, "b": b})
	readType(t, v, "e2ee")
	for i := 0; i < 40; i++ {
		sendWS(t, v, map[string]string{"type": "e2ee", "b": b})
	}
	sendWS(t, v, map[string]string{"type": "e2ee", "b": base64.RawURLEncoding.EncodeToString(make([]byte, 4097))})
	time.Sleep(30 * time.Millisecond)
	f.s.mu.Lock()
	drops := f.s.dropped["relay_rate"] + f.s.dropped["relay_size"]
	f.s.mu.Unlock()
	if drops == 0 {
		t.Fatal("unbounded relay")
	}
	status, out, _ := f.request("POST", "/api/v1/streams/"+id+"/commands", f.key, `{"op":"ban","args":{"connection_id":"`+conn+`"}}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	_, bad := f.join(id, "view", cap, "10.0.0.2")
	if string(bad["code"]) != `"unauthorized"` {
		t.Fatal(bad)
	}
}
func validState(id, instance string) hostview.SenderState {
	n := uint64(4)
	return hostview.SenderState{V: 1, StreamID: id, Instance: instance, Seq: 1, Phase: "live", Mode: "text", E2EE: hostview.E2EE{Status: "beta"}, Access: hostview.Access{Pin: hostview.PinState{Required: true, Kind: "digits", Length: &n}, Failures: hostview.Failures{Limit: 20}}, Admissions: []hostview.Admission{}, Knocks: []hostview.Knock{}, Ends: []hostview.End{}}
}
func TestStateAllowlistPrivacyAndSSE(t *testing.T) {
	f := setup(t)
	var logs lockedBuffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	id, pub, cap := f.create(`{}`)
	p, _ := f.join(id, "publish", pub, "10.0.0.1")
	readType(t, p, "e2ee.peers")
	v, joined := f.join(id, "view", cap, "10.0.0.2")
	var conn string
	_ = json.Unmarshal(joined["connection_id"], &conn)
	instance := publisherInstance(f.s, id)
	state := validState(id, instance)
	state.Admissions = []hostview.Admission{{AdmissionID: store.ID(), ViewerID: store.ID(), RowCode: "K7P", PeerID: conn, Connected: true, AdmittedAt: 1}}
	b, _ := json.Marshal(state)
	b = append(b[:len(b)-1], []byte(`,"pin":"SECRET_PIN","link":"SECRET_LINK"}`)...)
	sendWS(t, p, map[string]any{"type": "sender.state", "state": json.RawMessage(b)})
	sendWS(t, p, map[string]string{"type": "ping"})
	readType(t, p, "pong")
	status, out, _ := f.request("GET", "/api/v1/streams/"+id+"/viewers", f.key, "")
	if status != 200 || string(out["watching"]) != "1" {
		t.Fatal(status, out)
	}
	data, _ := json.Marshal(out)
	if bytes.Contains(data, []byte("SECRET")) {
		t.Fatal("unknown secret retained")
	}
	r, _ := http.NewRequest("GET", f.h.URL+"/api/v1/streams/"+id+"/events", nil)
	r.Header.Set("Authorization", "Bearer "+f.key)
	resp, e := http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	reader := bufio.NewReader(resp.Body)
	for _, kind := range []string{"stream", "viewers"} {
		line, e := reader.ReadString('\n')
		if e != nil || line != "event: "+kind+"\n" {
			t.Fatal(line, e)
		}
		for {
			line, e = reader.ReadString('\n')
			if e != nil {
				t.Fatal(e)
			}
			if line == "\n" {
				break
			}
		}
	}
	resp.Body.Close()
	v.CloseNow()
	time.Sleep(20 * time.Millisecond)
	_, cs, _ := f.request("GET", "/api/v1/streams/"+id+"/connections", f.key, "")
	var connections []hostview.Connection
	_ = json.Unmarshal(cs["connections"], &connections)
	if len(connections) != 1 || connections[0].IP != nil || connections[0].Geo != nil {
		t.Fatal(connections)
	}
	l := f.s.runtime(id)
	defer f.s.release(l)
	l.mu.Lock()
	state.Seq++
	state.Admissions = nil
	state.Ends = []hostview.End{{Kind: "admission", AdmissionID: l.state.Admissions[0].AdmissionID, ViewerID: l.state.Admissions[0].ViewerID, Cause: "kicked"}}
	l.state = &state
	_, _ = f.s.build(mustStream(t, f.s, id), l)
	for i := range l.mem.Rows {
		n := time.Now().Add(-6 * time.Minute).UnixMilli()
		l.mem.Rows[i].LeftAt = &n
	}
	_, _ = f.s.build(mustStream(t, f.s, id), l)
	l.mu.Unlock()
	_, history, _ := f.request("GET", "/api/v1/streams/"+id+"/history", f.key, "")
	if string(history["entries"]) == "[]" {
		t.Fatal(history)
	}
	for _, secret := range []string{f.key, pub, cap, "SECRET_PIN", "SECRET_LINK"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret logged")
		}
	}
}
func mustStream(t *testing.T, s *Server, id string) store.Stream {
	t.Helper()
	v, e := s.Store.Get(id)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestLimiterAndOverflow(t *testing.T) {
	f := setup(t)
	f.s.mu.Lock()
	for i := 0; i < 32; i++ {
		if !f.s.allow("test", 16, 32) {
			t.Fatal(i)
		}
	}
	if f.s.allow("test", 16, 32) {
		t.Fatal("burst unbounded")
	}
	f.s.mu.Unlock()
	id, _, cap := f.create(`{}`)
	v, _ := f.join(id, "view", cap, "10.0.0.8")
	p := &signalPeer{conn: v, queue: make(chan any, 64), done: make(chan struct{})}
	for i := 0; i < 64; i++ {
		p.queue <- 1
	}
	p.enqueue(2)
	select {
	case <-p.done:
	default:
		t.Fatal("overflow not closed")
	}
}

func TestPublisherLifecycleAndProtocolFence(t *testing.T) {
	f := setup(t)
	id, pub, cap := f.create(`{}`)
	v, j := f.join(id, "view", cap, "10.0.0.2")
	conn := string(j["connection_id"])
	p1, _ := f.join(id, "publish", pub, "10.0.0.1")
	peers := readType(t, p1, "e2ee.peers")
	if !bytes.Contains(peers["peers"], j["connection_id"]) {
		t.Fatal(peers)
	}
	readType(t, v, "e2ee.sender_back")
	p2, _ := f.join(id, "publish", pub, "10.0.0.1")
	readType(t, p2, "e2ee.peers")
	readType(t, v, "e2ee.sender_back")
	p2.CloseNow()
	readType(t, v, "e2ee.sender_away")
	p3, _ := f.join(id, "publish", pub, "10.0.0.1")
	readType(t, p3, "e2ee.peers")
	readType(t, v, "e2ee.sender_back")
	sendWS(t, p3, map[string]any{"type": "e2ee.close", "to": json.RawMessage(j["connection_id"])})
	closed := readType(t, p3, "e2ee.peer_closed")
	if string(closed["peer"]) != conn || string(closed["reason"]) != `"kicked"` {
		t.Fatal(closed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, _, e := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.h.URL, "http")+"/signal", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer c.CloseNow()
	sendWS(t, c, message{Type: "join", Role: "view", Credential: cap})
	bad := readType(t, c, "error")
	if string(bad["code"]) != `"upgrade_required"` {
		t.Fatal(bad)
	}
}
func TestStateAndJoinSurviveStoreRestart(t *testing.T) {
	f := setup(t)
	id, pub, cap := f.create(`{}`)
	p, _ := f.join(id, "publish", pub, "10.0.0.1")
	readType(t, p, "e2ee.peers")
	_, j := f.join(id, "view", cap, "10.0.0.2")
	var conn string
	_ = json.Unmarshal(j["connection_id"], &conn)
	instance := publisherInstance(f.s, id)
	state := validState(id, instance)
	state.Admissions = []hostview.Admission{{AdmissionID: store.ID(), ViewerID: store.ID(), RowCode: "K7P", PeerID: conn, Connected: true, AdmittedAt: 1}}
	sendWS(t, p, map[string]any{"type": "sender.state", "state": state})
	sendWS(t, p, map[string]any{"type": "ping"})
	readType(t, p, "pong")
	restored := f.s.readRuntime(id)
	if restored.state == nil || restored.state.Admissions[0].AdmissionID != state.Admissions[0].AdmissionID {
		t.Fatal("lost sender state")
	}
	list, _, _ := hostview.Join(hostview.JoinInput{Source: "server", Now: time.Now().UnixMilli(), State: restored.state, StateAt: restored.stateAt}, restored.mem)
	if list.Watching != 1 || !list.SenderStale {
		t.Fatal(list)
	}

}

func TestViewerOverflowCannotDisconnectPublisher(t *testing.T) {
	f := setup(t)
	id, _, _ := f.create(`{}`)
	publisher := &signalPeer{role: "publish", queue: make(chan any, 64), control: make(chan any, 64), done: make(chan struct{})}
	for i := 0; i < 64; i++ {
		publisher.queue <- 1
	}
	viewer := &signalPeer{done: make(chan struct{}), role: "view", stream: id, id: "c_" + store.ID(), ip: "192.0.2.1"}
	l := f.s.runtime(id)
	l.mu.Lock()
	l.publisher = publisher
	l.mu.Unlock()
	defer f.s.release(l)
	if e := f.s.handleSignal(viewer, message{Type: "e2ee", B: "AQ"}); e != nil {
		t.Fatal(e)
	}
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if f.s.dropped["relay_queue"] != 1 {
		t.Fatal("queue did not drop")
	}
	select {
	case <-publisher.done:
		t.Fatal("viewer disconnected publisher")
	default:
	}
}

func publisherInstance(s *Server, id string) string {
	l := s.runtime(id)
	defer s.release(l)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.publisher.instance
}

// A viewer whose stream ends is told so (close 1008 "stream_ended") instead
// of being dropped: dropped, it reconnects into a refused join, which the
// viewer can't tell from a kick.
func TestViewerSocketClosesWithStreamEnded(t *testing.T) {
	f := setup(t)
	id, pub, cap := f.create(`{}`)
	p, _ := f.join(id, "publish", pub, "10.0.0.1")
	readType(t, p, "e2ee.peers")
	v, j := f.join(id, "view", cap, "10.0.0.2")
	if string(j["type"]) != `"joined"` {
		t.Fatal(j)
	}
	if status, _, _ := f.request("DELETE", "/api/v1/streams/"+id, f.key, ""); status != 204 {
		t.Fatal(status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var m map[string]json.RawMessage
		e := wsjson.Read(ctx, v, &m)
		if e == nil {
			continue
		}
		if websocket.CloseStatus(e) != websocket.StatusPolicyViolation {
			t.Fatalf("close: %v", e)
		}
		var ce websocket.CloseError
		if !errors.As(e, &ce) || ce.Reason != "stream_ended" {
			t.Fatalf("reason: %v", e)
		}
		return
	}
}
