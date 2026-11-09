// Package ctrl is the per-viewer media controller: estimator, prober,
// allocator, pacer, repair policy, key policy and dependency continuity
// (docs/CONTROLLER.md).
//
// Every component is a plain struct driven by an explicit `now`. Nothing here
// imports Pion, reads a clock, starts a goroutine or takes a lock, so
// internal/sfusim drives the same code on a virtual clock. internal/sfu owns
// the wire: RTP parsing, the packet cache, key assembly, SRTP and locking.
//
// SFrame constraint: the SFU sees RTP headers, header extensions and the clear
// VP9 payload descriptor only. Every input below is derivable from those; no
// field may require the encrypted payload or an Opus TOC.
package ctrl

import "time"

// Rendition is one of the publisher's two independent video encodings.
type Rendition uint8

const (
	Native Rendition = iota // full resolution L1T3; SFrame track 0
	Low                     // ~320 px L1T3 fallback; SFrame track 2
)

// SFrameTrack is the SFrame track id the rendition is encrypted under. Opus
// audio is track 1.
func (r Rendition) SFrameTrack() int { return int(r) * 2 }

func (r Rendition) String() string {
	if r == Low {
		return "low"
	}
	return "native"
}

// Option is what a viewer is forwarded: one rendition up to temporal layer TID.
type Option struct {
	Rendition Rendition
	TID       uint8 // highest forwarded temporal layer, 0..2
}

// Options in allocator preference order: fewer frames before fewer pixels.
var Options = [6]Option{{Native, 2}, {Native, 1}, {Native, 0}, {Low, 2}, {Low, 1}, {Low, 0}}

// Kind is a packet's media kind, independent of its pacing class.
type Kind uint8

const (
	KindVideo Kind = iota
	KindAudio
	KindPadding
)

// Class is a packet's pacing priority, highest first.
type Class uint8

const (
	ClassAudio Class = iota
	ClassRTX
	ClassKey // every packet of a keyframe
	ClassTL0
	ClassTL1
	ClassTL2
	ClassFEC
	ClassPadding
)

// Sent describes one packet handed to the transport.
type Sent struct {
	Seq        uint64 // unwrapped transport-wide sequence number (TWCC)
	At         time.Time
	Size       int // bytes on the wire, including RTP/UDP/IP overhead
	Kind       Kind
	Class      Class
	Cluster    int    // probe cluster id; 0 outside a cluster (media and padding)
	Generation uint64 // path generation at send time
}

// Arrival is one TWCC entry. A zero At means the packet was reported lost.
type Arrival struct {
	Seq uint64
	At  time.Time // receiver clock; only differences are meaningful
}

// Feedback is one transport-wide congestion control report, in sequence order.
type Feedback struct{ Arrivals []Arrival }

// ReceiverReport carries RTCP RR fields the estimator may use.
type ReceiverReport struct {
	RTT          time.Duration // zero when unknown
	FractionLost float64
}

// Estimate is the estimator's output.
type Estimate struct {
	Rate       int           // R, bps of total wire (audio, video, repair, padding)
	Proven     int           // current-generation evidence: max acked over AppLimitedLookback or last delay-clean probe result
	Overuse    bool          // delay-slope overuse currently detected
	Congestive bool          // congestive loss within LossWindow
	Loss       float64       // media loss over LossWindow, padding excluded
	LossRun    float64       // mean consecutive-loss run in packets
	RTT        time.Duration // latest RTT, zero when unknown
	Ceiling    int           // wire capacity a saturated clean probe measured this generation; 0 = none
	// Draining: a congestion/drain episode is open, or its post-drain
	// AckedWindow has not passed. No upgrade and no upward probe.
	Draining bool
	// CongestedAt is the latest congestion cut or drain episode; zero if
	// none this generation.
	CongestedAt time.Time
	// Sustained is the evidence a native key is admitted on: Proven, but
	// within AppLimitedLookback of congestion the least acked window and
	// only probe results measured since (C2 §5 A10).
	Sustained int
	// LostAt is when a media packet was last reported lost; zero if none
	// this generation.
	LostAt time.Time
	// KeyRate is a fast-key cluster's delivered rate, kept after R was
	// clamped back to native T2's admit: a key in flight may leave at it.
	// Zero once it expires or congestion or a ceiling followed it.
	KeyRate int
	// Service and Debt, while an episode is open and measured: the
	// bottleneck's service rate for this sender (bps), and the bytes sent
	// and not yet reported, which wait in the network ahead of new work.
	Service, Debt int
}

// ProbeClass is what a finished cluster's measurement may be used for
// (docs/CONTROLLER.md, C2-2). Only ProbeSaturated bounds capacity; only
// ProbeSaturated and ProbeDelivered can prove a target; a clean Rate > 0 of
// any class is delivery evidence.
type ProbeClass uint8

const (
	ProbeDelivered   ProbeClass = iota // complete; delivery kept pace with the sustained offer
	ProbeSaturated                     // complete; the receiver fell measurably behind the sustained offer: bottleneck evidence
	ProbeSendLimited                   // the sender did not sustain the planned offer to the cluster's end
	ProbeUndersized                    // too few acked packets, no sustained bytes beyond one pacer burst, or a zero span
	ProbeMissing                       // some cluster packets were never reported by ProbeTimeout
	ProbeTimedOut                      // no cluster packet was reported by ProbeTimeout
	ProbeDirty                         // delay overuse without saturation evidence
)

func (c ProbeClass) String() string {
	switch c {
	case ProbeDelivered:
		return "delivered"
	case ProbeSaturated:
		return "saturated"
	case ProbeSendLimited:
		return "send-limited"
	case ProbeUndersized:
		return "undersized"
	case ProbeMissing:
		return "missing-feedback"
	case ProbeTimedOut:
		return "timed-out"
	case ProbeDirty:
		return "dirty"
	}
	return "unknown"
}

// ProbeResult is a finished probe cluster.
type ProbeResult struct {
	Cluster    int
	Class      ProbeClass
	Clean      bool // no overuse in the cluster/tail; target failures still prove Rate
	Capped     bool // required loss compensation exceeded the overdrive cap
	Generation uint64
	Target     int // admit(candidate), the rate the cluster tested
	// Rate is D: unique delivered bytes of the acked cluster packets, by the
	// lower of their receive and send dispersion (the send side paced at the
	// planned rate, so no burst credit shrinks it); 0 when not measurable.
	Rate    int
	Success bool
}

// Cluster asks the pacer to send at Rate (media plus padding) until Until.
type Cluster struct {
	ID         int
	Start      time.Time
	Generation uint64
	Rate       int
	Until      time.Time
	// MediaOnly experiments are funded by an already admitted native key
	// (A1/A16): no padding may replace media if its queue empties early.
	MediaOnly bool
}

// Costs are measured at SFU ingress. Rates include RTP/SRTP/UDP/IP headers.
type Costs struct {
	Rate       [2][3]int // cumulative wire bps of rendition r up to TID t
	Protected  [2][3]int // of which TL0 and key bytes (FEC-protected), bps
	Flush      [2][3]int // one parity packet per protected picture, bps: the bound on its partial group
	KeyBytes   [2]int    // Keys.Size: robust estimate of the rendition's key wire bytes; 0 = unknown
	KeySamples [2]int    // keys the estimate rests on
	AudioWire  int       // audio wire bps, including RED and headers
}

// Packet is a pacer entry. ID is opaque to ctrl; the caller maps it to RTP.
type Packet struct {
	ID             uint64
	Generation     uint64 // owning path when the key is authorized; survives to LAST
	Size           int
	Kind           Kind
	Class          Class
	Rendition      Rendition
	Frame          uint64 // per-rendition picture number
	Last           bool   // last packet of its picture (whole-frame admission)
	Packets        int    // packets in its picture
	FrameSize      int    // bytes in its picture
	TID            uint8
	Key            bool
	Switch         bool // switching point: keyframe or retained TL0
	Enqueued       time.Time
	Deadline       time.Time // useful deadline (repair and FEC share their frame's); Pop returns the frame's current one
	FreshUntil     time.Time // immutable original picture age boundary, independent of a reference lease
	Flexible       bool
	Refs           [3]uint64 // VP9 has at most three explicit P_DIFF references
	RefCount       uint8
	ProtectedClass Class // FEC inherits its protected group priority
	Superseded     bool  // replacement/purge drops do not request recovery
	Cluster        int   // set by the pacer during a cluster
}

func (p *Packet) SetReferences(f FrameInfo) {
	p.Flexible = f.Flexible
	p.RefCount = uint8(min(len(f.Refs), len(p.Refs)+1)) // oversized input stays invalid
	copy(p.Refs[:], f.Refs)
}

// PictureQuery forecasts observed metadata only. Offer is an optional complete
// candidate transaction; no future source pictures or physical writes are invented.
type PictureQuery struct {
	Rendition     Rendition
	Frame         uint64
	Generation    uint64
	WriterPending int
	Offer         []Packet
	Commit        bool // forecast the candidate's transactional start, not a deferred key
	Authorization *KeyAuthorization
}

type PictureDeparture struct {
	Frame                 uint64
	At, FreshUntil, Lease time.Time
	Key                   bool
}

type PictureService struct {
	Generation uint64
	Known      bool // complete observed scheduling inputs, no unresolved transport writes
	Broken     bool // the observed key/base chain expires or aborts
	Key        time.Time
	FirstFresh time.Time // zero if no known dependency-complete fresh output
	Pictures   []PictureDeparture
}

type SwitchOutcome uint8

const (
	SwitchIdle SwitchOutcome = iota
	SwitchMeasuring
	SwitchDemonstrated
	SwitchConstrained
	SwitchUnmeasured
)

// FrameInfo is one picture's clear VP9 descriptor, as continuity sees it.
type FrameInfo struct {
	Rendition Rendition
	Picture   uint64 // unwrapped picture id
	TID       uint8
	SID       uint8
	Key       bool
	Flexible  bool
	Refs      []uint64 // flexible mode: referenced pictures (picture − P_DIFF)
	TL0       uint8    // TL0PICIDX (non-flexible)
}

// Cause is why a viewer needs a keyframe.
type Cause uint8

const (
	CauseJoin     Cause = iota // first picture of a viewer session (starts on low)
	CauseUpgrade               // allocator moving to native, including a session's first upgrade
	CauseRecovery              // viewer decoder reported a stall or PLI
	CauseBroken                // pacer drop or abort of a base picture, or SFU-detected broken base
)

// KeyStatus is the key policy's answer to a request.
type KeyStatus uint8

const (
	KeyAccepted   KeyStatus = iota // a PLI goes out now
	KeyPending                     // shares the next budgeted key
	KeyRedirected                  // served on low instead
	KeyIgnored                     // recovery within RecoveryQuiet of the previous one
)

// Decision is the forward loop's output for the forwarder.
type Decision struct {
	Desired    Option // the forwarder applies it at a key or switching point and reports OnApplied
	Intent     AllocationIntent
	Key        KeyStatus // status of the key request this step made, if Requested
	Requested  bool
	Rate       int // pacer rate, bps (R outside clusters)
	Cluster    *Cluster
	FEC        float64 // parity ratio for TL0 and key packets
	Interleave int     // FEC interleave depth
	Untenable  bool    // even low T0 does not hold: slide-mode trigger
	// Stop: a drain episode is open, so a forwarded rendition other than
	// the desired one is obsolete. The forwarder stops it at once (purging
	// its backlog, as a refused key does) and forwards nothing of it until
	// the desired rendition's key.
	Stop bool
}

// Config is per-viewer configuration.
type Config struct {
	MaxRate           int     // bps; default 50 Mbit/s
	ProbeMaxOverdrive float64 // F-1: maximum offered/target rate, default 1.5
}

// RateState hands R to a replacement PC; it carries no admission evidence.
// Session is the stable identity of the admitted session behind the PC
// (the first PC's viewer ID): a replacement's low-key exception credit lives
// there, not at the new PC's ID.
type RateState struct {
	Rate    int
	Session int
	// Congested: the previous PC was draining network debt or congested.
	// The replacement inherits no evidence, so it starts in a drain
	// episode that ends only once its own delay trend is stable: recent
	// congestion imposes the stability requirement (C2 §5 A5/A10).
	Congested bool
	// Ceiling: the previous PC's live measured capacity bound. It is not
	// admission evidence; it caps the replacement's R and probe offers as
	// a ceiling does, so a replacement does not probe far past a path the
	// previous PC just measured (C2 §5 A10: cap offers by sustainable
	// evidence).
	Ceiling int
}

// KeyPhase and KeyEvent are per-viewer forwarder facts, not publisher capture.
type KeyPhase uint8

const (
	KeyAdmitted KeyPhase = iota
	KeyStarted
	KeyCompleted
	KeyAborted
	KeyCancelled  // unstarted switch lost eligibility; no decoder chain was interrupted
	KeySuperseded // a ready replacement key retires old delivery without new recovery
)

type KeyEvent struct {
	Rendition  Rendition
	Frame      uint64
	Phase      KeyPhase
	At         time.Time
	Generation uint64
}

// AllocationIntent labels the selecting branch, never a comparison with lagging
// Applied. D1/D2 and the real-degradation F1 controls own this structural enum.
type AllocationIntent uint8

const (
	Retain AllocationIntent = iota
	Upgrade
	Required
)

// KeyAuthorization is one bounded key identity/cap, retained at FIRST through
// LAST. Previous distinguishes a cross-rendition candidate from a refresh.
type KeyAuthorization struct {
	Generation uint64
	Rendition  Rendition
	Frame      uint64
	Option     Option
	Intent     AllocationIntent
	Previous   Rendition
	Phase      KeyPhase
}
