package sfusim

import (
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Regressions for C2 item 6b, "estimator, admission, repair"
// (docs/REPORT.md).

// probeLog wraps a viewer's prober and calls at for every cluster it starts.
type probeLog struct {
	ctrl.Prober
	at func(now time.Time, target int)
}

func (p *probeLog) Maybe(now time.Time, target, media int, e ctrl.Estimate, key bool) (ctrl.Cluster, bool) {
	c, ok := p.Prober.Maybe(now, target, media, e, key)
	if ok {
		p.at(now, target)
	}
	return c, ok
}
func (p *probeLog) MaybeKey(now time.Time, target, bytes int, e ctrl.Estimate) (ctrl.Cluster, bool) {
	c, ok := p.Prober.MaybeKey(now, target, bytes, e)
	if ok {
		p.at(now, target)
	}
	return c, ok
}

// withProbeLog is Default with every viewer's (and replacement's) prober
// wrapped by probeLog.
func withProbeLog(at func(now time.Time, target int)) Controller {
	c := Default
	c.Viewer = func(id int, k ctrl.Keys) *ctrl.Viewer {
		v := ctrl.NewViewer(id, k)
		v.Probe = &probeLog{v.Probe, at}
		return v
	}
	c.Replace = func(id int, k ctrl.Keys, s ctrl.RateState) *ctrl.Viewer {
		v := ctrl.NewViewerFrom(id, k, s)
		v.Probe = &probeLog{v.Probe, at}
		return v
	}
	return c
}

// C2 §5 A5: after the 5 Mbit/s → 256 kbit/s cliff of b1_flap, no upward
// experiment may start while the bottleneck still holds the debt the cliff
// left (the saved trace probed native at 55.7 s into a ≈3 s queue, and the
// replacement PC probed again at 54.5 s and 57.6 s).
func TestNoProbeIntoUnresolvedDebt(t *testing.T) {
	defer func() { traceLinks = nil }()
	p := b1Flap()
	for seed := uint64(1); seed <= 20; seed++ {
		var links []*Link
		traceLinks = func(l []*Link) { links = l }
		c := withProbeLog(func(now time.Time, target int) {
			if d := links[0].QueueDelay(); d > ctrl.OveruseTime {
				t.Errorf("seed %d: probe for %d bit/s at %v into a %v bottleneck queue", seed, target, now.Sub(epoch), d.Round(time.Millisecond))
			}
		})
		Run(p.Build(false), seed, c)
	}
}
