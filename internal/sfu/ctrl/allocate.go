package ctrl

import "time"

// LayerAllocator implements docs/CONTROLLER.md "Allocator". Dwell and down
// rules use the applied option; initial low acquisition is not a rendition
// change (F-5). A1, A4, A5 and A18 cover these clocks.
type LayerAllocator struct {
	acquired      bool // the first applied option was reported
	lastRendition Rendition
	changed       time.Time // last applied rendition change after acquisition
	over          time.Time // hold(applied) > R since
	cut           bool      // R fell during (or into) the current excess
	rate          int       // R at the previous decision
	up            time.Time // admit(next TID) ≤ min(R, Proven) since
	upFor         Option    // the option up refers to
	low           time.Time // hold(low T0) > R since
	basis         Option    // option-specific excess/up evidence (D1/D2 basis controls)
	hasBasis      bool
}

func (a *LayerAllocator) OnApplied(now time.Time, o Option) {
	if a.acquired && a.lastRendition != o.Rendition {
		a.changed = now
	}
	a.acquired, a.lastRendition = true, o.Rendition
}

func (a *LayerAllocator) OnGeneration(time.Time, uint64) {
	a.up, a.over = time.Time{}, time.Time{}
	a.cut = false
	a.rate = 0
	a.upFor = Option{}
	a.hasBasis = false
}

// ladder is discovery order, cheapest first: fewer frames before fewer pixels
// is the preference order read backwards.
var ladder = [6]Option{{Low, 0}, {Low, 1}, {Low, 2}, {Native, 0}, {Native, 1}, {Native, 2}}

func step(o Option) int {
	if o.Rendition == Native {
		return 3 + int(o.TID)
	}
	return int(o.TID)
}

func since(t *time.Time, now time.Time, cond bool) time.Duration {
	if !cond {
		*t = time.Time{}
		return -1
	}
	if t.IsZero() {
		*t = now
	}
	return now.Sub(*t)
}

func (a *LayerAllocator) Decide(now time.Time, in AllocInput) Allocation {
	e := in.Estimate
	operating := in.Applied
	if in.Operating != nil {
		operating = *in.Operating
	}
	if !a.hasBasis || a.basis != operating {
		a.basis, a.hasBasis = operating, true
		a.up, a.over = time.Time{}, time.Time{}
		a.upFor, a.cut, a.rate = Option{}, false, 0
	}
	hold := func(o Option) int { return Hold(in.Costs, o, in.FEC, e.Loss) }
	admit := func(o Option) int { t, _ := Admit(in.Costs, o, in.FEC, e.Loss); return t }
	proof := min(e.Rate, e.Proven)
	// With fewer than KeySizeSamples recent keys the size estimate is
	// provisional: one oversized key must not pin admission with no way to
	// refresh it. Discovery first seeks the full admit; once a probe measured
	// the path below it, the steady part admits and the assembled-key check
	// plus the refusal back-off bound the attempt (orchestrator decision on
	// the C0 r3 admission deadlock; A18 native_1500k).
	// A native key from low rests on sustainable evidence (C2 §5 A10);
	// temporal upgrades within a decodable rendition stay cheap.
	sustained := proof
	if !e.CongestedAt.IsZero() && now.Sub(e.CongestedAt) < AppLimitedLookback {
		sustained = min(e.Rate, e.Sustained)
	}
	admitted := func(o Option) bool {
		total, steady := Admit(in.Costs, o, in.FEC, e.Loss)
		provisional := in.Costs.KeySamples[o.Rendition] < KeySizeSamples
		p := proof
		if o.Rendition == Native && operating.Rendition == Low {
			p = sustained
		}
		return total <= p || provisional && steady <= p && e.Ceiling > 0 && e.Ceiling < total
	}
	out := Allocation{Desired: operating}
	out.Untenable = since(&a.low, now, hold(Option{Low, 0}) > e.Rate) >= DownTime

	// The best option the evidence already admits, and the next rung above
	// it: discovery climbs one rung per probe (F-2).
	// best starts at the applied option, but is the highest rung the evidence
	// admits: after a cost or loss shock leaves the applied option unproven,
	// the candidate re-proves it (or a lower rung) before climbing (A8 outage).
	best := step(operating)
	for i, o := range ladder {
		if admitted(o) {
			best = i
		}
	}
	if e.Draining {
		// A drain episode: nothing better is probed or admitted until the
		// debt cleared (C2 §5 A5).
	} else if best+1 < len(ladder) {
		out.Candidate = ladder[best+1]
		out.Target = admit(out.Candidate)
	} else if key := in.Costs.KeyBytes[Native]; key > 0 && (operating.Rendition == Low || operating == ladder[len(ladder)-1] && admitted(operating) &&
		(e.LostAt.IsZero() || now.Sub(e.LostAt) >= ProbeEvidenceTTL) && (e.CongestedAt.IsZero() || now.Sub(e.CongestedAt) >= ProbeEvidenceTTL)) {
		// Fast-key rung: a viewer on low sees nothing new from key
		// admission until the native key is delivered, and a native viewer
		// shows nothing new while a refresh key another viewer asked for is
		// delivered to it (A16: ≈0.6 s for a 251 KB key at 2.8 Mbit/s, a
		// 585 ms freeze). Discovery seeks the rate that delivers the key
		// within KeyFast. The sender's key boost removed the post-key
		// encoder pause that made faster refresh-key delivery split one
		// freeze into two (the A1 note behind excluding native viewers).
		// A native viewer seeks it only at native T2 (its clamp would hold
		// back the temporal climb) and once current evidence admits native
		// T2 (best starts at the applied option, so after a path change it
		// probed 8 Mbit/s into a 256 kbit/s path: A15), and only where the
		// path showed neither loss (on bad_wifi it raised freezes from
		// 2.56 to 3.14 per 300 s) nor congestion (on a shared or
		// constrained path each cluster queues into the bottleneck: A5,
		// A11 fq_512k_1v) for the evidence lifetime, ProbeEvidenceTTL.
		top := ladder[len(ladder)-1]
		// A native viewer stops seeking it once a measured ceiling shows
		// the path cannot deliver it (A7 rtt_step: 7.4 Mbit/s clusters into
		// 5 Mbit/s every back-off, each a delay cut).
		// Nor does it while current fast-key evidence already covers it.
		if t := hold(top) + KeyBits(key, in.FEC)*int(time.Second)/int(KeyFast); t > admit(top) && (operating.Rendition == Low || (e.Ceiling == 0 || e.Ceiling >= t) && e.KeyRate < t) {
			out.Candidate, out.Target = top, t
		}
	}

	holding := func(from int) Option {
		for i := from; i >= 0; i-- {
			if hold(ladder[i]) <= e.Rate {
				return ladder[i]
			}
		}
		return ladder[0]
	}
	if in.Recovering {
		out.Desired, out.Intent = holding(2), Required // accepted recovery, ordinary Low budget
		return out
	}

	// A cross-rendition native key keeps its live Hold contract after FIRST.
	// Same-rendition refresh and completed Native retain ordinary down timers.
	tx := in.Transaction
	if tx != nil && tx.Option.Rendition == Native && tx.Previous == Low && tx.Intent == Upgrade && tx.Phase < KeyCompleted {
		if tx.Generation != in.Generation || !nativeHolds(now, e, in.Costs, in.FEC) {
			if tx.Phase == KeyStarted {
				out.Desired, out.Intent = holding(2), Required
			}
			return out
		}
		cap := heldKeyOption(now, e, in.Costs, in.FEC, tx.Option)
		for tid := int(cap.TID) + 1; tid <= 2; tid++ {
			o := Option{Native, uint8(tid)}
			if admitted(o) {
				cap = o
			}
		}
		out.Desired = cap
		if operating.Rendition == Low {
			out.Intent = Upgrade
		} else if cap.TID > operating.TID {
			out.Intent = Upgrade
		} else if cap.TID < operating.TID {
			out.Intent = Required
		}
		return out
	}
	// A complete key resets temporal references. A genuinely admitted higher
	// cap may be authorized at this key, without borrowing another rendition's TID.
	if tx != nil && tx.Rendition == operating.Rendition && tx.Phase < KeyCompleted {
		for tid := int(operating.TID) + 1; tid <= 2; tid++ {
			o := Option{operating.Rendition, uint8(tid)}
			if admitted(o) {
				out.Desired, out.Intent = o, Upgrade
			}
		}
	}

	// A cut in R is answered within DownTime. An excess that only the cost
	// estimate caused must outlast the meter's own TrendWindow: within it,
	// one outlier picture and a demand change look alike, and a 2.3× TL0
	// picture lifted T0's cost 27% for ≈0.6 s (A18 native_1500k seed 1; C2 §5
	// "do not repeatedly revoke a native option because a 1 s estimate
	// jitters around the hold margin"). A real demand rise that congests
	// the path cuts R and takes the fast path.
	excess := hold(operating) > e.Rate
	if !excess || a.over.IsZero() {
		a.cut = false
	}
	a.cut = a.cut || excess && a.rate > 0 && e.Rate < a.rate
	a.rate = e.Rate
	persist := TrendWindow
	if a.cut {
		persist = DownTime
	}
	if e.Draining {
		// Congestion override: the obsolete option is not kept for a down
		// timer while the network drains the debt it created (C2 §5 A5).
		persist = 0
	}
	if since(&a.over, now, excess) >= persist {
		out.Desired, out.Intent = holding(step(operating)-1), Required
		a.up = time.Time{}
		return out
	}
	if e.Draining {
		return out
	}
	if a.over.IsZero() && operating.TID < 2 && out.Intent != Upgrade {
		next := Option{operating.Rendition, operating.TID + 1}
		if a.upFor != next {
			a.upFor, a.up = next, time.Time{}
		}
		if since(&a.up, now, admitted(next)) >= TemporalUpTime {
			out.Desired, out.Intent = next, Upgrade
		}
	}
	// Low → native at a key, onto the most frames the evidence admits. A
	// joiner first acquires low (prompt playback), so its join key is kept.
	//
	// This is eligibility, not permission to interrupt low with a key.
	// Viewer prepares the switch service explicitly: a probe timeout or
	// exhausted padding credit is not a measured capacity ceiling.
	// Recent observed congestion imposes the stability requirement: no
	// switch to native within RenditionDwell of a congestion cut or drain
	// episode (C2 §5 A10). A clean cold join is not held back.
	dwell := (a.changed.IsZero() || now.Sub(a.changed) >= RenditionDwell) && (e.CongestedAt.IsZero() || now.Sub(e.CongestedAt) >= RenditionDwell)
	_, steadyT0 := Admit(in.Costs, Option{Native, 0}, in.FEC, e.Loss)
	if a.acquired && operating.Rendition == Low && in.NativeKey && dwell && best >= 3 {
		out.Desired, out.Intent = ladder[best], Upgrade
	} else if operating.Rendition == Low && in.Switching && in.NativeKey && hold(Option{Native, 0}) <= e.Rate && steadyT0 <= sustained {
		// A requested switch persists until its key is admitted or refused
		// while the evidence still carries native T0's steady need; the
		// assembled-key check decides, so cost noise around the key margin
		// does not discard the key it asked for (A18 native_1500k). Lost
		// evidence cancels it (C2 §5 A10).
		out.Desired, out.Intent = Option{Native, 0}, Upgrade
		if best >= 3 {
			out.Desired, out.Intent = ladder[best], Upgrade
		}
	}
	return out
}
