package ctrl

import "time"

type switchPreparation struct {
	at         time.Time
	cluster    int
	outcome    SwitchOutcome
	choice     *Allocation // current controller intent, not the wire's rendition comparison
	generation uint64
}

func heldKeyOption(now time.Time, e Estimate, c Costs, fec float64, cap Option) Option {
	proof := min(e.Rate, e.Proven)
	if !e.CongestedAt.IsZero() && now.Sub(e.CongestedAt) < AppLimitedLookback {
		proof = min(e.Rate, e.Sustained)
	}
	for tid := int(cap.TID); tid >= 0; tid-- {
		o := Option{cap.Rendition, uint8(tid)}
		if Hold(c, o, fec, e.Loss) <= proof {
			return o
		}
	}
	return Option{cap.Rendition, 0}
}

// SwitchOutcome reports preparation evidence, never a capacity inference from
// a prober becoming idle. The round is local to one continuously eligible switch.
func (v *Viewer) SwitchOutcome() SwitchOutcome { return v.preparation.outcome }

// Admission's upward margin must not be charged again after a prepared key
// arrives. The actual wire key is rechecked separately; current sustainable
// Hold decides whether this already-prepared native reference chain survives
// cost noise (oversize-first-key/1500k), not a fresh temporal admission.
func nativeHolds(now time.Time, e Estimate, c Costs, fec float64) bool {
	proof := min(e.Rate, e.Proven)
	if !e.CongestedAt.IsZero() && now.Sub(e.CongestedAt) < AppLimitedLookback {
		proof = min(e.Rate, e.Sustained)
	}
	return !e.Draining && !e.Overuse && !e.Congestive && Hold(c, Option{Native, 0}, fec, e.Loss) <= proof
}

func (v *Viewer) prepareSwitch(now time.Time, o Option, c Costs, fec float64, e Estimate) (bool, int) {
	p := &v.preparation
	if c.KeyBytes[Native] <= 0 {
		p.outcome = SwitchUnmeasured
		return true, 0 // no invented key size; actual assembly still rechecks service
	}
	target := Hold(c, o, fec, e.Loss) + KeyBits(c.KeyBytes[Native], fec)*int(time.Second)/int(KeyFast)
	if max(min(e.Rate, e.Proven), e.KeyRate) >= target {
		p.outcome = SwitchDemonstrated
	} else if e.Ceiling > 0 && e.Ceiling < target {
		p.outcome = SwitchConstrained
	} else if p.outcome == SwitchDemonstrated || p.outcome == SwitchConstrained {
		// Costs or the key estimate can raise the objective without losing
		// path proof. Reclassify once, preserving the actual key for its
		// funded experiment and service check. BeforeSend separately cancels
		// genuinely lost eligibility/path evidence or an infeasible key.
		p.outcome = SwitchUnmeasured
	}
	if p.outcome == SwitchIdle {
		*p = switchPreparation{at: now, outcome: SwitchMeasuring}
	}
	if p.outcome == SwitchMeasuring {
		if r := v.result; r != nil && p.cluster != 0 && r.Cluster == p.cluster {
			// Incomplete/lost feedback, a capped offer or a dirty result
			// cannot establish a capacity limit. The live estimator above
			// handles actually demonstrated service and saturation.
			p.outcome = SwitchUnmeasured
		} else if now.Sub(p.at) >= ProbeTimeout {
			p.outcome = SwitchUnmeasured
		} else if !v.probing && !v.Probe.Ready(now) {
			p.outcome = SwitchUnmeasured
		}
	}
	return p.outcome != SwitchMeasuring, target
}

// BeforeSend is shared by the wire writer and simulator. Physical write time
// is a fact supplied by the writer, not an assumed empty network certificate.
// A candidate occupies only the existing bounded pacer, never a GOP/staging cache.
func (v *Viewer) BeforeSend(now time.Time, writerPending int, _ Option, c Costs) {
	v.Pacer.Maintain(now, writerPending)
	choice := v.preparation.choice
	if choice == nil || v.preparation.generation != v.generation {
		return
	}
	active, known := v.Pacer.Operating()
	e := v.Est.Estimate(now)
	// Only the current controller decision may update a key's cap. An
	// unrelated pending Low TID can never become the Native completion cap.
	if known && active.Generation == v.generation && active.Phase == KeyStarted {
		cap := heldKeyOption(now, e, c, 0, active.Option)
		if choice.Desired.Rendition == active.Rendition {
			cap = heldKeyOption(now, e, c, 0, choice.Desired)
		}
		active.Option = cap
		v.Pacer.Authorize(active)
	}
	for _, r := range []Rendition{Native, Low} {
		key, queued := v.Pacer.Key(r)
		if !queued || key.Phase == KeyStarted {
			continue
		}
		same := known && r == active.Rendition
		if !same && (choice.Desired.Rendition != r || (r == Low && known && choice.Intent != Required)) {
			v.Pacer.CancelKey(now, r)
			continue
		}
		if !same && r == Native && (choice.Intent != Upgrade || v.preparation.outcome == SwitchIdle || v.preparation.outcome == SwitchMeasuring || !nativeHolds(now, e, c, 0) || !v.Keys.Allowed(now, v.ID, Native, CauseUpgrade)) {
			v.Pacer.CancelKey(now, r)
			v.switching = false
			continue
		}
		key.Generation = v.generation
		key.Intent = choice.Intent
		key.Option = choice.Desired
		if same && choice.Desired.Rendition != r {
			key.Option = heldKeyOption(now, e, c, 0, active.Option)
			key.Intent = Retain
		}
		v.Pacer.SetCommit(r, false)
		service := v.PictureService(now, PictureQuery{Rendition: r, Frame: key.Frame, Generation: v.generation, WriterPending: writerPending, Commit: true, Authorization: &key})
		if !service.Known {
			continue
		}
		_, bytes := v.Pacer.QueuedKey(r)
		if service.Broken || service.Key.IsZero() {
			if !same {
				v.Pacer.CancelKey(now, r)
				if r == Native {
					v.refuse(now)
				}
			}
			continue
		}
		if !v.KeyFits(now, r, bytes, c) {
			if !same {
				v.Pacer.CancelKey(now, r)
			}
			continue
		}
		v.Pacer.Authorize(key)
	}
}

// Operating reports actual FIRST ownership; completed dwell is still Applied.
func (v *Viewer) Operating() Option {
	if a, ok := v.Pacer.Operating(); ok {
		return a.Option
	}
	return v.applied
}

func (v *Viewer) allocationTransaction(operating Option, now time.Time, recovering bool) *KeyAuthorization {
	active, known := v.Pacer.Operating()
	if known && active.Phase == KeyStarted && active.Rendition == Native {
		return &active
	}
	// I1: a started Low refresh remains the physical owner, but cannot hide
	// an already prepared Native candidate from its live-Hold retention check.
	// Queue still finishes the old Low picture before optional Native FIRST.
	if operating.Rendition == Low && v.preparation.outcome != SwitchIdle && v.preparation.outcome != SwitchMeasuring && !recovering && v.Keys.Allowed(now, v.ID, Native, CauseUpgrade) {
		a, queued := v.Pacer.Key(Native)
		// I2: an unadmitted target is a new acquisition, not a physical key
		// to retain. After actual refusal it must retire through Step's
		// ordinary Low decision, without bypassing the existing backoff.
		if queued || !now.Before(v.refusedUntil) {
			if !queued {
				a = KeyAuthorization{Rendition: Native, Phase: KeyAdmitted}
			}
			a.Generation, a.Previous, a.Intent = v.generation, Low, Upgrade
			a.Option = Option{Native, 0}
			if ch := v.preparation.choice; ch != nil && ch.Desired.Rendition == Native {
				a.Option = ch.Desired
			}
			return &a
		}
	}
	if known && active.Phase == KeyStarted {
		return &active
	}
	if a, ok := v.Pacer.Key(operating.Rendition); ok {
		return &a
	}
	return nil
}

// CompletionOption is fenced to the exact physical LAST and current generation.
// It supplies no cap from a different rendition or an old completed target.
func (v *Viewer) CompletionOption(now time.Time, p Packet, c Costs) (Option, bool) {
	a, ok := v.Pacer.Operating()
	if !ok || !p.Key || !p.Last || a.Phase != KeyCompleted || a.Generation != v.generation || !sameKey(a, KeyAuthorization{Generation: p.Generation, Rendition: p.Rendition, Frame: p.Frame}) {
		return Option{}, false
	}
	e := v.Est.Estimate(now)
	for tid := int(a.Option.TID); tid >= 0; tid-- {
		o := Option{a.Rendition, uint8(tid)}
		if Hold(c, o, 0, e.Loss) <= e.Rate {
			return o, true
		}
	}
	return Option{}, false
}

// Acquisition and commitment follow the decoder's first key departure.
// Applied deliberately waits for LAST to acknowledge rendition/TID and dwell.
func (v *Viewer) activeRendition() Rendition {
	if r, known := v.Pacer.Active(); known {
		return r
	}
	return v.applied.Rendition
}
