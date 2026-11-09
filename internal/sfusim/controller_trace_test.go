package sfusim

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// TestControllerTrace is diagnostic only: it logs one seed of one scenario
// with component-level events, never a gate rerun or a basis for selecting a
// browser pin.
//
//	SFUSIM_TRACE=<scenario> [SFUSIM_SEED=n] [SFUSIM_FROM=s SFUSIM_TO=s] [SFUSIM_VIEWER=i]
func TestControllerTrace(t *testing.T) {
	name := os.Getenv("SFUSIM_TRACE")
	if name == "" {
		t.Skip("set SFUSIM_TRACE to a scenario name")
	}
	seed := uint64(1)
	if s := os.Getenv("SFUSIM_SEED"); s != "" {
		n, _ := strconv.Atoi(s)
		seed = uint64(n)
	}
	from, to := envSeconds("SFUSIM_FROM", 0), envSeconds("SFUSIM_TO", time.Hour)
	only := -1
	if s := os.Getenv("SFUSIM_VIEWER"); s != "" {
		only, _ = strconv.Atoi(s)
	}
	var lines []string
	logf := func(at time.Duration, v int, format string, args ...any) {
		if at < from || at > to || only >= 0 && v != only && v >= 0 {
			return
		}
		lines = append(lines, fmt.Sprintf("%9.3f v%-2d %s", at.Seconds(), v, fmt.Sprintf(format, args...)))
	}
	tracef = logf
	var clock func() time.Duration
	traceSim = func(s *Sim) { clock = s.Now }
	defer func() { traceSim = nil }()
	defer func() { tracef = nil }()
	wrap := func(id int, v *ctrl.Viewer) *ctrl.Viewer {
		v.Est = &traceEst{Estimator: v.Est, log: logf, id: id}
		v.Probe = &traceProbe{Prober: v.Probe, log: logf, id: id}
		v.Alloc = &traceAlloc{Allocator: v.Alloc, log: logf, id: id}
		v.Cont = &traceCont{Continuity: v.Cont, log: logf, id: id, now: func() time.Duration { return clock() }}
		return v
	}
	c := Controller{Name: "trace",
		Keys:   func() ctrl.Keys { return &traceKeys{ctrl.NewRoomKeys(), logf} },
		Viewer: func(id int, k ctrl.Keys) *ctrl.Viewer { return wrap(id, ctrl.NewViewer(id, k)) },
		Replace: func(id int, k ctrl.Keys, s ctrl.RateState) *ctrl.Viewer {
			return wrap(id%1000, ctrl.NewViewerFrom(id, k, s))
		}}
	for _, p := range Properties() {
		sc := p.Build(false)
		if sc.Name != name {
			continue
		}
		r := Run(sc, seed, c)
		for i, v := range r.Viewers {
			if only >= 0 && i != only {
				continue
			}
			prev := v.Join
			for _, f := range v.Timeline {
				if f.At-prev > 300*time.Millisecond {
					logf(f.At, i, "RENDER GAP %v (frame %v tid %d)", f.At-prev, f.Rend, f.TID)
				}
				prev = f.At
			}
			for _, s := range v.Samples {
				logf(s.At, i, "sample desired %v/%d fwd %v/%d dec %v native %v R %d video %d pad %d", s.Desired.Rendition, s.Desired.TID, s.Forwarded, s.TID, s.Decoder, s.Native, s.Rate, s.WireVideo, s.Padding)
			}
		}
		for _, k := range r.Keys {
			logf(k.At, -1, "KEY assembled %v %dB", k.Rend, k.Bytes)
		}
		sortLines(lines)
		t.Log("\n" + strings.Join(lines, "\n"))
		if p.Check != nil {
			var control *Result
			if p.Control != nil {
				control = Run(p.Control(false), seed, Default)
			}
			ok := p.Check(r, control)
			t.Logf("verdict %v: %s", ok.Pass, ok.Detail)
		}
		return
	}
	t.Fatal("scenario not found")
}

func envSeconds(k string, d time.Duration) time.Duration {
	if s := os.Getenv(k); s != "" {
		f, _ := strconv.ParseFloat(s, 64)
		return time.Duration(f * float64(time.Second))
	}
	return d
}

func sortLines(l []string) {
	// Stable by timestamp prefix; lines are produced roughly in order.
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && l[j][:9] < l[j-1][:9]; j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

type traceFn = func(time.Duration, int, string, ...any)

func at(now time.Time) time.Duration { return now.Sub(epoch) }

type traceKeys struct {
	ctrl.Keys
	log traceFn
}

func (k *traceKeys) Request(now time.Time, v int, r ctrl.Rendition, c ctrl.Cause) ctrl.KeyStatus {
	s := k.Keys.Request(now, v, r, c)
	k.log(at(now), v, "keys.Request %v cause %d -> %d", r, c, s)
	return s
}
func (k *traceKeys) Replace(now time.Time, v, session int) ctrl.KeyStatus {
	s := k.Keys.Replace(now, v, session)
	k.log(at(now), v, "keys.Replace session %d -> %d", session, s)
	return s
}
func (k *traceKeys) Exceptions() []ctrl.Exception {
	if e, ok := k.Keys.(interface{ Exceptions() []ctrl.Exception }); ok {
		return e.Exceptions()
	}
	return nil
}
func (k *traceKeys) Due(now time.Time) []ctrl.Rendition {
	d := k.Keys.Due(now)
	if len(d) > 0 {
		k.log(at(now), -1, "keys.Due PLI %v", d)
	}
	return d
}

type traceEst struct {
	ctrl.Estimator
	log  traceFn
	id   int
	last ctrl.Estimate
}

func (e *traceEst) Estimate(now time.Time) ctrl.Estimate {
	x := e.Estimator.Estimate(now)
	if x.Overuse != e.last.Overuse || x.Congestive != e.last.Congestive || x.Draining != e.last.Draining || x.Rate < e.last.Rate*9/10 {
		e.log(at(now), e.id, "est R %d→%d proven %d overuse %v congestive %v draining %v (service %d debt %d) loss %.3f run %.1f rtt %v keyRate %d ceiling %d", e.last.Rate, x.Rate, x.Proven, x.Overuse, x.Congestive, x.Draining, x.Service, x.Debt, x.Loss, x.LossRun, x.RTT, x.KeyRate, x.Ceiling)
	}
	e.last = x
	return x
}

type traceProbe struct {
	ctrl.Prober
	log traceFn
	id  int
}

func (p *traceProbe) Maybe(now time.Time, target, media int, e ctrl.Estimate, key bool) (ctrl.Cluster, bool) {
	c, ok := p.Prober.Maybe(now, target, media, e, key)
	if ok {
		p.log(at(now), p.id, "probe start #%d target %d rate %d for %v (R %d proven %d)", c.ID, target, c.Rate, c.Until.Sub(now), e.Rate, e.Proven)
	}
	return c, ok
}
func (p *traceProbe) MaybeKey(now time.Time, target, bytes int, e ctrl.Estimate) (ctrl.Cluster, bool) {
	c, ok := p.Prober.MaybeKey(now, target, bytes, e)
	if ok {
		p.log(at(now), p.id, "key probe start #%d target %d rate %d for %v (%d media bytes, R %d proven %d)", c.ID, target, c.Rate, c.Until.Sub(now), bytes, e.Rate, e.Proven)
	}
	return c, ok
}
func (p *traceProbe) OnFeedback(now time.Time, f ctrl.Feedback) (ctrl.ProbeResult, bool) {
	r, ok := p.Prober.OnFeedback(now, f)
	if ok {
		p.log(at(now), p.id, "probe result %+v", r)
	}
	return r, ok
}
func (p *traceProbe) Tick(now time.Time) (ctrl.ProbeResult, bool) {
	r, ok := p.Prober.Tick(now)
	if ok {
		p.log(at(now), p.id, "probe result (tick) %+v", r)
	}
	return r, ok
}

type traceAlloc struct {
	ctrl.Allocator
	log  traceFn
	id   int
	last ctrl.Allocation
}

func (a *traceAlloc) Decide(now time.Time, in ctrl.AllocInput) ctrl.Allocation {
	x := a.Allocator.Decide(now, in)
	if x.Desired != a.last.Desired || x.Candidate != a.last.Candidate || x.Untenable != a.last.Untenable {
		a.log(at(now), a.id, "alloc desired %v/%d candidate %v/%d target %d untenable %v (applied %v/%d R %d proven %d key %v recovering %v)", x.Desired.Rendition, x.Desired.TID, x.Candidate.Rendition, x.Candidate.TID, x.Target, x.Untenable, in.Applied.Rendition, in.Applied.TID, in.Estimate.Rate, in.Estimate.Proven, in.NativeKey, in.Recovering)
	}
	a.last = x
	return x
}
func (a *traceAlloc) OnApplied(now time.Time, o ctrl.Option) {
	a.log(at(now), a.id, "applied %v/%d", o.Rendition, o.TID)
	a.Allocator.OnApplied(now, o)
}

type traceCont struct {
	ctrl.Continuity
	log traceFn
	id  int
	now func() time.Duration
}

func (c *traceCont) Allow(f ctrl.FrameInfo, maxTID uint8) bool {
	ok := c.Continuity.Allow(f, maxTID)
	if !ok && f.TID <= maxTID {
		c.log(c.now(), c.id, "cont refuses %v #%d tid %d key %v refs %v", f.Rendition, f.Picture, f.TID, f.Key, f.Refs)
	}
	return ok
}
func (c *traceCont) Lost(f ctrl.FrameInfo) {
	c.log(c.now(), c.id, "cont lost %v #%d tid %d key %v", f.Rendition, f.Picture, f.TID, f.Key)
	c.Continuity.Lost(f)
}
