package sfu

import (
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Regressions for C2 item 6b (docs/REPORT.md).

// drainingEstimator reports an open drain episode over its wrapped estimate.
type drainingEstimator struct{ ctrl.Estimator }

func (e drainingEstimator) Estimate(now time.Time) ctrl.Estimate {
	x := e.Estimator.Estimate(now)
	x.Rate, x.Proven, x.Draining, x.Service, x.Debt = 217_600, 0, true, 256_000, 70_000
	return x
}

// C2 §5 A5: a drain episode stops the obsolete native rendition at once
// (purging its backlog, without waiting for a down timer or the low key),
// forwards nothing more of it, and switches at the low key.
func TestDrainEpisodeStopsTheObsoleteRendition(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	// This harness starts after the read-loop meter. Supply the live Low
	// wire-demand facts so a drain's floor has real service, rather than
	// relying on one free burst at a zero pacing rate.
	h.r.meterVideo(h.now.Add(-264*time.Millisecond), ctrl.Low, 0, false, 1, 3300)
	h.r.meterVideo(h.now, ctrl.Low, 0, false, 2, 3300)
	h.r.keys.Due(h.now)
	v.mu.Lock()
	v.want, v.wantTID = ctrl.Native, 2
	v.mu.Unlock()
	h.send(ctrl.Native, desc{key: true, flexible: true}, 3, 1000)
	h.send(ctrl.Native, desc{tid: 0, flexible: true, pdiff: []uint8{1}}, 3, 1000)
	if n := pendingRenditions(v)[ctrl.Native]; n != 6 {
		t.Fatalf("%d native packets queued before the episode", n)
	}
	v.mu.Lock()
	v.ctl.Est = drainingEstimator{v.ctl.Est}
	v.step(h.now)
	stopped, want := v.stopped, v.want
	v.mu.Unlock()
	if !stopped || want != ctrl.Low || pendingRenditions(v)[ctrl.Native] != 0 {
		t.Fatalf("stopped %v want %v native queued %d", stopped, want, pendingRenditions(v)[ctrl.Native])
	}
	h.send(ctrl.Native, desc{tid: 2, flexible: true, pdiff: []uint8{1}}, 1, 500)
	if n := pendingRenditions(v)[ctrl.Native]; n != 0 {
		t.Fatalf("%d native packets forwarded after the stop", n)
	}
	h.send(ctrl.Low, desc{key: true}, 2, 1000)
	h.drain(v, 200*time.Millisecond)
	v.mu.Lock()
	rend, stopped := v.rend, v.stopped
	v.mu.Unlock()
	if rend != ctrl.Low || stopped {
		t.Fatalf("low key did not restart forwarding: rendition %v stopped %v", rend, stopped)
	}
}

func pendingRTX(v *viewer) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, o := range v.pending {
		if o.kind == outRTX {
			n++
		}
	}
	return n
}

// C2 §5 A12/A13: a key or TL0 packet that TWCC reports lost is resent at
// once, without waiting for the receiver's NACK; the NACK that follows
// within one RTT is a duplicate of that repair, and a lost delta of a
// higher TID is left to the NACK.
func TestTWCCLossResendsCriticalPacketOnce(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	h.r.keys.Due(h.now)
	h.send(ctrl.Low, desc{key: true}, 3, 500)
	h.send(ctrl.Low, desc{tid: 2}, 2, 500)
	h.drain(v, 300*time.Millisecond)
	v.mu.Lock()
	v.ctl.OnReport(h.now, ctrl.ReceiverReport{RTT: 80 * time.Millisecond})
	sent := v.tseq
	v.mu.Unlock()
	var media []*rtp.Packet
	for _, p := range w.written() {
		if !p.Padding && len(p.Payload) > 0 {
			media = append(media, p)
		}
	}
	if len(media) != 5 || int(sent) != 5 {
		t.Fatalf("%d media packets, %d transport sequences", len(media), sent)
	}
	// The key's second packet and the TL2 picture's first are lost.
	small := rtcp.TypeTCCPacketReceivedSmallDelta
	symbols := []uint16{small, rtcp.TypeTCCPacketNotReceived, small, rtcp.TypeTCCPacketNotReceived, small}
	var deltas []*rtcp.RecvDelta
	for range 3 {
		deltas = append(deltas, &rtcp.RecvDelta{Type: small, Delta: 250})
	}
	v.feedback(h.now, &rtcp.TransportLayerCC{BaseSequenceNumber: 0, PacketStatusCount: 5, ReferenceTime: 1, FbPktCount: 1, PacketChunks: chunks(symbols), RecvDeltas: deltas})
	if n := pendingRTX(v); n != 1 {
		t.Fatalf("%d retransmissions queued for one lost key packet", n)
	}
	h.drain(v, 20*time.Millisecond)
	v.nackVideo(h.now, []uint16{media[1].SequenceNumber})
	if n := pendingRTX(v); n != 0 {
		t.Fatalf("the NACK within one RTT of the early repair queued %d more", n)
	}
	v.nackVideo(h.now, []uint16{media[3].SequenceNumber})
	if n := pendingRTX(v); n != 1 {
		t.Fatalf("the NACKed TL2 packet queued %d retransmissions", n)
	}
}

// The last packet of a protected picture flushes its partial parity group:
// parity leaves with the picture rather than with the next base picture.
func TestProtectedPictureFlushesItsParity(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	h.r.keys.Due(h.now)
	v.mu.Lock()
	v.video.binding.red, v.video.binding.redPT, v.video.binding.fecPT = true, 116, 117
	v.mu.Unlock()
	h.send(ctrl.Low, desc{key: true}, 2, 500)
	h.drain(v, 200*time.Millisecond)
	before := len(w.written())
	v.mu.Lock()
	v.fec, v.interleave = 0.25, 1
	v.mu.Unlock()
	h.send(ctrl.Low, desc{tid: 2}, 1, 500)
	h.send(ctrl.Low, desc{tid: 1}, 1, 500)
	h.send(ctrl.Low, desc{tid: 2}, 1, 500)
	h.send(ctrl.Low, desc{tid: 0}, 2, 500) // two packets: half a one-per-four group
	h.drain(v, 200*time.Millisecond)
	parity := 0
	for _, p := range w.written()[before:] {
		if len(p.Payload) > 0 && p.Payload[0] == 117 {
			parity++
		}
	}
	if parity != 1 {
		t.Fatalf("%d parity packets for one two-packet TL0 picture", parity)
	}
}
