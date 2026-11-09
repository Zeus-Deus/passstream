package sfu

import (
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Forced allocation/rate isolates the wire transaction. The unchanged Default
// regressions exercise eligibility; these tests do not claim receiver rendering.
func resetWire(t *testing.T) (*harness, *viewer, *recordWriter, *keySwitchAllocation) {
	t.Helper()
	h := newHarness(t)
	v, w := h.viewer()
	a := &keySwitchAllocation{desired: ctrl.Option{Rendition: ctrl.Low, TID: 2}}
	v.ctl.Alloc, v.ctl.Probe = a, ctrl.NoProber{}
	v.ctl.Est = &ctrl.FixedEstimator{Rate: 1_000_000}
	v.step(h.now)
	h.send(ctrl.Low, desc{key: true, flexible: true}, 2, 500)
	h.drain(v, 100*time.Millisecond)
	a.desired = ctrl.Option{Rendition: ctrl.Native, TID: 0}
	v.step(h.now)
	return h, v, w, a
}

func TestRequiredLowResetFencesOldWireAndRepair(t *testing.T) {
	for _, partialKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial-delta", true: "partial-key-applied-still-low"}[partialKey], func(t *testing.T) {
			h, v, w, a := resetWire(t)
			k := &expiryKeys{Keys: v.ctl.Keys}
			v.ctl.Keys = k
			if !partialKey {
				h.send(ctrl.Native, desc{key: true, flexible: true}, 2, 500)
				h.drain(v, 100*time.Millisecond)
			}
			before := len(w.written())
			h.send(ctrl.Native, desc{key: partialKey, flexible: true, pdiff: []uint8{1}}, 24, 1000)
			h.drain(v, time.Millisecond)
			started := w.written()[before:]
			if len(started) == 0 || len(started) >= 24 {
				t.Fatal("Native picture is not partial")
			}
			seq := started[0].SequenceNumber
			old := v.cache.get(seq).frame
			reservedEnd := old.base + uint16(old.packets) - 1
			if !old.reserved || v.seq != reservedEnd || partialKey && v.ctl.Applied().Rendition != ctrl.Low {
				t.Fatal("fixture lost sequence reservation or lagging Applied")
			}
			// Retain a real old repair and parity packet in the queue. RTX may
			// overtake the reset, but neither may leave after its first packet.
			v.resendVideo(h.now, []uint16{seq}, false)
			if v.cache.get(seq).sends != 1 {
				t.Fatal("old RTX control was not admitted")
			}
			v.parity(h.now, []*rtp.Packet{v.cache.get(seq).pkt}, old)
			a.desired = ctrl.Option{Rendition: ctrl.Low, TID: 1}
			v.step(h.now)
			h.send(ctrl.Low, desc{key: true, flexible: true}, 6, 500)
			if !v.forwarding(ctrl.Native) || pendingRenditions(v)[ctrl.Native] == 0 {
				t.Fatal("Low admission erased Native playback")
			}
			oldTS := v.rewrite.lastTS
			// Advance in real writer-sized batches; observe the first Low
			// packet before LAST, without assuming a batch means WriteRTP.
			boundary, firstLow := -1, uint16(0)
			for end := h.now.Add(200 * time.Millisecond); h.now.Before(end); {
				h.drain(v, time.Millisecond)
				packets := w.written()
				for i := before; i < len(packets); i++ {
					p := packets[i]
					if p.Timestamp != oldTS && p.SequenceNumber != seq {
						boundary, firstLow = i, p.SequenceNumber
						break
					}
				}
				if boundary >= 0 {
					break
				}
			}
			if boundary < 0 {
				t.Fatal("required Low did not commit within actual service")
			}
			if firstLow <= reservedEnd {
				t.Fatal("Low reused an old picture's reserved sequence block")
			}
			if v.ctl.Applied() == a.desired || v.rend != ctrl.Low || pendingRenditions(v)[ctrl.Native] != 0 {
				t.Fatal("first departure applied LAST or retained old tail/repair")
			}
			// Late old arrivals and a stale cache request cannot revive Native.
			priorSends := v.cache.get(seq).sends
			v.resendVideo(h.now, []uint16{seq}, false)
			h.send(ctrl.Native, desc{flexible: true, pdiff: []uint8{1}}, 2, 500)
			if v.cache.get(seq).sends != priorSends || pendingRenditions(v)[ctrl.Native] != 0 {
				t.Fatal("old cache/arrival revived superseded chain")
			}
			// Change TID while the Low key is in flight: acknowledge only
			// current desired TID at its LAST, with no synthetic recovery.
			a.desired.TID = 2
			v.step(h.now)
			h.drain(v, 150*time.Millisecond)
			v.step(h.now)
			if v.ctl.Applied() != a.desired || k.broken != 0 {
				t.Fatalf("reset completion/recovery: applied%+v desired%+v broken%d", v.ctl.Applied(), a.desired, k.broken)
			}
			packets := w.written()
			if len(packets)-boundary != 6 || !packets[len(packets)-1].Marker {
				t.Fatalf("old original/RTX/FEC followed Low: %d packets", len(packets)-boundary)
			}
			for i, p := range packets[boundary:] {
				if p.SequenceNumber != firstLow+uint16(i) || p.Timestamp != packets[boundary].Timestamp {
					t.Fatal("Low rewrite/sequence changed inside its picture")
				}
			}
		})
	}
}

func TestRequiredLowResetCannotLeapfrogAnExtractedPhysicalBatch(t *testing.T) {
	h, v, w, a := resetWire(t)
	before := len(w.written())
	h.send(ctrl.Native, desc{key: true, flexible: true}, 24, 1000)
	v.mu.Lock()
	batch := v.drain(h.now, nil)
	v.writerPending.Add(int64(len(batch)))
	v.mu.Unlock()
	if len(batch) == 0 || len(batch) >= 24 {
		t.Fatal("no partial extracted Native batch")
	}
	w.block = make(chan struct{})
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(started)
		for _, p := range batch {
			w.WriteRTP(&p.h, p.payload)
			v.writerPending.Add(-1)
		}
		close(done)
	}()
	<-started
	defer func() {
		select {
		case <-done:
		default:
			close(w.block)
			<-done
		}
	}()
	a.desired = ctrl.Option{Rendition: ctrl.Low, TID: 2}
	v.step(h.now)
	h.send(ctrl.Low, desc{key: true, flexible: true}, 2, 500)
	h.now = h.now.Add(10 * time.Millisecond)
	v.mu.Lock()
	v.ctl.BeforeSend(h.now, int(v.writerPending.Load()), a.desired, h.r.costs(h.now))
	if v.rend != ctrl.Native || !v.ctl.Pacer.UncommittedKey(ctrl.Low) || pendingRenditionsUnlocked(v)[ctrl.Native] == 0 {
		t.Fatal("pending physical Native write was credited as free reset service")
	}
	v.mu.Unlock()
	// The actual blocked WriteRTP must return before its pending fact clears.
	close(w.block)
	<-done
	h.drain(v, 100*time.Millisecond)
	packets := w.written()[before:]
	if len(packets) != len(batch)+2 || !packets[len(packets)-1].Marker {
		t.Fatalf("reset failed after physical work completed or old tail followed Low: batch%d packets%d want%v rend%v queued%v", len(batch), len(packets), v.want, v.rend, pendingRenditions(v))
	}
	for _, p := range packets[len(batch):] {
		if p.Timestamp == packets[0].Timestamp {
			t.Fatal("old extracted tail followed Low")
		}
	}
}

func pendingRenditionsUnlocked(v *viewer) map[ctrl.Rendition]int {
	out := map[ctrl.Rendition]int{}
	for _, o := range v.pending {
		if o.kind == outMedia {
			out[o.frame.info.Rendition]++
		}
	}
	return out
}

func TestUnusableLowResetKeepsNativePlayback(t *testing.T) {
	for _, arm := range []string{"unsupported", "insufficient", "expired"} {
		t.Run(arm, func(t *testing.T) {
			h, v, _, a := resetWire(t)
			h.send(ctrl.Native, desc{key: true, flexible: true}, 2, 500)
			h.drain(v, 100*time.Millisecond)
			h.send(ctrl.Native, desc{flexible: true, pdiff: []uint8{1}}, 24, 1000)
			h.drain(v, time.Millisecond)
			a.desired = ctrl.Option{Rendition: ctrl.Low, TID: 2}
			v.step(h.now)
			d, count := desc{key: true, flexible: true}, 2
			if arm == "unsupported" {
				d.sid = 1
			}
			if arm == "insufficient" {
				count = 400
			}
			h.send(ctrl.Low, d, count, 1000)
			if arm == "expired" {
				v.ctl.BeforeSend(h.now.Add(ctrl.KeyEnvelope), 1, a.desired, h.r.costs(h.now))
			}
			if !v.forwarding(ctrl.Native) || v.ctl.Applied().Rendition != ctrl.Native || arm != "expired" && pendingRenditions(v)[ctrl.Native] == 0 {
				t.Fatal("unusable Low falsely completed reset or erased live Native")
			}
		})
	}
}
