package sfu

import (
	"testing"
	"time"

	"github.com/pion/rtcp"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// footprint is one viewer's retained per-viewer state: everything that can
// hold a payload or grow with session length, outside the reported queue.
type footprint struct {
	pendingPackets, pendingPayload int // payloads mapped by pacer IDs
	queuedBytes, queuedFrames      int // what the pacer reports
	drops, events                  int // facts waiting for Step
	estimator, prober              int // history entries
	fecBuckets, fecPackets         int // open parity groups
	cacheEntries, cacheBytes       int // fixed-size NACK caches (video + audio)
}

func account(v *viewer) footprint {
	v.mu.Lock()
	defer v.mu.Unlock()
	var f footprint
	for _, o := range v.pending {
		f.pendingPackets++
		f.pendingPayload += len(o.pkt.Payload)
	}
	f.queuedBytes, f.queuedFrames = v.ctl.Pacer.Queued()
	f.drops, f.events = v.ctl.Pacer.(*ctrl.Queue).Retained()
	f.estimator = v.ctl.Est.(*ctrl.Bandwidth).Retained()
	f.prober = v.ctl.Probe.(*ctrl.Discovery).Retained()
	f.fecBuckets = len(v.groups)
	for _, g := range v.groups {
		f.fecPackets += len(g)
	}
	for _, c := range []*packetCache{&v.cache, &v.audioCache} {
		for _, e := range c.entries {
			if e.pkt != nil {
				f.cacheEntries++
				f.cacheBytes += len(e.pkt.Payload)
			}
		}
	}
	return f
}

// hostileFeedback reports count transport sequences from base: all but the
// last fifth lost, the rest received on a receiver clock that never advances.
func hostileFeedback(base uint64, count int, id uint8) *rtcp.TransportLayerCC {
	got := max(1, count/5)
	fb := &rtcp.TransportLayerCC{BaseSequenceNumber: uint16(base), PacketStatusCount: uint16(count), ReferenceTime: 1, FbPktCount: id}
	if lost := count - got; lost > 0 {
		fb.PacketChunks = append(fb.PacketChunks, &rtcp.RunLengthChunk{PacketStatusSymbol: rtcp.TypeTCCPacketNotReceived, RunLength: uint16(lost)})
	}
	fb.PacketChunks = append(fb.PacketChunks, &rtcp.RunLengthChunk{PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta, RunLength: uint16(got)})
	for range got {
		fb.RecvDeltas = append(fb.RecvDeltas, &rtcp.RecvDelta{Type: rtcp.TypeTCCPacketReceivedSmallDelta})
	}
	return fb
}

// C2-1: a viewer whose receiver lies about its clock and loss, and whose
// socket then blocks for 10,000 pictures, keeps every per-viewer structure
// within a fixed bound; retained payloads are exactly what the pacer reports.
func TestViewerMemoryIsBoundedWithBlockedWriterAndHostileReceiver(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	v.mu.Lock()
	v.video.binding.red, v.video.binding.redPT, v.video.binding.fecPT = true, 116, 117
	v.mu.Unlock()
	n := 0
	picture := func() {
		if n%90 == 0 {
			h.send(ctrl.Low, desc{key: true}, 6, 900)
		} else {
			h.send(ctrl.Low, desc{tid: [4]uint8{0, 2, 1, 2}[n%90%4]}, 3, 600)
		}
		n++
	}
	// Phase 1: the writer flows for 120 s; once a second the receiver
	// reports everything sent with a constant clock and long loss runs.
	var reported uint64
	for i := range 120 * 30 {
		picture()
		h.drain(v, 33*time.Millisecond)
		if i%30 == 29 {
			v.mu.Lock()
			sent := v.tseq
			v.mu.Unlock()
			if count := min(sent-reported, 8000); count > 0 {
				v.feedback(h.now, hostileFeedback(sent-count, int(count), uint8(i/30)))
				reported = sent
			}
		}
	}
	flowing := account(v)
	// Phase 2: the writer is blocked in WriteRTP (no Pop, no Step) while
	// ingress keeps offering pictures for another 330 s.
	for range 10_000 {
		h.now = h.now.Add(33 * time.Millisecond)
		picture()
	}
	blocked := account(v)
	// The writer resumes: one batch leaves under the pacer's burst.
	v.mu.Lock()
	rate := v.ctl.Est.Estimate(h.now).Rate
	batch := v.drain(h.now, nil)
	v.mu.Unlock()
	batchBytes := 0
	for _, w := range batch {
		batchBytes += w.h.MarshalSize() + len(w.payload) + wireOverhead
	}
	t.Logf("flowing: %+v", flowing)
	t.Logf("blocked: %+v", blocked)
	t.Logf("resumed writer batch: %d packets / %d bytes at R=%d", len(batch), batchBytes, rate)

	for _, f := range []footprint{flowing, blocked} {
		if f.pendingPayload > f.queuedBytes {
			t.Errorf("%d payload bytes retained beyond the %d bytes the pacer reports", f.pendingPayload, f.queuedBytes)
		}
		if f.drops > 2 || f.events > 8 {
			t.Errorf("deferred facts not coalesced: %d drops, %d key events", f.drops, f.events)
		}
		if f.estimator > 5*8192 {
			t.Errorf("estimator history %d entries", f.estimator)
		}
		if f.prober > 1000 {
			t.Errorf("prober accounting %d entries", f.prober)
		}
		if f.fecBuckets > 16 || f.fecPackets > 16*16 {
			t.Errorf("FEC groups %d buckets / %d packets exceed the ULPFEC mask", f.fecBuckets, f.fecPackets)
		}
		if f.cacheEntries > 2048+128 {
			t.Errorf("NACK caches hold %d entries", f.cacheEntries)
		}
	}
	if burst := max(ctrl.BurstFloor, rate*int(ctrl.PacerBurst/time.Millisecond)/8000); batchBytes > burst+1500 {
		t.Errorf("resumed batch %d bytes exceeds the %d-byte burst", batchBytes, burst)
	}
}
