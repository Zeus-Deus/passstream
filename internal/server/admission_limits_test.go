package server

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/store"
)

func TestPINGuessRelayAndKnockBudgets(t *testing.T) {
	f := setup(t)
	id, _, _ := f.create(`{}`)
	l := f.s.runtime(id)
	defer f.s.release(l)
	p := &signalPeer{id: "c_" + store.ID(), role: "publish", stream: id, queue: make(chan any, 64)}
	v := &signalPeer{id: "c_" + store.ID(), role: "view", stream: id, ip: "192.0.2.10"}
	l.mu.Lock()
	l.publisher = p
	l.sockets[v.id] = v
	l.mu.Unlock()
	opaque := message{Type: "e2ee", B: base64.RawURLEncoding.EncodeToString([]byte("opaque PAKE guess"))}
	// First use the peer burst; drain the publisher between messages so this
	// measures admission rate limiting rather than output backpressure.
	for i := 0; i < 40; i++ {
		if err := f.s.handleSignal(v, opaque); err != nil {
			t.Fatal(err)
		}
		select {
		case <-p.queue:
		default:
		}
	}
	f.s.mu.Lock()
	drops := f.s.dropped["relay_rate"]
	f.s.mu.Unlock()
	if drops != 8 {
		t.Fatalf("PIN relay burst: %d drops, want 8", drops)
	}
	// Stream knock budget: 51 peers with separate IPs cannot bypass burst 50.
	alerts := make(chan event, 100)
	l.mu.Lock()
	l.subscribers[alerts] = true
	st := validState(id, store.ID())
	for i := 0; i < 51; i++ {
		peer := &signalPeer{id: "c_" + store.ID(), role: "view", stream: id, ip: "192.0.2." + strconv.Itoa(i+1)}
		l.sockets[peer.id] = peer
		st.Knocks = []hostview.Knock{{KnockID: store.ID(), PeerID: peer.id, RowCode: "K7P"}}
		f.s.stateAlerts(l, nil, &st)
	}
	delete(l.subscribers, alerts)
	l.publisher = nil
	clear(l.sockets)
	l.mu.Unlock()
	if len(alerts) != 50 {
		t.Fatalf("knock stream burst: %d alerts, want 50", len(alerts))
	}
	// HTTP handshake exhaustion is explicit, even before a WebSocket join.
	f.s.mu.Lock()
	f.s.limits["handshake-ip:127.0.0.1"] = &bucket{at: time.Now(), tokens: 0}
	f.s.mu.Unlock()
	r, err := http.Get(f.h.URL + "/signal")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatal("missing handshake limit", r.StatusCode, r.Header)
	}
}
