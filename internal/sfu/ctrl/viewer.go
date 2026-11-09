package ctrl

import "time"

// Viewer is the forward loop for one viewer. internal/sfu and internal/sfusim
// both drive this type; neither makes rate, layer, key or repair decisions
// outside it.
type Viewer struct {
	ID int
	// Session is the admitted session behind this PC: ID for a first PC,
	// inherited by a replacement (NewViewerFrom).
	Session int
	Cfg     Config
	Est     Estimator
	Probe   Prober
	Alloc   Allocator
	Pacer   Pacer
	Repair  Repair
	Cont    Continuity
	Keys    Keys // shared by the room

	generation uint64
	applied    Option
	started    bool
	requested  Rendition // rendition of the last key request
	requestAt  time.Time
	recoveryAt time.Time
	result     *ProbeResult
	// An assembled native key did not fit: no native upgrade before
	// refusedUntil; the back-off doubles from KeyRefusedMin to KeyRefusedMax.
	refusedUntil time.Time
	refusedFor   time.Duration
	// switching is set while a native key is requested for, or being
	// delivered to, this viewer.
	switching  bool
	delivery   [2]KeyEvent // generation/frame identity for delivery terminal fencing
	delivering [2]bool     // a key of the rendition is admitted to this viewer's pacer
	switchRung bool        // a fast-key cluster ran; clamp its evidence to R's use once native
	// replacement: this loop was handed off from the session's previous PC;
	// its join request is Keys.Replace.
	replacement bool
	keyTimes    [2]KeyTimes
	probing     bool              // a cluster this viewer started has no result yet
	preparation switchPreparation // bounded switch-service round (timeout/loss and constrained native owning cases)
}

// KeyTimes are one viewer's latest key milestones for a rendition
// (diagnostics): its request, the room's PLI emission and key assembly, and
// the key's delivery (last packet sent) to this viewer.
type KeyTimes struct {
	Requested, Emitted, Assembled, Delivered time.Time
	Exception                                bool
}

// NewRoomKeys returns the room's key policy. A1 replaces the stub.
func NewRoomKeys() Keys { return &RoomKeys{} }

// NewViewer composes the clock-free policy, shared by production and sfusim.
func NewViewer(id int, keys Keys) *Viewer {
	cfg := defaultConfig()
	probe := &Discovery{MaxOverdrive: cfg.ProbeMaxOverdrive}
	return &Viewer{ID: id, Session: id, Cfg: cfg, applied: Option{Low, 2}, Est: &Bandwidth{Config: cfg}, Probe: probe,
		Alloc: &LayerAllocator{}, Pacer: &Queue{padding: probe.AllowPadding}, Repair: DeadlineRepair{}, Cont: &Dependency{}, Keys: keys}
}

// NewViewerFrom starts a replacement PC from a handed-off rate, in a new
// generation and with no admission evidence. A1 seeds the estimator here.
func NewViewerFrom(id int, keys Keys, s RateState) *Viewer {
	v := NewViewer(id, keys)
	v.generation = 1
	v.replacement, v.Session = true, s.Session
	v.Est = &Bandwidth{Config: v.Cfg, rate: s.Rate, generation: 1, episode: s.Congested, inherited: s.Congested, handedCeiling: s.Ceiling}
	v.Probe.OnGeneration(time.Time{}, 1)
	v.Pacer.OnGeneration(time.Time{}, 1)
	return v
}

// Generation is the current path generation; stamp it on every Sent.
func (v *Viewer) Generation() uint64 { return v.generation }

// PictureService fences a plan to the path generation that supplied its facts.
func (v *Viewer) PictureService(now time.Time, in PictureQuery) PictureService {
	if in.Generation != v.generation {
		return PictureService{Generation: in.Generation}
	}
	return v.Pacer.PictureService(now, in)
}

// Handoff returns the rate state a replacement PC may inherit.
func (v *Viewer) Handoff(now time.Time) RateState {
	e := v.Est.Estimate(now)
	return RateState{Rate: e.Rate, Session: v.Session, Congested: e.Draining || e.Overuse || e.Congestive, Ceiling: e.Ceiling}
}

// noteRequest starts a new key-milestone record for r unless one is still
// outstanding (requested and not yet delivered).
func (v *Viewer) noteRequest(now time.Time, r Rendition, exception bool) {
	t := &v.keyTimes[r]
	if t.Requested.IsZero() || !t.Delivered.Before(t.Requested) {
		*t = KeyTimes{Requested: now}
	}
	t.Exception = t.Exception || exception
}

// KeyTimes returns the viewer's latest milestones for r's key (diagnostics).
func (v *Viewer) KeyTimes(r Rendition) KeyTimes {
	t := v.keyTimes[r]
	t.Emitted, t.Assembled = v.Keys.Times(r)
	return t
}

// OnSent must be called for every packet the transport sends to this viewer.
func (v *Viewer) OnSent(s Sent) {
	v.Est.OnSent(s)
	v.Probe.OnSent(s)
}

func (v *Viewer) OnFeedback(now time.Time, f Feedback) {
	v.Est.OnFeedback(now, f)
	v.Probe.Observe(now, v.Est.Estimate(now))
	if r, ok := v.Probe.OnFeedback(now, f); ok {
		v.accept(now, r)
	}
}

func (v *Viewer) accept(now time.Time, r ProbeResult) {
	if r.Generation != v.generation {
		return
	}
	v.probing = false
	v.Est.OnProbe(now, r)
	v.result = &r
}

func (v *Viewer) OnReport(now time.Time, r ReceiverReport) { v.Est.OnReport(now, r) }

// OnPathChange fences every component: the selected pair changed.
func (v *Viewer) OnPathChange(now time.Time) {
	v.generation++
	v.Est.OnGeneration(now, v.generation)
	v.Probe.OnGeneration(now, v.generation)
	v.Alloc.OnGeneration(now, v.generation)
	v.Pacer.SetCluster(nil)
	v.Pacer.OnGeneration(now, v.generation)
	v.result = nil
	v.probing = false
	v.preparation = switchPreparation{}
	v.switching = false
}

// OnDecoderStall handles the viewer's untrusted decoder report. It only
// requests a key; an accepted request forces low until RecoveryQuiet.
//
// True means the request was accepted: the viewer now wants low, and the
// forwarder must Step before it offers the next picture. The low key the
// request asked for can assemble within milliseconds, before the next
// periodic Step; a forwarder still waiting for the old rendition would skip
// it and the next low key would wait KeyInterval (A8 outage).
func (v *Viewer) OnDecoderStall(now time.Time) bool {
	return v.recover(now, v.applied.Rendition, CauseRecovery)
}

// OnBroken reports a base picture of r that this viewer cannot decode
// (detected by the SFU outside the pacer). True has OnDecoderStall's meaning.
func (v *Viewer) OnBroken(now time.Time, r Rendition) bool { return v.recover(now, r, CauseBroken) }

func (v *Viewer) recover(now time.Time, r Rendition, c Cause) bool {
	if s := v.Keys.Request(now, v.ID, r, c); s != KeyIgnored {
		v.recoveryAt = now
		v.noteRequest(now, Low, false)
		return true
	}
	return false
}

// OnApplied reports the option the forwarder actually forwards, once the
// target key or switching point went out.
func (v *Viewer) OnApplied(now time.Time, o Option) {
	v.applied = o
	v.Pacer.Apply(o)
	v.Alloc.OnApplied(now, o)
}

// Applied is the option currently forwarded.
func (v *Viewer) Applied() Option { return v.applied }

// KeyFits is the assembled-key admission check: forward the key of r only if
// audio plus its delivery within KeyEnvelope fits R. A1 may refine.
// bytes is the actual assembled key's wire size, not the admission estimate.
// A refused native key starts or doubles the viewer's upgrade back-off; a
// native key that fits resets it.
func (v *Viewer) KeyFits(now time.Time, r Rendition, bytes int, c Costs) bool {
	e := v.Est.Estimate(now)
	fec, _ := v.Repair.FEC(e, true)
	fits := c.AudioWire+KeyBits(bytes, fec)*int(time.Second)/int(KeyEnvelope) <= e.Rate
	if r == Native {
		if fits {
			v.refusedFor = 0
		} else {
			v.refuse(now)
		}
	}
	return fits
}

func (v *Viewer) refuse(now time.Time) {
	v.switching = false
	v.refusedFor = min(KeyRefusedMax, max(KeyRefusedMin, 2*v.refusedFor))
	v.refusedUntil = now.Add(v.refusedFor)
}

// Step runs one control decision. Call it after feedback and at least every
// OveruseTime.
func (v *Viewer) Step(now time.Time, c Costs) Decision {
	// A8/A19: a replacement has an empty decoder. Acquire low through the
	// existing recovery quiet period before a native key can interrupt the
	// first fresh frames, even when inherited R admits native immediately.
	if !v.started && v.replacement {
		v.recoveryAt = now
	}
	for _, p := range v.Pacer.Dropped() {
		if breaksChain(p) {
			v.recover(now, p.Rendition, CauseBroken)
			break
		}
	}
	if r, ok := v.Probe.Tick(now); ok {
		v.accept(now, r)
	}
	e := v.Est.Estimate(now)
	v.Probe.Observe(now, e)
	for _, ev := range v.Pacer.KeyEvents() {
		v.Keys.OnDelivery(v.ID, ev)
		r := ev.Rendition
		id := KeyAuthorization{Generation: ev.Generation, Rendition: r, Frame: ev.Frame}
		old := v.delivery[r]
		if ev.Phase == KeyAdmitted || ev.Phase == KeyStarted {
			if ev.Generation < old.Generation {
				continue
			}
			v.delivery[r], v.delivering[r] = ev, true
		} else {
			previous := KeyAuthorization{Generation: old.Generation, Rendition: r, Frame: old.Frame}
			if !sameKey(id, previous) {
				continue
			}
			v.delivering[r] = false
			v.delivery[r] = ev
			if ev.Phase == KeyCompleted {
				v.keyTimes[r].Delivered = ev.At
			}
			if r == Native {
				v.switching = false
			}
			if ev.Phase == KeyAborted {
				v.recover(now, r, CauseBroken)
				if r == Native {
					v.refuse(now)
				}
			}
		}
	}
	operating := v.Operating()

	if recovering := !v.recoveryAt.IsZero() && now.Sub(v.recoveryAt) < RecoveryQuiet; recovering || v.applied.Rendition == Native || e.Draining {
		v.switching = false
	}
	if e.Draining {
		v.Pacer.SetCluster(nil) // no padding into the debt
	}
	queued, _ := v.Pacer.Queued()
	feasible := e.Rate > 0 && e.RTT+time.Duration(queued*8)*time.Second/time.Duration(e.Rate) < FrameDeadline/2
	fec, interleave := v.Repair.FEC(e, feasible)
	// Wire budget (C2 §5 A12/A13): while NACK can still repair within the
	// deadline, protection that does not fit beside the applied option is
	// reduced, a step at a time, rather than pricing that option out.
	// Parity is charged as sent (Need), and AQM drops on a constrained link
	// (A11 FQ-CoDel) otherwise turned the protection on and the native
	// option off.
	for fec > 0 && feasible && Hold(c, operating, fec, e.Loss) > e.Rate {
		if fec > FECRatio {
			fec = FECRatio
		} else {
			fec, interleave = 0, 1
		}
	}
	recovering := !v.recoveryAt.IsZero() && now.Sub(v.recoveryAt) < RecoveryQuiet
	cause := CauseUpgrade
	if !v.started {
		cause = CauseJoin
	}
	// While a drain episode's debt waits in the network the allocator sees
	// the drain-paced rate, so it forwards only what that rate carries.
	pacedRate := v.paced(e, c, fec)
	ae := e
	ae.Rate = pacedRate
	tx := v.allocationTransaction(operating, now, recovering)
	a := v.Alloc.Decide(now, AllocInput{Estimate: ae, Costs: c, FEC: fec, Applied: v.applied, Operating: &operating, Transaction: tx, Generation: v.generation,
		NativeKey: !now.Before(v.refusedUntil) && v.Keys.Allowed(now, v.ID, Native, cause), Switching: v.switching, Recovering: recovering, Probe: v.result})
	if operating.Rendition == Low && a.Desired.Rendition == Native && a.Intent == Upgrade {
		ready, target := v.prepareSwitch(now, a.Desired, c, fec, e)
		if !ready {
			a.Desired, a.Intent, a.Target = operating, Retain, target
		}
	} else if !v.delivering[Native] {
		v.preparation = switchPreparation{}
	}
	v.preparation.choice = &a
	v.preparation.generation = v.generation

	v.result = nil
	d := Decision{Desired: a.Desired, Intent: a.Intent, FEC: fec, Interleave: interleave, Untenable: a.Untenable, Rate: pacedRate,
		Stop: e.Draining}
	// A key already admitted to this viewer serves the switch; re-requesting
	// while it is delivered would leave a stale room-wide request.
	// While a drain episode's debt waits in the network, a switch key is
	// requested only once the drain-paced rate admits its estimated size:
	// a key requested earlier is refused at assembly yet spends the room's
	// KeyInterval, and could only queue behind the debt (C2 §5 A5).
	deliverable := e.Service <= 0 || c.AudioWire+KeyBits(c.KeyBytes[a.Desired.Rendition], fec)*int(time.Second)/int(KeyEnvelope) <= pacedRate
	if r := a.Desired.Rendition; !v.started || a.Intent != Retain && r != operating.Rendition && !v.delivering[r] && (r != v.requested || now.Sub(v.requestAt) >= KeyRetry) && deliverable {
		if !v.started && v.replacement {
			d.Key = v.Keys.Replace(now, v.ID, v.Session)
		} else {
			d.Key = v.Keys.Request(now, v.ID, r, cause)
		}
		d.Requested = true
		if d.Key != KeyIgnored {
			v.noteRequest(now, r, !v.started && v.replacement && d.Key == KeyAccepted)
		}
		if d.Key == KeyRedirected {
			d.Desired = Option{Low, a.Desired.TID}
		}
		if d.Desired.Rendition == Native && (d.Key == KeyAccepted || d.Key == KeyPending) {
			v.switching = true
		}
		v.requested, v.requestAt, v.started = d.Desired.Rendition, now, true
	}
	top, _ := Admit(c, Option{Native, 2}, fec, e.Loss)
	// Clamped once native and the rung's result is in: a native viewer's
	// own fast-key cluster finishes after it started (6a escalation A16).
	if v.switchRung && v.applied.Rendition == Native && !v.probing {
		v.Est.Clamp(top)
		v.switchRung = false
	}
	// A native viewer climbs to its fast-key rate as a ceiling ratchet
	// against its current evidence (C2 §5 A10: cap the experiment by
	// current evidence): the prober offers at most one overdrive step above
	// it, in the smallest measurable cluster, and a clean step that
	// measured past it keeps climbing. One 100 ms cluster at the full rung
	// overshot a 3 Mbit/s path 2.8 times and cut R (A4 b1_3m).
	pe := e
	if v.preparation.outcome == SwitchMeasuring || a.Target > top && operating.Rendition == Native {
		pe.Ceiling = max(e.Proven, e.Rate, e.KeyRate)
		if e.Ceiling > 0 {
			pe.Ceiling = min(pe.Ceiling, e.Ceiling)
		}
	}
	// A1/A16: measure with an already admitted key instead of spending the
	// finite bootstrap on a fast-key cluster too short to escape one burst.
	// Use its actual full size for the delivery objective; queued bytes bound
	// the experiment. No key is requested or admitted through this path.
	if left, actual := v.Pacer.QueuedKey(Native); left > 0 && !recovering &&
		d.Desired.Rendition == Native {
		target := Hold(c, a.Desired, fec, e.Loss) + KeyBits(actual, fec)*int(time.Second)/int(KeyFast)
		if cl, ok := v.Probe.MaybeKey(now, max(top, target), left, e); ok {
			d.Cluster = &cl
			v.Pacer.SetCluster(&cl)
			v.probing, v.switchRung = true, true
		}
	}
	if a.Target > 0 {
		if cl, ok := v.Probe.Maybe(now, a.Target, c.Rate[operating.Rendition][operating.TID], pe, v.Pacer.KeyInFlight()); ok {
			cl.Generation = v.generation
			d.Cluster = &cl
			v.Pacer.SetCluster(&cl)
			v.probing = true
			v.switchRung = v.switchRung || a.Target > top
			if v.preparation.outcome == SwitchMeasuring {
				v.preparation.cluster = cl.ID
			}
		} else if v.preparation.outcome == SwitchMeasuring && !v.probing {
			// No experiment can be funded/started under the existing
			// credit/backoff rules. Fall back once, at the next live decision.
			v.preparation.outcome = SwitchUnmeasured
		}
	}
	// A key in flight leaves at the fast-key rate its rung proved (C2 item
	// 6b, 6a escalation A16); R itself stays clamped to native T2's admit,
	// so pictures are not burst at that rate.
	if v.Pacer.KeyInFlight() && e.Service <= 0 {
		pacedRate = max(pacedRate, e.KeyRate)
	}
	v.Pacer.SetRate(now, pacedRate)
	return d
}

// paced is the pacer's rate: R, except while a drain episode's debt waits
// in the network. New work then leaves only at the rate that drains the
// debt within the freshness bound: a picture queued behind more than
// FrameDeadline of debt is useless on arrival, and every byte sent into the
// debt delays the drain (C2 §5 A5: at 256 kbit/s with 48 kbit/s of audio
// the drain is 26 KB/s). Audio always flows, and a viewer decoding low keeps
// its base chain (low T0). The allocator uses the same rate, and Step
// requests a switch key only once this rate admits it, so the recovery
// allowance grows as the debt drains. An admitted key (a recovery the
// viewer asked for) waits in the pacer rather than in the network.
func (v *Viewer) paced(e Estimate, c Costs, fec float64) int {
	if e.Service <= 0 {
		return e.Rate
	}
	floor := c.AudioWire
	if v.Operating().Rendition == Low {
		floor = Hold(c, Option{Low, 0}, fec, e.Loss)
	}
	drain := e.Service - int(int64(e.Debt)*8*int64(time.Second)/int64(FrameDeadline))
	return min(e.Rate, max(floor, drain))
}
