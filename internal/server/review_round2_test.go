package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/store"
)

func TestCommandWaitersShareDeadSenderTimeout(t *testing.T) {
	f := setup(t)
	id, credential, _ := f.create(`{}`)
	p, _ := f.join(id, "publish", credential, "10.0.0.1")
	readType(t, p, "e2ee.peers")
	path := "/api/v1/streams/" + id + "/commands"
	type response struct {
		status  int
		elapsed time.Duration
	}
	results := make(chan response, 4)
	start := time.Now()
	request := func() {
		status, body, _ := f.request("POST", path, f.key, `{"id":"dead-sender","op":"pause"}`)
		if errCode(body) != "sender_timeout" {
			t.Errorf("unexpected response: %s", body)
		}
		results <- response{status, time.Since(start)}
	}
	go request()
	readType(t, p, "control")
	for i := 0; i < 3; i++ {
		go request()
	}
	deadline := time.After(6 * time.Second)
	for i := 0; i < 4; i++ {
		select {
		case r := <-results:
			if r.status != 504 || r.elapsed > 6*time.Second {
				t.Fatalf("waiter: %+v", r)
			}
			t.Logf("waiter %d: 504 after %s", i+1, r.elapsed)
		case <-deadline:
			t.Fatal("joined waiter restarted the dead-sender timeout")
		}
	}
	// A ping is a read barrier: no waiter may have forwarded another command.
	sendWS(t, p, message{Type: "ping"})
	next := readWS(t, p)
	if string(next["type"]) != `"pong"` {
		t.Fatalf("unexpected retry: %s", next)
	}
}

func TestPublisherOverflowReplaysBanAndTighteningCommands(t *testing.T) {
	f := setup(t)
	id, credential, capability := f.create(`{}`)
	_, joined := f.join("", "view", capability, "192.0.2.1")
	var target string
	_ = json.Unmarshal(joined["connection_id"], &target)
	_, joined = f.join("", "view", capability, "192.0.2.2")
	var remaining string
	_ = json.Unmarshal(joined["connection_id"], &remaining)

	// Stop the publisher at the application queue, independent of TCP buffer sizes.
	pub := &signalPeer{role: "publish", stream: id, queue: make(chan any, 64), control: make(chan any, 64), done: make(chan struct{}), drop: f.s.drop}
	l := f.s.runtime(id)
	defer f.s.release(l)
	l.mu.Lock()
	l.publisher = pub
	state := validState(id, store.ID())
	wantAdmissions := map[string]bool{}
	for i := 0; i < 500; i++ {
		aid := store.ID()
		wantAdmissions[aid] = true
		state.Admissions = append(state.Admissions, hostview.Admission{AdmissionID: aid, ViewerID: store.ID(), PeerID: target, RowCode: "ACD", AdmittedAt: timeNowMS()})
	}
	l.state = &state
	l.mu.Unlock()
	path := "/api/v1/streams/" + id + "/commands"
	results := make(chan int, 2)
	for _, body := range []string{`{"id":"lock","op":"lock","args":{"locked":true}}`, fmt.Sprintf(`{"id":"kick","op":"kick","args":{"admission_id":%q,"lock":true}}`, state.Admissions[0].AdmissionID)} {
		go func() { status, _, _ := f.request("POST", path, f.key, body); results <- status }()
	}
	deadline := time.Now().Add(time.Second)
	for len(pub.control) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("tightening commands not queued")
		}
		time.Sleep(time.Millisecond)
	}
	status, out, _ := f.request("POST", path, f.key, fmt.Sprintf(`{"op":"ban","args":{"connection_id":%q}}`, target))
	var result struct {
		AdmissionIDs []string `json:"admission_ids"`
	}
	_ = json.Unmarshal(out["result"], &result)
	if status != 200 || len(result.AdmissionIDs) != 500 {
		t.Fatalf("ban: %d %s", status, out)
	}
	select {
	case <-pub.done:
		if pub.reason != "resync" {
			t.Fatal(pub.reason)
		}
	default:
		t.Fatal("overflow did not request resync")
	}
	f.s.mu.Lock()
	overflows, drops := f.s.dropped["publisher_control_overflow"], f.s.dropped["publisher_control_drop"]
	f.s.mu.Unlock()
	if overflows != 1 || drops < 438 {
		t.Fatalf("missing overflow metrics: overflow=%d drops=%d", overflows, drops)
	}

	// Neither timeout nor idle-cache eviction may discard unacknowledged restrictions.
	l.mu.Lock()
	if len(l.pending) != 502 {
		t.Fatalf("pending: %d", len(l.pending))
	}
	l.mu.Unlock()
	want := map[string]string{}
	for round := 0; round < 2; round++ {
		p, _ := f.join(id, "publish", credential, "10.0.0.1")
		peers := readType(t, p, "e2ee.peers")
		var ids []string
		_ = json.Unmarshal(peers["peers"], &ids)
		if len(ids) != 1 || ids[0] != remaining {
			t.Fatalf("stale peer list: %s", peers)
		}
		seen, admissions := map[string]bool{}, map[string]bool{}
		for i := 0; i < 502; i++ {
			msg := readWS(t, p)
			var c controlMessage
			b, _ := json.Marshal(msg)
			if err := json.Unmarshal(b, &c); err != nil {
				t.Fatal(err)
			}
			if c.Type != "control" || seen[c.ID] {
				t.Fatalf("duplicate/unexpected replay: %s", msg)
			}
			seen[c.ID] = true
			if round == 0 {
				want[c.ID] = string(b)
			} else if want[c.ID] != string(b) {
				t.Fatal("forwarding id or command changed on reconnect")
			}
			if c.Op == "kick" {
				var aid string
				_ = json.Unmarshal(c.Args["admission_id"], &aid)
				if !wantAdmissions[aid] {
					t.Fatal("unexpected kick", aid)
				}
				admissions[aid] = true
			}
			if round == 1 {
				sendWS(t, p, map[string]any{"type": "control.result", "id": c.ID, "ok": true, "result": map[string]any{}})
			}
		}
		if len(admissions) != 500 {
			t.Fatalf("only %d ban kicks delivered", len(admissions))
		}
		if round == 1 {
			sendWS(t, p, message{Type: "ping"})
			readType(t, p, "pong")
			for i := 0; i < 2; i++ {
				select {
				case status := <-results:
					if status != 200 {
						t.Fatalf("in-flight tightening result: %d", status)
					}
				case <-time.After(time.Second):
					t.Fatal("replayed command did not complete HTTP attempt")
				}
			}
		}
		p.CloseNow()
	}
	// Acknowledgements remove replay work; the next reconnect sends only fresh peers.
	p, _ := f.join(id, "publish", credential, "10.0.0.1")
	readType(t, p, "e2ee.peers")
	sendWS(t, p, message{Type: "ping"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var next message
	if err := wsjson.Read(ctx, p, &next); err != nil || next.Type != "pong" {
		t.Fatalf("acknowledged command replayed: %+v %v", next, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) != 0 {
		t.Fatalf("acknowledged work retained: %d", len(l.pending))
	}
}

func TestPublisherControlOverflowCloseReason(t *testing.T) {
	counts := make(chan map[string]int, 1)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		dropped := map[string]int{}
		p := &signalPeer{role: "publish", conn: c, control: make(chan any, 64), done: make(chan struct{}), drop: func(reason string) { dropped[reason]++ }}
		for i := 0; i < 66; i++ {
			p.enqueue(message{Type: "candidate"})
		}
		counts <- dropped
		_, _, _ = c.Read(r.Context())
	}))
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_, _, err = c.Read(ctx)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Code != websocket.StatusTryAgainLater || closed.Reason != "resync" {
		t.Fatalf("missing reconnect reason: %v", err)
	}
	got := <-counts
	if got["publisher_control_overflow"] != 1 || got["publisher_control_drop"] != 66 {
		t.Fatal(got)
	}
}

func TestPendingControlBoundAcknowledgementAndEviction(t *testing.T) {
	f := setup(t)
	id, _, _ := f.create(`{}`)
	l := f.s.runtime(id)
	p := &signalPeer{role: "publish", stream: id, control: make(chan any, 64), done: make(chan struct{})}
	target, ip := "c_"+store.ID(), "192.0.2.1"
	l.mu.Lock()
	l.publisher = p
	l.conns[target] = hostview.Connection{IP: &ip}
	l.state = &hostview.SenderState{Admissions: []hostview.Admission{{AdmissionID: store.ID(), PeerID: target}}}
	for i := 0; i < maxPendingControls; i++ {
		id := fmt.Sprint(i)
		l.pending[id] = controlMessage{ID: id, Op: "kick", queuedAt: time.Now()}
	}
	l.mu.Unlock()
	path := "/api/v1/streams/" + id + "/commands"
	for _, body := range []string{`{"op":"lock","args":{"locked":true}}`, fmt.Sprintf(`{"op":"ban","args":{"connection_id":%q}}`, target)} {
		status, out, _ := f.request("POST", path, f.key, body)
		if status != 429 || errCode(out) != "rate_limited" {
			t.Fatalf("pending bound: %d %s", status, out)
		}
	}
	if f.s.Store.Banned(id, ip) || len(p.control) != 0 {
		t.Fatal("over-capacity restriction partially applied")
	}
	ok := false
	for _, code := range []string{"storage", "internal", "unknown_admission"} {
		if err := f.s.handleSignal(p, message{Type: "control.result", ID: "0", OK: &ok, Error: &apiError{Code: code}}); err != nil {
			t.Fatal(err)
		}
		l.mu.Lock()
		_, pending := l.pending["0"]
		l.mu.Unlock()
		if pending != (code != "unknown_admission") {
			t.Fatal("incorrect acknowledgement handling", code)
		}
	}
	l.mu.Lock()
	l.publisher = nil
	l.mu.Unlock()
	f.s.release(l)
	f.s.mu.Lock()
	l.used = time.Now().Add(-10 * time.Minute)
	f.s.mu.Unlock()
	f.s.maintain()
	f.s.mu.Lock()
	retained := f.s.live[id] == l
	f.s.mu.Unlock()
	if !retained {
		t.Fatal("idle eviction discarded pending controls")
	}
	status, out, _ := f.request("DELETE", "/api/v1/streams/"+id, f.key, "")
	if status != 204 {
		t.Fatalf("stop: %d %s", status, out)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) != 0 {
		t.Fatal("ended stream retained pending controls")
	}
}
