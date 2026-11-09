package ctrl

import (
	"encoding/json"
	"testing"
	"time"
)

// Owning real-default component regression promoted from the independent
// approach counterexample. Costs/arrivals are reconstructed, not browser replay.
func switchServiceCosts() Costs {
	return Costs{Rate: [2][3]int{{1_000_000, 1_600_000, 2_000_000}, {50_000, 70_000, 100_000}},
		KeyBytes: [2]int{248_400, 9_000}, KeySamples: [2]int{8, 8}}
}

func switchServiceProof(t *testing.T, v *Viewer, start time.Time, target, service int, missing bool) time.Time {
	t.Helper()
	p := v.Probe.(*Discovery)
	c, ok := p.Maybe(start, target, 0, v.Est.Estimate(start), false)
	if !ok {
		t.Fatal("expected real Discovery cluster")
	}
	_ = c
	return driveSwitchService(t, v, start, service, missing)
}

func driveSwitchService(t *testing.T, v *Viewer, start time.Time, service int, missing bool) time.Time {
	t.Helper()
	p := v.Probe.(*Discovery)
	if p.cluster == nil {
		t.Fatal("missing service cluster")
	}
	c := *p.cluster
	q := &Queue{padding: p.AllowPadding}
	q.SetRate(start, v.Est.Estimate(start).Rate)
	q.SetCluster(&c)
	var feedback Feedback
	var lastArrival time.Time
	seq := uint64(1000 * c.ID)
	for at := start; at.Before(c.Until); {
		for {
			k, ok := q.Pop(at)
			if !ok {
				break
			}
			s := Sent{Seq: seq, At: at, Size: k.Size, Kind: k.Kind, Class: k.Class, Cluster: c.ID, Generation: v.Generation()}
			seq++
			v.OnSent(s)
			arrival := at.Add(20 * time.Millisecond)
			if service > 0 {
				arrival = later(arrival, lastArrival).Add(time.Duration(k.Size*8) * time.Second / time.Duration(service))
			}
			lastArrival = arrival
			feedback.Arrivals = append(feedback.Arrivals, Arrival{Seq: s.Seq, At: arrival})
		}
		next := q.NextAt(at)
		if next.IsZero() || !next.Before(c.Until) {
			break
		}
		if !next.After(at) {
			t.Fatal("nonadvancing queue")
		}
		at = next
	}
	now := later(c.Until.Add(OveruseTime), lastArrival.Add(20*time.Millisecond))
	if missing {
		now = c.Until.Add(ProbeTimeout)
		// Feedback for this synthetic cluster is wholly missing. The real
		// timeout path must classify this as measurement failure, not a ceiling.
		if r, ok := p.Tick(now); ok {
			v.accept(now, r)
		} else {
			t.Fatal("missing timeout")
		}
	} else {
		v.OnFeedback(now, feedback)
	}
	if p.cluster != nil {
		t.Fatal("cluster failed to finalize")
	}
	if v.result == nil {
		t.Fatal("missing real probe result")
	}
	b, _ := json.Marshal(map[string]any{"event": "cluster", "result": v.result, "estimate": v.Est.Estimate(now)})
	t.Log(string(b))
	return now
}

func switchServiceViewer(t *testing.T) (*Viewer, time.Time) {
	t.Helper()
	v := NewViewer(1, NewRoomKeys())
	// Reconstructed state: an established low/T2 decoder, with the startup
	// low key already delivered. This does not force any native decision.
	v.started = true
	v.OnApplied(epoch, Option{Low, 2})
	now := switchServiceProof(t, v, epoch.Add(time.Second), 1_400_000, 0, false)
	return v, now
}

func TestSwitchPreparationRealDefaultOutcomes(t *testing.T) {
	for _, arm := range []string{"timeout-noncongestive-loss", "clean-key-rescue", "saturated-capacity", "credit-exhausted"} {
		t.Run(arm, func(t *testing.T) {
			v, now := switchServiceViewer(t)
			costs := switchServiceCosts()
			p := v.Probe.(*Discovery)
			if arm == "credit-exhausted" {
				p.bootstrap = BootstrapPadding
			}
			d := v.Step(now, costs)
			if arm == "credit-exhausted" {
				if d.Desired != (Option{Native, 0}) || v.SwitchOutcome() != SwitchUnmeasured || d.Cluster != nil {
					t.Fatalf("credit became capacity or starvation: %+v outcome%v", d, v.SwitchOutcome())
				}
				return
			}
			if d.Desired.Rendition != Low || d.Requested || v.SwitchOutcome() != SwitchMeasuring || p.cluster == nil {
				t.Fatalf("unresolved preparation interrupted low: %+v outcome%v", d, v.SwitchOutcome())
			}
			cluster := p.cluster.ID
			if check := v.Step(now.Add(time.Millisecond), costs); check.Desired.Rendition != Low || check.Requested || p.cluster.ID != cluster {
				t.Fatal("measurement reset or low lost")
			}
			start := now
			service := 0
			if arm == "saturated-capacity" {
				service = 1_500_000
			}
			now = driveSwitchService(t, v, start, service, arm != "saturated-capacity")
			if arm == "timeout-noncongestive-loss" {
				var feedback Feedback
				for i := 0; i < 101; i++ {
					at := now.Add(time.Duration(i) * time.Millisecond)
					s := Sent{Seq: uint64(100000 + i), At: at, Size: 100, Kind: KindVideo, Class: ClassTL2}
					v.OnSent(s)
					arrival := at.Add(20 * time.Millisecond)
					if i == 100 {
						arrival = time.Time{}
					}
					feedback.Arrivals = append(feedback.Arrivals, Arrival{Seq: s.Seq, At: arrival})
				}
				now = now.Add(150 * time.Millisecond)
				v.OnFeedback(now, feedback)
			}
			e := v.Est.Estimate(now)
			d = v.Step(now, costs)
			want := SwitchUnmeasured
			if arm == "saturated-capacity" {
				want = SwitchConstrained
			}
			if d.Desired != (Option{Native, 0}) || !d.Requested || v.SwitchOutcome() != want || !v.KeyFits(now, Native, 248400, costs) {
				t.Fatalf("finite constrained/unmeasured fallback lost: %+v estimate%+v outcome%v", d, e, v.SwitchOutcome())
			}
			if want == SwitchUnmeasured && e.Ceiling != 0 {
				t.Fatal("measurement failure relabelled capacity")
			}
			if arm == "timeout-noncongestive-loss" {
				if e.Loss <= 0 || e.Overuse || e.Congestive || e.Draining {
					t.Fatalf("not the owning noncongestive-loss route: %+v", e)
				}
				if _, ok := p.MaybeKey(now, 6_000_000, 248400, e); ok {
					t.Fatal("recent loss bypassed key-probe guards")
				}
			}
			if arm == "clean-key-rescue" {
				serviceFrame(v.Pacer.(*Queue), now, 1, true, 0, 200, 1242, now.Add(FrameDeadline), now.Add(KeyEnvelope))
				d = v.Step(now, costs)
				if d.Cluster == nil || !d.Cluster.MediaOnly {
					t.Fatalf("clean key-funded rescue lost: %+v", d)
				}
			}
			for i := 1; i < 5; i++ {
				check := v.Step(now.Add(time.Duration(i)*OveruseTime), costs)
				if check.Desired.Rendition != Native || check.Requested || v.SwitchOutcome() != want {
					t.Fatal("fallback reset/re-request loop")
				}
			}
		})
	}
}

func TestPreparationRoundDoesNotRestartWhenItsBoundExpires(t *testing.T) {
	v := NewViewer(1, NewRoomKeys())
	v.Probe = NoProber{}
	v.preparation = switchPreparation{at: epoch, cluster: 8, outcome: SwitchConstrained}
	c := switchServiceCosts()
	e := Estimate{Rate: 1_500_000, Proven: 1_500_000}
	for i := 1; i <= 3; i++ {
		ready, _ := v.prepareSwitch(epoch.Add(time.Duration(i)*ProbeTimeout), Option{Native, 0}, c, 0, e)
		if !ready || v.SwitchOutcome() != SwitchUnmeasured || v.preparation.at != epoch || v.preparation.cluster != 8 {
			t.Fatal("expired bound restarted preparation or starved fallback")
		}
	}
}
