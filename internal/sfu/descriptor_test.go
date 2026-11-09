package sfu

import (
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// C2-4: every packet's clear descriptor must agree with its picture's first
// and respect the picture's boundaries; a valid head cannot carry a
// malformed or unsupported tail past refusal. Payload bytes in the harness:
// [0] flags, [1:3] picture ID, [3] TID/U/SID/D, [4] TL0PICIDX or P_DIFF.
func TestEveryPacketDescriptorIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name   string
		d      desc
		tamper func([]*rtp.Packet)
	}{
		{"tail TID differs", desc{tid: 2}, func(p []*rtp.Packet) { p[1].Payload[3] = p[1].Payload[3]&0x1f | 1<<5 }},
		{"tail spatial layer", desc{tid: 2}, func(p []*rtp.Packet) { p[1].Payload[3] |= 1 << 1 }},
		{"tail picture ID differs", desc{tid: 2}, func(p []*rtp.Packet) { p[1].Payload[2]++ }},
		{"tail TL0PICIDX differs", desc{tid: 2}, func(p []*rtp.Packet) { p[1].Payload[4]++ }},
		{"tail claims intra", desc{tid: 2}, func(p []*rtp.Packet) { p[1].Payload[0] &^= 0x40 }},
		{"tail begins a frame", desc{tid: 2}, func(p []*rtp.Packet) { p[1].Payload[0] |= 0x08 }},
		{"head ends the frame", desc{tid: 2}, func(p []*rtp.Packet) { p[0].Payload[0] |= 0x04 }},
		{"tail without picture ID", desc{tid: 2}, func(p []*rtp.Packet) { p[1].Payload[0] &^= 0x80 }},
		{"tail truncated", desc{tid: 2}, func(p []*rtp.Packet) { p[1].Payload = p[1].Payload[:1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			v, w := h.viewer()
			h.send(ctrl.Low, desc{key: true}, 1, 500)
			h.drain(v, 100*time.Millisecond)
			pkts := h.packets(ctrl.Low, tc.d, 2, 500)
			tc.tamper(pkts)
			for _, p := range pkts {
				h.asm[ctrl.Low].push(h.now, p)
			}
			h.drain(v, 100*time.Millisecond)
			if n := len(w.written()); n != 1 || v.forwarding(ctrl.Low) {
				t.Fatalf("picture with an invalid tail forwarded or tolerated: %d packets", n)
			}
		})
	}
	t.Run("flexible tail references differ", func(t *testing.T) {
		h := newHarness(t)
		v, w := h.viewer()
		h.send(ctrl.Low, desc{key: true, flexible: true}, 1, 500)
		h.drain(v, 100*time.Millisecond)
		pkts := h.packets(ctrl.Low, desc{tid: 2, flexible: true, pdiff: []uint8{1}}, 2, 500)
		pkts[1].Payload[4] = 2 << 1 // P_DIFF 2 in the tail, 1 in the head
		for _, p := range pkts {
			h.asm[ctrl.Low].push(h.now, p)
		}
		h.drain(v, 100*time.Millisecond)
		if n := len(w.written()); n != 1 || v.forwarding(ctrl.Low) {
			t.Fatalf("flexible picture with inconsistent references forwarded: %d packets", n)
		}
	})
	t.Run("repeated picture ID", func(t *testing.T) {
		h := newHarness(t)
		v, w := h.viewer()
		h.send(ctrl.Low, desc{key: true}, 1, 500)
		pkts := h.packets(ctrl.Low, desc{tid: 2}, 1, 500)
		again := &rtp.Packet{Header: pkts[0].Header, Payload: append([]byte(nil), pkts[0].Payload...)}
		again.SequenceNumber++
		h.seq[ctrl.Low]++
		for _, p := range []*rtp.Packet{pkts[0], again} {
			h.drain(v, 100*time.Millisecond)
			h.asm[ctrl.Low].push(h.now, p)
		}
		h.drain(v, 100*time.Millisecond)
		if n := len(w.written()); n != 2 || v.forwarding(ctrl.Low) {
			t.Fatalf("a second picture under one picture ID forwarded or tolerated: %d packets", n)
		}
	})
}

// C2-4: a non-flexible pattern violation is invisible to the stateless
// Supported check; continuity's latched state makes the forwarder refuse
// the rendition at once and request recovery, instead of silently
// withholding every later picture.
func TestNonFlexiblePatternViolationRefusesAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  func(h *harness) []*rtp.Packet
	}{
		{"TID out of pattern", func(h *harness) []*rtp.Packet { return h.packets(ctrl.Low, desc{tid: 1}, 1, 500) }},
		{"TL0PICIDX skips", func(h *harness) []*rtp.Packet {
			h.tl0[ctrl.Low] += 2
			return h.packets(ctrl.Low, desc{tid: 2}, 1, 500)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			v, w := h.viewer()
			h.r.keys.Due(h.now)
			h.send(ctrl.Low, desc{key: true}, 1, 500)
			h.drain(v, 100*time.Millisecond)
			for _, p := range tc.bad(h) { // expected next: TID 2 at the key's TL0PICIDX
				h.asm[ctrl.Low].push(h.now, p)
			}
			h.drain(v, 100*time.Millisecond)
			if len(w.written()) != 1 || v.forwarding(ctrl.Low) || !v.ctl.Cont.Unsupported(ctrl.Low) {
				t.Fatalf("pattern violation tolerated: %d packets, forwarding %v", len(w.written()), v.forwarding(ctrl.Low))
			}
			if due := h.r.keys.Due(h.now.Add(ctrl.KeyInterval)); len(due) != 1 {
				t.Fatalf("refusal requested no recovery key: %v", due)
			}
		})
	}
}
