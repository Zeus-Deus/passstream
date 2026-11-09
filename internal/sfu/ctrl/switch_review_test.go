package ctrl

import (
	"testing"
	"time"
)

// Both known wire keys fit the same demonstrated service. A p75 cost
// update at assembly must not throw away the already funded actual key.
func TestPreparedKeySurvivesAChangedServiceObjective(t *testing.T) {
	v := NewViewer(1, NewRoomKeys())
	v.started = true
	v.OnApplied(epoch, Option{Low, 2})
	now := switchServiceProof(t, v, epoch.Add(time.Second), 400000, 0, false)
	c := Costs{Rate: [2][3]int{{120000, 160000, 200000}, {50000, 70000, 100000}}, KeyBytes: [2]int{10000, 9000}, KeySamples: [2]int{8, 8}}
	d := v.Step(now, c)
	if d.Desired != (Option{Native, 2}) || !d.Requested {
		t.Fatalf("setup: %+v", d)
	}
	for i := 0; i < 13; i++ {
		if !v.Pacer.Push(now, Packet{ID: uint64(i + 1), Kind: KindVideo, Class: ClassKey, Rendition: Native, Frame: 7, Key: true, Size: 1000, FrameSize: 13000, Packets: 13, Last: i == 12, Deadline: now.Add(KeyEnvelope)}) {
			t.Fatal("queue setup")
		}
	}
	c.KeyBytes[Native] = 13000 // legal p75 shift: 6x10k+2x13k -> 5x10k+3x13k
	if !v.KeyFits(now, Native, 13000, c) {
		t.Fatal("actual key must fit")
	}
	// Execute a private copy through actual Pop/NextAt before the policy call.
	live := v.Pacer.(*Queue)
	real := &Queue{rate: live.rate, tokens: live.tokens, at: live.at, bytes: live.bytes, originals: map[frameID]*queuedFrame{}}
	for _, f := range live.frames {
		copy := *f
		real.frames = append(real.frames, &copy)
		if live.originals[f.id] == f {
			real.originals[f.id] = &copy
		}
	}
	for at := now; ; {
		k, ok := real.Pop(at)
		if ok && k.Key && k.Last {
			t.Logf("actual full key service=%s", at.Sub(now))
			if at.Sub(now) > KeyFast {
				t.Fatal("counterexample key is not fast")
			}
			break
		}
		if !ok {
			at = real.NextAt(at)
			if at.IsZero() {
				t.Fatal("no actual service")
			}
		}
	}
	d = v.Step(now, c)
	left, total := v.Pacer.QueuedKey(Native)
	t.Logf("rate/proof=%+v actualKey=%d queued=%d total=%d decision=%+v events=%+v", v.Est.Estimate(now), 13000, left, total, d, v.Pacer.KeyEvents())
	if left == 0 {
		t.Fatal("serviceable unstarted key cancelled solely because fast-key objective grew at assembly")
	}
	if d.Cluster == nil || !d.Cluster.MediaOnly {
		t.Fatal("retained actual key lost its clean funded experiment")
	}
	v.BeforeSend(now, 0, d.Desired, c)
	finished := false
	for at, steps := now, 0; steps < 10000; steps++ {
		p, ok := v.Pacer.Pop(at)
		if ok && p.Key && p.Last {
			finished = true
			if at.Sub(now) > KeyFast {
				t.Fatal("actual paced commitment missed existing fast objective")
			}
			break
		}
		if !ok {
			at = v.Pacer.NextAt(at)
			if at.IsZero() {
				break
			}
		}
	}
	if !finished {
		t.Fatal("retained key did not actually depart")
	}
}

type commitEstimate struct{ value Estimate }

func (e *commitEstimate) OnSent(Sent)                        {}
func (e *commitEstimate) OnFeedback(time.Time, Feedback)     {}
func (e *commitEstimate) OnReport(time.Time, ReceiverReport) {}
func (e *commitEstimate) OnProbe(time.Time, ProbeResult)     {}
func (e *commitEstimate) OnGeneration(time.Time, uint64)     {}
func (e *commitEstimate) Estimate(time.Time) Estimate        { return e.value }
func (e *commitEstimate) Clamp(int)                          {}

func TestCommitRefusalChargesBackoffOnce(t *testing.T) {
	for _, keyRate := range []int{0, 5_000_000} {
		t.Run(map[bool]string{false: "forecast-broken", true: "actual-key-refusal"}[keyRate > 0], func(t *testing.T) {
			v := NewViewer(1, NewRoomKeys())
			v.started = true
			v.OnApplied(epoch, Option{Low, 2})
			v.Probe = NoProber{}
			e := &commitEstimate{Estimate{Rate: 5_000_000, Proven: 5_000_000, KeyRate: keyRate}}
			v.Est = e
			c := Costs{Rate: [2][3]int{{1_000_000, 1_600_000, 2_000_000}, {50_000, 70_000, 100_000}}, KeyBytes: [2]int{10000, 9000}, KeySamples: [2]int{8, 8}}
			now := epoch.Add(time.Second)
			v.Step(now, c)
			if !v.KeyFits(now, Native, 1_000_000, c) {
				t.Fatal("assembly does not fit")
			}
			for i := 0; i < 1000; i++ {
				if !v.Pacer.Push(now, Packet{ID: uint64(i + 1), Kind: KindVideo, Class: ClassKey, Rendition: Native, Frame: 7, Key: true, Size: 1000, FrameSize: 1_000_000, Packets: 1000, Last: i == 999, Deadline: now.Add(KeyEnvelope)}) {
					t.Fatal("queue setup")
				}
			}
			// Valid fast-key proof still paces a key at 5M while ordinary R is 3M.
			// KeyFits intentionally remains conservative against R, as before.
			e.value.Rate, e.value.Proven = 3_000_000, 3_000_000
			d := v.Step(now, c)
			service := v.PictureService(now, PictureQuery{Rendition: Native, Frame: 7, Generation: v.Generation(), Commit: true})
			if !service.Known || service.Broken != (keyRate == 0) {
				t.Fatal("refusal branch fixture did not select the intended real service outcome")
			}
			v.BeforeSend(now, 0, d.Desired, c)
			t.Logf("one commit refusal backoff=%s refusedUntil=%s", v.refusedFor, v.refusedUntil.Sub(now))
			if v.refusedFor != KeyRefusedMin {
				t.Fatalf("one actual refusal charged %s; want unchanged first refusal %s", v.refusedFor, KeyRefusedMin)
			}
			// Re-entering the writer with no new candidate is the same event.
			for i := 0; i < 3; i++ {
				v.BeforeSend(now, 0, d.Desired, c)
			}
			if v.refusedFor != KeyRefusedMin {
				t.Fatal("cancelled candidate charged again")
			}
			// A distinct later failed assembly advances the existing ladder once.
			now = v.refusedUntil
			if v.KeyFits(now, Native, 1_000_000, c) || v.refusedFor != 2*KeyRefusedMin {
				t.Fatal("next real failed event lost the existing refusal sequence")
			}
			v.BeforeSend(now, 0, d.Desired, c)
			if v.refusedFor != 2*KeyRefusedMin {
				t.Fatal("empty commitment doubled an assembly refusal")
			}
			// A genuinely fitting key retains the existing successful-reset rule.
			e.value.Rate = 5_000_000
			if !v.KeyFits(now, Native, 1_000_000, c) || v.refusedFor != 0 {
				t.Fatal("successful actual key did not reset refusal")
			}
		})
	}
}

func TestSwitchPreparationUsesTheExistingBoundedExperimentStep(t *testing.T) {
	v, now := switchServiceViewer(t)
	e := v.Est.Estimate(now)
	d := v.Step(now, switchServiceCosts())
	if d.Cluster == nil || d.Cluster.MediaOnly {
		t.Fatal("no padding service preparation")
	}
	bound := int(v.Cfg.ProbeMaxOverdrive * float64(max(e.Rate, e.Proven, e.KeyRate)))
	if d.Cluster.Rate > bound {
		t.Fatalf("new service objective bypassed existing proof-bounded experiment step: offer%d proof%d bound%d", d.Cluster.Rate, e.Proven, bound)
	}
}

func TestLowerNativeOptionPreparationDoesNotDependOnT2Cost(t *testing.T) {
	v := NewViewer(1, NewRoomKeys())
	v.started = true
	v.OnApplied(epoch, Option{Low, 2})
	now := switchServiceProof(t, v, epoch.Add(time.Second), 400000, 0, false)
	e := v.Est.Estimate(now)
	c := Costs{Rate: [2][3]int{{120000, 400000, 1000000}, {50000, 70000, 100000}}, AudioWire: 65000, KeyBytes: [2]int{50000, 9000}, KeySamples: [2]int{8, 8}}
	top, _ := Admit(c, Option{Native, 2}, 0, 0)
	goal := Hold(c, Option{Native, 0}, 0, 0) + KeyBits(50000, 0)*int(time.Second)/int(KeyFast)
	if goal >= top {
		t.Fatal("fixture must exercise a T0 service goal below T2 admission")
	}
	d := v.Step(now, c)
	if d.Cluster == nil || d.Cluster.Rate > int(v.Cfg.ProbeMaxOverdrive*float64(max(e.Rate, e.Proven, e.KeyRate))) {
		t.Fatalf("T2 cost bypassed T0 preparation bound: %+v", d)
	}
}
