package sfu

import (
	"encoding/binary"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"io"
	"log"
	"maps"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Pacer charges media, parity and replay to one wire-rate token bucket.
// Idle credit is at most 100 ms of wire budget or four MTUs, plus one packet of debt.
// Complete pictures are queued; a started picture finishes at the budget rate.
// Idle pacers sleep until queue arrival, and transport writes hold no queue lock.
type Pacer struct {
	pictures                                        pictureHistory
	mu                                              sync.Mutex
	writers                                         map[uint32]interceptor.RTPWriter
	queue                                           []pacedPacket
	bytes                                           int
	rate                                            atomic.Int64
	effective                                       atomic.Int64
	networkExcess                                   atomic.Int64
	temporal                                        atomic.Int32
	thumbnail                                       atomic.Bool
	fullFrames                                      [16]uint32
	fullN, fullHead                                 int
	mediaPT                                         atomic.Int32
	redPT                                           atomic.Int32
	fecPT                                           atomic.Int32
	twccID                                          atomic.Int32
	twccSeq                                         uint16
	pathEpoch                                       atomic.Uint64
	departures                                      [2048]departure
	feedbackRate, feedbackRateAt                    atomic.Int64
	receiptRate                                     atomic.Int64
	sustainedRate                                   atomic.Int64
	sustainedRateAt                                 atomic.Int64
	returnRate, returnRateAt                        atomic.Int64
	feedbackDuplicates                              atomic.Uint64
	trialDelivery                                   deliveryWindow
	trialDeliveryID                                 int64
	trialReceived, trialReceivedAt, trialReceivedID atomic.Int64
	trialLossID                                     int64
	trialLossFor                                    atomic.Int64
	trialLossPPM                                    atomic.Uint32
	trialLossAt                                     atomic.Int64
	trialLossSamples                                mediaLossWindow
	mediaLossPPM                                    atomic.Uint32
	mediaLossAt                                     atomic.Int64
	parityLossAt                                    atomic.Int64
	severeParityLossAt                              atomic.Int64
	mediaLossSamples                                mediaLossWindow
	feedbackEpoch                                   uint64
	feedbackHigh                                    uint16
	feedbackSeen                                    bool
	stableFlightRate                                atomic.Int64
	stableFlightAt                                  atomic.Int64
	stableFlightRTT                                 atomic.Int64
	flightDemand                                    atomic.Int64
	wireReservation                                 atomic.Int64
	delivery                                        deliveryWindow
	transitAt, transitDelay                         atomic.Int64
	transitReports                                  atomic.Uint64
	transitSendAt                                   int64
	transitReceivedAt                               time.Duration
	probeRate, probeRateAt                          atomic.Int64
	departedBytes                                   atomic.Int64
	acknowledgedBytes                               atomic.Int64
	minimumFeedbackRTT                              atomic.Int64
	boundFlight                                     atomic.Bool
	smoothWire                                      atomic.Bool
	probeCeiling                                    atomic.Int64
	Drops                                           atomic.Uint64
	done                                            chan struct{}
	wake                                            chan struct{}
	rtt                                             atomic.Int64
	pliRTT                                          atomic.Int64
	Suppressed                                      atomic.Uint64
	FeedbackRepairs                                 atomic.Uint64
	once                                            sync.Once
	pending                                         []pacedPacket
	pendingBytes                                    int
	Errors                                          atomic.Uint64
	wireBytes                                       atomic.Uint64
	videoWireBytes, audioWireBytes                  atomic.Uint64
	repairBytes                                     atomic.Uint64
	paddingBytes                                    atomic.Uint64
	probeUntil                                      atomic.Int64
	probeID, probeEnd                               atomic.Int64
	failedTrial                                     atomic.Int64
	unrepairableReferenceAt                         atomic.Int64
	OnDrop                                          func()
	waitKey                                         bool
	waitTemporal                                    bool
	active                                          atomic.Bool
	activeKey                                       atomic.Bool
	activeTimestamp                                 atomic.Uint32
	pliAfter                                        atomic.Int64
	graceBytes                                      atomic.Int64
	catchupAt                                       atomic.Int64
	graceRate                                       atomic.Int64
	keyframeBytes                                   atomic.Int64
	replayPending                                   map[uint16]bool
	wireSeq                                         uint16
	lastWire                                        atomic.Uint32
	mapping                                         [2048]sequenceMapping
	cache                                           packetCache
	audio                                           pacerAudio // opaque audio lane (audio.go)
}
type sequenceMapping struct {
	input, output uint16
	valid         bool
}

type retained interface {
	Retain()
	Release(bool)
}

// Media kind stays explicit in wire and feedback accounting. The future Opus
// lane charges the common bucket but cannot establish received video proof.
type mediaKind uint32

const (
	unknownKind mediaKind = iota
	videoKind
	audioKind
)

type pacedPacket struct {
	picture                uint64
	pictureKnown, flexible bool
	references             [3]uint8
	referenceCount         uint8
	admittedFull           bool
	kind                   mediaKind
	h                      rtp.Header
	p                      []byte
	a                      interceptor.Attributes
	at                     time.Time
	deadline               time.Time
	refs                   []retained
	key                    bool
	keyPacket              bool
	end                    bool
	repair                 bool
	fec                    bool
	padding                bool
	tid                    uint8
	temporalRecovery       bool
	up                     bool
}

// Only low-rendition upgrade trials use padding. It carries no encoded media,
// and a five-millisecond idle credit keeps trials from borrowing a keyframe burst.
func (p *Pacer) paddingPacket(now time.Time) (pacedPacket, bool) {
	// Allow one complete discovery trial, then earn one padding byte per four
	// video bytes. A fixed byte allowance can stop just below an affordable
	// native key's rate and make a second proof permanently impossible.
	wire, padding := p.videoWireBytes.Load(), p.paddingBytes.Load()
	video := wire - min(wire, padding)
	discovery := uint64(max(256*1024, p.probeCeiling.Load()*int64(capacityTrialDuration)/int64(time.Second)/8))
	if padding >= discovery+video/4 || time.Duration(p.networkExcess.Load()) >= standingDelayLimit || now.UnixNano() >= p.probeUntil.Load() || p.twccID.Load() <= 0 || p.mediaPT.Load() < 0 {
		return pacedPacket{}, false
	}
	for ssrc := range p.writers {
		h := rtp.Header{Version: 2, Padding: true, PaddingSize: 255, SSRC: ssrc, PayloadType: uint8(p.mediaPT.Load()), Timestamp: p.activeTimestamp.Load()}
		_ = h.SetExtension(uint8(p.twccID.Load()), []byte{0, 0})
		return pacedPacket{kind: videoKind, h: h, at: now, end: true, padding: true}, true
	}
	return pacedPacket{}, false
}

const wireMTU = 1500

// Carry up to two milliseconds of timer lateness. Padding packets are only
// 255 bytes: resetting their schedule on every 1ms wake caps trials near
// 2.5Mbit/s irrespective of the requested rate. Catch-up remains tiny and
// every packet is also charged to the common wire bucket.
const pacingQuantum = 2 * time.Millisecond

func wireBurstBytes(rate int64) int64 { return max(4*wireMTU, rate/80) }

// A keyframe can take several seconds at a constrained viewer's wire rate.
// Keep its dependent pictures through that serialization so expiry cannot
// cause an endless keyframe loop. Memory is still capped by queue admission.
const maxKeyframeCatchup = 6 * time.Second

// IPv4 + UDP + SRTP authentication and typical transport extensions.
const wireOverhead = 50

func wireSize(x pacedPacket) int {
	return x.h.MarshalSize() + len(x.p) + int(x.h.PaddingSize) + wireOverhead
}

type wireBudget struct {
	credit float64
	last   time.Time
}

func (b *wireBudget) accrue(now time.Time, rate int64) {
	b.credit = min(b.credit+float64(rate)*now.Sub(b.last).Seconds()/8, float64(wireBurstBytes(rate)))
	b.last = now
}
func (p *Pacer) notify() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// QueueDelay measures non-keyframe backlog for layer admission. The active
// keyframe is serialized at the budget rate and has a bounded catch-up window.
func (p *Pacer) QueueDelay(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 || p.queue[0].key || p.activeKey.Load() || (p.graceBytes.Load() > 0 && now.Sub(time.Unix(0, p.catchupAt.Load())) < maxKeyframeCatchup) {
		return 0
	}
	return now.Sub(p.queue[0].at)
}

func (p *Pacer) KeyframePending() bool {
	if p.activeKey.Load() {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, x := range p.queue {
		if x.keyPacket {
			return true
		}
	}
	return false
}

func (p *Pacer) SetThumbnail(low bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if low != p.thumbnail.Load() {
		p.pictures = pictureHistory{}
	}
	if low {
		p.boundFlight.Store(true)
	}
	if p.thumbnail.Swap(low) != low {
		p.fullN, p.fullHead = 0, 0
		// The new independent keyframe replaces the old decoder references.
		// Finishing a large native picture first would pin the low-rate switch.
		for len(p.queue) > 0 {
			p.discard()
		}
		for _, x := range p.pending {
			for _, r := range x.refs {
				r.Release(false)
			}
		}
		p.pending, p.pendingBytes = nil, 0
		p.active.Store(false)
		p.activeKey.Store(false)
		p.waitKey = true
		p.waitTemporal = false
		p.graceBytes.Store(0)
		p.graceRate.Store(0)
		p.pliAfter.Store(0)
		p.cache.keyframe(uint16(p.lastWire.Load()) + 1)
	}
}

func (p *Pacer) SetTemporal(layer uint8) {
	if layer == 2 && p.temporal.Load() == 2 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.temporal.Store(int32(layer))
	if layer < 2 {
		p.fullN = 0
		p.fullHead = 0
	}
}
func (p *Pacer) recordFullFrame(timestamp uint32, admitted ...bool) {
	full := p.temporal.Load() == 2 || len(admitted) > 0 && admitted[0]
	if !full || p.thumbnail.Load() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !full || p.thumbnail.Load() || p.waitTemporal {
		return
	}
	p.fullFrames[p.fullHead] = timestamp
	p.fullHead = (p.fullHead + 1) % len(p.fullFrames)
	p.fullN = min(p.fullN+1, len(p.fullFrames))
}
func (p *Pacer) FullLayerFrames() []uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]uint32, p.fullN)
	for i := range out {
		out[i] = p.fullFrames[(p.fullHead-p.fullN+i+len(p.fullFrames))%len(p.fullFrames)]
	}
	return out
}

// CatchupDelay reports actual queued serialization, not a network-delay waiver.
func (p *Pacer) CatchupDelay(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.graceBytes.Load() == 0 || now.Sub(time.Unix(0, p.catchupAt.Load())) >= maxKeyframeCatchup || len(p.queue) == 0 {
		return 0
	}
	rate := p.wireRate(now)
	rate = p.catchupCeiling(rate, now)
	return min(maxKeyframeCatchup, max(time.Millisecond, time.Duration(float64(p.bytes)*8/float64(rate)*float64(time.Second))))
}

// The flight window can drain a large refresh more slowly than its numerical
// wire ceiling. Deadline grace must cover that actual bounded drain rate.
func (p *Pacer) catchupCeiling(rate int64, now time.Time) int64 {
	if p.boundFlight.Load() && !p.thumbnail.Load() && p.minimumFeedbackRTT.Load() > 0 && p.acknowledgedBytes.Load() > 0 {
		if delivered := p.flightCapacity(now); delivered > 0 {
			rate = min(rate, delivered*5/4)
		}
	}
	return max(32_000, rate)
}

func NewPacer() *Pacer {
	p := &Pacer{writers: map[uint32]interceptor.RTPWriter{}, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	p.mediaPT.Store(-1)
	p.redPT.Store(-1)
	p.fecPT.Store(-1)
	p.SetTargetBitrate(2_000_000)
	go p.run()
	return p
}
func (p *Pacer) SetTargetBitrate(n int) { p.rate.Store(int64(max(n, 32_000))) }

func (p *Pacer) wireRate(now time.Time) int64 {
	rate := p.rate.Load()
	if effective := p.effective.Load(); effective > 0 {
		rate = effective
	}
	if now.UnixNano() < p.probeUntil.Load() {
		if ceiling := p.probeCeiling.Load(); ceiling > 0 {
			rate = min(rate, ceiling)
		}
	} else if proof := p.probeRate.Load(); proof > 0 {
		// Application-limited ceilings cannot become line-rate native bursts.
		// Only a bounded explicit trial may exceed proven video delivery.
		rate = min(rate, max(128000, proof*110/100))
	} else if p.thumbnail.Load() {
		// Unknown low paths cannot spend an unused room ceiling between
		// bounded trials, including immediately after a PC replacement.
		rate = min(rate, initialWireRate)
	}
	return rate
}

// Discovery may offer padding above proof, but encoded video never borrows
// that exploratory rate. Both still charge the same aggregate wire bucket.
func (p *Pacer) mediaRate(now time.Time) int64 {
	rate := p.wireRate(now)
	if proof := p.probeRate.Load(); proof > 0 {
		rate = min(rate, max(128000, proof*110/100))
	} else if p.thumbnail.Load() || now.UnixNano() < p.probeUntil.Load() {
		rate = min(rate, initialWireRate)
	}
	return rate
}
func (p *Pacer) AddStream(ssrc uint32, w interceptor.RTPWriter) {
	p.mu.Lock()
	p.writers[ssrc] = w
	p.mu.Unlock()
}
func (p *Pacer) Write(h *rtp.Header, payload []byte, a interceptor.Attributes) (int, error) {
	if p.audio.is(h) {
		return p.writeAudio(h, payload, a)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.notify()
	select {
	case <-p.done:
		return 0, io.ErrClosedPipe
	default:
	}
	now := time.Now()
	n := h.MarshalSize() + len(payload)

	item := pacedPacket{admittedFull: p.temporal.Load() == 2 && !p.thumbnail.Load(), kind: videoKind, h: h.Clone(), p: append([]byte(nil), payload...), at: now}
	if len(a) > 0 {
		item.a = maps.Clone(a)
	}
	// NACK cache attributes carry reference-counted buffers. Preserve their lifetime
	// across asynchronous scheduling without importing Pion's internal package.
	for _, v := range a {
		if r, ok := v.(retained); ok {
			r.Retain()
			item.refs = append(item.refs, r)
		}
	}
	var referenceBuffer [3]uint8
	vp := codecs.VP9Packet{PDiff: referenceBuffer[:0]}
	media := payload
	video := int32(h.PayloadType) == p.mediaPT.Load()
	fec := false
	if int32(h.PayloadType) == p.redPT.Load() && len(media) > 0 {
		video = true
		if int32(media[0]) == p.fecPT.Load() {
			fec = true
		} else {
			media = media[1:]
		}
	}
	_, err := vp.Unmarshal(media)
	item.end = fec || !video || h.Marker || err != nil
	item.fec = fec
	item.tid = vp.TID
	item.temporalRecovery = video && !fec && err == nil && vp.L && !vp.F && vp.SID == 0
	item.up = vp.U
	item.key = video && !fec && err == nil && vp.B && !vp.P
	item.repair, _ = a["passstream.repair"].(bool)
	item.keyPacket, _ = a["passstream.keyRepair"].(bool)
	item.deadline, _ = a["passstream.deadline"].(time.Time)
	if item.repair {
		if p.replayPending[item.h.SequenceNumber] {
			for _, r := range item.refs {
				r.Release(false)
			}
			p.Suppressed.Add(1)
			return n, nil
		}
		for len(p.queue) > 0 && (p.bytes+n > 2<<20 || len(p.queue) >= 2048) {
			if p.active.Load() {
				for _, r := range item.refs {
					r.Release(false)
				}
				p.Suppressed.Add(1)
				return n, nil
			}
			p.discardFrame()
		}
		item.end = true
		if item.keyPacket {
			p.pliAfter.Store(max(p.pliAfter.Load(), now.Add(min(replayAge, 2*time.Duration(p.rtt.Load()))).UnixNano()))
		}
		// Replay is admitted ahead of queued media, then scheduled by deadline, including the remainder
		// of a keyframe. RTP replay can interleave a started picture safely.
		if p.replayPending == nil {
			p.replayPending = make(map[uint16]bool)
		}
		p.replayPending[item.h.SequenceNumber] = true
		rate := p.wireRate(now)
		deadline := p.packetDeadline(item, rate)
		i := 0
		for i < len(p.queue) && p.queue[i].repair && !deadline.Before(p.packetDeadline(p.queue[i], rate)) {
			i++
		}
		p.queue = append(p.queue, pacedPacket{})
		copy(p.queue[i+1:], p.queue[i:])
		p.queue[i] = item
		p.bytes += n
		return n, nil
	}
	// Deferred parity belongs to an older picture and can arrive between the
	// current picture's packets. It must never flush or join media assembly.
	// Discard expendable parity rather than evicting video at the queue bound.
	if fec {
		if p.bytes+p.pendingBytes+n > 2<<20 || len(p.queue)+len(p.pending) >= 2048 {
			for _, r := range item.refs {
				r.Release(false)
			}
			p.Suppressed.Add(1)
			return n, nil
		}
		p.queue = append(p.queue, item)
		p.bytes += n
		return n, nil
	}
	if len(p.pending) > 0 && p.pending[0].h.Timestamp != h.Timestamp {
		p.dropPending()
	}
	if video && !fec && (err != nil || (len(p.pending) == 0 && !vp.B)) {
		for _, r := range item.refs {
			r.Release(false)
		}
		p.Drops.Add(1)
		return n, nil
	}
	if len(p.pending) > 0 {
		first := p.pending[0]
		item.tid, item.temporalRecovery, item.up = first.tid, first.temporalRecovery, first.up
		item.picture, item.pictureKnown, item.flexible = first.picture, first.pictureKnown, first.flexible
		item.references, item.referenceCount = first.references, first.referenceCount
	} else if video && err == nil && vp.I && vp.SID == 0 {
		mask := uint16(0x7fff)
		if len(media) > 1 && media[1]&0x80 == 0 {
			mask = 0x7f
		}
		item.picture = p.pictures.sequence(vp.PictureID, mask)
		item.pictureKnown = true
		item.flexible = vp.F && (!vp.P || len(vp.PDiff) > 0)
		item.referenceCount = uint8(copy(item.references[:], vp.PDiff))
	}
	p.pending = append(p.pending, item)
	p.pendingBytes += n
	for len(p.queue) > 0 && !p.active.Load() && (p.bytes+p.pendingBytes > 2<<20 || len(p.queue)+len(p.pending) > 2048) {
		p.discardFrame()
	}
	if p.bytes+p.pendingBytes > 2<<20 || len(p.queue)+len(p.pending) > 2048 {
		p.dropPending()
		return n, nil
	}
	if item.end {
		first := p.pending[0]
		if len(p.pending) > 0 && p.pending[0].key {
			for i := range p.pending {
				p.pending[i].keyPacket = true
			}
			// A newer keyframe supersedes every unstarted picture. Preserve only
			// the remainder of a picture already on the wire; never build a FIFO
			// of deadline-exempt keyframes behind a constrained transport.
			keep := 0
			if p.active.Load() {
				for keep < len(p.queue) {
					if p.queue[keep].repair {
						keep++
						continue
					}
					if p.queue[keep].h.Timestamp != p.activeTimestamp.Load() {
						break
					}
					end := p.queue[keep].end
					keep++
					if end {
						break
					}
				}
			}
			prefix := p.queue[:keep]
			rest := p.queue[keep:]
			p.queue = rest
			for len(p.queue) > 0 {
				p.discard()
			}
			p.queue = prefix

			p.keyframeBytes.Store(int64(p.pendingBytes))
		}
		rate := p.catchupCeiling(p.wireRate(now), now)
		wire := 0
		for _, x := range p.pending {
			wire += wireSize(x)
		}
		serialization := keySerialization(int64(wire), rate)
		// A reference delta can be as large as a key (for example after a
		// shared capture refresh). Apply the same serialization envelope;
		// a byte threshold or a 100ms deadline must not sever its dependents.
		burst := video && first.pictureKnown && first.tid < 2 && serialization > 100*time.Millisecond && serialization <= keyEnvelope
		if first.key || burst {
			for _, x := range p.queue {
				wire += wireSize(x)
			}
			serialization = keySerialization(int64(wire), rate)
			rtt := time.Duration(p.pliRTT.Load())
			if rtt == 0 {
				rtt = time.Duration(p.rtt.Load())
			}
			if rtt == 0 {
				rtt = 50 * time.Millisecond
			}
			p.pliAfter.Store(now.Add(serialization + 2*rtt).UnixNano())
			// Preserve dependent pictures during one keyframe's serialization,
			// including a mid-frame rate decrease. After catch-up the queue
			// returns to 100ms expiry; catch-up grace is bounded at six seconds.
			if first.key || p.graceBytes.Load() == 0 {
				p.catchupAt.Store(now.UnixNano())
				p.graceRate.Store(rate)
			}
			p.graceBytes.Store(max(p.graceBytes.Load(), int64(wire)))
		}
		for len(p.queue) > 0 && (p.bytes+p.pendingBytes > 2<<20 || len(p.queue)+len(p.pending) > 2048) {
			if p.active.Load() {
				p.dropPending()
				return n, nil
			}
			p.discardFrame()
		}
		p.queue = append(p.queue, p.pending...)
		p.bytes += p.pendingBytes
		p.pending = nil
		p.pendingBytes = 0
	}
	return n, nil
}
func (p *Pacer) discard() { p.discardAt(0) }
func (p *Pacer) take(i int) pacedPacket {
	x := p.queue[i]
	if i == 0 {
		p.queue[0] = pacedPacket{}
		p.queue = p.queue[1:]
	} else {
		copy(p.queue[i:], p.queue[i+1:])
		p.queue[len(p.queue)-1] = pacedPacket{}
		p.queue = p.queue[:len(p.queue)-1]
	}
	p.bytes -= x.h.MarshalSize() + len(x.p)
	return x
}
func (p *Pacer) discardAt(i int) {
	x := p.take(i)
	if x.repair {
		delete(p.replayPending, x.h.SequenceNumber)
	}
	for _, r := range x.refs {
		r.Release(false)
	}
	if !x.padding {
		p.Drops.Add(1)
	}
}

// Deadline ordering keeps a NACK burst from expiring fresh base pictures.
// A long paced keyframe still yields to replay before its usefulness expires.
func (p *Pacer) packetDeadline(x pacedPacket, rate int64) time.Time {
	if x.repair && !x.deadline.IsZero() {
		return x.deadline.Add(-time.Duration(p.rtt.Load()) / 2)
	}
	if x.fec {
		return x.at.Add(100 * time.Millisecond)
	}
	rate = p.catchupCeiling(rate, time.Now())
	if slowest := p.graceRate.Load(); slowest > 0 {
		rate = min(rate, slowest)
	}
	// A stream near its ceiling drains keyframe debt using only its spare
	// capacity. One serialization interval alone expires valid references
	// before that catch-up completes. Bound this allowance by time and memory.
	grace := min(maxKeyframeCatchup, time.Duration(float64(p.graceBytes.Load())*8*4/float64(rate)*float64(time.Second)))
	return x.at.Add(100*time.Millisecond + grace)
}
func (p *Pacer) nextPacket(rate int64) int {
	if len(p.queue) == 0 || !p.queue[0].repair {
		return 0
	}
	i := 0
	for i < len(p.queue) && p.queue[i].repair {
		i++
	}
	if i < len(p.queue) && p.packetDeadline(p.queue[i], rate).Before(p.packetDeadline(p.queue[0], rate)) {
		return i
	}
	return 0
}

// Non-flexible L1T3 keeps independent base references after a TID1 expiry.
// Recover at an actual retained switching point instead of repeatedly forcing
// room-wide keys. Unknown dependency structures still require a keyframe.
func (p *Pacer) lostPicture(x pacedPacket) {
	if x.repair || x.fec || x.padding {
		return
	}
	if !x.key && x.flexible && x.tid > 0 {
		// Future flexible pictures decide from their own references; a lost
		// enhancement does not invalidate an independent native base.
		p.pictures.delivered[x.picture%128] = 0
		return
	}
	if !x.key && x.tid == 1 && x.temporalRecovery {
		p.waitTemporal = true
		p.fullN, p.fullHead = 0, 0
		return
	}
	if x.tid < 2 {
		if os.Getenv("PASSSTREAM_TRACE_BUDGET") == "1" {
			payload := x.p
			if int32(x.h.PayloadType) == p.redPT.Load() && len(payload) > 0 {
				payload = payload[1:]
			}
			var descriptor codecs.VP9Packet
			_, _ = descriptor.Unmarshal(payload)
			log.Printf("media pacer reference drop tid=%d key=%v age=%s bytes=%d grace=%d rate=%d L=%v F=%v SID=%d", x.tid, x.key, time.Since(x.at), p.bytes, p.graceBytes.Load(), p.wireRate(time.Now()), descriptor.L, descriptor.F, descriptor.SID)
		}
		p.waitKey = true
		if p.OnDrop != nil {
			p.OnDrop()
		}
	}
}

func (p *Pacer) allowTemporalPicture(x pacedPacket) bool {
	if x.flexible && !x.key {
		return p.pictures.contains(x.picture, x.references, x.referenceCount)
	}
	if x.key || x.temporalRecovery && x.up && x.tid == 0 {
		p.waitTemporal = false
	}
	return !p.waitTemporal || x.tid == 0
}

func (p *Pacer) discardFrameAt(i int) {
	if i == 0 {
		p.discardFrame()
		return
	}
	first := p.queue[i]
	ts := p.queue[i].h.Timestamp
	end := i
	for end < len(p.queue) && p.queue[end].h.Timestamp == ts {
		x := p.queue[end]
		p.bytes -= x.h.MarshalSize() + len(x.p)
		if x.repair {
			delete(p.replayPending, x.h.SequenceNumber)
		}
		for _, r := range x.refs {
			r.Release(false)
		}
		end++
		if x.end {
			break
		}
	}
	p.Drops.Add(uint64(end - i))
	remaining := copy(p.queue[i:], p.queue[end:])
	clear(p.queue[i+remaining:])
	p.queue = p.queue[:i+remaining]
	p.lostPicture(first)
}
func (p *Pacer) dropPending() {
	if len(p.pending) > 0 {
		p.lostPicture(p.pending[0])
	}
	for _, x := range p.pending {
		for _, r := range x.refs {
			r.Release(false)
		}
		p.Drops.Add(1)
	}
	p.pending = nil
	p.pendingBytes = 0
}
func (p *Pacer) discardFrame() {
	if len(p.queue) == 0 {
		return
	}
	first := p.queue[0]
	ts := p.queue[0].h.Timestamp
	for len(p.queue) > 0 && p.queue[0].h.Timestamp == ts {
		end := p.queue[0].end
		p.discard()
		if end {
			break
		}
	}
	p.lostPicture(first)
}
func (p *Pacer) run() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	budget := wireBudget{last: time.Now(), credit: float64(wireBurstBytes(p.rate.Load()))}
	active := false
	lastFlightProbe := time.Time{}
	var nextWire time.Time
	for {
		select {
		case <-p.done:
			return
		case <-p.wake:
		case <-timer.C:
		}
		for {
			now := time.Now()
			rate := p.wireRate(now)
			if slowest, drain := p.graceRate.Load(), p.catchupCeiling(rate, now); slowest > drain {
				p.graceRate.CompareAndSwap(slowest, drain)
			}
			budget.accrue(now, rate)
			p.mu.Lock()
			// Audio first while its priority lasts, from the same budget (audio.go).
			if p.audioFirst(now, rate) {
				if budget.credit <= 0 {
					p.mu.Unlock()
					timer.Reset(max(time.Millisecond, time.Duration(-budget.credit*8/float64(rate)*float64(time.Second))))
					break
				}
				spare := p.audio.priority.credit < float64(wireSize(p.audio.queue[0]))
				x, w := p.popAudio()
				p.mu.Unlock()
				n := float64(p.sendAudio(w, x))
				budget.credit -= n
				p.mu.Lock()
				p.audio.priority.credit -= n
				p.mu.Unlock()
				if spare {
					p.audio.Spare.Add(1)
				}
				continue
			}
			choice := 0
			for len(p.queue) > 0 {
				choice = p.nextPacket(rate)
				x := p.queue[choice]
				lateRepair := (x.repair || x.fec) && ((!x.deadline.IsZero() && now.Add(time.Duration(p.rtt.Load())/2).After(x.deadline)) || x.fec && now.Sub(x.at) > 100*time.Millisecond)
				if lateRepair {
					if x.repair && replayReference(&rtp.Packet{Header: x.h, Payload: x.p}, p.mediaPT.Load(), p.redPT.Load()) {
						p.unrepairableReferenceAt.Store(now.UnixNano())
					}
					p.discardAt(choice)
					p.Suppressed.Add(1)
					continue
				}
				if !active && !x.repair && !x.fec && !x.padding && !p.allowTemporalPicture(x) {
					p.discardFrameAt(choice)
					continue
				}
				if !active && !x.key && !x.repair && !x.fec && !x.padding && (p.waitKey || now.After(p.packetDeadline(x, rate))) {
					p.discardFrameAt(choice)
					continue
				}
				break
			}
			if len(p.queue) == 0 {
				// An encoder pause may separate the key from its dependent
				// burst. Do not revoke the one-second admission envelope at
				// the instant its key's last packet leaves the queue.
				if now.Sub(time.Unix(0, p.catchupAt.Load())) >= keyEnvelope {
					p.graceBytes.Store(0)
					p.graceRate.Store(0)
				}
				if p.audioReady(now) {
					p.mu.Unlock()
					continue
				}
				if padding, ok := p.paddingPacket(now); ok {
					p.queue = append(p.queue, padding)
					p.bytes += padding.h.MarshalSize()
					choice = 0
				} else {
					p.mu.Unlock()
					break
				}
			}
			if budget.credit <= 0 {
				p.mu.Unlock()
				delay := time.Duration(-budget.credit * 8 / float64(rate) * float64(time.Second))
				timer.Reset(max(time.Millisecond, delay))
				break
			}
			if p.smoothWire.Load() && now.Before(nextWire) {
				p.mu.Unlock()
				timer.Reset(max(time.Millisecond, nextWire.Sub(now)))
				break
			}
			// A rate ceiling alone cannot bound the first burst after a path
			// narrows. Keep at most one feedback interval plus the original RTT
			// in flight. A sparse probe lets feedback resume after an outage.
			// Small native deltas already share the rate bucket and 100ms
			// queue deadline. Waiting for an old picture's feedback here can
			// expire their references on a resident FIFO and force another key.
			// Bound independent refreshes and capacity padding, plus all large
			// native trains, without imposing that wait on steady small media.
			pacingRate, flightRate := rate, rate
			if p.queue[choice].kind == videoKind && !p.queue[choice].padding {
				pacingRate = p.mediaRate(now)
				flightRate = pacingRate
				// A refresh spends proven capacity, not the source's steady
				// demand. Capping flight at that demand would throttle every
				// admitted key to one second even on an already proven fast path.
				if !p.queue[choice].keyPacket {
					if reservation := p.wireReservation.Load(); reservation > 0 {
						flightRate = min(flightRate, reservation)
					}
				}
			}
			flightBound := p.queue[choice].kind == videoKind && (p.queue[choice].padding || !p.thumbnail.Load() && (p.queue[choice].key || p.boundFlight.Load())) && p.flightLimited(flightRate, p.queue[choice].padding)
			if flightBound && (p.queue[choice].padding || now.Sub(lastFlightProbe) < 100*time.Millisecond) {
				if p.queue[choice].padding {
					// Padding cannot hold fresh low-rate media behind its own
					// outstanding trial. New media still obeys the wire bucket.
					p.discardAt(choice)
				}
				p.mu.Unlock()
				timer.Reset(5 * time.Millisecond)
				break
			}
			lastFlightProbe = now
			x := p.take(choice)
			if x.key && !x.repair {
				p.unrepairableReferenceAt.Store(0)
				p.waitKey = false
			}
			// Repair/parity between pictures must not change the active-picture state.
			if !x.repair && !x.fec && !x.padding {
				if x.key {
					p.activeKey.Store(true)
				}
				p.activeTimestamp.Store(x.h.Timestamp)
				active = !x.end
				if !active {
					p.activeKey.Store(false)
				}
				p.active.Store(active)
			}

			w := p.writers[x.h.SSRC]
			p.mu.Unlock()
			n := wireSize(x)
			if x.padding {
				budget.credit = min(budget.credit, float64(max(int64(n), rate/1600)))
			}
			var e error
			if w != nil {
				if !x.repair {
					if x.fec && !p.translateFEC(x.p) {
						for _, r := range x.refs {
							r.Release(false)
						}
						continue
					}
					input := x.h.SequenceNumber
					p.wireSeq++
					p.lastWire.Store(uint32(p.wireSeq))
					x.h.SequenceNumber = p.wireSeq
					if !x.padding {
						p.mapping[int(input)%len(p.mapping)] = sequenceMapping{input, p.wireSeq, true}
					}
					if x.key {
						p.cache.keyframe(p.wireSeq)
					}
					// Parity is expendable and must never be replayed in response to NACK.
					if !x.fec && !x.padding {
						lifetime := replayAge
						if x.keyPacket {
							lifetime = maxKeyframeCatchup
						}
						p.cache.putOwnedFor(&rtp.Packet{Header: x.h, Payload: x.p}, time.Now(), lifetime)
					}
				}
				// The interceptor assigned its value before queueing. Dropped
				// pictures and direct NACK replay must not reuse that sequence.
				// GCC's OnSent wrapper runs inside w.Write, so stamp here.
				p.stampDeparture(&x, n)
				_, e = w.Write(&x.h, x.p, x.a)
				if e == nil {
					p.wireBytes.Add(uint64(n))
					if x.kind == audioKind {
						p.audioWireBytes.Add(uint64(n))
					} else if x.kind == videoKind {
						p.videoWireBytes.Add(uint64(n))
					}
					if x.end && !x.repair && !x.fec && !x.padding {
						if x.pictureKnown {
							p.mu.Lock()
							p.pictures.mark(x.picture, x.keyPacket)
							p.mu.Unlock()
						}
						if x.tid == 2 {
							p.recordFullFrame(x.h.Timestamp, x.admittedFull)
						}
					}
					if x.kind == videoKind && (x.fec || x.repair) {
						p.repairBytes.Add(uint64(n))
					}
					if x.padding {
						p.paddingBytes.Add(uint64(n))
					}

				}
			}
			for _, r := range x.refs {
				r.Release(w != nil && e == nil)
			}
			if x.repair {
				p.mu.Lock()
				delete(p.replayPending, x.h.SequenceNumber)
				p.mu.Unlock()
			}
			budget.credit -= float64(n)
			if p.smoothWire.Load() {
				spacing := time.Duration(float64(n) * 8 / float64(pacingRate) * float64(time.Second))
				if nextWire.IsZero() {
					nextWire = time.Now()
				}
				if time.Since(nextWire) > pacingQuantum {
					nextWire = time.Now().Add(-pacingQuantum)
				}
				nextWire = nextWire.Add(spacing)
			} else {
				nextWire = time.Time{}
			}
			if e != nil {
				p.Errors.Add(1)
				p.mu.Lock()
				p.waitKey = true
				if active && len(p.queue) > 0 && p.queue[0].h.Timestamp == x.h.Timestamp {
					p.discardFrame()
				}
				p.mu.Unlock()
				active = false
				p.active.Store(false)
				if p.OnDrop != nil {
					p.OnDrop()
				}
			}
		}
	}
}
func (p *Pacer) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		close(p.done)
		p.clearAudio()
		p.dropPending()
		for len(p.queue) > 0 {
			p.discard()
		}
		p.mu.Unlock()
	})
	return nil
}

func (p *Pacer) translateFEC(payload []byte) bool {
	if len(payload) < 15 {
		return false
	}
	fec := payload[1:]
	base := binary.BigEndian.Uint16(fec[2:4])
	mask := binary.BigEndian.Uint16(fec[12:14])
	first := p.mapping[int(base)%len(p.mapping)]
	if !first.valid || first.input != base {
		return false
	}
	var out uint16
	for i := uint16(0); i < 16; i++ {
		if mask&(0x8000>>i) == 0 {
			continue
		}
		n := base + i
		m := p.mapping[int(n)%len(p.mapping)]
		if !m.valid || m.input != n || uint16(m.output-first.output) >= 16 {
			return false
		}
		out |= 0x8000 >> uint16(m.output-first.output)
	}
	binary.BigEndian.PutUint16(fec[2:4], first.output)
	binary.BigEndian.PutUint16(fec[12:14], out)
	return true
}

// Waiting a full RTT between lost repairs serializes Wi-Fi bursts. Short,
// stable paths with spare wire capacity can honor another NACK after 30ms;
// constrained paths retain RTT spacing and every replay uses the wire bucket.
func (p *Pacer) replaySpacing(now time.Time) time.Duration {
	rtt := time.Duration(p.rtt.Load())
	if rtt == 0 {
		return 100 * time.Millisecond
	}
	rate, demand := p.mediaRate(now), p.flightDemand.Load()
	if rtt >= 50*time.Millisecond && rtt < 150*time.Millisecond && demand > 0 && rate >= demand*2 && rtt-time.Duration(p.minimumFeedbackRTT.Load()) < rttRise && p.QueueDelay(now) < 20*time.Millisecond {
		return 30 * time.Millisecond
	}
	return rtt
}

func (p *Pacer) spreadParity(now time.Time) bool {
	rtt := time.Duration(p.rtt.Load())
	return rtt >= 50*time.Millisecond && rtt < 150*time.Millisecond && p.QueueDelay(now) < 20*time.Millisecond
}

// Keep a positive loss episode across clean or missing feedback batches, so a
// scheduled repair survives until its callback. Capacity and queue guards are
// still evaluated at send time; old feedback cannot extend the episode.
func (p *Pacer) retainParityLoss(at int64) {
	for previous := p.parityLossAt.Load(); at > previous; previous = p.parityLossAt.Load() {
		if p.parityLossAt.CompareAndSwap(previous, at) {
			break
		}
	}
}

func (p *Pacer) extraParity(now time.Time) bool {
	demand := p.flightDemand.Load()
	if p.mediaLoss(now) > .01 {
		p.retainParityLoss(p.mediaLossAt.Load())
	}
	at := p.parityLossAt.Load()
	return at > 0 && now.Sub(time.Unix(0, at)) < 2*time.Second && p.spreadParity(now) && demand > 0 && p.wireRate(now) >= demand*33/10
}

// Two independent 30% lossy relay legs lose about half of original packets.
// Extra copies are affordable only with the complete five-copy wire cost plus
// NACK headroom reserved; ordinary Wi-Fi retains its existing copy count.
func (p *Pacer) retainSevereParityLoss(at int64) {
	for previous := p.severeParityLossAt.Load(); at > previous; previous = p.severeParityLossAt.Load() {
		if p.severeParityLossAt.CompareAndSwap(previous, at) {
			break
		}
	}
}

func (p *Pacer) severeParity(now time.Time) bool {
	if p.mediaLoss(now) > .25 {
		p.retainSevereParityLoss(p.mediaLossAt.Load())
	}
	at, demand := p.severeParityLossAt.Load(), p.flightDemand.Load()
	return at > 0 && now.Sub(time.Unix(0, at)) < 2*time.Second && p.spreadParity(now) && demand > 0 && p.wireRate(now) >= (demand*53+9)/10
}

func (p *Pacer) Replay(seq uint16) bool {
	p.mu.Lock()
	pending := p.replayPending[seq]
	p.mu.Unlock()
	if pending {
		p.Suppressed.Add(1)
		return false
	}
	now := time.Now()
	spacing := p.replaySpacing(now)
	if pkt, deadline := p.cache.replaySpaced(seq, now, spacing); pkt != nil {
		p.mu.Lock()
		rate := p.wireRate(now)
		ahead := 0
		for _, x := range p.queue {
			if x.repair {
				ahead += wireSize(x)
			}
		}
		arrival := now.Add(time.Duration(float64(ahead+pkt.MarshalSize()+wireOverhead+1500)*8/float64(rate)*float64(time.Second)) + time.Duration(p.rtt.Load())/2)
		p.mu.Unlock()
		if arrival.After(deadline) {
			if replayReference(pkt, p.mediaPT.Load(), p.redPT.Load()) {
				p.unrepairableReferenceAt.Store(now.UnixNano())
			}
			p.Suppressed.Add(1)
			return false
		}
		_, err := p.Write(&pkt.Header, pkt.Payload, interceptor.Attributes{"passstream.repair": true, "passstream.deadline": deadline, "passstream.keyRepair": deadline.Sub(now) > replayAge})
		return err == nil
	}
	if p.cache.unrepairableReference(seq, now, p.mediaPT.Load(), p.redPT.Load()) {
		p.unrepairableReferenceAt.Store(now.UnixNano())
	}
	return false
}

// stampDeparture is called only by the pacer writer, for both media lanes.
func (p *Pacer) stampDeparture(x *pacedPacket, n int) {
	if id := p.twccID.Load(); id > 0 {
		_ = x.h.SetExtension(uint8(id), []byte{byte(p.twccSeq >> 8), byte(p.twccSeq)})
		slot := &p.departures[int(p.twccSeq)%len(p.departures)]
		slot.sequence.Store(0)
		slot.pathEpoch.Store(p.pathEpoch.Load())
		media := uint32(0)
		if x.kind == videoKind && !x.repair && !x.fec && !x.padding {
			media = uint32(x.h.SequenceNumber) + 1
		}
		slot.kind.Store(uint32(x.kind))
		slot.media.Store(media)
		slot.trial.Store(0)
		if until := p.probeUntil.Load(); x.kind == videoKind && time.Now().UnixNano() < until {
			slot.trial.Store(p.probeID.Load())
		}
		slot.at.Store(time.Now().UnixNano())
		slot.bytes.Store(int64(n))
		if x.kind == videoKind {
			slot.total.Store(p.departedBytes.Add(int64(n)))
		} else {
			slot.total.Store(0)
		}
		slot.sequence.Store(uint32(p.twccSeq) + 1)
		p.twccSeq++
	}
}
