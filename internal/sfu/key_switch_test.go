package sfu

import (
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

type keySwitchAllocation struct{ desired ctrl.Option }
type admittedKeyProbe struct {
	ctrl.Prober
	bytes int
}

func (p *admittedKeyProbe) MaybeKey(_ time.Time, _, bytes int, _ ctrl.Estimate) (ctrl.Cluster, bool) {
	p.bytes = bytes
	return ctrl.Cluster{}, false
}

func (a *keySwitchAllocation) Decide(_ time.Time, in ctrl.AllocInput) ctrl.Allocation {
	if in.Recovering {
		return forcedAllocation(in, ctrl.Option{Rendition: ctrl.Low, TID: 2})
	}
	return forcedAllocation(in, a.desired)
}
func (*keySwitchAllocation) OnApplied(time.Time, ctrl.Option) {}
func (*keySwitchAllocation) OnGeneration(time.Time, uint64)   {}

// The new rendition is applied when its whole key leaves. A temporary TID
// downgrade while that key is queued must not announce an already-applied
// rendition and start the ordinary temporal-up timer. At completion the
// key is still a switching point for the controller's current desired TID.
func TestKeySwitchAppliesTheLatestDesiredTIDOnlyAtCompletion(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	h.send(ctrl.Low, desc{key: true}, 3, 500)
	h.drain(v, 200*time.Millisecond)
	a := &keySwitchAllocation{desired: ctrl.Option{Rendition: ctrl.Native, TID: 2}}
	v.mu.Lock()
	v.ctl.Est = &ctrl.FixedEstimator{Rate: 5_000_000}
	v.ctl.Probe = ctrl.NoProber{}
	v.ctl.Alloc = a
	v.step(h.now)
	v.mu.Unlock()
	h.send(ctrl.Native, desc{key: true}, 250, 1000)
	h.drain(v, 20*time.Millisecond)
	v.mu.Lock()
	left, _ := v.ctl.Pacer.QueuedKey(ctrl.Native)
	a.desired.TID = 1
	v.step(h.now)
	appliedDuring := v.ctl.Applied()
	// Capacity proof arrives while the key is still going out.
	a.desired.TID = 2
	v.step(h.now)
	v.mu.Unlock()
	if left == 0 {
		t.Fatal("test key finished before the mid-delivery allocation")
	}
	if appliedDuring.Rendition != ctrl.Low {
		t.Errorf("incomplete native key was reported applied: %+v", appliedDuring)
	}
	h.drain(v, time.Second)
	v.mu.Lock()
	appliedAfter := v.ctl.Applied()
	v.mu.Unlock()
	if appliedAfter != (ctrl.Option{Rendition: ctrl.Native, TID: 2}) {
		t.Errorf("completed native key ignored current desired TID: %+v", appliedAfter)
	}
}

func TestAssembledKeyIsObservedBeforeTheWriterOrPeriodicTick(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	h.send(ctrl.Low, desc{key: true}, 3, 500)
	h.drain(v, 200*time.Millisecond)
	p := &admittedKeyProbe{Prober: ctrl.NoProber{}}
	v.mu.Lock()
	v.ctl.Est, v.ctl.Probe = &ctrl.FixedEstimator{Rate: 5_000_000}, p
	v.ctl.Alloc = &keySwitchAllocation{desired: ctrl.Option{Rendition: ctrl.Native, TID: 2}}
	v.step(h.now)
	v.mu.Unlock()
	h.send(ctrl.Native, desc{key: true}, 250, 1000)
	if p.bytes < 250_000 {
		t.Fatalf("key waited for the writer/tick instead of funding discovery at admission: %d bytes", p.bytes)
	}
}

// The forced allocator fixtures specify an explicit action to isolate wire
// ordering. Real eligibility/intent is exercised by the real LayerAllocator tests.
func forcedAllocation(in ctrl.AllocInput, desired ctrl.Option) ctrl.Allocation {
	basis := in.Applied
	if in.Operating != nil {
		basis = *in.Operating
	}
	action := ctrl.Retain
	if desired.Rendition != basis.Rendition {
		if desired.Rendition == ctrl.Native {
			action = ctrl.Upgrade
		} else {
			action = ctrl.Required
		}
	} else if desired.TID > basis.TID {
		action = ctrl.Upgrade
	} else if desired.TID < basis.TID {
		action = ctrl.Required
	}
	if in.Recovering {
		action = ctrl.Required
	}
	return ctrl.Allocation{Desired: desired, Intent: action}
}
