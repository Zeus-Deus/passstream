package server

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/store"
)

// Annotation envelopes (PROTOCOL §20, types 0x09/0x0A) have their own relay
// budgets and at most a quarter of any queue, so drawing can never displace
// admission, rekey or ACK traffic (review D2 #1, from the reviewer's probe).

func relayFixture(t *testing.T, viewers int) (*fixture, string, *liveStream, *signalPeer, []*signalPeer) {
	t.Helper()
	f := setup(t)
	id, _, _ := f.create(`{}`)
	l := f.s.runtime(id)
	t.Cleanup(func() { f.s.release(l) })
	pub := &signalPeer{id: "c_" + store.ID(), role: "publish", stream: id, ip: "192.0.2.1", queue: make(chan any, 64), control: make(chan any, 64), done: make(chan struct{})}
	vs := make([]*signalPeer, viewers)
	l.mu.Lock()
	l.publisher = pub
	for i := range vs {
		vs[i] = &signalPeer{id: "c_" + store.ID(), role: "view", stream: id, ip: fmt.Sprintf("198.51.100.%d", i+1), queue: make(chan any, 64), done: make(chan struct{})}
		l.sockets[vs[i].id] = vs[i]
	}
	l.mu.Unlock()
	t.Cleanup(func() { l.mu.Lock(); l.publisher = nil; clear(l.sockets); l.mu.Unlock() })
	return f, id, l, pub, vs
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

var (
	annotationDown = b64(append([]byte{1, 0x0a, 4, 0, 0, 0, 1, 8, 0, 0, 0, 0, 0, 0, 0, 1, 22}, make([]byte, 22)...))
	annotationUp   = b64(append([]byte{1, 0x09, 4, 0, 0, 0, 1, 8, 0, 0, 0, 0, 0, 0, 0, 1, 17}, make([]byte, 17)...))
	rekeyAck       = b64([]byte{1, 7, 8, 0, 0, 0, 0, 0, 0, 0, 1, 16, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	rekey          = b64(append([]byte{1, 6, 8, 0, 0, 0, 0, 0, 0, 0, 1, 54}, make([]byte, 54)...))
)

// The reviewer's case: the control budget near depletion, a burst of
// annotation fan-out, then an unrelated viewer's ACK. It must get through.
func TestAnnotationFanoutNeverDropsAnotherViewersAck(t *testing.T) {
	f, id, _, pub, vs := relayFixture(t, 1)
	viewer := vs[0]
	f.s.mu.Lock()
	f.s.limits["relay-stream:"+id] = &bucket{at: time.Now(), tokens: 128}
	f.s.mu.Unlock()
	for i := 0; i < 128; i++ {
		if e := f.s.handleSignal(pub, message{Type: "e2ee", To: viewer.id, B: annotationDown}); e != nil {
			t.Fatal(e)
		}
		select {
		case <-viewer.queue:
		default:
		}
	}
	if e := f.s.handleSignal(viewer, message{Type: "e2ee", B: rekeyAck}); e != nil {
		t.Fatal(e)
	}
	f.s.mu.Lock()
	dropped := f.s.dropped["relay_rate"]
	f.s.mu.Unlock()
	if len(pub.queue) != 1 || dropped != 0 {
		t.Fatalf("ACK must reach the publisher: queue=%d relay_rate drops=%d", len(pub.queue), dropped)
	}
}

// 100 viewers for 3 s: the publisher fans annotations out far beyond the
// host's 160/s and five viewers draw far beyond 8/s, while a full rekey round
// (100 rekeys + 100 ACKs) runs every second, within the control budget. Every
// rekey reaches its viewer, every ACK reaches the publisher, nobody is closed.
func TestAnnotationFloodAt100ViewersKeepsEveryRekeyAndAck(t *testing.T) {
	f, _, _, pub, vs := relayFixture(t, 100)
	drain := func() (rekeys, acks int) {
		for _, v := range vs {
			for len(v.queue) > 0 {
				m := (<-v.queue).(map[string]string)
				if m["b"] == rekey {
					rekeys++
				}
			}
		}
		for len(pub.queue) > 0 {
			m := (<-pub.queue).(map[string]string)
			if m["b"] == rekeyAck {
				acks++
			}
		}
		return
	}
	start := time.Now()
	var rekeysSent, rekeysGot, acksSent, acksGot, annSent int
	for round := 0; time.Since(start) < 3*time.Second; round++ {
		// Annotation fan-out: one per viewer per round, far beyond the host's 160/s.
		for _, v := range vs {
			_ = f.s.handleSignal(pub, message{Type: "e2ee", To: v.id, B: annotationDown})
			annSent++
		}
		// Five viewers draw (each far beyond 8/s).
		for _, v := range vs[:5] {
			_ = f.s.handleSignal(v, message{Type: "e2ee", B: annotationUp})
		}
		// A rekey to everyone and their ACKs once a second (200 control envelopes/s, within 512/s).
		if round%100 == 0 {
			for _, v := range vs {
				_ = f.s.handleSignal(pub, message{Type: "e2ee", To: v.id, B: rekey})
				rekeysSent++
			}
			r, a := drain()
			rekeysGot += r
			acksGot += a
			for i, v := range vs {
				_ = f.s.handleSignal(v, message{Type: "e2ee", B: rekeyAck})
				acksSent++
				if i%8 == 7 { // the publisher reads its queue as ACKs arrive
					_, a := drain()
					acksGot += a
				}
			}
		}
		r, a := drain()
		rekeysGot += r
		acksGot += a
		time.Sleep(10 * time.Millisecond)
	}
	r, a := drain()
	rekeysGot += r
	acksGot += a
	f.s.mu.Lock()
	drops := map[string]int{}
	for k, v := range f.s.dropped {
		drops[k] = int(v)
	}
	f.s.mu.Unlock()
	t.Logf("100 viewers, 3 s: annotation envelopes offered=%d, rekeys %d/%d, acks %d/%d, drops=%v", annSent, rekeysGot, rekeysSent, acksGot, acksSent, drops)
	if rekeysGot != rekeysSent || acksGot != acksSent {
		t.Fatalf("control traffic lost: rekeys %d/%d acks %d/%d drops %v", rekeysGot, rekeysSent, acksGot, acksSent, drops)
	}
	if drops["relay_rate"] != 0 || drops["relay_queue"] != 0 {
		t.Fatalf("control drops: %v", drops)
	}
	if drops["relay_annotation_rate"] == 0 {
		t.Fatalf("the annotation budget never engaged: %v", drops)
	}
	for _, v := range vs {
		select {
		case <-v.done:
			t.Fatal("a viewer was closed by annotation traffic")
		default:
		}
	}
}

// Annotations never take more than a quarter of a viewer's queue and never
// close it; a rekey still fits behind them.
func TestAnnotationsUseAtMostAQuarterOfAQueue(t *testing.T) {
	f, _, _, pub, vs := relayFixture(t, 1)
	v := vs[0]
	for i := 0; i < 200; i++ {
		_ = f.s.handleSignal(pub, message{Type: "e2ee", To: v.id, B: annotationDown})
	}
	if n := len(v.queue); n > cap(v.queue)/4 {
		t.Fatalf("annotations filled %d of %d", n, cap(v.queue))
	}
	if e := f.s.handleSignal(pub, message{Type: "e2ee", To: v.id, B: rekey}); e != nil {
		t.Fatal(e)
	}
	select {
	case <-v.done:
		t.Fatal("viewer closed")
	default:
	}
	last := ""
	for len(v.queue) > 0 {
		last = (<-v.queue).(map[string]string)["b"]
	}
	if last != rekey {
		t.Fatal("the rekey was not queued behind the annotations")
	}
}
