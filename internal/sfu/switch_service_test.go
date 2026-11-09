package sfu

import (
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

func TestSwitchCommitFinishesTheStartedOldPicture(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	a := &keySwitchAllocation{desired: ctrl.Option{Rendition: ctrl.Low, TID: 2}}
	v.mu.Lock()
	v.ctl.Alloc = a
	v.ctl.Est = &ctrl.FixedEstimator{Rate: 1_000_000}
	v.ctl.Probe = ctrl.NoProber{}
	v.step(h.now)
	v.mu.Unlock()
	h.send(ctrl.Low, desc{key: true, flexible: true}, 2, 500)
	h.drain(v, 100*time.Millisecond)
	before := len(w.written())
	h.send(ctrl.Low, desc{flexible: true, pdiff: []uint8{1}}, 20, 1000)
	h.drain(v, time.Millisecond)
	if n := len(w.written()) - before; n == 0 || n >= 20 {
		t.Fatalf("old frame not partial: %d", n)
	}
	v.mu.Lock()
	a.desired = ctrl.Option{Rendition: ctrl.Native, TID: 0}
	v.step(h.now)
	v.mu.Unlock()
	h.send(ctrl.Native, desc{key: true, flexible: true}, 3, 1000)
	if !v.forwarding(ctrl.Low) || pendingRenditions(v)[ctrl.Low] == 0 {
		t.Fatal("old playback purged at admission")
	}
	h.drain(v, 300*time.Millisecond)
	packets := w.written()[before:]
	if len(packets) != 23 || !packets[19].Marker || packets[20].Timestamp == packets[19].Timestamp || !packets[22].Marker {
		t.Fatal("switch cut/interleaved an old picture")
	}
	for i := 1; i < len(packets); i++ {
		if packets[i].SequenceNumber != packets[i-1].SequenceNumber+1 {
			t.Fatal("reserved picture sequence order changed")
		}
	}
	if v.ctl.Applied() != (ctrl.Option{Rendition: ctrl.Native, TID: 0}) {
		t.Fatal("new option not applied at complete key")
	}
}

func TestUncommittedSwitchLosesPathProofWithoutInterruptingLow(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	a := &keySwitchAllocation{desired: ctrl.Option{Rendition: ctrl.Low, TID: 2}}
	v.mu.Lock()
	v.ctl.Alloc = a
	v.ctl.Est = &ctrl.FixedEstimator{Rate: 5_000_000}
	v.ctl.Probe = ctrl.NoProber{}
	v.step(h.now)
	v.mu.Unlock()
	h.send(ctrl.Low, desc{key: true}, 2, 500)
	h.drain(v, 100*time.Millisecond)
	v.mu.Lock()
	a.desired = ctrl.Option{Rendition: ctrl.Native, TID: 0}
	v.step(h.now)
	v.mu.Unlock()
	h.send(ctrl.Native, desc{key: true}, 200, 1175)
	v.mu.Lock()
	v.ctl.BeforeSend(h.now, 1, a.desired, h.r.costs(h.now))
	if !v.forwarding(ctrl.Low) || !v.ctl.Pacer.UncommittedKey(ctrl.Native) {
		t.Fatal("unknown write time committed switch")
	}
	v.ctl.OnPathChange(h.now)
	v.ctl.BeforeSend(h.now, 0, a.desired, h.r.costs(h.now))
	if !v.forwarding(ctrl.Low) || v.ctl.Pacer.UncommittedKey(ctrl.Native) {
		t.Fatal("lost path proof left candidate or broke old low")
	}
	v.mu.Unlock()
}
