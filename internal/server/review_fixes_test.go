package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/store"
)

func TestReviewMultilineProxyChain(t *testing.T) {
	f, err := openFacts(Config{TrustProxy: "127.0.0.1/32,10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Add("X-Forwarded-For", "203.0.113.99") // attacker supplied first line
	r.Header.Add("X-Forwarded-For", "192.0.2.8, 10.0.0.1")
	if got := f.address(r); got != "192.0.2.8" {
		t.Fatal(got)
	}
}

func TestReviewKnockEndAllowlist(t *testing.T) {
	for _, reason := range []string{"denied", "timeout", "peer_closed", "replaced", "pin_rotated", "stopped", "commit_failed", "queue_full", "SECRET", ""} {
		t.Run(reason, func(t *testing.T) {
			good := reason != "SECRET" && reason != ""
			st := validState(store.ID(), store.ID())
			st.Ends = []hostview.End{{Kind: "knock", KnockID: store.ID(), Reason: reason}}
			b, _ := json.Marshal(st)
			_, err := parseState(b)
			if (err == nil) != good {
				t.Fatalf("state accepted=%v", err == nil)
			}
			for _, id := range []string{store.ID(), "SECRET"} {
				b, _ = json.Marshal(map[string]any{"kind": "knock_ended", "knock_id": id, "reason": reason})
				if got := parseAlert(b); (got != nil) != (good && id != "SECRET") {
					t.Fatal(string(b), got)
				}
			}
		})
	}
}

func TestReviewIPv6HandshakeLimit(t *testing.T) {
	f := setup(t)
	for i := 0; i < 20; i++ {
		if !f.s.permit("handshake-ip:"+store.IPKey(fmt.Sprintf("2001:db8::%x", i+1)), 2, 20) {
			t.Fatal(i)
		}
	}
	r := httptest.NewRequest("GET", "http://localhost/signal", nil)
	r.RemoteAddr = "[2001:db8::abcd]:1234"
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	if w.Code != 429 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestReviewSlowPublisherRelayIsolation(t *testing.T) {
	f := setup(t)
	id, _, _ := f.create(`{}`)
	// A stopped reader makes queue occupancy deterministic, independent of kernel buffers.
	pub := &signalPeer{role: "publish", stream: id, queue: make(chan any, 64), control: make(chan any, 64), done: make(chan struct{}), peerChanges: make(chan struct{}, 1), drop: f.s.drop}
	l := f.s.runtime(id)
	defer f.s.release(l)
	l.mu.Lock()
	l.publisher = pub
	l.mu.Unlock()
	for i := 0; i < 200; i++ {
		viewer := &signalPeer{role: "view", id: fmt.Sprint(i), stream: id, ip: fmt.Sprintf("192.0.2.%d", i%200), done: make(chan struct{})}
		if err := f.s.handleSignal(viewer, message{Type: "e2ee", B: "AQ"}); err != nil {
			t.Fatal(err)
		}
		if i >= 32 {
			select {
			case <-viewer.done:
				l.mu.Lock()
				f.s.peerClosed(l, viewer)
				l.mu.Unlock()
			default:
				t.Fatal("flooding viewer not backpressured")
			}
		}
	}
	if len(pub.queue) != 32 {
		t.Fatalf("relay queue: %d", len(pub.queue))
	}
	for _, kind := range []string{"pong", "e2ee.peer_closed", "control", "stream.ended"} {
		if !pub.enqueue(map[string]any{"type": kind}) {
			t.Fatal(kind)
		}
	}
	select {
	case <-pub.done:
		t.Fatal("publisher disconnected")
	default:
	}
	f.s.mu.Lock()
	overflows := f.s.dropped["publisher_control_overflow"]
	f.s.mu.Unlock()
	if overflows != 0 || len(pub.control) != 4 || len(pub.peerChanges) != 1 {
		t.Fatalf("viewer flood consumed control capacity: overflows=%d queued=%d", overflows, len(pub.control))
	}
	// A resumed, slow reader can consume lifecycle messages without draining relay first.
	msg := (<-pub.control).(map[string]any)
	if msg["type"] != "pong" {
		t.Fatal(msg)
	}
}

func TestReviewIdleCacheAndSQLPagination(t *testing.T) {
	f := setup(t)
	for i := 0; i < 30; i++ {
		v, _, _, err := f.s.Store.Create()
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err = f.s.Store.Delete(v.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, state := range []string{"live", "ended", "all"} {
		status, out, _ := f.request("GET", "/api/v1/streams?limit=3&state="+state, f.key, "")
		if status != 200 {
			t.Fatal(status, out)
		}
		var vv []store.Stream
		_ = json.Unmarshal(out["streams"], &vv)
		if len(vv) != 3 || string(out["next_cursor"]) == "null" {
			t.Fatal(out)
		}
		for _, v := range vv {
			if state == "live" && v.EndedAt != nil || state == "ended" && v.EndedAt == nil {
				t.Fatal(state, v)
			}
		}
	}
	f.s.mu.Lock()
	n := len(f.s.live)
	f.s.mu.Unlock()
	if n != 0 {
		t.Fatalf("historical listing populated %d live entries", n)
	}
	status, _, _ := f.request("POST", "/api/v1/streams/"+store.ID()+"/commands", f.key, `{"op":"pause"}`)
	if status != 404 {
		t.Fatal(status)
	}
	f.s.mu.Lock()
	n = len(f.s.live)
	cached := len(f.s.commands)
	f.s.mu.Unlock()
	if n != 0 || cached != 0 {
		t.Fatalf("nonexistent/idless command retained state: %d %d", n, cached)
	}
	id, _, _ := f.create(`{}`)
	f.request("GET", "/api/v1/streams/"+id, f.key, "")
	f.s.mu.Lock()
	f.s.live[id].used = time.Now().Add(-6 * time.Minute)
	f.s.mu.Unlock()
	f.s.maintain()
	f.s.mu.Lock()
	n = len(f.s.live)
	f.s.mu.Unlock()
	if n != 0 {
		t.Fatal("idle entry not evicted")
	}
	id, _, _ = f.create(`{}`)
	f.request("DELETE", "/api/v1/streams/"+id, f.key, "")
	f.s.maintain()
	f.s.mu.Lock()
	n = len(f.s.live)
	f.s.mu.Unlock()
	if n != 0 {
		t.Fatal("ended entry not evicted")
	}
}

func TestReviewSQLiteWaitDoesNotHoldGlobalLock(t *testing.T) {
	f := setup(t)
	id, _, _ := f.create(`{}`)
	l := f.s.runtime(id)
	ch := make(chan event, 32)
	l.mu.Lock()
	l.subscribers[ch] = true
	l.mu.Unlock()
	defer func() { l.mu.Lock(); delete(l.subscribers, ch); l.mu.Unlock(); f.s.release(l) }()
	conn, err := f.s.Store.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() { f.s.maintain(); close(finished) }()
	// Wait until maintenance has obtained the per-stream lock and is blocked on SQLite.
	deadline := time.Now().Add(time.Second)
	for {
		if !l.mu.TryLock() {
			break
		}
		l.mu.Unlock()
		if time.Now().After(deadline) {
			conn.Close()
			t.Fatal("maintenance did not start")
		}
		time.Sleep(time.Millisecond)
	}
	global := make(chan bool, 1)
	go func() { global <- f.s.permit("unrelated", 1, 1) }()
	select {
	case ok := <-global:
		if !ok {
			t.Fatal("unrelated limit failed")
		}
	case <-time.After(time.Second):
		conn.Close()
		t.Fatal("SQLite held global lock")
	}
	conn.Close()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance stuck")
	}
}

func TestReviewCallerBudgets(t *testing.T) {
	f := setup(t)
	k, err := f.s.Store.AuthKey(f.key)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a caller's cache at its documented cap; other callers retain their own budget.
	f.s.mu.Lock()
	for i := 0; i < 128; i++ {
		f.s.commands[fmt.Sprint(i)] = &commandCall{caller: k.ID, at: time.Now(), response: failure("sender_offline")}
	}
	f.s.mu.Unlock()
	path := "/api/v1/streams/" + store.ID() + "/commands"
	status, _, _ := f.request("POST", path, f.key, `{"id":"new","op":"pause"}`)
	if status != 429 {
		t.Fatal(status)
	}
	_, other, err := f.s.Store.NewKey("other", "streams", nil)
	if err != nil {
		t.Fatal(err)
	}
	status, _, _ = f.request("POST", path, other, `{"id":"new","op":"pause"}`)
	if status != 404 {
		t.Fatal(status)
	}
	status, _, _ = f.request("POST", path, f.key, `{"op":"pause"}`)
	if status != 404 {
		t.Fatalf("idless command consumed cache: %d", status)
	}

	f.s.mu.Lock()
	for i := 0; i < 8; i++ {
		f.s.inflight[fmt.Sprint(i)] = &commandCall{caller: k.ID, running: true}
	}
	f.s.mu.Unlock()
	status, _, _ = f.request("POST", path, f.key, `{"op":"pause"}`)
	if status != 429 {
		t.Fatalf("per-caller in-flight cap: %d", status)
	}
	status, _, _ = f.request("POST", path, other, `{"op":"pause"}`)
	if status != 404 {
		t.Fatalf("other caller in-flight budget: %d", status)
	}
	f.s.mu.Lock()
	clear(f.s.inflight)
	f.s.mu.Unlock()
	f.s.mu.Lock()
	f.s.limits["http:"+k.ID] = &bucket{at: time.Now()}
	f.s.mu.Unlock()
	status, _, headers := f.request("GET", "/api/v1/settings", f.key, "")
	if status != 429 || headers.Get("Retry-After") == "" {
		t.Fatal(status, headers)
	}
	status, _, _ = f.request("GET", "/api/v1/settings", other, "")
	if status != 200 {
		t.Fatal(status)
	}
}

func TestReviewSSECallerCapAndRevocation(t *testing.T) {
	for _, cookie := range []bool{false, true} {
		t.Run(fmt.Sprint(cookie), func(t *testing.T) {
			f := setup(t)
			k, key, err := f.s.Store.NewKey("reader", "read", nil)
			if err != nil {
				t.Fatal(err)
			}
			token := ""
			if cookie {
				token, _, err = f.s.Store.Login(k)
				if err != nil {
					t.Fatal(err)
				}
			}
			bodies := []io.ReadCloser{}
			defer func() {
				for _, b := range bodies {
					b.Close()
				}
			}()
			for i := 0; i < 9; i++ {
				v, _, _, err := f.s.Store.Create()
				if err != nil {
					t.Fatal(err)
				}
				r, _ := http.NewRequest("GET", f.h.URL+"/api/v1/streams/"+v.ID+"/events", nil)
				if cookie {
					r.AddCookie(&http.Cookie{Name: "ps_session", Value: token})
				} else {
					r.Header.Set("Authorization", "Bearer "+key)
				}
				resp, err := http.DefaultClient.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				if i == 8 {
					resp.Body.Close()
					if resp.StatusCode != 429 {
						t.Fatal(resp.StatusCode)
					}
					break
				}
				if resp.StatusCode != 200 {
					t.Fatal(resp.StatusCode)
				}
				bodies = append(bodies, resp.Body)
			}
			if cookie {
				err = f.s.Store.Logout(token)
			} else {
				err = f.s.Store.DeleteKey(k.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { io.Copy(io.Discard, bodies[0]); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("revoked SSE stayed open")
			}
		})
	}
}

func TestReviewSenderEndReasonAndWireOmissions(t *testing.T) {
	for _, reason := range []string{"stopped", "window_closed", "deleted", "state_lost", ""} {
		t.Run(reason, func(t *testing.T) {
			f := setup(t)
			id, pub, _ := f.create(`{}`)
			p, _ := f.join(id, "publish", pub, "10.0.0.1")
			readType(t, p, "e2ee.peers")
			st := validState(id, publisherInstance(f.s, id))
			st.Phase = "ended"
			st.EndReason = reason
			sendWS(t, p, map[string]any{"type": "sender.state", "state": st})
			readType(t, p, "stream.ended")
			v := mustStream(t, f.s, id)
			if v.EndReason == nil || *v.EndReason != senderEndReason(reason) {
				t.Fatal(v)
			}
		})
	}
	b, _ := json.Marshal(message{Type: "candidate"})
	for _, field := range []string{"resumed", "sender_online", "sender_instance"} {
		if strings.Contains(string(b), field) {
			t.Fatal(string(b))
		}
	}
}

func TestReviewRecentDisconnectedBan(t *testing.T) {
	f := setup(t)
	id, _, cap := f.create(`{}`)
	v, j := f.join(id, "view", cap, "2001:db8::1")
	var cid string
	_ = json.Unmarshal(j["connection_id"], &cid)
	v.CloseNow()
	l := f.s.runtime(id)
	defer f.s.release(l)
	deadline := time.Now().Add(time.Second)
	for {
		l.mu.Lock()
		c := l.conns[cid]
		left := c.LeftAt != nil
		l.mu.Unlock()
		if left {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnect not observed")
		}
		time.Sleep(time.Millisecond)
	}
	status, out, _ := f.request("POST", "/api/v1/streams/"+id+"/commands", f.key, `{"op":"ban","args":{"connection_id":"`+cid+`"}}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	if !f.s.Store.Banned(id, "2001:db8::abcd") || f.s.Store.Banned(id, "2001:db8:0:1::1") {
		t.Fatal("ban did not cover exactly /64")
	}
	_, bad := f.join(id, "view", cap, "2001:db8::99")
	if string(bad["code"]) != `"unauthorized"` {
		t.Fatal(bad)
	}
	l.mu.Lock()
	recent := l.recentIPs[cid]
	recent.until = timeNowMS() - 1
	l.recentIPs[cid] = recent
	l.mu.Unlock()
	status, _, _ = f.request("POST", "/api/v1/streams/"+id+"/commands", f.key, `{"op":"ban","args":{"connection_id":"`+cid+`"}}`)
	if status != 404 {
		t.Fatal("expired memory accepted", status)
	}
	f.request("DELETE", "/api/v1/streams/"+id, f.key, "")
	if f.s.Store.Banned(id, "2001:db8::1") {
		t.Fatal("ended stream retained bans")
	}
}

func TestReviewCanonicalBaseURL(t *testing.T) {
	for _, base := range []string{"https://EXAMPLE.COM/", "https://[2001:DB8::A]:8443/"} {
		t.Run(base, func(t *testing.T) {
			s, err := New(Config{Data: t.TempDir(), Listen: free(t), ICEUDP: free(t), ICETCP: free(t), TURN: free(t), BaseURL: base})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			want := strings.ToLower(strings.TrimSuffix(base, "/"))
			if s.cfg.BaseURL != want {
				t.Fatal(s.cfg.BaseURL)
			}
			_, token, err := s.Store.NewKey("test", "streams", nil)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", want+"/api/v1/streams", strings.NewReader(`{}`))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Origin", want)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			var out map[string]json.RawMessage
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			var link string
			_ = json.Unmarshal(out["view_link_base"], &link)
			if w.Code != 201 || !strings.HasPrefix(link, want+"/v#c=") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestReviewSenderStateRateLimit(t *testing.T) {
	f := setup(t)
	id, token, _ := f.create(`{}`)
	p, _ := f.join(id, "publish", token, "192.0.2.1")
	readType(t, p, "e2ee.peers")
	l := f.s.runtime(id)
	defer f.s.release(l)
	st := validState(id, publisherInstance(f.s, id))
	sendWS(t, p, map[string]any{"type": "sender.state", "state": st})
	sendWS(t, p, map[string]string{"type": "ping"})
	readType(t, p, "pong")
	f.s.mu.Lock()
	f.s.limits["state:"+id] = &bucket{at: time.Now()}
	f.s.mu.Unlock()
	st.Seq++
	st.Phase = "paused"
	sendWS(t, p, map[string]any{"type": "sender.state", "state": st})
	sendWS(t, p, map[string]string{"type": "ping"})
	readType(t, p, "pong")
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil || l.state.Seq != 1 || l.state.Phase != "live" {
		t.Fatal("documented state limit not enforced")
	}
}

func TestReviewSlowPublisherWebSocketFlood(t *testing.T) {
	f := setup(t)
	// Small kernel buffers expose a stalled socket writer without a large load.
	h := httptest.NewUnstartedServer(f.s.Handler())
	h.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetWriteBuffer(1024)
		}
		return ctx
	}
	h.Start()
	defer h.Close()
	f.h = h
	id, pub, cap := f.create(`{}`)
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.SetReadBuffer(1024)
			}
		}
		return conn, err
	}}
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	p, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.URL, "http")+"/signal", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.CloseNow()
	if err = wsjson.Write(ctx, p, message{Type: "join", Protocol: 1, ID: id, Role: "publish", Credential: pub, Instance: store.ID(), SenderKind: "browser"}); err != nil {
		t.Fatal(err)
	}
	readType(t, p, "joined")
	readType(t, p, "e2ee.peers")
	viewers := []*websocket.Conn{}
	for i := 0; i < 16; i++ {
		v, _ := f.join(id, "view", cap, fmt.Sprintf("192.0.2.%d", i+1))
		viewers = append(viewers, v)
	}
	finished := make(chan struct{}, len(viewers))
	for _, v := range viewers {
		go func() {
			defer func() { finished <- struct{}{} }()
			for i := 0; i < 32; i++ {
				if wsjson.Write(ctx, v, message{Type: "e2ee", B: strings.Repeat("AQEB", 1024)}) != nil {
					break
				}
			}
		}()
	}
	// No publisher reads occur while the viewer flood fills the socket and relay queue.
	for range viewers {
		select {
		case <-finished:
		case <-ctx.Done():
			t.Fatal("flood did not finish")
		}
	}
	sendWS(t, p, message{Type: "ping"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		var msg message
		if err = wsjson.Read(ctx, p, &msg); err != nil {
			t.Fatal("publisher disconnected by flood", err)
		}
		if msg.Type == "pong" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control message starved by relay")
		}
	}
	f.s.mu.Lock()
	drops := f.s.dropped["relay_queue"]
	overflows := f.s.dropped["publisher_control_overflow"]
	f.s.mu.Unlock()
	if overflows != 0 {
		t.Fatalf("viewer flood caused %d control overflows", overflows)
	}
	if drops == 0 {
		t.Fatal("slow-reader backpressure not exercised")
	}
}

func TestReviewMaintenanceSkipsIdleAndUnchangedWrites(t *testing.T) {
	f := setup(t)
	id, _, _ := f.create(`{}`)
	if f.s.cfg.AuditRetention != 30*24*time.Hour {
		t.Fatal("unexpected audit default")
	}
	l := f.s.runtime(id)
	defer f.s.release(l)
	// An idle cache entry must not even wait for the sole SQLite connection.
	conn, err := f.s.Store.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() { f.s.maintain(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		conn.Close()
		t.Fatal("idle maintenance touched SQLite")
	}
	conn.Close()
	ch := make(chan event, 32)
	l.mu.Lock()
	l.subscribers[ch] = true
	l.mu.Unlock()
	defer func() { l.mu.Lock(); delete(l.subscribers, ch); l.mu.Unlock() }()
	f.s.maintain()
	var before, after int
	if err = f.s.Store.DB.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	f.s.maintain()
	if err = f.s.Store.DB.QueryRow("SELECT total_changes()").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("unchanged join rewritten: %d -> %d", before, after)
	}
}
