package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/store"
)

func TestReviewConcurrentRetryAfterTimeout(t *testing.T) {
	f := setup(t)
	id, pub, _ := f.create(`{}`)
	path := "/api/v1/streams/" + id + "/commands"
	p, _ := f.join(id, "publish", pub, "10.0.0.1")
	readType(t, p, "e2ee.peers")
	body := `{"id":"race","op":"pause"}`

	results := make(chan int, 48)
	for i := 0; i < 24; i++ {
		go func() { status, _, _ := f.request("POST", path, f.key, body); results <- status }()
	}
	first := readType(t, p, "control")
	select {
	case status := <-results:
		if status != 504 {
			t.Fatalf("first attempt: %d", status)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("initial timeout never completed")
	}
	// Every caller that joined the first attempt receives its timeout.
	for i := 1; i < 24; i++ {
		select {
		case status := <-results:
			if status != 504 {
				t.Fatalf("joined attempt: %d", status)
			}
		case <-time.After(time.Second):
			t.Fatal("waiter started a serialized retry")
		}
	}
	// Only a new request can start the next attempt, keeping the forwarding id.
	for i := 0; i < 24; i++ {
		go func() { status, _, _ := f.request("POST", path, f.key, body); results <- status }()
	}
	retry := readType(t, p, "control")
	if string(first["id"]) != string(retry["id"]) {
		t.Fatal("forward id changed")
	}
	time.Sleep(100 * time.Millisecond) // let all retries wait on the same in-flight call
	sendWS(t, p, map[string]any{"type": "control.result", "id": json.RawMessage(retry["id"]), "ok": true, "result": map[string]any{}})
	deadline := time.After(8 * time.Second)
	for i := 0; i < 24; i++ {
		select {
		case status := <-results:
			if status != 200 {
				t.Fatalf("retry: %d", status)
			}
		case <-deadline:
			t.Fatalf("only %d retries returned", i)
		}
	}
	sendWS(t, p, map[string]string{"type": "ping"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var next map[string]json.RawMessage
	if err := wsjson.Read(ctx, p, &next); err != nil || string(next["type"]) != `"pong"` {
		t.Fatal("extra command restart", next, err)
	}

}

func TestReviewKnockLimitFreezesState(t *testing.T) {
	f := setup(t)
	id, pub, view := f.create(`{}`)
	p, _ := f.join(id, "publish", pub, "10.0.0.1")
	readType(t, p, "e2ee.peers")
	peers := []string{}
	for i := 0; i < 6; i++ {
		_, j := f.join("", "view", view, "10.0.0.9")
		var c string
		_ = json.Unmarshal(j["connection_id"], &c)
		peers = append(peers, c)
	}
	l := f.s.runtime(id)
	defer f.s.release(l)
	alerts := make(chan event, 256)
	l.mu.Lock()
	l.subscribers[alerts] = true
	l.mu.Unlock()
	defer func() { l.mu.Lock(); delete(l.subscribers, alerts); l.mu.Unlock() }()
	inst := publisherInstance(f.s, id)
	st := validState(id, inst)
	now := time.Now().UnixMilli()
	for _, c := range peers {
		st.Knocks = append(st.Knocks, hostview.Knock{KnockID: store.ID(), PeerID: c, RowCode: "ACD", At: now, ExpiresAt: now + 60000})
	}
	for seq := uint64(1); seq <= 6; seq++ {
		st.Seq = seq
		sendWS(t, p, map[string]any{"type": "sender.state", "state": st})
		time.Sleep(1100 * time.Millisecond)
	}
	sendWS(t, p, map[string]string{"type": "ping"})
	readType(t, p, "pong")
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for len(alerts) > 0 {
		e := <-alerts
		if a, ok := e.Data.(map[string]any); ok && e.Kind == "alert" && a["kind"] == "knock" {
			count++
		}
	}
	if count != 5 {
		t.Fatalf("knock alerts: %d", count)
	}
	if l.state == nil || l.state.Seq != 6 || len(l.state.Knocks) != 6 {
		t.Fatalf("no SenderState accepted after 6.6 s of valid reports (6 knocks from one IP)")
	}
}
