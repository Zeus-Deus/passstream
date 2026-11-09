package sfusim

import (
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// This is the wire-sized phase fixture through model ingress, shared policy,
// paced transport and model receiver, not a canonical source/distribution or
// Default eligibility experiment. Real-default preparation has separate tests.
type phaseAllocation struct{ desired ctrl.Option }

func (a *phaseAllocation) Decide(_ time.Time, in ctrl.AllocInput) ctrl.Allocation {
	if in.Recovering {
		return forcedAllocation(in, ctrl.Option{Rendition: ctrl.Low, TID: 2})
	}
	return forcedAllocation(in, a.desired)
}
func (*phaseAllocation) OnApplied(time.Time, ctrl.Option) {}
func (*phaseAllocation) OnGeneration(time.Time, uint64)   {}

type phasePacer struct {
	ctrl.Pacer
	departures map[uint64]ctrl.Packet
	at         map[uint64]time.Time
}

func (p *phasePacer) Pop(now time.Time) (ctrl.Packet, bool) {
	k, ok := p.Pacer.Pop(now)
	if ok && k.Kind == ctrl.KindVideo && k.Class != ctrl.ClassRTX && k.Class != ctrl.ClassFEC && k.Rendition == ctrl.Native && k.Last {
		p.departures[k.Frame] = k
		p.at[k.Frame] = now
	}
	return k, ok
}

func TestPhaseWireSizedLargeDeltaCatchupAndCollapse(t *testing.T) {
	for _, collapse := range []bool{false, true} {
		s := newSim(1)
		r := &room{sim: s, sc: &Scenario{}, keys: ctrl.NewRoomKeys(), noRecovery: true}
		r.src[0], r.src[1] = &Source{prof: Detailed}, &Source{prof: LowProfile}
		v := &viewer{room: r, id: 0, joined: false, pending: map[uint64]*outPkt{}, fwd: newLink(s, 1, LinkConfig{}), rev: newLink(s, 2, LinkConfig{})}
		v.cache[0], v.cache[1], v.audioCache = map[int64]*cacheEntry{}, map[int64]*cacheEntry{}, map[int64]*cacheEntry{}
		v.ctl = ctrl.NewViewer(0, r.keys)
		allocation := &phaseAllocation{desired: ctrl.Option{Rendition: ctrl.Low, TID: 2}}
		est := &ctrl.FixedEstimator{Rate: 1_500_000}
		v.ctl.Alloc, v.ctl.Est, v.ctl.Probe = allocation, est, ctrl.NoProber{}
		pacer := &phasePacer{Pacer: v.ctl.Pacer, departures: map[uint64]ctrl.Packet{}, at: map[uint64]time.Time{}}
		v.ctl.Pacer = pacer
		pacer.SetRelease(v.release)
		r.viewers = []*viewer{v}
		v.join()
		offer := func(f *Frame) {
			for i := 0; i < f.Packets; i++ {
				r.ingress(f, i)
			}
		}
		lowNum := uint64(1)
		low := &Frame{Rend: ctrl.Low, Num: lowNum, Key: true, Flexible: true, Size: 1000, Packets: 2}
		offer(low)
		s.Every(66*ms, 66*ms, func() {
			lowNum++
			next := &Frame{Rend: ctrl.Low, Num: lowNum, Flexible: true, Ref: low, Capture: s.Now(), Size: 300, Packets: 1}
			low = next
			offer(next)
		})
		key := &Frame{Rend: ctrl.Native, Num: 1, Key: true, Flexible: true, Capture: 100 * ms, Size: 236400, Packets: 200}
		delta := &Frame{Rend: ctrl.Native, Num: 2, Flexible: true, Ref: key, Capture: 166 * ms, Size: 152316, Packets: 130}
		if key.Wire() != 248400 || delta.Wire() != 160116 {
			t.Fatal("phase wire units changed")
		}
		s.At(100*ms, func() { allocation.desired = ctrl.Option{Rendition: ctrl.Native, TID: 0}; v.step(); offer(key) })
		s.At(166*ms, func() { offer(delta) })
		if collapse {
			s.At(1500*ms, func() { est.Rate = 128000; v.step() })
			s.At(3100*ms, func() {
				due := r.keys.Due(s.Time())
				if len(due) != 1 || due[0] != ctrl.Low {
					t.Fatalf("missing budgeted low recovery: %v", due)
				}
				lowNum++
				low = &Frame{Rend: ctrl.Low, Num: lowNum, Key: true, Flexible: true, Capture: s.Now(), Size: 1000, Packets: 2}
				offer(low)
			})
		} else {
			s.At(2100*ms, func() {
				offer(&Frame{Rend: ctrl.Native, Num: 3, Flexible: true, Ref: delta, Capture: s.Now(), Size: 1000, Packets: 1})
			})
		}
		s.Run(4 * time.Second)
		if b, n := pacer.Queued(); b > ctrl.QueueBytes || n > ctrl.QueueFrames {
			t.Fatal("phase exceeded queue bounds")
		}
		if collapse {
			if v.ctl.Applied().Rendition != ctrl.Low || v.Drops == 0 {
				t.Fatal("uncatchable chain did not recover low")
			}
			if _, ok := pacer.departures[2]; ok {
				t.Fatal("collapsed old base escaped its finite reference lease")
			}
		} else {
			dep, ok := pacer.departures[2]
			if !ok || dep.FreshUntil != epoch.Add(delta.Capture+pubDelay+ctrl.FrameDeadline) || !pacer.at[2].After(dep.FreshUntil) {
				t.Fatal("old reference was missing or relabelled fresh")
			}
			fresh := false
			for _, f := range v.rx.Timeline {
				if f.Rend == ctrl.Native && f.Capture == 2100*ms && f.G2G() <= time.Second {
					fresh = true
				}
			}
			if !fresh || v.rx.deadComplete() != 0 {
				t.Fatal("necessary old reference failed to support fresh dependency-complete output")
			}
		}
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
