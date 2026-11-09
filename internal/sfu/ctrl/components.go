package ctrl

import "time"

// Stub is true while any default component is a placeholder. Tier A
// properties in internal/sfusim report but do not enforce while it is set.
// A1 clears it when the real components land.
const Stub = false

// Estimator turns transport feedback into R. Inputs are TWCC, RTCP RR, probe
// results and generation changes only; decoder signals never reach it.
// Feedback and results from an older generation must be ignored.
type Estimator interface {
	OnSent(s Sent)
	OnFeedback(now time.Time, f Feedback)
	OnReport(now time.Time, r ReceiverReport)
	OnProbe(now time.Time, r ProbeResult)
	OnGeneration(now time.Time, generation uint64) // clear path evidence, keep R
	Estimate(now time.Time) Estimate
	// Clamp discards R and probe evidence above rate: switch-rung evidence
	// is not kept once the switch is done.
	Clamp(rate int)
}

// Prober plans probe clusters and measures them from TWCC. It owns the
// padding budget, so it sees every sent packet.
type Prober interface {
	OnSent(s Sent)
	OnFeedback(now time.Time, f Feedback) (ProbeResult, bool)
	// Tick finalises a cluster ProbeTimeout after its end even without feedback.
	Tick(now time.Time) (ProbeResult, bool)
	// Maybe returns a cluster to start now; target is admit(candidate) and
	// media the current option's video bps.
	Maybe(now time.Time, target, media int, e Estimate, keyInFlight bool) (Cluster, bool)
	// MaybeKey uses actual queued native-key bytes instead of padding. The
	// key already passed admission; this never authorizes a new key (A1/A16).
	MaybeKey(now time.Time, target, bytes int, e Estimate) (Cluster, bool)
	// Ready: no cluster in flight, the backoff elapsed and the padding
	// budget affords a measurable cluster, so the next rung can be probed
	// once nothing else blocks it.
	Ready(now time.Time) bool
	OnGeneration(now time.Time, generation uint64) // cancel cluster, reset bootstrap
	Observe(now time.Time, e Estimate)
	AllowPadding(now time.Time, bytes int) bool
	BootstrapUsed() int // bytes of bootstrap padding spent (reported)
}

// AllocInput is everything the allocator may look at.
type AllocInput struct {
	Estimate    Estimate
	Costs       Costs
	FEC         float64           // parity ratio from the repair policy
	Applied     Option            // what the forwarder actually forwards
	Operating   *Option           // actual FIRST owner/cap; nil before acquisition or in legacy allocator fixtures
	Transaction *KeyAuthorization // prepared/started key, not completed dwell (transaction-intent D1/D2)
	Generation  uint64
	NativeKey   bool         // Keys.Allowed for this viewer's native upgrade
	Switching   bool         // a native key was requested or is being delivered to this viewer
	Recovering  bool         // an accepted recovery or broken request within RecoveryQuiet
	Probe       *ProbeResult // a current-generation cluster finished since the previous step
}

// Allocation is the allocator's output.
type Allocation struct {
	Desired   Option
	Intent    AllocationIntent
	Candidate Option // next better option worth probing for
	Target    int    // admit(Candidate) in bps; 0 = nothing to probe
	Untenable bool   // hold(low T0) > R for DownTime
}

// Allocator picks the desired option from applied state.
type Allocator interface {
	Decide(now time.Time, in AllocInput) Allocation
	OnApplied(now time.Time, o Option)
	OnGeneration(now time.Time, generation uint64) // invalidate pending upgrades
}

// Pacer schedules one viewer's packets. Outside a cluster departures obey
// bytes ≤ R·elapsed/8 + max(BurstFloor, R·PacerBurst/8). It enforces the
// hard QueueBytes/QueueFrames bounds with whole-frame admission, FrameAge,
// key supersession and the KeyEnvelope key abort; drops are reported once.
//
// Per-viewer memory stays bounded whether or not the writer that runs Step
// makes progress (C2-1): every discarded packet goes to the release hook
// inside the call that discarded it, and the facts Step drains are coalesced.
type Pacer interface {
	SetRate(now time.Time, bps int)
	SetCluster(c *Cluster) // nil ends any cluster
	// SetRelease registers fn, called synchronously with every packet the
	// pacer discards (refused, superseded, purged, expired or aborted), so
	// the caller frees what p.ID maps to and reports a picture whose Last
	// packet is not Superseded to continuity, under the lock it already
	// holds for the mutation.
	SetRelease(fn func(p Packet))
	// Push offers one packet; the first packet of a picture decides
	// whole-frame admission. False means the picture was refused.
	Push(now time.Time, p Packet) bool
	Pop(now time.Time) (Packet, bool)
	NextAt(now time.Time) time.Time                                                   // earliest time Pop may succeed; zero if idle
	RTXDeparture(now time.Time, kind Kind, size, writerPending int) (time.Time, bool) // unknown while transport work is in flight
	PictureService(now time.Time, q PictureQuery) PictureService
	UncommittedKey(r Rendition) bool
	Active() (Rendition, bool) // actual first-key departure ownership, before OnApplied
	Operating() (KeyAuthorization, bool)
	Key(r Rendition) (KeyAuthorization, bool)
	Authorize(KeyAuthorization) bool
	Apply(Option)
	OnGeneration(time.Time, uint64)
	SetCommit(r Rendition, allow bool)
	CancelKey(now time.Time, r Rendition)
	Maintain(now time.Time, writerPending int)
	// Dropped returns the chain-breaking drops since the last call (a
	// non-superseded original key or TL0 picture), at most one per rendition.
	Dropped() []Packet
	// KeyEvents returns key-delivery facts since the last call, coalesced
	// per rendition to the latest key's phases plus its latest abort.
	KeyEvents() []KeyEvent
	KeyInFlight() bool
	QueuedKey(r Rendition) (remaining, total int)
	Queued() (bytes, frames int)
	Purge(r Rendition) // the rendition is no longer forwarded
}

// RepairQuery describes a NACKed packet, or a critical packet TWCC
// reported lost.
type RepairQuery struct {
	Age              time.Duration // since the packet entered the cache
	Sends            int           // retransmissions so far
	Since            time.Duration // since its latest copy (original or retransmission) was sent
	RTT              time.Duration // the estimator's RTT
	PredictedArrival time.Time     // now + RTX queue delay + RTT/2
	Deadline         time.Time     // its frame's useful deadline
	Current          bool          // frame still forwarded, not superseded or aborted, current generation
}

// Repair is the repair policy table.
type Repair interface {
	Resend(q RepairQuery) bool
	// FEC returns the parity ratio and interleave depth for TL0 and keys.
	FEC(e Estimate, nackFeasible bool) (ratio float64, interleave int)
}

// Keys is shared by all viewers of a room. It is the only place that decides
// whether the publisher is asked for a keyframe.
type Keys interface {
	Request(now time.Time, viewer int, r Rendition, c Cause) KeyStatus
	Allowed(now time.Time, viewer int, r Rendition, c Cause) bool
	Due(now time.Time) []Rendition // PLIs to send now (new or retried)
	// OnKey reports an assembled key, requested or not, with its wire bytes
	// and capture time; it satisfies only requests made before that capture.
	OnKey(now time.Time, r Rendition, bytes int, captured time.Time)
	// Size is the admission estimate of r's key wire bytes: the
	// KeySizeQuantile of the last KeySizeSamples keys within KeySizeAge
	// (0 = unknown), and how many keys it rests on.
	Size(now time.Time, r Rendition) (bytes, samples int)
	OnDelivery(viewer int, e KeyEvent)
	Leave(viewer int)
	// Replace is a replacement PC's first request, handed off from the
	// admitted session's previous PC: low, with the session's exception
	// credit (RoomKeys.Replace).
	Replace(now time.Time, viewer, session int) KeyStatus
	// Times reports r's latest PLI emission and key assembly (diagnostics).
	Times(r Rendition) (emitted, assembled time.Time)
}

// Continuity applies the supported VP9 dependency rules for one viewer
// (docs/CONTROLLER.md "Supported VP9 dependency structures"). internal/sfu and
// internal/sfusim both call it per picture, in order.
type Continuity interface {
	Supported(f FrameInfo) bool // false: unsupported structure, no temporal adaptation
	Allow(f FrameInfo, maxTID uint8) bool
	// Unsupported reports that r showed this viewer an unsupported structure,
	// including a stateful one Supported cannot see (a non-flexible pattern or
	// TL0PICIDX violation). It is latched: the forwarder refuses r at once and
	// recovers on the other rendition (C2-4).
	Unsupported(r Rendition) bool
	Lost(f FrameInfo) // picture not delivered completely to this viewer
}

// Need is option o's video wire need: cumulative ingress rate, FEC on the
// protected share only, and expected retransmission.
//
// Parity is charged as the forwarder sends it: fec × the protected bytes,
// plus the partial group each protected picture flushes at its end, at
// most one parity packet less the share the ratio already charged.
func Need(c Costs, o Option, fec, loss float64) int {
	n := float64(c.Rate[o.Rendition][o.TID]) + fec*float64(c.Protected[o.Rendition][o.TID]) + loss*float64(c.Rate[o.Rendition][o.TID])
	if fec > 0 {
		n += (1 - fec) * float64(c.Flush[o.Rendition][o.TID])
	}
	return int(n)
}

// Hold is the total R needed to keep forwarding o.
func Hold(c Costs, o Option, fec, loss float64) int { return c.AudioWire + Need(c, o, fec, loss) }

// Admit is the total R (and current-generation evidence) needed to move to o,
// and the target a probe for o must deliver: audio plus the larger of the
// margined steady need and the estimated key's delivery within KeyEnvelope.
// steady omits the key term; while the key size is unknown they are equal
// and the assembled-key check decides.
func Admit(c Costs, o Option, fec, loss float64) (total, steady int) {
	steady = c.AudioWire + int(UpMargin*float64(Need(c, o, fec, loss)))
	if key := c.KeyBytes[o.Rendition]; key > 0 {
		return max(steady, c.AudioWire+KeyBits(key, fec)*int(time.Second)/int(KeyEnvelope)), steady
	}
	return steady, steady
}

// KeyBits is a key's wire bits including its parity.
func KeyBits(bytes int, fec float64) int { return int(float64(bytes*8) * (1 + fec)) }
