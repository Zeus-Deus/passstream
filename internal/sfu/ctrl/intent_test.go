package ctrl

import (
	"testing"
	"time"
)

func intentPushKey(t *testing.T, q *Queue, now time.Time, r Rendition, id uint64, bytes int) {
	t.Helper()
	count := (bytes + 1259) / 1260
	for i := 0; i < count; i++ {
		size := min(1260, bytes-i*1260)
		if !q.Push(now, Packet{Kind: KindVideo, Class: ClassKey, Rendition: r, Frame: id, Key: true, Size: size, FrameSize: bytes, Packets: count, Last: i == count-1, Deadline: now.Add(KeyEnvelope), FreshUntil: now.Add(FrameDeadline)}) {
			t.Fatal("fixture refused")
		}
	}
}
func intentQuietFixture(t *testing.T) (*Viewer, Costs, time.Time) {
	t.Helper()
	now := epoch.Add(5150 * time.Millisecond)
	keys := NewRoomKeys()
	keys.Request(now.Add(-4*time.Second), 1, Low, CauseJoin)
	keys.Due(now.Add(-4 * time.Second))
	keys.OnKey(now.Add(-4*time.Second), Low, 100, now.Add(-4*time.Second))
	v := NewViewer(1, keys)
	v.started = true
	v.requested = Native
	v.requestAt = now.Add(-50 * time.Millisecond)
	v.Est = &commitEstimate{Estimate{Rate: 1500000, Proven: 1500000, Sustained: 1500000, Ceiling: 1500000}}
	v.preparation = switchPreparation{outcome: SwitchConstrained, choice: &Allocation{Desired: Option{Native, 0}, Intent: Upgrade}, generation: v.Generation()}
	v.OnApplied(now.Add(-4*time.Second), Option{Low, 2})
	q := v.Pacer.(*Queue)
	q.SetRate(now.Add(-4*time.Second), 1500000)
	intentPushKey(t, q, now.Add(-4*time.Second), Low, 1, 100)
	q.Pop(now.Add(-4 * time.Second))
	q.KeyEvents()
	first := now.Add(-2 * time.Millisecond)
	intentPushKey(t, q, first, Native, 73, 264453)
	v.BeforeSend(first, 0, Option{Native, 0}, Costs{})
	if p, ok := q.Pop(first); !ok || p.Last || p.Rendition != Native {
		t.Fatal("no Native FIRST")
	}
	c := Costs{Rate: [2][3]int{{1335818, 1719037, 2362168}, {59044, 82948, 115870}}, KeyBytes: [2]int{617480, 9963}, KeySamples: [2]int{2, 2}, AudioWire: 65202}
	return v, c, now
}
func intentReverseFixture(t *testing.T) (*Viewer, Costs, time.Time) {
	t.Helper()
	now := epoch.Add(6 * time.Second)
	v := NewViewer(1, NewRoomKeys())
	v.started = true
	v.requested = Low
	v.requestAt = now.Add(-300 * time.Millisecond)
	v.Est = &commitEstimate{Estimate{Rate: 128495, Proven: 329928, Sustained: 146477, Congestive: true, Loss: 2.0 / 11, CongestedAt: epoch.Add(5846284375 * time.Nanosecond)}}
	v.OnApplied(epoch.Add(2*time.Second), Option{Native, 2})
	c := Costs{Rate: [2][3]int{{46594, 113024, 219797}, {48373, 69796, 102691}}, KeyBytes: [2]int{9640, 4626}, KeySamples: [2]int{2, 3}, AudioWire: 64550}
	q := v.Pacer.(*Queue)
	q.SetRate(now.Add(-time.Second), 128495)
	intentPushKey(t, q, now.Add(-time.Second), Native, 1, 100)
	q.Pop(now.Add(-time.Second))
	q.KeyEvents()
	intentPushKey(t, q, now.Add(-10*time.Millisecond), Low, 80, 4000)
	auth, _ := q.Key(Low)
	auth.Option, auth.Intent = Option{Low, 0}, Required
	q.Authorize(auth)
	if p, ok := q.Pop(now.Add(-10 * time.Millisecond)); !ok || p.Rendition != Low || p.Last {
		t.Fatal("no Low FIRST")
	}
	// Prime the real ordinary native down timer; this is reconstructed owning
	// history, not a claimed exact replay of every FQ allocator decision.
	a := v.Alloc.(*LayerAllocator)
	e := v.Est.(*commitEstimate).value
	e.Rate = 200000
	a.Decide(now.Add(-time.Second), AllocInput{Applied: v.Applied(), Estimate: e, Costs: c, NativeKey: true})
	a.Decide(now.Add(-600*time.Millisecond), AllocInput{Applied: v.Applied(), Estimate: v.Est.(*commitEstimate).value, Costs: c, NativeKey: true})
	return v, c, now
}

func TestTransactionIntentDirections(t *testing.T) {
	t.Run("quiet-held-native", func(t *testing.T) {
		v, c, now := intentQuietFixture(t)
		old := v.Applied()
		changed := v.Alloc.(*LayerAllocator).changed
		d := v.Step(now, c)
		if d.Desired != (Option{Native, 0}) || d.Intent != Retain || d.Requested {
			t.Fatalf("inherited Low interrupted live Native: %+v", d)
		}
		if v.OnDecoderStall(now) {
			t.Fatal("active decoder request accepted")
		}
		if v.Keys.Allowed(now, v.ID, Native, CauseUpgrade) {
			t.Fatal("ignored report did not block new upgrades")
		}
		if d = v.Step(now, c); d.Intent != Retain || d.Desired.Rendition != Native || d.Requested || !v.recoveryAt.IsZero() {
			t.Fatal("ignored report fabricated required reset")
		}
		for at, steps := now, 0; steps < 1000; steps++ {
			v.BeforeSend(at, 0, d.Desired, c)
			p, ok := v.Pacer.Pop(at)
			if ok && p.Last {
				o, valid := v.CompletionOption(at, p, c)
				if !valid || o != (Option{Native, 0}) {
					t.Fatalf("foreign/unaffordable completion cap: %+v %v", o, valid)
				}
				if v.Applied() != old || v.Alloc.(*LayerAllocator).changed != changed {
					t.Fatal("early Applied/dwell")
				}
				v.OnApplied(at, o)
				if v.Operating() != o || v.Alloc.(*LayerAllocator).changed != at {
					t.Fatal("LAST did not reconcile Operating/Applied/dwell")
				}
				return
			}
			if !ok {
				at = v.Pacer.NextAt(at)
				if at.IsZero() {
					break
				}
			}
		}
		t.Fatal("held Native did not complete under hard lease")
	})
	t.Run("reverse-inherited-native", func(t *testing.T) {
		v, c, now := intentReverseFixture(t)
		changed := v.Alloc.(*LayerAllocator).changed
		d := v.Step(now, c)
		if d.Desired.Rendition != Low || d.Intent != Retain || d.Requested || len(v.Keys.Due(now)) != 0 {
			t.Fatalf("reverse request bypassed eligibility: %+v", d)
		}
		if v.Applied() != (Option{Native, 2}) || v.Alloc.(*LayerAllocator).changed != changed {
			t.Fatal("FIRST rewrote completed dwell")
		}
	})
}

func TestTransactionIntentCapsAtExactCompletion(t *testing.T) {
	for _, arm := range []string{"pending-low-foreign-tid", "newly-admitted-raise", "affordable-reduction"} {
		t.Run(arm, func(t *testing.T) {
			v, c, now := intentQuietFixture(t)
			e := v.Est.(*commitEstimate)
			if arm == "newly-admitted-raise" {
				c.Rate[Native] = [3]int{500000, 800000, 1100000}
				c.KeyBytes[Native], c.KeySamples[Native] = 264453, 8
			}
			if arm == "affordable-reduction" {
				e.value = Estimate{Rate: 5000000, Proven: 5000000, Sustained: 5000000}
				d := v.Step(now, c)
				v.BeforeSend(now, 0, d.Desired, c)
				if v.Operating() != (Option{Native, 2}) {
					t.Fatal("real high proof did not authorize initial T2")
				}
				e.value = Estimate{Rate: 1500000, Proven: 1500000, Sustained: 1500000, Ceiling: 1500000}
			}
			if arm == "pending-low-foreign-tid" {
				e.value.Overuse = true
				e.value.CongestedAt = now
			}
			d := v.Step(now, c)
			want := Option{Native, 0}
			if arm == "newly-admitted-raise" {
				want.TID = 2
			}
			if arm == "pending-low-foreign-tid" {
				if d.Intent != Required || d.Desired.Rendition != Low || !d.Requested {
					t.Fatal("not a genuine pending Low reset")
				}
			} else if d.Desired != want {
				t.Fatalf("wrong current cap: %+v", d)
			}
			for at, steps := now, 0; steps < 1000; steps++ {
				v.BeforeSend(at, 0, d.Desired, c)
				p, ok := v.Pacer.Pop(at)
				if ok && p.Last {
					for _, wrong := range []Packet{
						{Key: true, Last: true, Frame: p.Frame + 1, Rendition: p.Rendition, Generation: p.Generation},
						{Key: true, Last: true, Frame: p.Frame, Rendition: Low, Generation: p.Generation},
						{Key: true, Last: true, Frame: p.Frame, Rendition: p.Rendition, Generation: p.Generation + 1},
					} {
						if _, valid := v.CompletionOption(at, wrong, c); valid {
							t.Fatal("wrong identity borrowed completion cap")
						}
					}
					o, valid := v.CompletionOption(at, p, c)
					if !valid || o != want {
						t.Fatalf("completion cap%s: %+v %v want%+v", arm, o, valid, want)
					}
					v.OnApplied(at, o)
					if v.Operating() != o {
						t.Fatal("temporal completion split Operating/Applied")
					}
					return
				}
				if !ok {
					at = v.Pacer.NextAt(at)
					if at.IsZero() {
						break
					}
				}
			}
			t.Fatal("no bounded key completion")
		})
	}
}

func TestAllocationOperatingBasisAndCompletedTimers(t *testing.T) {
	for _, kind := range []string{"cost-only", "rate-cut", "draining"} {
		t.Run(kind, func(t *testing.T) {
			now := epoch.Add(10 * time.Second)
			o := Option{Native, 0}
			a := &LayerAllocator{}
			a.OnApplied(now.Add(-5*time.Second), o)
			c := Costs{Rate: [2][3]int{{200000, 250000, 300000}, {10000, 20000, 30000}}}
			e := Estimate{Rate: 300000, Proven: 300000, Sustained: 300000}
			if kind == "cost-only" {
				e.Rate = 100000
				c.Rate[Native] = [3]int{90000, 95000, 99000}
			}
			a.Decide(now.Add(-time.Second), AllocInput{Applied: o, Operating: &o, Estimate: e, Costs: c})
			e.Rate = 100000
			c.Rate[Native] = [3]int{200000, 250000, 300000}
			if kind == "draining" {
				e.Draining = true
			}
			refresh := KeyAuthorization{Generation: 0, Rendition: Native, Frame: 8, Option: o, Previous: Native, Intent: Retain, Phase: KeyStarted}
			in := AllocInput{Applied: o, Operating: &o, Transaction: &refresh, Estimate: e, Costs: c}
			start := a.Decide(now, in)
			wait := TrendWindow
			if kind == "rate-cut" {
				wait = DownTime
			}
			if kind == "draining" {
				wait = 0
			}
			if wait > 0 && start.Intent != Retain {
				t.Fatal("candidate guard leaked into ordinary refresh down timer")
			}
			if wait > 0 && a.Decide(now.Add(wait-time.Nanosecond), in).Intent != Retain {
				t.Fatal("completed down timer shortened")
			}
			if got := a.Decide(now.Add(wait), in); got.Intent != Required || got.Desired.Rendition != Low {
				t.Fatalf("lost existing down boundary: %+v", got)
			}
		})
	}
	now := epoch.Add(20 * time.Second)
	a := &LayerAllocator{}
	completed := Option{Native, 2}
	a.OnApplied(now.Add(-5*time.Second), completed)
	changed := a.changed
	c := Costs{Rate: [2][3]int{{1000000, 1400000, 2000000}, {50000, 70000, 100000}}}
	e := Estimate{Rate: 3000000, Proven: 3000000, Sustained: 3000000}
	a.Decide(now.Add(-time.Second), AllocInput{Applied: completed, Operating: &completed, Costs: c, Estimate: e})
	e.Rate = 1500000
	a.Decide(now.Add(-DownTime/2), AllocInput{Applied: completed, Operating: &completed, Costs: c, Estimate: e})
	if a.over.IsZero() || !a.cut {
		t.Fatal("old Native excess was not actually primed")
	}
	low := Option{Low, 0}
	e.CongestedAt = now
	d := a.Decide(now, AllocInput{Applied: completed, Operating: &low, Costs: c, Estimate: e, NativeKey: true})
	if d.Desired.Rendition != Low || d.Intent != Retain || !a.over.IsZero() || a.cut || a.changed != changed {
		t.Fatalf("old Native excess/dwell crossed FIRST: %+v", d)
	}
	if a.upFor != (Option{Low, 1}) || a.up != now {
		t.Fatal("temporal evidence was not scoped to the new option")
	}
	a.OnGeneration(now.Add(time.Millisecond), 1)
	if !a.over.IsZero() || !a.up.IsZero() || a.cut || a.hasBasis || a.changed != changed {
		t.Fatal("generation reset moved completed dwell or kept option evidence")
	}
}

func TestTransactionGenerationAndTerminalFences(t *testing.T) {
	v, c, now := intentQuietFixture(t)
	v.Step(now, c)
	old, _ := v.Pacer.Operating()
	v.OnPathChange(now.Add(time.Millisecond))
	d := v.Step(now.Add(time.Millisecond), c)
	if d.Intent != Required || d.Desired.Rendition != Low || v.Operating().Rendition != Native {
		t.Fatal("path fence fell back to old Applied deltas")
	}
	if v.Pacer.Authorize(old) {
		t.Fatal("old-generation authorization was accepted")
	}
	keys := NewRoomKeys().(*RoomKeys)
	keys.OnDelivery(1, KeyEvent{Rendition: Native, Frame: 73, Phase: KeyAdmitted, Generation: 1})
	for _, phase := range []KeyPhase{KeyCompleted, KeyAborted, KeyCancelled, KeySuperseded} {
		keys.OnDelivery(1, KeyEvent{Rendition: Native, Frame: 73, Phase: phase, Generation: 0})
		if !keys.delivery[1].active {
			t.Fatal("old generation cleared replacement delivery")
		}
	}
	keys.OnDelivery(1, KeyEvent{Rendition: Low, Frame: 73, Phase: KeyCompleted, Generation: 1})
	if !keys.delivery[1].active {
		t.Fatal("other rendition cleared replacement")
	}
	// The controller's delivery cache has the same fence as RoomKeys.
	q := v.Pacer.(*Queue)
	q.event(KeyEvent{Rendition: Low, Frame: 74, Phase: KeyAdmitted, Generation: v.Generation(), At: now})
	q.event(KeyEvent{Rendition: Low, Frame: 74, Phase: KeyAborted, Generation: 0, At: now})
	v.Step(now.Add(2*time.Millisecond), c)
	if !v.delivering[Low] || v.refusedFor != 0 {
		t.Fatal("late old terminal poisoned current delivery/refusal")
	}
}

func TestTransactionAcceptedRecoveryAndCandidateHoldLoss(t *testing.T) {
	for _, arm := range []string{"broken", "hold-excess", "overuse"} {
		t.Run(arm, func(t *testing.T) {
			v, c, now := intentQuietFixture(t)
			v.Step(now, c)
			if arm == "broken" {
				if !v.OnBroken(now, Native) {
					t.Fatal("genuine broken recovery not accepted")
				}
			} else if arm == "hold-excess" {
				c.Rate[Native][0] = 1600000
			} else {
				v.Est.(*commitEstimate).value.Overuse = true
			}
			d := v.Step(now, c)
			if d.Intent != Required || d.Desired.Rendition != Low {
				t.Fatalf("real degradation suppressed: %+v", d)
			}
		})
	}
}
