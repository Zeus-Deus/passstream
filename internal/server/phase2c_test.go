package server

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The viewer learns whether to show a PIN screen before its first
// handshake; the hint follows the sender's report once there is one.
func TestViewerJoinedCarriesPinHint(t *testing.T) {
	f := setup(t)
	id, pub, view := f.create(`{"pin":{"kind":"digits","length":6}}`)
	_, joined := f.join("", "view", view, "10.0.0.1")
	var hint map[string]any
	if err := json.Unmarshal(joined["pin_hint"], &hint); err != nil || hint["kind"] != "digits" || hint["length"] != float64(6) {
		t.Fatalf("declared hint: %s", joined["pin_hint"])
	}
	pc, pj := f.join(id, "publish", pub, "10.0.0.2")
	if _, ok := pj["pin_hint"]; ok {
		t.Fatal("publisher got a pin hint")
	}
	instance := ""
	f.s.mu.Lock()
	streams := []*liveStream{}
	for _, l := range f.s.live {
		streams = append(streams, l)
	}
	f.s.mu.Unlock()
	for _, l := range streams {
		l.mu.Lock()
		if l.publisher != nil {
			instance = l.publisher.instance
		}
		l.mu.Unlock()
	}
	// The sender's report wins over the declared policy.
	st := validState(id, instance)
	st.Access.Pin.Required = false
	st.Access.Pin.Kind = "none"
	st.Access.Pin.Length = nil
	sendWS(t, pc, map[string]any{"type": "sender.state", "state": st})
	time.Sleep(100 * time.Millisecond)
	_, joined = f.join("", "view", view, "10.0.0.3")
	if err := json.Unmarshal(joined["pin_hint"], &hint); err != nil || hint["kind"] != "none" {
		t.Fatalf("reported hint: %s", joined["pin_hint"])
	}
}

// host_url carries what the server-served sender needs in its fragment only.
func TestHostURLFragment(t *testing.T) {
	f := setup(t)
	status, v, _ := f.request("POST", "/api/v1/streams", f.key, `{"pin":{"kind":"none"},"pin_explicit":true,"knock":true}`)
	if status != 201 {
		t.Fatal(status)
	}
	var host, viewBase string
	_ = json.Unmarshal(v["host_url"], &host)
	_ = json.Unmarshal(v["view_link_base"], &viewBase)
	u, err := url.Parse(host)
	if err != nil || u.Path != "/host" || u.RawQuery != "" {
		t.Fatal(host)
	}
	q, _ := url.ParseQuery(u.Fragment)
	if q.Get("c") != strings.TrimPrefix(viewBase, "http://localhost/v#c=") || q.Get("pin") != "none" || q.Get("knock") != "1" || len(q.Get("t")) != 43 {
		t.Fatal(u.Fragment)
	}
}

// A media peer waiting for its first offer survives the reaper's grace; an
// abandoned one doesn't.
func TestReaperSparesNewPeersWithinGrace(t *testing.T) {
	f := setup(t)
	_, _, view := f.create(`{}`)
	_, _ = f.join("", "view", view, "10.0.0.9")
	count := func() int {
		f.s.mu.Lock()
		defer f.s.mu.Unlock()
		return len(f.s.peers)
	}
	if count() != 1 {
		t.Fatal("no media peer")
	}
	f.s.reapOnce()
	if count() != 1 {
		t.Fatal("a new peer was reaped within its grace")
	}
}
