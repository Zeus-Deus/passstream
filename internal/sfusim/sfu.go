package sfusim

import (
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// room is the SFU: ingress cost measurement, key assembly, the shared key
// policy and one forwarder per viewer. Only RTP-header and VP9-descriptor
// facts (sizes, TID, key, switching point, picture number, references) are
// used, as SFrame requires.
type room struct {
	sim     *Sim
	sc      *Scenario
	src     [2]*Source
	keys    ctrl.Keys
	viewers []*viewer
	links   []*Link

	meter    ctrl.Ingress
	audioSeq int
	keys0    []KeyRecord
	resend   func(time.Duration, int) bool
	adjust   func(int, time.Duration, *ctrl.Decision)
	rtxCopy  time.Duration
	fecAll   bool
	replace  func(int, ctrl.Keys, ctrl.RateState) *ctrl.Viewer

	in         [2]ingressState // per rendition: reorder window and picture assembly
	noRecovery bool            // Controller.NoAppRecovery
	plis       [2][]time.Duration
}

// ingressState is one rendition's internal/sfu read loop: the reorder
// window, then the assembler.
type ingressState struct {
	cur   *Frame // picture being assembled
	got   int
	queue []heldPacket // held behind an unrepaired gap
}

type heldPacket struct {
	f  *Frame
	i  int
	at time.Duration // arrival at the SFU
}

// reorderDeadline is internal/sfu's reorder window: packets behind a gap
// upstream NACK could not repair wait this long for it, then the window
// gives up and the damaged picture is reported lost.
const reorderDeadline = 300 * time.Millisecond

// KeyRecord is a keyframe as it completed SFU ingress.
type KeyRecord struct {
	At    time.Duration
	Rend  ctrl.Rendition
	Bytes int
}

func (r *room) link(i int, stream uint64) *Link {
	if i == Ideal {
		return newLink(r.sim, stream, LinkConfig{})
	}
	return r.links[i]
}

func (r *room) costs() ctrl.Costs { return r.meter.Costs(r.sim.Time(), r.keys) }

// ingress receives one publisher packet. As in internal/sfu (publisher.go
// read), every picture, key or delta, is assembled completely before any
// viewer sees its first packet (docs/CONTROLLER.md).
func (r *room) ingress(f *Frame, i int) {
	if i == 0 && f.Packets > 1 && r.sc.IngressLoss != nil {
		f.ingressLost = r.sc.IngressLoss(f) // evaluated once per picture
	}
	if i > 0 && f.ingressLost {
		return // publisher-leg loss the SFU could not repair
	}
	r.arrive(heldPacket{f, i, r.sim.Now()})
}

// arrive is the reorder window. Packets of one rendition arrive in order, so
// a packet of a new picture while the current one is incomplete reveals an
// unrepaired gap: it and everything after it wait until reorderDeadline
// after its arrival, as reorder.pop does.
func (r *room) arrive(h heldPacket) {
	st := &r.in[h.f.Rend]
	if len(st.queue) > 0 {
		st.queue = append(st.queue, h)
		return
	}
	if st.cur != nil && st.cur != h.f {
		st.queue = append(st.queue, h)
		r.sim.At(h.at+reorderDeadline, func() { r.release(h.f.Rend) })
		return
	}
	r.assemble(h.f, h.i)
}

// release gives up on the gap: the damaged picture is reported to every
// viewer forwarding its rendition (internal/sfu Room.ingressLost), and the
// held packets continue through the window.
func (r *room) release(rend ctrl.Rendition) {
	st := &r.in[rend]
	q := st.queue
	st.queue = nil
	if f := st.cur; f != nil {
		st.cur = nil
		for _, v := range r.viewers {
			v.lostAtIngress(f)
		}
	}
	for k, h := range q {
		r.arrive(h)
		if len(st.queue) > 0 {
			st.queue = append(st.queue, q[k+1:]...)
			return
		}
	}
}

// assemble meters one in-order packet and emits its picture once complete.
func (r *room) assemble(f *Frame, i int) {
	st := &r.in[f.Rend]
	r.meter.Video(r.sim.Time(), f.Rend, f.TID, f.Key, f.Num, f.PacketSize(i))
	if st.cur != f {
		st.cur, st.got = f, 0
	}
	if st.got++; st.got < f.Packets {
		return
	}
	st.cur = nil
	f.Assembled = r.sim.Now()
	if f.Key {
		// Keys are reported to the room's key policy at assembly, before any
		// viewer forwards them.
		r.keys.OnKey(r.sim.Time(), f.Rend, f.Wire(), epoch.Add(f.Capture))
		r.keys0 = append(r.keys0, KeyRecord{r.sim.Now(), f.Rend, f.Size})
	}
	for _, v := range r.viewers {
		v.offer(f)
	}
}

// tracef, when set by a diagnostic test, logs forwarder events. It is nil in
// Tier A runs.
var tracef func(at time.Duration, viewer int, format string, args ...any)

// Frame fates for the missed-frame cause breakdown (C2 §5 Wi-Fi); fateLog
// is set only by that diagnostic test.
const (
	fateDropped  = iota // the SFU discarded or refused the picture (local queue drop)
	fateSent            // its last packet left the SFU
	fateComplete        // the receiver had every packet (original, RTX or FEC)
)

var fateLog func(viewer int, f *Frame, fate int, at time.Duration)

// Production audio (internal/sfu audio.go, web/src/audio.ts): one 20 ms
// Opus frame per packet as an opaque SFrame ciphertext, no RED (refused
// under SFrame), NACK negotiated and served from a cache for audioMaxAge.
// Measured payload (a1-smoke-main, Chromium outbound-rtp audio, 4 rows):
// 100.9 B per packet at the 32 kbit/s Opus target, 12 B RTP header, 50
// packets/s. The SFU's wire accounting is rtpHeader + payload + wireOverhead
// (12 + 101 + 50). Opus inband FEC is negotiated (useinbandfec=1) but not
// modelled: libopus adds LBRR only when its packet-loss input is above zero,
// and that input comes from the publisher's own clean leg; the constant
// 100.9 B payload at the 32 kbit/s target shows no LBRR bytes.
const (
	audioWire   = 12 + 101 + 50
	audioMaxAge = 200 * time.Millisecond // internal/sfu audioMaxAge: queue and repair deadline
)

func (r *room) audio() {
	r.meter.Audio(r.sim.Time(), audioWire)
	r.audioSeq++
	now := r.sim.Now()
	for _, v := range r.viewers {
		if v.joined && v.ready() {
			v.push(&outPkt{kind: kindAudio, size: audioWire, seq: int64(r.audioSeq), origin: now},
				ctrl.Packet{Size: audioWire, Kind: ctrl.KindAudio, Class: ctrl.ClassAudio, Last: true, Packets: 1, Deadline: epoch.Add(now + audioMaxAge)})
		}
	}
}

func (r *room) pollKeys() {
	for _, rend := range r.keys.Due(r.sim.Time()) {
		r.plis[rend] = append(r.plis[rend], r.sim.Now())
		s := r.src[rend]
		r.sim.After(2*pubDelay, s.RequestKey)
	}
}

type outPkt struct {
	kind   int
	size   int
	frame  *Frame
	idx    int
	seq    int64
	rend   ctrl.Rendition
	group  []fecEntry
	origin time.Duration // audio: SFU ingress time of the frame
	rtx    bool          // audio: a retransmission
}

type cacheEntry struct {
	pkt        *outPkt
	pushed     time.Duration
	last       time.Duration // latest copy sent (original or retransmission)
	rtx        int
	generation uint64
	deadline   time.Duration // the picture's useful deadline at departure (ctrl.Packet.Deadline)
}

// Sample is one 500 ms observation of a viewer, like a browser getStats tick.
type Sample struct {
	At        time.Duration
	Desired   ctrl.Option
	Forwarded ctrl.Rendition
	TID       uint8
	Decoder   ctrl.Rendition
	Native    bool  // decoded native pixels at any TID
	Full      bool  // native and forwarding T2
	WireVideo int64 // cumulative video wire bytes (media, RTX, FEC)
	Padding   int64 // cumulative padding bytes
	Audio     int64
	Received  int64 // cumulative media+RTX+FEC bytes at the receiver
	Unique    int64 // cumulative unique original media bytes at the receiver
	Rate      int   // R
}

type viewer struct {
	room *room
	id   int
	cfg  ViewerConfig
	ctl  *ctrl.Viewer
	fwd  *Link
	rev  *Link
	rx   *Receiver

	joined     bool
	rend       ctrl.Rendition
	hasRend    bool
	stopped    bool    // the forwarded rendition's key did not fit; waiting for the other
	away       [2]bool // a base was missed behind the media barrier; reported on return
	want       ctrl.Rendition
	wantTID    uint8
	tid        uint8
	seq        [2]int64
	tseq       uint64
	tsent      [8192]sentMedia // transport sequence → media packet, for TWCC loss repair
	cache      [2]map[int64]*cacheEntry
	audioCache map[int64]*cacheEntry
	pending    map[uint64]*outPkt
	nextID     uint64
	pumpAt     time.Duration
	fec        float64
	group      [2][][]fecEntry
	interleave int
	latestKey  [2]uint64

	WireVideo, Padding, Audio int64
	SentPkts                  [6]int64 // packets sent by kind (audio includes retransmissions)
	AudioRTX                  int
	Drops, Refused            int
	MaxQueuedBytes            int
	MaxQueuedFrames           int
	Samples                   []Sample
	lastRate                  int

	lastUp time.Duration // last current-generation reverse-path packet (Consent)
	pcGen  int           // PeerConnection generation (Replace)
	state  ctrl.RateState
}

// sentMedia maps one transport sequence to the video media packet it
// carried (internal/sfu mediaAudit receipts).
type sentMedia struct {
	tseq uint64 // +1; zero is empty
	rend ctrl.Rendition
	seq  int64
}

// release is the pacer's drop hook, as in internal/sfu: inside the mutation
// that discarded k it forgets the packet and reports a dropped picture to
// continuity.
func (v *viewer) release(k ctrl.Packet) {
	o := v.pending[k.ID]
	delete(v.pending, k.ID)
	v.Drops++
	if tracef != nil && k.Last && o != nil && o.frame != nil {
		tracef(v.room.sim.Now(), v.id, "drop %v #%d tid %d key %v class %d superseded %v", o.frame.Rend, o.frame.Num, o.frame.TID, o.frame.Key, k.Class, k.Superseded)
	}
	if o != nil && o.frame != nil && k.Last && !k.Superseded {
		v.ctl.Cont.Lost(info(o.frame))
		if fateLog != nil {
			fateLog(v.id, o.frame, fateDropped, v.room.sim.Now())
		}
	}
}

func info(f *Frame) ctrl.FrameInfo {
	fi := ctrl.FrameInfo{Rendition: f.Rend, Picture: f.Num, TID: f.TID, Key: f.Key, Flexible: f.Flexible, TL0: f.TL0}
	if f.Flexible && f.Ref != nil {
		fi.Refs = []uint64{f.Ref.Num}
	}
	return fi
}

func (v *viewer) join() {
	s := v.room.sim
	v.joined = true
	v.lastUp = s.Now()
	v.rx = newReceiver(s, s.Now(), v.cfg.NominalRTT+20*time.Millisecond, v.upFrom(v.pcGen))
	v.rx.id = v.id
	v.rx.endpoint = [2]time.Duration{v.room.src[ctrl.Native].prof.Endpoint, v.room.src[ctrl.Low].prof.Endpoint}
	v.appInputs()
	s.Every(s.Now(), ctrl.OveruseTime, v.step)
	s.Every(s.Now()+time.Second, time.Second, func() {
		v.fwd.Send(&Packet{Size: rtpOverhead + 28, Flow: v.id, deliver: v.rx.Receive, payload: &rtpPkt{kind: kindSR, sent: s.Now()}})
	})
	s.Every(s.Now()+500*time.Millisecond, 500*time.Millisecond, v.sample)
	if v.room.sc.SampleQueue {
		s.Every(s.Now(), 10*time.Millisecond, func() {
			b, f := v.ctl.Pacer.Queued()
			v.MaxQueuedBytes, v.MaxQueuedFrames = max(v.MaxQueuedBytes, b), max(v.MaxQueuedFrames, f)
		})
	}
	for _, sp := range v.cfg.Spam {
		for t := max(sp.From, s.Now()); t < sp.To; t += 10 * time.Millisecond {
			s.At(t, func() { v.up(rtpOverhead+100, stallMsg{}) })
		}
	}
	if sw := v.cfg.Switch; sw != nil {
		s.At(sw.At, func() {
			v.fwd, v.rev = v.room.link(sw.Fwd, uint64(600+v.id)), v.room.link(sw.Rev, uint64(700+v.id))
			v.ctl.OnPathChange(s.Time())
		})
	}
	if rp := v.cfg.Replace; rp != nil {
		at := rp.start(s.seed)
		s.At(at, v.replacePC)
		for _, d := range rp.Again {
			s.At(at+d, v.replacePC)
		}
	}
}

// appInputs gives the viewer app's playout and recovery policies what
// session.ts reads every 500 ms: the selected pair's RTT (here the
// controller's measured RTT, else the nominal one), the SFU diagnostics'
// averageLoss > 1% (production also reads Est.Estimate) and ingress frames,
// and whether signaling answers. Signaling is modelled down only during a
// link outage (a total outage takes the WebSocket with it); Wi-Fi loss does
// not touch it (web/torture.mjs exempts the HTTP port). A scripted
// PCReplace (A19) stands in for the recovery policy.
func (v *viewer) appInputs() {
	r := v.room
	v.rx.baseRTT = v.cfg.NominalRTT
	if v.cfg.Replace == nil && !r.noRecovery {
		v.rx.replace = v.replacePC
	}
	v.rx.sourceAt = func() time.Duration {
		if c := r.src[ctrl.Native].Captures; len(c) > 0 {
			return c[len(c)-1] + pubDelay
		}
		return never
	}
	v.rx.signaling = func() bool { return !v.fwd.inOutage() && !v.rev.inOutage() }
	v.rx.diagnostics = func() (time.Duration, float64, bool) {
		e := v.ctl.Est.Estimate(v.room.sim.Time())
		rtt := e.RTT
		if rtt <= 0 {
			rtt = v.cfg.NominalRTT
		}
		// keyTransit > 0: a key in the pacer, or network debt draining.
		return rtt, e.Loss, v.ctl.Pacer.KeyInFlight() && e.Rate > 0 || e.Service > 0 && e.Debt > 0
	}
}

// ready is the production media barrier (internal/sfu viewer.ready): with
// Consent modelled, forwarding stops ConsentTimeout after the last
// reverse-path packet of the current PC and resumes with the next one.
func (v *viewer) ready() bool {
	return !v.cfg.Consent || v.room.sim.Now()-v.lastUp < ConsentTimeout
}

// forwarding reports whether r is the rendition this viewer decodes now.
func (v *viewer) forwarding(r ctrl.Rendition) bool { return v.hasRend && !v.stopped && v.rend == r }

// missed reports a picture of the forwarded rendition that the barrier kept
// from the viewer; a missed base is a broken chain the SFU detected
// (internal/sfu viewer.lost).
//
// The broken chain is reported only once the viewer is reachable again, as
// internal/sfu's offer does: a key requested now would be delivered to no
// one, yet spend the room's budget and satisfy the request (viewer_flap).
func (v *viewer) missed(f *Frame) {
	if !v.forwarding(f.Rend) {
		return
	}
	v.ctl.Cont.Lost(info(f))
	v.away[f.Rend] = v.away[f.Rend] || f.Key || f.TID == 0
}

// broken reports a broken chain of r; an accepted recovery wants low at
// once, so the decision is re-run before the next picture (A8 outage: the
// requested low key assembles before the next periodic step).
func (v *viewer) broken(r ctrl.Rendition) {
	if v.ctl.OnBroken(v.room.sim.Time(), r) {
		v.step()
	}
}

// replacePC models a PeerConnection replacement the way internal/sfu does
// it (Peer.Close then InheritPath): the old loop leaves the room's key
// policy and the old receiver stops; after the scenario's Setup (signaling,
// ICE, DTLS) a new loop starts from the old one's Handoff in a new
// generation under a new ID and the same admitted session, and a new
// receiver starts without decoder state. The displayed timeline and the
// session's last-frame time carry over, as they do in web/src/session.ts.
// A replacement requested while the previous one is still being set up
// restarts it.
func (v *viewer) replacePC() {
	s := v.room.sim
	now := s.Time()
	if v.joined {
		old := v.ctl
		v.state = old.Handoff(now)
		v.room.keys.Leave(old.ID)
		v.joined = false
		v.rx.closed = true
		// The old PC's late feedback (a TWCC report and a decoder stall
		// still in flight) arrives after the close. Production delivers it
		// to the closed Peer's viewer, which no longer joins the room's key
		// policy (internal/sfu viewer.leave); here the generation fence
		// discards it.
		late := v.upFrom(v.pcGen)
		s.After(100*time.Millisecond, func() {
			late(rtpOverhead+100, stallMsg{})
			late(rtpOverhead+40, twccMsg{ctrl.Feedback{Arrivals: []ctrl.Arrival{{Seq: v.tseq}}}})
		})
	}
	v.pcGen++
	gen := v.pcGen
	setup := time.Duration(0)
	if rp := v.cfg.Replace; rp != nil {
		setup = rp.Setup
	}
	if tracef != nil {
		tracef(s.Now(), v.id, "PC closed for replacement: generation %d, handed-off rate %d session %d", gen, v.state.Rate, v.state.Session)
	}
	s.After(setup, func() {
		if gen != v.pcGen {
			return // superseded by a later request
		}
		v.ctl = v.room.replace(v.id+1000*gen, v.room.keys, v.state)
		v.ctl.Pacer.SetRelease(v.release)
		// The new PC's forwarder starts as newViewer does.
		v.rend, v.hasRend, v.stopped, v.want, v.wantTID, v.tid = 0, false, false, 0, 0, 0
		v.away = [2]bool{}
		v.seq, v.tseq, v.latestKey, v.tsent = [2]int64{}, 0, [2]uint64{}, [8192]sentMedia{}
		v.cache[0], v.cache[1], v.audioCache = map[int64]*cacheEntry{}, map[int64]*cacheEntry{}, map[int64]*cacheEntry{}
		v.pending, v.pumpAt = map[uint64]*outPkt{}, 0
		v.group, v.fec, v.interleave = [2][][]fecEntry{}, 0, 0
		oldRx := v.rx
		v.rx = newReceiver(s, s.Now(), v.cfg.NominalRTT+20*time.Millisecond, v.upFrom(gen))
		v.rx.id = v.id
		v.rx.carry(oldRx)
		v.appInputs()
		v.lastUp = s.Now()
		v.joined = true
		if tracef != nil {
			tracef(s.Now(), v.id, "PC replaced: generation %d", gen)
		}
		v.step() // joined: the first step requests the join key at once
	})
}

func (v *viewer) sample() {
	native := v.hasRend && !v.stopped && v.rend == ctrl.Native && v.rx.hasDecoder && v.rx.cur == ctrl.Native
	v.Samples = append(v.Samples, Sample{At: v.room.sim.Now(), Desired: ctrl.Option{Rendition: v.want, TID: v.wantTID}, Forwarded: v.rend, TID: v.tid, Decoder: v.rx.cur,
		Native: native, Full: native && v.tid == 2, WireVideo: v.WireVideo, Padding: v.Padding, Audio: v.Audio,
		Received: v.rx.Bytes[kindMedia] + v.rx.Bytes[kindRTX] + v.rx.Bytes[kindFEC], Unique: v.rx.Unique, Rate: v.lastRate})
}

func (v *viewer) apply() {
	if left, _ := v.ctl.Pacer.QueuedKey(v.rend); left > 0 {
		return // production applies the whole key, never its partial prefix
	}
	v.ctl.OnApplied(v.room.sim.Time(), ctrl.Option{Rendition: v.rend, TID: v.tid})
}

// offer is internal/sfu viewer.offer: one assembled picture.
func (v *viewer) offer(f *Frame) {
	if !v.joined {
		return
	}
	if !v.ready() {
		v.missed(f) // the decoder misses this picture
		return
	}
	for r, broken := range v.away {
		if broken {
			v.away[r] = false
			v.broken(ctrl.Rendition(r))
		}
	}
	if f.Key {
		v.forwardKey(f)
	} else {
		v.forwardDelta(f)
	}
}

// refuse stops forwarding r (its key did not fit or its structure is
// unsupported); OnBroken moves the viewer to the other rendition.
func (v *viewer) refuse(r ctrl.Rendition) {
	if v.forwarding(r) {
		v.stopped = true
		v.ctl.Pacer.Purge(r)
		v.broken(r)
	}
}

// forwardKey runs the assembled-key admission check for this viewer.
func (v *viewer) forwardKey(f *Frame) {
	if f.Rend != v.want && !(v.hasRend && f.Rend == v.rend) {
		return
	}
	now := v.room.sim.Time()
	if !v.ctl.KeyFits(now, f.Rend, f.Wire(), v.room.costs()) {
		if tracef != nil {
			tracef(v.room.sim.Now(), v.id, "key %v #%d %dB refused (applied %v stopped %v)", f.Rend, f.Num, f.Size, v.rend, v.stopped)
		}
		// Never forwarded, never stalled: a viewer on this rendition stops
		// it and recovers on the other one.
		v.refuse(f.Rend)
		return
	}
	if !v.hasRend || v.rend == f.Rend && v.stopped {
		v.rend, v.hasRend, v.stopped = f.Rend, true, false
		v.tid = v.wantTID
		if f.Rend != v.want {
			v.tid = 2
		}
	}
	if !v.ctl.Cont.Allow(info(f), v.wantTID) {
		v.refuse(f.Rend) // an earlier picture showed an unsupported structure
		return
	}
	v.latestKey[f.Rend] = f.Num
	if tracef != nil {
		tracef(v.room.sim.Now(), v.id, "key %v #%d %dB forwarded tid %d", f.Rend, f.Num, f.Size, v.tid)
	}
	v.enqueue(f)
}

// lostAtIngress reports a picture the SFU never received whole to a viewer
// forwarding its rendition. A lost base breaks the chain, which the SFU
// detects (OnBroken).
func (v *viewer) lostAtIngress(f *Frame) {
	if v.joined && v.forwarding(f.Rend) {
		v.ctl.Cont.Lost(info(f))
		if f.Key || f.TID == 0 {
			v.broken(f.Rend)
		}
	}
}

// forwardDelta applies the forwarding rules to an assembled delta:
// temporal downswitches are immediate, upswitches wait for a switching
// point, continuity decides dependencies.
func (v *viewer) forwardDelta(f *Frame) {
	if !v.forwarding(f.Rend) {
		if left, _ := v.ctl.Pacer.QueuedKey(f.Rend); left > 0 && f.Rend == v.want && v.ctl.Cont.Allow(info(f), v.wantTID) {
			v.enqueue(f)
		}
		return
	}
	// The desired TID belongs to the desired rendition; while waiting for its
	// key the current rendition keeps its layers.
	if v.want == v.rend && (v.wantTID < v.tid || v.wantTID > v.tid && f.Switch) {
		v.tid = v.wantTID
		v.apply()
	}
	if !v.ctl.Cont.Allow(info(f), v.tid) {
		if v.ctl.Cont.Unsupported(f.Rend) {
			// As internal/sfu's refuse: stop the rendition at once and
			// recover on the other one.
			v.refuse(f.Rend)
		}
		return
	}
	v.enqueue(f)
}

// enqueue offers a whole picture to the pacer, then wakes the writer, as
// internal/sfu's enqueue does.
func (v *viewer) enqueue(f *Frame) {
	r := f.Rend
	class := ctrl.ClassTL0 + ctrl.Class(f.TID)
	if f.Key {
		class = ctrl.ClassKey
	}
	defer v.pump()
	for i := range f.Packets {
		o := &outPkt{kind: kindMedia, size: f.PacketSize(i), frame: f, idx: i, rend: r}
		p := ctrl.Packet{Size: o.size, Kind: ctrl.KindVideo, Class: class, Rendition: r, Frame: f.Num, TID: f.TID, Key: f.Key, Switch: f.Switch,
			Last: i == f.Packets-1, Packets: f.Packets, FrameSize: f.Wire(), Deadline: epoch.Add(deadline(f)), FreshUntil: epoch.Add(f.Capture + pubDelay + ctrl.FrameDeadline)}
		p.SetReferences(info(f))
		if !v.queue(o, p) {
			if tracef != nil {
				b, n := v.ctl.Pacer.Queued()
				tracef(v.room.sim.Now(), v.id, "refused %v #%d tid %d size %d (queued %dB %d frames)", f.Rend, f.Num, f.TID, p.FrameSize, b, n)
			}
			// Whole-frame admission refused the picture.
			v.Refused++
			v.ctl.Cont.Lost(info(f))
			if fateLog != nil {
				fateLog(v.id, f, fateDropped, v.room.sim.Now())
			}
			return
		}
	}
	if f.Key && f.Rend == ctrl.Native {
		v.step() // production observes the admitted key before its writer wakes
	}
}

// deadline is a picture's useful deadline (docs/CONTROLLER.md, repair).
func deadline(f *Frame) time.Duration {
	if f.Key {
		return f.Assembled + ctrl.KeyEnvelope
	}
	return f.Capture + pubDelay + ctrl.FrameDeadline
}

// sequence assigns the RTP sequence number at send time, as an SFU that
// rewrites sequences does, so pacer drops leave no holes for the receiver
// to NACK. It also caches the packet and builds FEC groups over TL0 and key
// packets.
func (v *viewer) sequence(o *outPkt, dl time.Duration) {
	r := o.rend
	v.seq[r]++
	o.seq = v.seq[r]
	v.cache[r][o.seq] = &cacheEntry{pkt: o, pushed: v.room.sim.Now(), last: v.room.sim.Now(), generation: v.ctl.Generation(), deadline: dl}
	delete(v.cache[r], o.seq-8192)
	if v.fec > 0 && (o.frame.Key || o.frame.TID == 0 || v.room.fecAll) {
		// As internal/sfu's protect: the depth is bounded by the 16-packet
		// ULPFEC mask, a group whose span would exceed it closes early, and
		// groups that can no longer help (deadline passed, or a newer key)
		// are discarded.
		size, depth := ctrl.FECGroup(v.fec, v.interleave)
		if len(v.group[r]) != depth {
			v.group[r] = make([][]fecEntry, depth)
		}
		now := v.room.sim.Now()
		for b, g := range v.group[r] {
			if n := len(g); n > 0 && (now >= deadline(g[n-1].frame) || o.frame.Key && g[n-1].frame != o.frame) {
				v.group[r][b] = nil
			}
		}
		bucket := int(o.seq) % depth
		g := v.group[r][bucket]
		if len(g) > 0 && o.seq-g[0].seq >= 16 {
			v.parity(r, g)
			g = nil
		}
		g = append(g, fecEntry{o.seq, o.frame, o.idx})
		if len(g) >= size {
			v.parity(r, g)
			g = nil
		}
		v.group[r][bucket] = g
		// As internal/sfu's protect: a protected picture's last packet
		// flushes its partial groups, so parity leaves while it can still
		// repair the picture instead of waiting for the next base picture
		// (C2 §5 Wi-Fi: parity chosen in packet time).
		if o.idx == o.frame.Packets-1 {
			for b, g := range v.group[r] {
				if len(g) > 0 {
					v.parity(r, g)
					v.group[r][b] = nil
				}
			}
		}
	}
}

// parity queues one FEC packet over g, in the class of its newest picture.
func (v *viewer) parity(r ctrl.Rendition, g []fecEntry) {
	f := g[len(g)-1].frame
	// As internal/sfu's ulpParity: the parity payload is as long as the
	// longest protected packet, plus the ULPFEC (14) and RED (1) headers.
	size := 0
	for _, e := range g {
		size = max(size, e.frame.PacketSize(e.idx))
	}
	fo := &outPkt{kind: kindFEC, size: size + 15, rend: r, group: g}
	protected := ctrl.ClassTL0
	if f.Key {
		protected = ctrl.ClassKey
	}
	v.push(fo, ctrl.Packet{Size: fo.size, Kind: ctrl.KindVideo, Class: ctrl.ClassFEC, ProtectedClass: protected, Rendition: r, Frame: f.Num, TID: f.TID, Last: true, Packets: 1, Deadline: epoch.Add(deadline(f))})
}

func (v *viewer) push(o *outPkt, p ctrl.Packet) bool {
	if !v.queue(o, p) {
		return false
	}
	v.pump()
	return true
}

// queue hands one packet to the pacer without sending.
func (v *viewer) queue(o *outPkt, p ctrl.Packet) bool {
	v.nextID++
	p.ID = v.nextID
	p.Enqueued = v.room.sim.Time()
	if !v.ctl.Pacer.Push(v.room.sim.Time(), p) {
		return false
	}
	v.pending[p.ID] = o
	return true
}

func (v *viewer) pump() {
	s := v.room.sim
	now := s.Time()
	v.ctl.BeforeSend(now, 0, ctrl.Option{Rendition: v.want, TID: v.wantTID}, v.room.costs())
	for {
		p, ok := v.ctl.Pacer.Pop(now)
		if !ok {
			break
		}
		v.send(p)
	}
	next := v.ctl.Pacer.NextAt(now)
	if next.IsZero() {
		return
	}
	at := max(next.Sub(epoch), s.Now()+time.Microsecond)
	if v.pumpAt > s.Now() && v.pumpAt <= at {
		return
	}
	v.pumpAt = at
	s.At(at, func() {
		if v.pumpAt == at {
			v.pumpAt = 0
			v.pump()
		}
	})
}

func (v *viewer) send(p ctrl.Packet) {
	s := v.room.sim
	o := v.pending[p.ID]
	delete(v.pending, p.ID)
	if o == nil {
		if p.Class != ctrl.ClassPadding {
			return
		}
		o = &outPkt{kind: kindPadding, size: p.Size}
	}
	if o.kind == kindMedia {
		if o.frame.Key && o.idx == 0 && (!v.hasRend || v.rend != o.rend) {
			v.rend, v.hasRend, v.stopped = o.rend, true, false
			v.tid = v.ctl.Operating().TID
		}
		dl := deadline(o.frame)
		if !p.Deadline.IsZero() {
			dl = p.Deadline.Sub(epoch) // extended behind a key (ctrl.Queue)
		}
		v.sequence(o, dl)
	}
	k := &rtpPkt{kind: o.kind, tseq: v.tseq, rend: o.rend, seq: o.seq, frame: o.frame, idx: o.idx, group: o.group, sent: o.origin}
	kind := ctrl.KindVideo
	switch o.kind {
	case kindPadding:
		kind = ctrl.KindPadding
		v.Padding += int64(o.size)
	case kindAudio:
		kind = ctrl.KindAudio
		v.Audio += int64(o.size)
		if !o.rtx {
			// As internal/sfu prepare: cached at send for audioMaxAge.
			v.audioCache[o.seq] = &cacheEntry{pkt: o, pushed: s.Now(), last: s.Now(), generation: v.ctl.Generation()}
			delete(v.audioCache, o.seq-128)
		}
	default:
		v.WireVideo += int64(o.size)
	}
	if p.Key && p.Last {
		if option, ok := v.ctl.CompletionOption(s.Time(), p, v.room.costs()); ok {
			v.tid = option.TID
			v.apply()
		}
	}
	if fateLog != nil && o.kind == kindMedia && p.Last {
		fateLog(v.id, o.frame, fateSent, s.Now())
	}
	v.tsent[v.tseq%uint64(len(v.tsent))] = sentMedia{}
	if o.kind == kindMedia || o.kind == kindRTX {
		v.tsent[v.tseq%uint64(len(v.tsent))] = sentMedia{v.tseq + 1, o.rend, o.seq}
	}
	v.ctl.OnSent(ctrl.Sent{Seq: v.tseq, At: s.Time(), Size: o.size, Kind: kind, Class: p.Class, Cluster: p.Cluster, Generation: v.ctl.Generation()})
	v.tseq++
	v.SentPkts[o.kind]++
	if o.rtx {
		v.AudioRTX++
	}
	v.fwd.Send(&Packet{Size: o.size, Flow: v.id, deliver: v.rx.Receive, payload: k})
}

// nackAudio is internal/sfu nackAudio: a cached Opus packet is resent as is
// while the repair policy predicts it arrives before its deadline.
func (v *viewer) nackAudio(seq int64, e ctrl.Estimate) {
	s := v.room.sim
	c := v.audioCache[seq]
	if c == nil {
		return
	}
	departure, scheduled := v.ctl.Pacer.RTXDeparture(s.Time(), ctrl.KindAudio, c.pkt.size, 0)
	if !scheduled {
		return
	}
	arrival := departure.Add(e.RTT / 2).Sub(epoch)
	deadline := c.pushed + audioMaxAge
	if !v.ctl.Repair.Resend(ctrl.RepairQuery{Age: s.Now() - c.pushed, Sends: c.rtx, Since: s.Now() - c.last, RTT: e.RTT, PredictedArrival: epoch.Add(arrival), Deadline: epoch.Add(deadline), Current: c.generation == v.ctl.Generation()}) {
		return
	}
	c.rtx++
	c.last = s.Now()
	o := *c.pkt
	o.rtx = true
	v.push(&o, ctrl.Packet{Size: o.size, Kind: ctrl.KindAudio, Class: ctrl.ClassRTX, Last: true, Packets: 1, Deadline: epoch.Add(deadline)})
}

// resend is internal/sfu's resend of one cached video packet, for a NACK or
// a TWCC loss report: the repair policy decides. A packet of a picture above
// the forwarded TID is obsolete enhancement and is not repaired (C2 §5
// A12/A13).
func (v *viewer) resend(c *cacheEntry, e ctrl.Estimate) {
	s := v.room.sim
	f := c.pkt.frame
	departure, scheduled := v.ctl.Pacer.RTXDeparture(s.Time(), ctrl.KindVideo, c.pkt.size, 0)
	if !scheduled {
		return
	}
	arrival := departure.Add(e.RTT / 2).Sub(epoch)
	current := v.hasRend && !v.stopped && f.Rend == v.rend && c.generation == v.ctl.Generation() && f.Num >= v.latestKey[f.Rend] && f.TID <= v.tid
	ok := v.ctl.Repair.Resend(ctrl.RepairQuery{Age: s.Now() - c.pushed, Sends: c.rtx, Since: s.Now() - c.last, RTT: e.RTT, PredictedArrival: epoch.Add(arrival), Deadline: epoch.Add(c.deadline), Current: current})
	if v.room.resend != nil {
		ok = v.room.resend(s.Now()-c.pushed, c.rtx)
	}
	if !ok {
		return
	}
	c.rtx++
	c.last = s.Now()
	o := *c.pkt
	o.kind = kindRTX
	rtx := func() {
		cp := o
		v.push(&cp, ctrl.Packet{Size: cp.size, Kind: ctrl.KindVideo, Class: ctrl.ClassRTX, Rendition: cp.rend, Frame: cp.frame.Num, TID: cp.frame.TID, Last: true, Packets: 1, Deadline: epoch.Add(c.deadline)})
	}
	rtx()
	if d := v.room.rtxCopy; d > 0 {
		s.After(d, rtx)
	}
}

// up sends a receiver message of the current PC over the current reverse path.
func (v *viewer) up(size int, msg any) { v.upFrom(v.pcGen)(size, msg) }

// upFrom sends messages of PC generation gen; once that PC is replaced its
// late feedback is discarded, as a closed PeerConnection's would be.
func (v *viewer) upFrom(gen int) func(int, any) {
	return func(size int, msg any) {
		v.rev.Send(&Packet{Size: size, Flow: 1000 + v.id, payload: msg, deliver: func(p *Packet) {
			if gen == v.pcGen {
				v.onUp(p)
			}
		}})
	}
}

func (v *viewer) onUp(p *Packet) {
	s := v.room.sim
	now := s.Time()
	v.lastUp = s.Now()
	switch m := p.payload.(type) {
	case twccMsg:
		v.ctl.OnFeedback(now, m.fb)
		// As internal/sfu's feedback: a key or TL0 packet TWCC reports lost
		// is repaired at once, without waiting for the receiver's NACK
		// (C2 §5 A12/A13); Repair.Resend deduplicates the two triggers.
		e := v.ctl.Est.Estimate(now)
		for _, a := range m.fb.Arrivals {
			if t := v.tsent[a.Seq%uint64(len(v.tsent))]; a.At.IsZero() && t.tseq == a.Seq+1 {
				if c := v.cache[t.rend][t.seq]; c != nil && (c.pkt.frame.Key || c.pkt.frame.TID == 0) {
					v.resend(c, e)
				}
			}
		}
		v.step()
	case nackMsg:
		e := v.ctl.Est.Estimate(now)
		for _, it := range m.items {
			if it.audio {
				v.nackAudio(it.seq, e)
				continue
			}
			if c := v.cache[it.rend][it.seq]; c != nil {
				v.resend(c, e)
			}
		}
	case rrMsg:
		r := ctrl.ReceiverReport{FractionLost: m.lost}
		if m.lsr >= 0 {
			r.RTT = s.Now() - m.lsr - m.dlsr
		}
		v.ctl.OnReport(now, r)
	case stallMsg:
		if tracef != nil {
			tracef(s.Now(), v.id, "decoder stall")
		}
		if v.ctl.OnDecoderStall(now) {
			v.step()
		}
	}
}

func (v *viewer) step() {
	if !v.joined {
		return
	}
	d := v.ctl.Step(v.room.sim.Time(), v.room.costs())
	if v.room.adjust != nil {
		v.room.adjust(v.id, v.room.sim.Now(), &d)
	}
	v.lastRate = d.Rate
	v.fec = d.FEC
	v.interleave = d.Interleave
	v.want, v.wantTID = d.Desired.Rendition, d.Desired.TID
	if d.Stop && v.forwarding(v.rend) && v.want != v.rend {
		// As internal/sfu's step: a drain episode stops the obsolete
		// rendition at once; the desired rendition's key restarts.
		v.stopped = true
		v.ctl.Pacer.Purge(v.rend)
	}
	if v.hasRend && v.want == v.rend && v.wantTID < v.tid {
		v.tid = v.wantTID
		v.apply()
	}
	v.pump()
}
