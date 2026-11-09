package sfu

import (
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Regressions for C2 item 6a (docs/REPORT.md).

// viewer_flap diagnosis: behind the media barrier a missed base picture
// must not request a low key (it would be delivered to no one, spend the
// room's budget and satisfy the request); the broken chain is reported when
// the viewer is reachable again.
func TestBrokenChainBehindTheBarrierWaitsForReturn(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	h.r.keys.Due(h.now) // the join PLI
	h.send(ctrl.Low, desc{key: true}, 2, 1000)
	h.drain(v, 200*time.Millisecond)
	h.now = h.now.Add(ctrl.KeyInterval)
	v.peer.admitted.Store(false) // the barrier closes
	h.send(ctrl.Low, desc{tid: 0}, 1, 500)
	if due := h.r.keys.Due(h.now); len(due) != 0 {
		t.Fatalf("a key was requested for an unreachable viewer: %v", due)
	}
	v.peer.admitted.Store(true)
	h.send(ctrl.Low, desc{tid: 2}, 1, 500)
	if due := h.r.keys.Due(h.now); len(due) != 1 || due[0] != ctrl.Low {
		t.Fatalf("the broken chain was not reported on return: %v", due)
	}
}

// A closed PC's late RTCP or heartbeat must not re-enter the room's key
// policy after Keys.Leave (A19 late old-PC feedback).
func TestClosedViewerStaysOutOfTheKeyPolicy(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	h.r.keys.Due(h.now)
	h.send(ctrl.Low, desc{key: true}, 2, 1000)
	h.drain(v, 200*time.Millisecond)
	v.peer.Close()
	h.now = h.now.Add(ctrl.KeyInterval)
	v.stall(h.now)
	v.mu.Lock()
	rejoins := v.joined || v.ready(h.now)
	v.mu.Unlock()
	if rejoins {
		t.Fatal("a closed viewer would rejoin")
	}
	if due := h.r.keys.Due(h.now); len(due) != 0 {
		t.Fatalf("a closed viewer requested a key: %v", due)
	}
}

// A replacement PC handed off from the same admitted session gets its low
// key at once, through the session's exception credit, although the room's
// low budget is spent; the session identity is the first PC's, not the new
// Peer's ID.
func TestReplacementPCGetsItsLowKeyAtOnce(t *testing.T) {
	h := newHarness(t)
	a, _ := h.viewer()
	h.r.keys.Due(h.now)
	h.send(ctrl.Low, desc{key: true}, 2, 1000) // spends the low budget
	h.drain(a, 200*time.Millisecond)
	b := h.replacement(a)
	if b.id == a.id || b.ctl.Session != a.ctl.Session {
		t.Fatalf("ids %d/%d sessions %d/%d", a.id, b.id, a.ctl.Session, b.ctl.Session)
	}
	if due := h.r.keys.Due(h.now); len(due) != 1 || due[0] != ctrl.Low {
		t.Fatalf("no exception PLI for the replacement: %v", due)
	}
	h.send(ctrl.Low, desc{key: true}, 2, 1000)
	h.drain(b, 200*time.Millisecond)
	b.mu.Lock()
	b.step(h.now) // delivery facts reach ctrl when the writer steps
	b.mu.Unlock()
	d := b.diagnostics(h.now)["keyTimes"].(map[string]any)["low"].(map[string]any)
	if d["exception"] != true || d["requestedAt"].(int64) == 0 || d["emittedAt"].(int64) == 0 || d["assembledAt"].(int64) == 0 || d["deliveredAt"].(int64) == 0 {
		t.Fatalf("key times %v", d)
	}
	// A second replacement of the session within KeyInterval waits.
	c := h.replacement(b)
	if due := h.r.keys.Due(h.now); len(due) != 0 {
		t.Fatalf("a second exception within KeyInterval: %v", due)
	}
	_ = c
}

// replacement closes old's peer and builds the PC that replaces it in the
// same admitted session, as signaling does (Close, Peer, InheritPath), then
// joins it.
func (h *harness) replacement(old *viewer) *viewer {
	p := &Peer{Room: h.r, done: make(chan struct{})}
	p.admitted.Store(true)
	w := &recordWriter{}
	video := &boundTrack{binding: binding{writer: w, ssrc: 10, mediaPT: 98, twccID: 3}}
	audio := &boundTrack{binding: binding{writer: w, ssrc: 20, mediaPT: audioPayloadType, twccID: 3}}
	p.view = newViewer(p, h.r, video, audio)
	old.peer.Close()
	p.InheritPath(old.peer)
	h.r.mu.Lock()
	h.r.viewers[p] = true
	h.r.mu.Unlock()
	v := p.view
	v.mu.Lock()
	v.joined = true
	v.step(h.now)
	v.mu.Unlock()
	return v
}

// A8 outage: the low key an accepted recovery asked for can assemble before
// the next periodic step. The forwarder must already want low, or it skips
// that key and the next low key waits KeyInterval.
func TestRecoveryAcceptsTheLowKeyItRequested(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	h.r.keys.Due(h.now) // the join PLI
	h.send(ctrl.Low, desc{key: true}, 2, 1000)
	h.drain(v, 200*time.Millisecond)
	v.mu.Lock()
	v.ctl.Alloc = &keySwitchAllocation{desired: ctrl.Option{Rendition: ctrl.Native, TID: 2}}
	v.ctl.Probe = ctrl.NoProber{}
	v.want, v.wantTID = ctrl.Native, 2
	v.mu.Unlock()
	h.send(ctrl.Native, desc{key: true}, 3, 1000)
	h.drain(v, 300*time.Millisecond)
	if !v.forwarding(ctrl.Native) {
		t.Fatal("setup: not on native")
	}
	v.mu.Lock()
	v.step(h.now) // the real writer consumes completed-key facts before later decoder reports
	v.mu.Unlock()
	h.now = h.now.Add(ctrl.KeyEnvelope + time.Second) // past the key's stall suppression
	v.stall(h.now)
	h.send(ctrl.Low, desc{key: true}, 2, 1000) // assembled 0 ms later
	if pendingRenditions(v)[ctrl.Low] != 2 {
		t.Fatalf("the requested low key was skipped (want %v)", v.want)
	}
	h.drain(v, 200*time.Millisecond)
	if !v.forwarding(ctrl.Low) {
		t.Fatal("low did not commit at departure")
	}
}
