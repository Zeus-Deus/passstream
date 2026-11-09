package sfu

import (
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

type demandEstimate struct {
	ctrl.FixedEstimator
	value ctrl.Estimate
}

func (e *demandEstimate) Estimate(time.Time) ctrl.Estimate { return e.value }

type demandApplied struct {
	ctrl.Allocator
	applied []ctrl.Option
}

func (a *demandApplied) OnApplied(at time.Time, o ctrl.Option) {
	a.applied = append(a.applied, o)
	a.Allocator.OnApplied(at, o)
}

// Real allocator/RoomKeys -> ordinary PLI demand -> source RTP -> real ingress
// assembly -> paced wire reset. Only estimator input and source bodies/timing
// are reconstructed; no desired option or ready reset key is injected.
func TestRequiredLowAcquisitionReachesSourceAssemblyAndWire(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	lowAt, lowTS := h.now, uint32(6000)
	sourcePicture := func(r ctrl.Rendition, d desc, count, bytes int) {
		// The lightweight harness starts at assembler.push; supply the
		// production read loop's packet-meter facts before that boundary.
		for _, p := range h.packets(r, d, count, bytes) {
			h.r.meterVideo(h.now, r, d.tid, d.key, uint64(p.Timestamp), p.MarshalSize()+wireOverhead)
			h.asm[r].push(h.now, p)
		}
	}
	emitRequested := func(r ctrl.Rendition, count, bytes int) {
		t.Helper()
		due := h.r.keys.Due(h.now)
		if len(due) != 1 || due[0] != r {
			t.Fatalf("source emitted without ordinary requested key: wanted%v due%v decision%+v", r, due, v.decision)
		}
		if len(h.r.keys.Due(h.now)) != 0 {
			t.Fatal("one acquisition emitted redundant PLI")
		}
		if r == ctrl.Low {
			h.ts[r] = lowTS + uint32(h.now.Sub(lowAt)*90000/time.Second) - 6000
		}
		sourcePicture(r, desc{key: true, flexible: true}, count, bytes)
	}
	emitRequested(ctrl.Low, 2, 500)
	h.drain(v, 100*time.Millisecond)
	// Meter a live independent Native source while Low is applied. These
	// flexible delta bodies establish nonzero steady demand before its key.
	for range 60 {
		h.now = h.now.Add(time.Second / 15)
		sourcePicture(ctrl.Native, desc{flexible: true, pdiff: []uint8{1}}, 3, 1000)
	}
	e := &demandEstimate{value: ctrl.Estimate{Rate: 3_000_000, Proven: 3_000_000, Sustained: 3_000_000}}
	v.ctl.Est = e
	a := &demandApplied{Allocator: v.ctl.Alloc}
	v.ctl.Alloc = a
	v.step(h.now)
	if !v.decision.Requested || v.want != ctrl.Native {
		t.Fatalf("real allocator did not acquire Native: %+v costs%+v", v.decision, h.r.costs(h.now))
	}
	emitRequested(ctrl.Native, 24, 1000)
	h.drain(v, time.Millisecond)
	active, known := v.ctl.Pacer.Active()
	if !known || active != ctrl.Native || v.ctl.Applied().Rendition != ctrl.Low || len(a.applied) != 0 {
		t.Fatal("fixture did not reach actual Native with lagging Applied Low")
	}
	before := len(w.written())
	e.value = ctrl.Estimate{Rate: 129212, Proven: 152015, Sustained: 101895, Overuse: true, CongestedAt: h.now, RTT: 42718750 * time.Nanosecond}
	v.step(h.now)
	if v.want != ctrl.Low || !v.decision.Requested || v.decision.Key != ctrl.KeyAccepted {
		t.Fatalf("real current Low desire did not acquire normal reset key: %+v costs%+v", v.decision, h.r.costs(h.now))
	}
	if pendingRenditions(v)[ctrl.Low] != 0 {
		t.Fatal("test already supplied a ready Low key before acquisition")
	}
	emitRequested(ctrl.Low, 8, 500)
	if v.ctl.KeyTimes(ctrl.Low).Assembled != h.now || !v.forwarding(ctrl.Native) || pendingRenditions(v)[ctrl.Native] == 0 {
		t.Fatal("source assembly did not preserve Native until actual Low departure")
	}
	h.drain(v, time.Millisecond)
	if v.rend != ctrl.Low || pendingRenditions(v)[ctrl.Native] != 0 || len(a.applied) != 0 {
		t.Fatal("first Low departure failed retirement or acknowledged LAST early")
	}
	h.drain(v, 400*time.Millisecond)
	v.step(h.now)
	if len(a.applied) != 1 || a.applied[0] != v.ctl.Applied() || a.applied[0].Rendition != ctrl.Low || !v.ctl.KeyTimes(ctrl.Native).Delivered.IsZero() {
		t.Fatalf("wrong completion ownership: applied%+v nativeTimes%+v", a.applied, v.ctl.KeyTimes(ctrl.Native))
	}
	packets := w.written()[before:]
	if len(packets) != 8 || !packets[len(packets)-1].Marker {
		t.Fatalf("old Native tail or duplicate reset followed Low: %d packets", len(packets))
	}
	for i, p := range packets {
		if p.Timestamp != packets[0].Timestamp || p.SequenceNumber != packets[0].SequenceNumber+uint16(i) {
			t.Fatal("reset crossed picture/sequence boundary")
		}
	}
}

// Reconstruct both operating contexts with the real allocator and ordinary
// room/source path. No forced desired option or unsolicited target key.
func TestTransactionReverseDemandAndGenuineWireUpgrade(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	var last [2]time.Time
	picture := func(r ctrl.Rendition, d desc, count, body int) {
		if !last[r].IsZero() {
			h.ts[r] += uint32(h.now.Sub(last[r])*90000/time.Second) - 6000
		}
		for _, p := range h.packets(r, d, count, body) {
			h.r.meterVideo(h.now, r, d.tid, d.key, uint64(p.Timestamp), p.MarshalSize()+wireOverhead)
			h.asm[r].push(h.now, p)
		}
		last[r] = h.now
	}
	emit := func(r ctrl.Rendition, count, body int) {
		t.Helper()
		due := h.r.keys.Due(h.now)
		if len(due) != 1 || due[0] != r {
			t.Fatalf("no ordinary source demand for%v: %v %+v", r, due, v.decision)
		}
		h.now = h.now.Add(time.Second / 15)
		picture(r, desc{key: true, flexible: true}, count, body)
	}
	emit(ctrl.Low, 2, 500)
	h.drain(v, 100*time.Millisecond)
	for range 60 {
		h.now = h.now.Add(time.Second / 15)
		picture(ctrl.Native, desc{flexible: true, pdiff: []uint8{1}}, 3, 1000)
	}
	e := &demandEstimate{value: ctrl.Estimate{Rate: 3000000, Proven: 3000000, Sustained: 3000000}}
	v.ctl.Est = e
	v.step(h.now)
	emit(ctrl.Native, 24, 1000)
	h.drain(v, 200*time.Millisecond)
	v.step(h.now)
	if v.ctl.Applied().Rendition != ctrl.Native {
		t.Fatal("initial real Native did not complete")
	}
	e.value = ctrl.Estimate{Rate: 128000, Proven: 329928, Sustained: 101895, Overuse: true, CongestedAt: h.now}
	v.step(h.now)
	h.now = h.now.Add(ctrl.DownTime)
	v.step(h.now)
	if v.want != ctrl.Low || !v.decision.Requested {
		t.Fatalf("real ordinary degradation did not acquire Low: %+v", v.decision)
	}
	emit(ctrl.Low, 16, 500)
	h.drain(v, time.Millisecond)
	if v.ctl.Operating().Rendition != ctrl.Low || v.ctl.Applied().Rendition != ctrl.Native {
		t.Fatal("not actual Low with historical completed Native")
	}
	// Current source becomes sparse while the Low key is still partial;
	// this makes an inherited Native hold cheap, not a new upgrade proof.
	for range 10 {
		h.now = h.now.Add(time.Second / 15)
		picture(ctrl.Native, desc{flexible: true, pdiff: []uint8{1}}, 1, 300)
	}
	congested := h.now
	e.value = ctrl.Estimate{Rate: 128495, Proven: 329928, Sustained: 146477, Congestive: true, Loss: 2.0 / 11, CongestedAt: congested}
	keysBefore := h.r.Keyframes.Load()
	v.step(h.now)
	if v.decision.Intent != ctrl.Retain || v.decision.Requested || v.want != ctrl.Low || len(h.r.keys.Due(h.now)) != 0 || h.r.Keyframes.Load() != keysBefore {
		t.Fatalf("reverse context requested source/co-viewer Native refresh: %+v", v.decision)
	}
	if left, _ := v.ctl.Pacer.QueuedKey(ctrl.Low); left == 0 {
		t.Fatal("reverse control lost the incomplete Low key")
	}
	h.drain(v, 800*time.Millisecond)
	v.step(h.now)
	if v.ctl.Applied().Rendition != ctrl.Low {
		t.Fatal("Low did not complete")
	}
	// After the original quiet/dwell, another ordinary room join asks for
	// a Low refresh. Genuine Native eligibility during that Low delivery
	// must still acquire/commit, with the optional whole-picture fence.
	h.now = h.now.Add(ctrl.RenditionDwell + time.Second)
	e.value = ctrl.Estimate{Rate: 1000000, Proven: 1000000, Sustained: 1000000, CongestedAt: congested}
	if s := h.r.keys.Request(h.now, v.id+1000, ctrl.Low, ctrl.CauseJoin); s != ctrl.KeyAccepted {
		t.Fatal("ordinary refresh budget unavailable")
	}
	emit(ctrl.Low, 16, 500)
	h.drain(v, time.Millisecond)
	if left, _ := v.ctl.Pacer.QueuedKey(ctrl.Low); left == 0 {
		t.Fatal("no Low partial picture for genuine optional control")
	}
	before := len(w.written())
	v.step(h.now)
	if !v.decision.Requested || v.decision.Intent != ctrl.Upgrade || v.want != ctrl.Native {
		t.Fatalf("genuine actual-Low upgrade was prohibited: %+v", v.decision)
	}
	emit(ctrl.Native, 8, 1000)
	h.drain(v, 300*time.Millisecond)
	packets := w.written()[before:]
	nativeTS := packets[len(packets)-1].Timestamp
	firstNative := -1
	for i, p := range packets {
		if p.Timestamp == nativeTS {
			firstNative = i
			break
		}
	}
	if firstNative <= 0 || !packets[firstNative-1].Marker || !packets[len(packets)-1].Marker || v.ctl.Applied().Rendition != ctrl.Native {
		t.Fatal("genuine optional upgrade leapfrogged old Low or failed completion")
	}
	for _, p := range packets[firstNative:] {
		if p.Timestamp != nativeTS {
			t.Fatal("old Low tail followed optional Native FIRST")
		}
	}
}
