package ctrl

import "time"

// The types in this file exist so internal/sfusim can drive a complete loop
// before A1 lands. They implement none of the rules in docs/CONTROLLER.md
// beyond what the simulator needs to move packets. A1 replaces each one.

// FixedEstimator reports a constant rate. STUB.
type FixedEstimator struct{ Rate int }

func (e *FixedEstimator) OnSent(Sent)                        {}
func (e *FixedEstimator) OnFeedback(time.Time, Feedback)     {}
func (e *FixedEstimator) OnReport(time.Time, ReceiverReport) {}
func (e *FixedEstimator) OnProbe(time.Time, ProbeResult)     {}
func (e *FixedEstimator) OnGeneration(time.Time, uint64)     {}
func (e *FixedEstimator) Estimate(time.Time) Estimate        { return Estimate{Rate: e.Rate, Proven: e.Rate} }
func (e *FixedEstimator) Clamp(int)                          {}

// NoProber never probes. STUB.
type NoProber struct{}

func (NoProber) OnSent(Sent)                                               {}
func (NoProber) OnFeedback(time.Time, Feedback) (ProbeResult, bool)        { return ProbeResult{}, false }
func (NoProber) Tick(time.Time) (ProbeResult, bool)                        { return ProbeResult{}, false }
func (NoProber) Maybe(time.Time, int, int, Estimate, bool) (Cluster, bool) { return Cluster{}, false }
func (NoProber) MaybeKey(time.Time, int, int, Estimate) (Cluster, bool)    { return Cluster{}, false }
func (NoProber) Ready(time.Time) bool                                      { return false }
func (NoProber) OnGeneration(time.Time, uint64)                            {}
func (NoProber) BootstrapUsed() int                                        { return 0 }

// ReferenceAllocator picks the most preferred option whose admission total
// fits R, with no hysteresis, dwell, evidence or probing. It is the trivial
// baseline the simulator drives before A1; it is not the controller's allocator.
type ReferenceAllocator struct{}

func (ReferenceAllocator) OnApplied(time.Time, Option)    {}
func (ReferenceAllocator) OnGeneration(time.Time, uint64) {}
func (ReferenceAllocator) Decide(_ time.Time, in AllocInput) Allocation {
	for _, o := range Options {
		if o.Rendition == Native && (in.Recovering || !in.NativeKey) {
			continue
		}
		if total, _ := Admit(in.Costs, o, in.FEC, in.Estimate.Loss); total <= in.Estimate.Rate {
			return referenceChoice(in, o)
		}
	}
	low := Option{Low, 0}
	out := referenceChoice(in, low)
	out.Untenable = Hold(in.Costs, low, in.FEC, in.Estimate.Loss) > in.Estimate.Rate
	return out
}

// FIFOPacer is a single token bucket with no priorities, bounds or drops. STUB.
type FIFOPacer struct {
	rate    int
	tokens  float64 // bytes
	at      time.Time
	queue   []Packet
	bytes   int
	cluster *Cluster
	padding uint64
}

func (p *FIFOPacer) current(now time.Time) int {
	if p.cluster != nil && now.Before(p.cluster.Until) {
		return max(p.rate, p.cluster.Rate)
	}
	return p.rate
}
func (p *FIFOPacer) refill(now time.Time) {
	rate := p.current(now)
	if !p.at.IsZero() {
		p.tokens += now.Sub(p.at).Seconds() * float64(rate) / 8
	}
	p.at = now
	if burst := max(BurstFloor, PacerBurst.Seconds()*float64(rate)/8); p.tokens > burst {
		p.tokens = burst
	}
}
func (p *FIFOPacer) SetRate(now time.Time, bps int) { p.refill(now); p.rate = bps }
func (p *FIFOPacer) SetCluster(c *Cluster)          { p.cluster = c }
func (p *FIFOPacer) SetRelease(func(Packet))        {} // never drops
func (p *FIFOPacer) Push(_ time.Time, k Packet) bool {
	p.queue = append(p.queue, k)
	p.bytes += k.Size
	return true
}
func (p *FIFOPacer) Dropped() []Packet { return nil }
func (p *FIFOPacer) Purge(Rendition)   {}
func (p *FIFOPacer) Queued() (int, int) {
	frames := 0
	for _, k := range p.queue {
		if k.Last {
			frames++
		}
	}
	return p.bytes, frames
}
func (p *FIFOPacer) KeyInFlight() bool {
	for _, k := range p.queue {
		if k.Key {
			return true
		}
	}
	return false
}
func (p *FIFOPacer) QueuedKey(r Rendition) (remaining, total int) {
	for _, k := range p.queue {
		if k.Key && k.Rendition == r {
			remaining += k.Size
			total = max(total, k.FrameSize)
		}
	}
	return remaining, total
}
func (p *FIFOPacer) Pop(now time.Time) (Packet, bool) {
	p.refill(now)
	if p.tokens < 0 {
		return Packet{}, false
	}
	if len(p.queue) == 0 {
		if p.cluster == nil || !now.Before(p.cluster.Until) {
			return Packet{}, false
		}
		p.padding++
		k := Packet{ID: p.padding, Size: 1200, Kind: KindPadding, Class: ClassPadding, Enqueued: now, Cluster: p.cluster.ID}
		p.tokens -= float64(k.Size)
		return k, true
	}
	k := p.queue[0]
	p.queue = p.queue[1:]
	p.bytes -= k.Size
	p.tokens -= float64(k.Size)
	return k, true
}
func (p *FIFOPacer) NextAt(now time.Time) time.Time {
	if len(p.queue) == 0 && (p.cluster == nil || !now.Before(p.cluster.Until)) {
		return time.Time{}
	}
	rate := p.current(now)
	if p.tokens >= 0 || rate <= 0 {
		return now
	}
	return now.Add(time.Duration(-p.tokens * 8 / float64(rate) * float64(time.Second)))
}

// SingleRepair resends once within the deadline and sends no FEC. STUB.
type SingleRepair struct{}

func (SingleRepair) Resend(q RepairQuery) bool {
	return q.Current && q.Sends < 1 && q.PredictedArrival.Before(q.Deadline)
}
func (SingleRepair) FEC(Estimate, bool) (float64, int) { return 0, 1 }

// ImmediateKeys forwards every request at once, with no budget, and never
// redirects recovery to Low. STUB.
type ImmediateKeys struct{ due [2]bool }

func (k *ImmediateKeys) Request(_ time.Time, _ int, r Rendition, _ Cause) KeyStatus {
	k.due[r] = true
	return KeyAccepted
}
func (k *ImmediateKeys) Allowed(time.Time, int, Rendition, Cause) bool { return true }
func (k *ImmediateKeys) OnKey(time.Time, Rendition, int, time.Time)    {}
func (k *ImmediateKeys) Size(time.Time, Rendition) (int, int)          { return 0, 0 }
func (k *ImmediateKeys) Leave(int)                                     {}
func (k *ImmediateKeys) Due(time.Time) []Rendition {
	var out []Rendition
	for r, due := range k.due {
		if due {
			out = append(out, Rendition(r))
		}
	}
	k.due = [2]bool{}
	return out
}

// AllowAll forwards every picture up to maxTID and ignores losses. STUB:
// it violates the TL1 rule and fails Tier A A17 by design.
type AllowAll struct{}

func (AllowAll) Supported(FrameInfo) bool             { return true }
func (AllowAll) Allow(f FrameInfo, maxTID uint8) bool { return f.Key || f.TID <= maxTID }
func (AllowAll) Lost(FrameInfo)                       {}
func (AllowAll) Unsupported(Rendition) bool           { return false }

func (NoProber) Observe(time.Time, Estimate)      {}
func (NoProber) AllowPadding(time.Time, int) bool { return false }
func (p *FIFOPacer) KeyEvents() []KeyEvent        { return nil }
func (k *ImmediateKeys) OnDelivery(int, KeyEvent) {}

func referenceChoice(in AllocInput, o Option) Allocation {
	basis := in.Applied
	if in.Operating != nil {
		basis = *in.Operating
	}
	action := Retain
	if in.Recovering || step(o) < step(basis) {
		action = Required
	} else if step(o) > step(basis) {
		action = Upgrade
	}
	return Allocation{Desired: o, Intent: action}
}
