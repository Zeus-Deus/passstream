package sfusim

import (
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// oracleEstimator knows the true capacity. It is a satisfiability check for
// the gates, not a controller design.
type oracleEstimator struct {
	ctrl.FixedEstimator
	id       int
	capacity func(int, time.Duration) int
}

func (e *oracleEstimator) Estimate(now time.Time) ctrl.Estimate {
	return ctrl.Estimate{Rate: int(0.95 * float64(e.capacity(e.id, now.Sub(epoch))))}
}

// oracleKeys coalesces requests, serves recovery with a low key and refuses
// native upgrades to a viewer that asked for recovery within Dwell.
type oracleKeys struct {
	due      [2]bool
	last     [2]time.Time
	recovery map[int]time.Time
}

func (k *oracleKeys) Request(now time.Time, viewer int, r ctrl.Rendition, c ctrl.Cause) ctrl.KeyStatus {
	status := ctrl.KeyAccepted
	if c == ctrl.CauseRecovery || c == ctrl.CauseBroken {
		quiet := now.Sub(k.recovery[viewer]) >= ctrl.RecoveryQuiet
		k.recovery[viewer] = now
		if !quiet {
			return ctrl.KeyIgnored
		}
		r, status = ctrl.Low, ctrl.KeyRedirected
	}
	if now.Sub(k.last[r]) >= 500*time.Millisecond {
		k.due[r], k.last[r] = true, now
	}
	return status
}
func (k *oracleKeys) Allowed(now time.Time, viewer int, _ ctrl.Rendition, c ctrl.Cause) bool {
	t, ok := k.recovery[viewer]
	return c == ctrl.CauseJoin || !ok || now.Sub(t) >= ctrl.RecoveryQuiet
}
func (k *oracleKeys) OnKey(time.Time, ctrl.Rendition, int, time.Time) {}
func (k *oracleKeys) OnDelivery(int, ctrl.KeyEvent)                   {}
func (k *oracleKeys) Size(time.Time, ctrl.Rendition) (int, int)       { return 0, 0 }
func (k *oracleKeys) Leave(int)                                       {}
func (k *oracleKeys) Replace(now time.Time, v, _ int) ctrl.KeyStatus {
	return k.Request(now, v, ctrl.Low, ctrl.CauseJoin)
}
func (k *oracleKeys) Times(ctrl.Rendition) (time.Time, time.Time) { return time.Time{}, time.Time{} }
func (k *oracleKeys) Due(time.Time) []ctrl.Rendition {
	var out []ctrl.Rendition
	for r, d := range k.due {
		if d {
			out = append(out, ctrl.Rendition(r))
		}
	}
	k.due = [2]bool{}
	return out
}

// oracleAllocator picks the best option that fits and keeps recovery on the
// current rendition (no hysteresis needed: the oracle rate is exact).
type oracleAllocator struct{}

func (oracleAllocator) OnApplied(time.Time, ctrl.Option) {}
func (oracleAllocator) OnGeneration(time.Time, uint64)   {}
func (oracleAllocator) Decide(_ time.Time, in ctrl.AllocInput) ctrl.Allocation {
	for _, o := range ctrl.Options {
		native := o.Rendition == ctrl.Native
		if native && (!in.NativeKey || in.Recovering) {
			continue
		}
		if total, _ := ctrl.Admit(in.Costs, o, in.FEC, in.Estimate.Loss); total <= in.Estimate.Rate {
			return forcedAllocation(in, o)
		}
	}
	return forcedAllocation(in, ctrl.Option{Rendition: ctrl.Low})
}

// Oracle is the omniscient reference used by TestOracleSatisfiable.
func Oracle(capacity func(int, time.Duration) int) Controller {
	return Controller{Name: "oracle",
		ResendNACK: func(age time.Duration, sends int) bool { return age < ctrl.FrameDeadline && sends < ctrl.NACKMaxSends },
		Keys:       func() ctrl.Keys { return &oracleKeys{recovery: map[int]time.Time{}} },
		Viewer: func(id int, keys ctrl.Keys) *ctrl.Viewer {
			v := ctrl.NewViewer(id, keys)
			v.Est = &oracleEstimator{id: id, capacity: capacity}
			v.Alloc = oracleAllocator{}
			return v
		},
		// A replacement PC (A19) keeps the viewer's capacity; its ID is
		// index + 1000 × generation.
		Replace: func(id int, keys ctrl.Keys, _ ctrl.RateState) *ctrl.Viewer {
			v := ctrl.NewViewer(id, keys)
			v.Est = &oracleEstimator{id: id % 1000, capacity: capacity}
			v.Alloc = oracleAllocator{}
			return v
		}}
}
