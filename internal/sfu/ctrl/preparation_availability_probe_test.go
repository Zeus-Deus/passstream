package ctrl

import (
	"os"
	"testing"
	"time"
)

// Opt-in before counterexample, not a normal passing regression or policy fix.
// All components remain NewViewer defaults. A real missing-feedback experiment
// establishes the existing backoff; only the exhausted-credit control supplies
// a reconstructed fully-spent bootstrap state.
func TestPreparationAvailabilityCounterexample(t *testing.T) {
	if os.Getenv("SFU_PREPARATION_AVAILABILITY_PROBE") != "1" {
		t.Skip("set SFU_PREPARATION_AVAILABILITY_PROBE=1 for the retained preparation counterexample")
	}
	for _, arm := range []string{"temporary-backoff-with-credit", "credit-exhausted", "backoff-beyond-round"} {
		t.Run(arm, func(t *testing.T) {
			v, now := switchServiceViewer(t)
			p := v.Probe.(*Discovery)
			// An earlier ordinary experiment (not this switch's preparation)
			// timed out. It owns its existing backoff, never a capacity ceiling.
			if _, ok := p.Maybe(now, 1_600_000, 0, v.Est.Estimate(now), false); !ok {
				t.Fatal("unrelated prior experiment unavailable")
			}
			now = driveSwitchService(t, v, now, 0, true)
			at := p.next.Add(-200 * time.Millisecond)
			if arm == "backoff-beyond-round" {
				at = p.next.Add(-ProbeTimeout - time.Nanosecond)
			}
			if arm == "credit-exhausted" {
				p.bootstrap = BootstrapPadding
			}
			e := v.Est.Estimate(at)
			budget, next := p.budget(at), p.next
			if p.Ready(at) || e.Ceiling != 0 || e.Overuse || e.Congestive || e.Draining || p.cluster != nil || v.probing {
				t.Fatalf("not an idle backoff/no-capacity-certificate fixture: %+v", e)
			}
			costs := switchServiceCosts()
			d := v.Step(at, costs)
			t.Logf("arm=%s ready=false credit=%d minimum=%d backoff=%s untilReady=%s withinProbeTimeout=%v proof=%d rate=%d ceiling=%d desired=%+v outcome=%v requested=%v", arm, budget, ProbeMinPackets*BurstFloor, p.backoff, next.Sub(at), !next.After(at.Add(ProbeTimeout)), e.Proven, e.Rate, e.Ceiling, d.Desired, v.SwitchOutcome(), d.Requested)
			if arm != "temporary-backoff-with-credit" {
				if d.Desired.Rendition != Native || v.SwitchOutcome() != SwitchUnmeasured || !d.Requested || d.Cluster != nil {
					t.Fatal("definitely unavailable/budget-limited preparation lost finite honest fallback")
				}
				return
			}
			if budget < ProbeMinPackets*BurstFloor || next.After(at.Add(ProbeTimeout)) {
				t.Fatal("temporary-backoff control cannot fund a round inside the existing bound")
			}
			// Independently show the actual Discovery can start at its existing
			// next time, with the same generation/credit/proof and bounded offer.
			pe := v.Est.Estimate(next)
			pe.Ceiling = max(pe.Rate, pe.Proven, pe.KeyRate)
			target := Hold(costs, Option{Native, 0}, 0, pe.Loss) + KeyBits(costs.KeyBytes[Native], 0)*int(time.Second)/int(KeyFast)
			cl, ok := p.Maybe(next, target, costs.Rate[Low][2], pe, false)
			t.Logf("same-generation actual Discovery at+%s: ready/start=%v rate=%d existingProofOverdriveBound=%d remainingCredit=%d", next.Sub(at), ok, cl.Rate, int(defaultConfig().ProbeMaxOverdrive*float64(pe.Ceiling)), p.budget(next))
			if !ok || cl.Rate > int(defaultConfig().ProbeMaxOverdrive*float64(pe.Ceiling)) {
				t.Fatal("future affordable experiment control failed")
			}
			if d.Desired.Rendition != Low || d.Requested || v.SwitchOutcome() != SwitchMeasuring {
				t.Error("temporary backoff with usable credit prematurely spends native fallback instead of retaining low during the bounded unresolved preparation")
			}
		})
	}
}
