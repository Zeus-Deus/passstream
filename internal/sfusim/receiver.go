package sfusim

import (
	"slices"
	"sort"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Wire payload kinds between the SFU model and a receiver.
const (
	kindMedia = iota
	kindRTX
	kindFEC
	kindPadding
	kindAudio
	kindSR
)

type rtpPkt struct {
	kind  int
	tseq  uint64
	rend  ctrl.Rendition
	seq   int64
	frame *Frame
	idx   int
	group []fecEntry    // FEC: protected packets
	sent  time.Duration // SR: send time; audio: the frame's SFU ingress time
}

type fecEntry struct {
	seq   int64
	frame *Frame
	idx   int
}

// Reverse-path messages from a receiver to the SFU.
type (
	twccMsg struct{ fb ctrl.Feedback }
	nackMsg struct{ items []nackItem }
	rrMsg   struct {
		lsr, dlsr time.Duration
		lost      float64
	}
	stallMsg struct{}
)

type nackItem struct {
	rend  ctrl.Rendition
	seq   int64
	audio bool
}

type missing struct {
	first, next time.Duration
	tries       int
}

type stream struct {
	highest  int64
	started  bool
	have     map[int64]bool
	missing  map[int64]*missing
	expected int64 // RR interval accounting
	received int64
	base     int64
}

type rxFrame struct {
	have     []bool
	got      int
	first    time.Duration
	complete bool
	doneAt   time.Duration
	decoded  bool
	dead     bool
}

// Rendered is one displayed picture.
type Rendered struct {
	At, Capture time.Duration
	Endpoint    time.Duration
	Rend        ctrl.Rendition
	TID         uint8
	Key         bool
	Buffered    time.Duration // first packet arrival to display (Chromium jitterBufferDelay)
}

// Receiver models the viewer browser: TWCC and RR generation, NACK, FEC
// recovery, VP9 decodability, a 50–150 ms jitter buffer and decoder signals.
type Receiver struct {
	id       int // the viewer's (fateLog)
	sim      *Sim
	up       func(size int, msg any) // send to the SFU over the reverse path
	nackRTT  time.Duration
	clock    time.Time // receiver clock origin (only differences matter)
	endpoint [2]time.Duration

	arrivals   []ctrl.Arrival
	highTSeq   uint64
	reported   uint64
	anyTSeq    bool
	streams    [2]*stream
	frames     map[*Frame]*rxFrame
	pending    []*Frame
	groups     [][]fecEntry
	cur        ctrl.Rendition
	hasDecoder bool

	lsr, lsrAt time.Duration
	base       time.Duration
	transits   []timed
	excess     []time.Duration
	lastRender time.Duration
	lastFrame  time.Duration // like session.ts lastVideoFrame: starts at join
	lastPLI    time.Duration
	lastPacket time.Duration
	stuckSince time.Duration
	joined     time.Duration
	closed     bool // its PeerConnection was replaced (PCReplace)

	// The viewer app's playout policy (web/src/playout.ts) sets the
	// jitter-buffer minimum from these, every 500 ms stats tick.
	play       playout
	videoNACKs int // NACK packets for video (inbound-rtp nackCount)
	audioNACKs int
	playSum    time.Duration // diagnostics: sum and count of playout targets
	playN      int
	freezes    int             // Chromium freezeCount
	intervals  []time.Duration // recent inter-frame intervals for freezeCount
	// diagnostics returns the selected pair's RTT and the SFU diagnostics'
	// averageLoss (session.ts lossEvidence: > 1%).
	// catchup reports the SFU diagnostics' catchupMs > 0 (an admitted key in
	// flight to this viewer).
	diagnostics func() (rtt time.Duration, averageLoss float64, catchup bool)
	lossSum     float64 // calibration: averageLoss summed over stats ticks

	// The viewer app's recovery policy (web/src/recovery.ts). replace is nil
	// when a scripted PCReplace stands in for it (A19).
	rec       recoveryPolicy
	replace   func()
	sourceAt  func() time.Duration // last publisher frame the SFU reported
	signaling func() bool          // WebSocket pings answered
	baseRTT   time.Duration        // the SFU's initial RTT (initialRTTMs)

	Timeline        []Rendered
	Unique          int64 // first arrivals of original media bytes (incl. FEC-recovered)
	dead            int
	audio           map[int64]time.Duration // audio seq → age at first arrival (since SFU ingress)
	audioHigh       int64
	audioMiss       map[int64]*missing
	TTFF            time.Duration
	PLIs, Stalls    int
	Bytes           [6]int64
	Pkts            [6]int64 // packets received by kind
	LossRuns        int64    // forward loss runs and packets, from transport-wide sequence gaps
	LossPkts        int64
	Recovered, Lost int
	Replacements    []time.Duration // when the recovery policy replaced the PC
}

func newReceiver(s *Sim, joined, nackRTT time.Duration, up func(int, any)) *Receiver {
	r := &Receiver{sim: s, up: up, nackRTT: nackRTT, clock: epoch.Add(time.Hour), frames: map[*Frame]*rxFrame{}, joined: joined, TTFF: -1, lastRender: -1, lastFrame: joined, base: -1, audio: map[int64]time.Duration{}, audioMiss: map[int64]*missing{}}
	for i := range r.streams {
		r.streams[i] = &stream{have: map[int64]bool{}, missing: map[int64]*missing{}}
	}
	s.Every(joined+50*time.Millisecond, 50*time.Millisecond, r.sendTWCC)
	s.Every(joined+20*time.Millisecond, 20*time.Millisecond, r.sendNACK)
	s.Every(joined+time.Second, time.Second, r.sendRR)
	s.Every(joined+500*time.Millisecond, 500*time.Millisecond, r.heartbeat)
	s.Every(joined+500*time.Millisecond, 500*time.Millisecond, r.stats)
	r.play = newPlayout()
	r.rec = newRecovery(joined)
	return r
}

// stats is session.ts's 500 ms getStats tick feeding PlayoutPolicy.observe.
func (r *Receiver) stats() {
	if r.closed {
		return
	}
	var rtt time.Duration
	loss, catchup := 0.0, false
	if r.diagnostics != nil {
		rtt, loss, catchup = r.diagnostics()
	}
	// session.ts: CatchupGrace defers the video stall clock for up to 6 s
	// while the SFU reports a key in flight (recovery.deferVideo).
	if !catchup {
		r.rec.catchUntil = never
	} else {
		if r.rec.catchUntil == never {
			r.rec.catchUntil = r.sim.Now() + 6*time.Second
		}
		if r.sim.Now() < r.rec.catchUntil && r.rec.videoAt != never {
			r.rec.videoAt = r.sim.Now()
		}
	}
	r.lossSum += loss
	r.play.observe(r.videoNACKs, r.freezes, rtt, r.sim.Now(), loss > 0.01)
	r.playSum += r.play.target
	r.playN++
	if r.replace != nil && r.recover(rtt) {
		r.replace()
	}
}

// recoveryPolicy is web/src/recovery.ts RecoveryPolicy (with session.ts's
// CatchupGrace and decoderLiveness inputs): a decoded-video stall of
// max(1 s, 4 × baseline RTT) while video packets or publisher frames still
// advance replaces the PeerConnection at once if signaling answers, or if
// signaling returns after the stall was already stale (session.ts acts on
// that first pong; here the 500 ms stats tick sees it), else reconnectGrace
// (1 s) after signaling returns; any stall of 6 s replaces regardless. Replacements back off from 6 s, doubling within 30 s, reset
// after 3 s of fresh video.
type recoveryPolicy struct {
	videoAt, healthySince      time.Duration // never: none yet
	receivedAt, alive, suspect time.Duration
	signalingSince             time.Duration // never: signaling down
	resetAt                    time.Duration
	catchUntil                 time.Duration // CatchupGrace
	replaceAfter, lastReplaced time.Duration
	backoff                    time.Duration
	videoPkts                  int64
}

func newRecovery(now time.Duration) recoveryPolicy {
	return recoveryPolicy{videoAt: never, healthySince: never, receivedAt: never, alive: now, suspect: never, signalingSince: never,
		resetAt: now, catchUntil: never, replaceAfter: never, lastReplaced: never, backoff: 6 * time.Second}
}

// video is RecoveryPolicy.video for one displayed frame.
func (c *recoveryPolicy) video(at time.Duration, fresh bool) {
	if c.videoAt == never || fresh {
		c.videoAt = at
	}
	if !fresh {
		c.healthySince = never
		return
	}
	c.alive = max(c.alive, at)
	if c.healthySince == never {
		c.healthySince = at
	}
	if at-c.healthySince >= 3*time.Second {
		c.replaceAfter, c.lastReplaced, c.backoff = never, never, 6*time.Second
	}
}

// recover runs observe and action on the stats tick; true means replace.
func (r *Receiver) recover(rtt time.Duration) bool {
	c, now := &r.rec, r.sim.Now()
	if v := r.Pkts[kindMedia] + r.Pkts[kindRTX] + r.Pkts[kindFEC] + r.Pkts[kindPadding]; v > c.videoPkts {
		c.receivedAt, c.videoPkts = now, v
	}
	if r.lastPacket > c.resetAt {
		c.alive = max(c.alive, r.lastPacket) // transport bytes, audio included
	}
	if r.signaling == nil || r.signaling() {
		if c.signalingSince == never {
			c.signalingSince = now
		}
	} else {
		c.signalingSince = never
	}
	source := never
	if r.sourceAt != nil && r.sourceAt() > c.resetAt {
		source = r.sourceAt()
	}
	videoStall := max(time.Second, 4*r.baseRTT)
	stall, alive := max(time.Second, 4*rtt), c.alive
	if c.videoAt != never && max(source, c.receivedAt)-c.videoAt >= videoStall {
		stall, alive = videoStall, min(c.alive, c.videoAt)
	}
	if now-alive <= stall {
		c.suspect = never
		return false
	}
	if now < c.replaceAfter {
		return false
	}
	if c.suspect == never {
		c.suspect = now
	}
	// recovery.ts: replace at once if signaling answered throughout the
	// stall or returned only after the stall was already stale (early
	// restore), else reconnectGrace after signaling returned.
	replace := now-c.suspect >= 6*time.Second ||
		c.signalingSince != never && (c.signalingSince <= alive || c.signalingSince-alive > stall || now-c.signalingSince >= time.Second)
	if replace {
		if now-c.lastReplaced < 30*time.Second {
			c.backoff = min(30*time.Second, 2*c.backoff)
		} else {
			c.backoff = 6 * time.Second
		}
		c.lastReplaced, c.replaceAfter, c.healthySince = now, now+c.backoff, never
		r.Replacements = append(r.Replacements, now)
	}
	return replace
}

// playout is web/src/playout.ts PlayoutPolicy: NACKs alone keep the low
// target; repeated freezes during repair, or measured loss with active NACK
// repair, raise it to an RTT-scaled target; quiet paths return to low.
type playout struct {
	target                     time.Duration
	nacks, freezes             int
	lastNack, lastFreeze, seen time.Duration
	raised                     bool
	repairRTT, lastRTT         time.Duration
	hasRepairRTT               bool
}

const never = -time.Hour * 24

func newPlayout() playout {
	return playout{target: 50 * time.Millisecond, lastNack: never, lastFreeze: never, seen: never, lastRTT: never}
}

func (p *playout) observe(nacks, freezes int, rtt, now time.Duration, rawLoss bool) {
	if rawLoss {
		p.seen = now
	}
	if nacks > p.nacks {
		p.lastNack = now
	}
	p.nacks = nacks
	if freezes > p.freezes {
		repeated := freezes-p.freezes > 1 || now-p.lastFreeze < 30*time.Second
		if repeated && now-p.lastNack < 5*time.Second {
			p.raised = true
		}
		p.lastFreeze = now
	}
	p.freezes = freezes
	if now-p.seen < 5*time.Second && now-p.lastNack < 5*time.Second {
		p.raised = true
	}
	if (now-p.lastNack >= 30*time.Second || now-p.lastFreeze >= 120*time.Second) && now-p.seen >= 30*time.Second {
		p.raised = false
	}
	if p.raised && rtt > 0 {
		if !p.hasRepairRTT || now-p.lastRTT > 5*time.Second {
			p.repairRTT = rtt
		} else {
			p.repairRTT = p.repairRTT*9/10 + rtt/10
		}
		p.hasRepairRTT, p.lastRTT = true, now
	} else if !p.raised {
		p.hasRepairRTT = false
	}
	desired := 50 * time.Millisecond
	if p.raised {
		base := rtt
		if p.hasRepairRTT {
			base = p.repairRTT
		}
		desired = min(300*time.Millisecond, max(150*time.Millisecond, 65*time.Millisecond+base*3/2))
	}
	p.target = min(desired, p.target+100*time.Millisecond)
}

// carry continues the session-level state of a replaced receiver: what was
// displayed, the counters, and session.ts lastVideoFrame/lastKeyframeRequest.
// Decoder, jitter-buffer, NACK and feedback state start empty.
func (r *Receiver) carry(old *Receiver) {
	r.Timeline, r.TTFF, r.PLIs, r.Stalls = old.Timeline, old.TTFF, old.PLIs, old.Stalls
	r.play, r.videoNACKs, r.freezes = old.play, old.videoNACKs, old.freezes // the app's policy outlives the PC
	r.audioNACKs, r.playSum, r.playN, r.lossSum = old.audioNACKs, old.playSum, old.playN, old.lossSum
	// RecoveryPolicy.reset keeps only the replacement back-off.
	r.rec.replaceAfter, r.rec.lastReplaced, r.rec.backoff = old.rec.replaceAfter, old.rec.lastReplaced, old.rec.backoff
	r.Replacements = old.Replacements
	r.Recovered, r.Lost, r.dead, r.audio = old.Recovered, old.Lost, old.dead, old.audio
	r.audioHigh = old.audioHigh
	r.Bytes, r.Pkts, r.Unique = old.Bytes, old.Pkts, old.Unique
	r.lastFrame, r.lastPLI, r.lastRender = old.lastFrame, old.lastPLI, old.lastRender
}

// Receive handles a packet from the forward link.
func (r *Receiver) Receive(p *Packet) {
	if r.closed {
		return
	}
	k := p.payload.(*rtpPkt)
	now := r.sim.Now()
	r.Bytes[k.kind] += int64(p.Size)
	r.Pkts[k.kind]++
	if k.kind == kindSR {
		r.lsr, r.lsrAt = k.sent, now
		return
	}
	r.lastPacket = now
	r.arrivals = append(r.arrivals, ctrl.Arrival{Seq: k.tseq, At: r.clock.Add(now)})
	if !r.anyTSeq || k.tseq > r.highTSeq {
		if !r.anyTSeq {
			r.reported = k.tseq
		} else if gap := k.tseq - r.highTSeq - 1; gap > 0 {
			r.LossRuns++ // forward transport-wide loss run (calibration)
			r.LossPkts += int64(gap)
		}
		r.anyTSeq, r.highTSeq = true, k.tseq
	}
	switch k.kind {
	case kindMedia, kindRTX:
		r.media(k)
	case kindAudio:
		r.audioPacket(k)
	case kindFEC:
		r.groups = append(r.groups, k.group)
		r.tryFEC()
	}
}

func (r *Receiver) media(k *rtpPkt) {
	now := r.sim.Now()
	s := r.streams[k.rend]
	if !s.started {
		s.started, s.highest, s.base = true, k.seq-1, k.seq
	}
	if s.have[k.seq] {
		return
	}
	s.have[k.seq] = true
	r.Unique += int64(k.frame.PacketSize(k.idx))
	s.received++
	delete(s.missing, k.seq)
	if k.seq > s.highest {
		for q := s.highest + 1; q < k.seq; q++ {
			if !s.have[q] {
				s.missing[q] = &missing{first: now, next: now}
			}
		}
		s.highest = k.seq
	}
	if k.seq%512 == 0 {
		for q := range s.have {
			if q < k.seq-4096 {
				delete(s.have, q)
			}
		}
	}
	r.packet(k.frame, k.idx)
	r.tryFEC()
}

func (r *Receiver) packet(f *Frame, idx int) {
	now := r.sim.Now()
	x := r.frames[f]
	if x == nil {
		x = &rxFrame{have: make([]bool, f.Packets), first: now}
		r.frames[f] = x
		r.observeTransit(now - f.Capture)
	}
	if x.have[idx] || x.dead {
		return
	}
	x.have[idx] = true
	x.got++
	if x.got == f.Packets {
		x.complete, x.doneAt = true, now
		if fateLog != nil {
			fateLog(r.id, f, fateComplete, now)
		}
		r.pending = append(r.pending, f)
		r.decode()
	}
}

// FEC recovers exactly one missing packet per parity group.
func (r *Receiver) tryFEC() {
	n := 0
	for _, g := range r.groups {
		lost := -1
		missing := 0
		for i, e := range g {
			if !r.streams[e.frame.Rend].have[e.seq] {
				lost = i
				missing++
			}
		}
		switch {
		case missing == 1:
			e := g[lost]
			s := r.streams[e.frame.Rend]
			s.have[e.seq] = true
			delete(s.missing, e.seq)
			r.Recovered++
			r.Unique += int64(e.frame.PacketSize(e.idx))
			r.packet(e.frame, e.idx)
		case missing > 1 && r.sim.Now()-r.lastPacket < time.Second:
			r.groups[n] = g
			n++
		}
	}
	clear(r.groups[n:])
	r.groups = r.groups[:n]
	if len(r.groups) > 64 {
		r.groups = r.groups[len(r.groups)-64:]
	}
}

// jitterCap bounds the receiver's own jitter-buffer estimate.
var jitterCap = 150 * time.Millisecond

// observeTransit keeps the minimum first-packet transit over the last 10 s
// (a monotonic deque), the playout clock's reference.
func (r *Receiver) observeTransit(t time.Duration) {
	now := r.sim.Now()
	for len(r.transits) > 0 && r.transits[len(r.transits)-1].v >= t {
		r.transits = r.transits[:len(r.transits)-1]
	}
	r.transits = append(r.transits, timed{now, t})
	for r.transits[0].at < now-10*time.Second {
		r.transits = r.transits[1:]
	}
	r.base = r.transits[0].v
}

type timed struct{ at, v time.Duration }

// decode runs libwebrtc FrameBuffer semantics: decode any frame whose
// reference is decoded; decoding a frame abandons every older frame.
func (r *Receiver) decode() {
	now := r.sim.Now()
	sort.Slice(r.pending, func(i, j int) bool { return r.pending[i].Capture < r.pending[j].Capture })
	for {
		pick := -1
		for i, f := range r.pending {
			if f.Key || r.hasDecoder && f.Rend == r.cur && f.Ref != nil && r.frames[f.Ref] != nil && r.frames[f.Ref].decoded {
				pick = i
				break
			}
		}
		if pick < 0 {
			if len(r.pending) > 0 && r.stuckSince == 0 {
				r.stuckSince = now
			}
			return
		}
		f := r.pending[pick]
		for _, old := range r.pending[:pick] {
			r.frames[old].dead = true
			if old.Rend == f.Rend {
				r.dead++ // complete, skipped: its reference never decoded
			}
		}
		r.pending = slices.Delete(r.pending, 0, pick+1)
		r.frames[f].decoded = true
		r.stuckSince = 0
		r.cur, r.hasDecoder = f.Rend, true
		r.render(f, r.frames[f].first)
		r.prune(f)
	}
}

func (r *Receiver) render(f *Frame, first time.Duration) {
	now := r.sim.Now()
	excess := now - f.Capture - r.base
	r.excess = append(r.excess, excess)
	if len(r.excess) > 45 {
		r.excess = r.excess[1:]
	}
	target := 50 * time.Millisecond
	if len(r.excess) >= 10 {
		s := slices.Clone(r.excess)
		slices.Sort(s)
		target = min(jitterCap, max(target, s[len(s)*95/100]))
	}
	// jitterBufferTarget is a minimum playout delay.
	target = max(target, r.play.target)
	at := max(now, f.Capture+r.base+target, r.lastRender)
	if r.lastRender >= 0 {
		r.freeze(at - r.lastRender)
	}
	// session.ts decoderLiveness: an advancing picture counts as live video
	// unless its pixel delay exceeds max(3 s, 2 × videoFreshnessLimit).
	r.rec.video(at, at-f.Capture+r.endpoint[f.Rend] <= 3*time.Second)
	r.lastRender, r.lastFrame = at, at
	if r.TTFF < 0 {
		r.TTFF = at - r.joined
	}
	r.Timeline = append(r.Timeline, Rendered{At: at, Capture: f.Capture, Endpoint: r.endpoint[f.Rend], Rend: f.Rend, TID: f.TID, Key: f.Key, Buffered: at - first})
}

// freeze counts Chromium's freezeCount: an inter-frame interval of at least
// max(3 × mean, mean + 150 ms) over the previous 30 intervals.
func (r *Receiver) freeze(d time.Duration) {
	if n := len(r.intervals); n > 0 {
		sum := time.Duration(0)
		for _, x := range r.intervals {
			sum += x
		}
		mean := sum / time.Duration(n)
		if d >= max(3*mean, mean+150*time.Millisecond) {
			r.freezes++
		}
	}
	r.intervals = append(r.intervals, d)
	if len(r.intervals) > 30 {
		r.intervals = r.intervals[1:]
	}
}

func (r *Receiver) prune(f *Frame) {
	if len(r.frames) < 256 {
		return
	}
	for k, x := range r.frames {
		if k.Capture < f.Capture-3*time.Second && (x.decoded || x.dead || !x.complete) {
			delete(r.frames, k)
		}
	}
}

func (r *Receiver) sendTWCC() {
	if r.closed || !r.anyTSeq || len(r.arrivals) == 0 && r.reported >= r.highTSeq {
		return
	}
	got := map[uint64]bool{}
	fb := ctrl.Feedback{}
	for _, a := range r.arrivals {
		got[a.Seq] = true
	}
	for q := r.reported; q <= r.highTSeq; q++ {
		if !got[q] {
			fb.Arrivals = append(fb.Arrivals, ctrl.Arrival{Seq: q})
		}
	}
	fb.Arrivals = append(fb.Arrivals, r.arrivals...)
	sort.Slice(fb.Arrivals, func(i, j int) bool { return fb.Arrivals[i].Seq < fb.Arrivals[j].Seq })
	r.arrivals = r.arrivals[:0]
	r.reported = r.highTSeq + 1
	r.up(rtpOverhead+20+2*len(fb.Arrivals), twccMsg{fb})
}

// audioPlayout is the age after SFU ingress beyond which an Opus frame is
// concealed rather than played: internal/sfu's audioMaxAge, the deadline
// production itself gives queued and repaired audio.
const audioPlayout = audioMaxAge

// audioPacket records an Opus packet (original or retransmission); a jump
// in sequence numbers marks the frames in between missing for NACK.
func (r *Receiver) audioPacket(k *rtpPkt) {
	now := r.sim.Now()
	if _, ok := r.audio[k.seq]; ok {
		return
	}
	r.audio[k.seq] = now - k.sent
	delete(r.audioMiss, k.seq)
	if r.audioHigh > 0 {
		for q := r.audioHigh + 1; q < k.seq; q++ {
			if _, ok := r.audio[q]; !ok {
				r.audioMiss[q] = &missing{first: now, next: now}
			}
		}
	}
	r.audioHigh = max(r.audioHigh, k.seq)
}

func (r *Receiver) sendNACK() {
	if r.closed {
		return
	}
	now := r.sim.Now()
	var items []nackItem
	// Audio NACK (web/src/audio.ts negotiates it): requested until the frame
	// can no longer play.
	keys := make([]int64, 0, len(r.audioMiss))
	for q := range r.audioMiss {
		keys = append(keys, q)
	}
	slices.Sort(keys)
	for _, q := range keys {
		m := r.audioMiss[q]
		if now-m.first > audioPlayout || m.tries >= 10 {
			delete(r.audioMiss, q)
			continue
		}
		if m.next <= now {
			items = append(items, nackItem{seq: q, audio: true})
			m.tries++
			m.next = now + r.nackRTT
		}
	}
	for rend, s := range r.streams {
		keys := make([]int64, 0, len(s.missing))
		for q := range s.missing {
			keys = append(keys, q)
		}
		slices.Sort(keys)
		for _, q := range keys {
			m := s.missing[q]
			if now-m.first > time.Second || m.tries >= 10 {
				delete(s.missing, q)
				r.Lost++
				continue
			}
			if m.next <= now {
				items = append(items, nackItem{rend: ctrl.Rendition(rend), seq: q})
				m.tries++
				m.next = now + r.nackRTT
			}
		}
	}
	if len(items) > 0 {
		video, audio := false, false
		for _, it := range items {
			video, audio = video || !it.audio, audio || it.audio
		}
		if video {
			r.videoNACKs++
		}
		if audio {
			r.audioNACKs++
		}
		r.up(rtpOverhead+12+4*len(items), nackMsg{items})
	}
}

func (r *Receiver) sendRR() {
	if r.closed {
		return
	}
	now := r.sim.Now()
	lost := 0.0
	exp, got := int64(0), int64(0)
	for _, s := range r.streams {
		if !s.started {
			continue
		}
		e := s.highest - s.base + 1 - s.expected
		exp += e
		got += s.received
		s.expected += e
		s.received = 0
	}
	if exp > 0 && got < exp {
		lost = float64(exp-got) / float64(exp)
	}
	m := rrMsg{lost: lost, lsr: -1}
	if r.lsrAt > 0 {
		m.lsr, m.dlsr = r.lsr, now-r.lsrAt
	}
	r.up(rtpOverhead+32, m)
}

// heartbeat mirrors web/src/session.ts tick(): with no video frame for 1 s
// the viewer sends a keyframe request, at most once per second. It reaches
// the SFU as a decoder signal. libwebrtc's own PLI paths (NACK list
// overflow, 3 s frame timeout) are dominated by it and are not modelled.
func (r *Receiver) heartbeat() {
	if r.closed {
		return
	}
	now := r.sim.Now()
	if now-r.lastFrame > time.Second && now-r.lastPLI > time.Second {
		r.lastPLI = now
		r.Stalls++
		r.up(rtpOverhead+100, stallMsg{})
	}
}

// deadComplete counts complete pictures that never decoded for lack of a
// reference: abandoned ones plus those still pending at the end.
func (r *Receiver) deadComplete() int {
	n := r.dead
	for _, f := range r.pending {
		if r.hasDecoder && f.Rend == r.cur {
			n++
		}
	}
	return n
}

// audioGaps returns gaps >200 ms between playable 20 ms Opus frames and the
// number of frames that never played. A frame is playable if it, or its
// retransmission, arrived within audioPlayout of SFU ingress; there is no
// RED copy (production refuses audio RED under SFrame).
func (r *Receiver) audioGaps() (gaps []time.Duration, missing int) {
	if len(r.audio) == 0 {
		return nil, 0
	}
	lo, hi := int64(1<<62), int64(0)
	for q := range r.audio {
		lo, hi = min(lo, q), max(hi, q)
	}
	run := 0
	for q := lo; q <= hi; q++ {
		if age, ok := r.audio[q]; ok && age <= audioPlayout {
			if d := time.Duration(run+1) * 20 * time.Millisecond; d > 200*time.Millisecond {
				gaps = append(gaps, d)
			}
			run = 0
		} else {
			run++
			missing++
		}
	}
	return gaps, missing
}
