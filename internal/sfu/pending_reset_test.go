package sfu

import (
	"github.com/passstream/passstream/internal/sfu/ctrl"
	"testing"
	"time"
)

// Real allocator and room -> Pending -> ordinary PLI -> meter/assembler ->
// WriteRTP. The estimator and the room's recent observed-Low-key time are
// reconstructed seams. No forced Desired or pre-injected target Low payload.
func TestRequiredLowPendingResetWithBlockedOriginal(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	lowOrigin := h.now
	picture := func(r ctrl.Rendition, d desc, n, size int) {
		if r == ctrl.Low {
			h.ts[r] = 6000 + uint32(h.now.Sub(lowOrigin)*90000/time.Second) - 6000
		}
		for _, p := range h.packets(r, d, n, size) {
			h.r.meterVideo(h.now, r, d.tid, d.key, uint64(p.Timestamp), p.MarshalSize()+wireOverhead)
			h.asm[r].push(h.now, p)
		}
	}
	due := h.r.keys.Due(h.now)
	if len(due) != 1 || due[0] != ctrl.Low {
		t.Fatal("no ordinary initial Low")
	}
	picture(ctrl.Low, desc{key: true, flexible: true}, 2, 500)
	h.drain(v, 100*time.Millisecond)
	for range 60 {
		h.now = h.now.Add(time.Second / 15)
		picture(ctrl.Native, desc{flexible: true, pdiff: []uint8{1}}, 3, 1000)
	}
	e := &demandEstimate{value: ctrl.Estimate{Rate: 3000000, Proven: 3000000, Sustained: 3000000}}
	v.ctl.Est = e
	v.step(h.now)
	if !v.decision.Requested || v.want != ctrl.Native {
		t.Fatalf("real Native demand missing %+v", v.decision)
	}
	due = h.r.keys.Due(h.now)
	if len(due) != 1 || due[0] != ctrl.Native {
		t.Fatal("no normal Native PLI")
	}
	picture(ctrl.Native, desc{key: true, flexible: true}, 24, 1000)
	before := len(w.written())
	v.mu.Lock()
	batch := v.drain(h.now, nil)
	v.writerPending.Add(int64(len(batch)))
	v.mu.Unlock()
	if len(batch) == 0 || len(batch) >= 24 {
		t.Fatal("not partial physical Native batch")
	}
	if v.ctl.Operating().Rendition != ctrl.Native || v.ctl.Applied().Rendition != ctrl.Low {
		t.Fatal("not actual Native with lagging Applied")
	}
	reservedEnd := v.seq
	nativeSeq := batch[0].h.SequenceNumber
	w.block = make(chan struct{})
	entered, finished := make(chan struct{}), make(chan struct{})
	go func() {
		close(entered)
		for _, p := range batch {
			w.WriteRTP(&p.h, p.payload)
			v.writerPending.Add(-1)
		}
		close(finished)
	}()
	<-entered
	defer func() {
		select {
		case <-finished:
		default:
			close(w.block)
			<-finished
		}
	}()
	// Prior observed room assembly consumes the ordinary budget, without a
	// Low payload for this viewer or a new Required reset yet.
	h.r.keys.OnKey(h.now.Add(-ctrl.KeyInterval+500*time.Millisecond), ctrl.Low, 1000, h.now.Add(-ctrl.KeyInterval+500*time.Millisecond))
	e.value = ctrl.Estimate{Rate: 129212, Proven: 152015, Sustained: 101895, Overuse: true, CongestedAt: h.now}
	v.step(h.now)
	if !v.decision.Requested || v.decision.Key != ctrl.KeyPending || v.decision.Intent != ctrl.Required || v.want != ctrl.Low {
		t.Fatalf("not normal Pending reset %+v", v.decision)
	}
	if len(h.r.keys.Due(h.now)) != 0 || pendingRenditions(v)[ctrl.Low] != 0 {
		t.Fatal("pending bypassed budget or target already supplied")
	}
	h.now = h.now.Add(500 * time.Millisecond)
	due = h.r.keys.Due(h.now)
	if len(due) != 1 || due[0] != ctrl.Low {
		t.Fatal("Pending lost eventual ordinary PLI")
	}
	if len(h.r.keys.Due(h.now)) != 0 {
		t.Fatal("duplicate PLI")
	}
	picture(ctrl.Low, desc{key: true, flexible: true}, 4, 500)
	v.mu.Lock()
	v.ctl.BeforeSend(h.now, int(v.writerPending.Load()), ctrl.Option{Rendition: v.want, TID: v.wantTID}, h.r.costs(h.now))
	v.mu.Unlock()
	if v.ctl.Operating().Rendition != ctrl.Native || !v.ctl.Pacer.UncommittedKey(ctrl.Low) || pendingRenditions(v)[ctrl.Native] == 0 || len(w.written()) != before {
		t.Fatal("reset overtook unresolved physical original")
	}
	close(w.block)
	<-finished
	h.drain(v, 100*time.Millisecond)
	pkts := w.written()[before:]
	if len(pkts) != len(batch)+4 || pkts[len(batch)].SequenceNumber <= reservedEnd || !pkts[len(pkts)-1].Marker || v.ctl.Applied().Rendition != ctrl.Low {
		t.Fatalf("bad reset ownership/sequence: batch%d packets%d reserved%d applied%v", len(batch), len(pkts), reservedEnd, v.ctl.Applied())
	}
	oldSends := v.cache.get(nativeSeq).sends
	v.resendVideo(h.now, []uint16{nativeSeq}, false)
	picture(ctrl.Native, desc{flexible: true, pdiff: []uint8{1}}, 2, 500)
	if v.cache.get(nativeSeq).sends != oldSends || pendingRenditions(v)[ctrl.Native] != 0 {
		t.Fatal("late old repair/original revived Native")
	}
	for _, p := range pkts[len(batch):] {
		if p.Timestamp == pkts[0].Timestamp {
			t.Fatal("old tail followed Low FIRST")
		}
	}
}
