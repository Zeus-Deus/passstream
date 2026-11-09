package sfu

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// viewer is one viewer's forwarder: the wire around its ctrl.Viewer, driven
// the way internal/sfusim drives it. Ingress, RTCP and the writer goroutine
// share it under mu. Only the writer goroutine (run) writes to the transport,
// and never while holding mu, so a slow socket blocks no one but this viewer.
// Every rate, layer, key and repair decision is ctrl's.
type viewer struct {
	mu           sync.Mutex
	id           int
	peer         *Peer
	room         *Room
	ctl          *ctrl.Viewer
	video, audio *boundTrack
	wake         chan struct{}
	// Popped transport writes, still owned by the writer (blocked-writer repair regression).
	writerPending atomic.Int64

	joined     bool    // admitted and connected once: the controller runs
	away       [2]bool // a base of the rendition was missed behind the media barrier; reported on return
	hasRend    bool
	stopped    bool // the forwarded rendition's key was refused or unsupported
	rend, want ctrl.Rendition
	tid        uint8
	wantTID    uint8
	latestKey  [2]uint64 // picture number of the latest forwarded key
	pending    map[uint64]*outPacket
	nextID     uint64
	nextStep   time.Time
	decision   ctrl.Decision
	fec        float64
	interleave int
	groups     [][]*rtp.Packet // FEC groups per interleave bucket
	groupFrame []*outFrame
	seq        uint16 // video sequence number, assigned at departure
	tseq       uint64 // transport-wide sequence number, unwrapped
	rewrite    continuity
	cache      packetCache
	feedbacks  [8]uint64 // recent TWCC report identities (see feedback)
	feedbackN  int

	audioSeq     uint16
	audioSource  uint64
	audioStarted bool
	audioOffset  uint32
	audioLastTS  uint32
	audioCache   packetCache

	wireBytes, paddingBytes, fecPackets, drops atomic.Uint64
	audioSent, audioDropped, audioReplayed     atomic.Uint64
	// Pictures whose last packet departed, by TID, and forwarded keys
	// (diagnostics); blackholed counts the test-only negative control.
	departed     [3]atomic.Uint64
	departedKeys atomic.Uint64
	blackholed   atomic.Uint64

	media mediaAudit // Tier B harness diagnostics, fixed size, under mu
}

// mediaAudit is what the Tier B harness (web/torture.mjs) correlates with the
// receiver; every field is fixed-size, so it adds no per-session growth.
//   - full holds the rewritten RTP timestamps of the latest native pictures
//     whose last packet departed while T2 was forwarded (GATES "Full
//     native"); first is the first picture of that continuous episode.
//   - receipts maps recent transport sequence numbers to the original video
//     packet they carried, so TWCC counts each video sequence number once at
//     the receiver (GATES "Unique original media": duplicates, RTX copies
//     beyond the first, FEC and padding are excluded). Bytes are RTP payload
//     bytes, the same unit as offered.
//   - offered counts the RTP payload bytes of assembled ingress pictures by
//     rendition and TID.
type mediaAudit struct {
	full               [16]uint32
	fullN, fullHead    int
	first              uint32
	inEpisode          bool
	receipts           [2048]receipt
	received           [4096 / 64]uint64 // by video sequence number mod 4096
	unique, uniquePkts uint64
	lastSeq            uint16
	offered            [2][3]uint64
}

type receipt struct {
	tseq uint32 // transport sequence + 1; zero is empty
	seq  uint16
	size uint16
}

func (m *mediaAudit) fullPicture(ts uint32) {
	if !m.inEpisode {
		m.first, m.inEpisode, m.fullN = ts, true, 0
	}
	m.full[m.fullHead] = ts
	m.fullHead = (m.fullHead + 1) % len(m.full)
	m.fullN = min(m.fullN+1, len(m.full))
}

func (m *mediaAudit) endEpisode() { m.inEpisode, m.fullN = false, 0 }

func (m *mediaAudit) fullTimestamps() []uint32 {
	out := make([]uint32, m.fullN)
	for i := range out {
		out[i] = m.full[(m.fullHead-m.fullN+i+len(m.full))%len(m.full)]
	}
	return out
}

// sent records a departing video media packet (original or retransmission)
// under its transport sequence; an original starts a new incarnation of seq.
func (m *mediaAudit) sent(tseq uint64, seq uint16, size int, original bool) {
	if original {
		m.received[seq%4096/64] &^= 1 << (seq % 64)
	}
	m.receipts[tseq%uint64(len(m.receipts))] = receipt{tseq: uint32(tseq) + 1, seq: seq, size: uint16(min(size, 0xffff))}
}

// seqOf maps a transport sequence to the video media sequence it carried.
func (m *mediaAudit) seqOf(tseq uint64) (uint16, bool) {
	r := m.receipts[tseq%uint64(len(m.receipts))]
	return r.seq, r.tseq == uint32(tseq)+1
}

// arrived counts a TWCC-reported arrival once per video sequence number.
func (m *mediaAudit) arrived(tseq uint64) {
	r := &m.receipts[tseq%uint64(len(m.receipts))]
	if r.tseq != uint32(tseq)+1 {
		return
	}
	word, bit := r.seq%4096/64, uint64(1)<<(r.seq%64)
	if m.received[word]&bit != 0 {
		return
	}
	m.received[word] |= bit
	m.unique += uint64(r.size)
	m.uniquePkts++
	m.lastSeq = r.seq
}

type outKind uint8

const (
	outMedia outKind = iota
	outRTX
	outFEC
	outAudio
	outAudioRTX
)

// outPacket is what a ctrl.Packet ID stands for until it departs.
type outPacket struct {
	kind   outKind
	pkt    *rtp.Packet
	frame  *outFrame
	index  int
	source uint64 // audio: publisher epoch
}

// outFrame is one forwarded picture. Its sequence numbers are reserved as
// one block when its first packet departs, so a picture stays contiguous
// even when the pacer interleaves a higher-priority picture into it.
type outFrame struct {
	info     ctrl.FrameInfo
	packets  int
	base     uint16
	reserved bool
	deadline time.Time
	capture  time.Time // immutable ingress clock mapping for departure-time rewrite
	source   uint64
}

// wireWrite is one packet the writer goroutine sends outside the lock.
type wireWrite struct {
	writer  webrtc.TrackLocalWriter
	h       rtp.Header
	payload []byte
}

func newViewer(p *Peer, r *Room, video, audio *boundTrack) *viewer {
	v := &viewer{id: int(r.nextViewer.Add(1)), peer: p, room: r, video: video, audio: audio, want: ctrl.Low, tid: testMaxTID, wantTID: testMaxTID,
		pending: map[uint64]*outPacket{}, wake: make(chan struct{}, 1), cache: newPacketCache(2048), audioCache: newPacketCache(128)}
	v.setController(ctrl.NewViewer(v.id, r.keys))
	return v
}

// release is the pacer's drop hook, called under mu inside the mutation that
// discarded p: the payload is freed and a dropped picture reported to
// continuity at once, not when the writer next runs Step (C2-1).
func (v *viewer) release(p ctrl.Packet) {
	o := v.pending[p.ID]
	delete(v.pending, p.ID)
	v.drops.Add(1)
	if o == nil {
		return
	}
	if o.kind == outAudio {
		v.audioDropped.Add(1)
	}
	if o.kind == outMedia && p.Last && !p.Superseded {
		v.ctl.Cont.Lost(o.frame.info)
	}
}

func (v *viewer) setController(c *ctrl.Viewer) {
	c.Pacer.SetRelease(v.release)
	v.ctl = c
}

func (v *viewer) notify() {
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

func (v *viewer) ready(now time.Time) bool { return v.peer.admitted.Load() && v.peer.mediaReady(now) }

// forwarding reports whether r is the rendition this viewer decodes now.
func (v *viewer) forwarding(r ctrl.Rendition) bool { return v.hasRend && !v.stopped && v.rend == r }

func (v *viewer) apply(now time.Time) {
	if left, _ := v.ctl.Pacer.QueuedKey(v.rend); left > 0 {
		return // a partial key has not applied its rendition/TID yet
	}
	v.ctl.OnApplied(now, ctrl.Option{Rendition: v.rend, TID: v.tid})
	v.peer.Layer.Store(int32(v.tid))
	v.peer.thumbnail.Store(v.rend == ctrl.Low)
	if v.rend != ctrl.Native || v.tid != 2 {
		v.media.endEpisode()
	}
}

// offer is called by ingress, under the room lock, with every assembled
// picture of either rendition.
func (v *viewer) offer(now time.Time, pic *picture) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.joined {
		return
	}
	if r, tid := pic.rend, pic.info.TID; int(r) < len(v.media.offered) && int(tid) < len(v.media.offered[r]) {
		for _, p := range pic.packets {
			v.media.offered[r][tid] += uint64(len(p.Payload))
		}
	}
	if !v.ready(now) {
		// The decoder misses this picture. A broken chain is reported once
		// the viewer is reachable again: a key requested now would be
		// delivered to no one, yet spend the room's budget and satisfy the
		// request (viewer_flap diagnosis).
		if v.forwarding(pic.rend) {
			v.ctl.Cont.Lost(pic.info)
			v.away[pic.rend] = v.away[pic.rend] || pic.info.Key || pic.info.TID == 0
		}
		return
	}
	for r, broken := range v.away {
		if broken {
			v.away[r] = false
			v.broken(now, ctrl.Rendition(r))
		}
	}
	if pic.info.Key {
		v.forwardKey(now, pic)
	} else {
		v.forward(now, pic)
	}
	v.notify()
}

// lost reports a picture of the forwarded rendition the viewer never gets;
// a lost base is a broken chain the SFU detected.
func (v *viewer) lost(now time.Time, info ctrl.FrameInfo) {
	v.ctl.Cont.Lost(info)
	if info.Key || info.TID == 0 {
		v.broken(now, info.Rendition)
	}
}

// broken reports a broken chain of r. An accepted recovery wants low at
// once, so the decision is re-run before the next picture: the low key it
// requested can assemble before the next periodic step (A8 outage).
func (v *viewer) broken(now time.Time, r ctrl.Rendition) {
	if v.ctl.OnBroken(now, r) && v.joined {
		v.step(now)
	}
}

// ingressLost reports publisher-leg loss of a picture (or a whole missing
// picture) of rendition r.
func (v *viewer) ingressLost(now time.Time, info ctrl.FrameInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.joined && v.forwarding(info.Rendition) {
		v.lost(now, info)
	}
}

// refuse stops forwarding r: its key did not fit or its structure is
// unsupported. The viewer is never left waiting for a key it cannot receive;
// OnBroken moves it to the other rendition.
func (v *viewer) refuse(now time.Time, r ctrl.Rendition) {
	if v.forwarding(r) {
		v.stopped = true
		v.ctl.Pacer.Purge(r)
		v.broken(now, r)
	}
}

// forwardKey runs the assembled-key admission check for this viewer.
func (v *viewer) forwardKey(now time.Time, pic *picture) {
	r := pic.rend
	if r != v.want && !(v.hasRend && r == v.rend) {
		return
	}
	if !v.ctl.Cont.Supported(pic.info) {
		v.ctl.Cont.Allow(pic.info, 0) // marks the rendition unsupported
		v.refuse(now, r)
		return
	}
	if !v.ctl.KeyFits(now, r, pic.wire, v.room.costs(now)) {
		v.refuse(now, r)
		return
	}
	if !v.hasRend || v.rend == r && v.stopped {
		v.rend, v.hasRend, v.stopped = r, true, false
		v.tid = v.wantTID
		if r != v.want {
			v.tid = min(2, testMaxTID)
		}
	}
	if !v.ctl.Cont.Allow(pic.info, v.wantTID) {
		v.refuse(now, r) // an earlier picture showed an unsupported structure
		return
	}
	v.latestKey[r] = pic.info.Picture
	v.enqueue(now, pic)
}

// forward applies the per-picture rules to a delta: temporal downswitches
// are immediate, upswitches wait for a switching point, continuity decides
// dependencies.
func (v *viewer) forward(now time.Time, pic *picture) {
	if !v.forwarding(pic.rend) {
		if left, _ := v.ctl.Pacer.QueuedKey(pic.rend); left > 0 && pic.rend == v.want && v.ctl.Cont.Allow(pic.info, v.wantTID) {
			v.enqueue(now, pic) // the same bounded queue holds its known closure
		}
		return
	}
	// The desired TID belongs to the desired rendition; while waiting for its
	// key the current rendition keeps its layers.
	if v.want == v.rend && (v.wantTID < v.tid || v.wantTID > v.tid && pic.switchPoint) {
		v.tid = v.wantTID
		v.apply(now)
	}
	if !v.ctl.Cont.Allow(pic.info, v.tid) {
		// Unsupported includes the stateful pattern and TL0PICIDX checks
		// the stateless Supported cannot see.
		if v.ctl.Cont.Unsupported(pic.rend) {
			v.refuse(now, pic.rend)
		}
		return
	}
	if pic.info.TID == testBlackholeTID {
		v.blackholed.Add(1)
		return
	}
	v.enqueue(now, pic)
}

func (v *viewer) enqueue(now time.Time, pic *picture) {
	n := len(pic.packets)
	f := &outFrame{info: pic.info, packets: n, deadline: pic.deadline, capture: pic.capture, source: pic.source}
	class := ctrl.ClassTL0 + ctrl.Class(pic.info.TID)
	if pic.info.Key {
		class = ctrl.ClassKey
	}
	red := 0
	if v.video.bound().red {
		red = 1
	}
	out := make([]*outPacket, n)
	sizes := make([]int, n)
	total := 0
	for i, orig := range pic.packets {
		// Upstream header extensions belong to the publisher's SDP; the
		// descriptor rewrite needs a private payload.
		pkt := &rtp.Packet{Header: rtp.Header{Version: 2, Marker: i == n-1, Timestamp: orig.Timestamp, SSRC: orig.SSRC}, Payload: append([]byte(nil), orig.Payload...)}
		out[i] = &outPacket{kind: outMedia, pkt: pkt, frame: f, index: i}
		sizes[i] = rtpHeader + red + len(pkt.Payload) + wireOverhead
		total += sizes[i]
	}
	for i, o := range out {
		p := ctrl.Packet{Size: sizes[i], Kind: ctrl.KindVideo, Class: class, Rendition: pic.rend, Frame: pic.info.Picture, Last: i == n-1, Packets: n,
			FrameSize: total, TID: pic.info.TID, Key: pic.info.Key, Switch: pic.switchPoint, Deadline: pic.deadline, FreshUntil: pic.capture.Add(ctrl.FrameDeadline)}
		p.SetReferences(pic.info)
		if !v.push(now, o, p) {
			// Whole-frame admission refused the picture.
			v.ctl.Cont.Lost(pic.info)
			return
		}
	}
	if pic.info.Key && pic.rend == ctrl.Native {
		v.step(now) // the assembled key can fund discovery before the next periodic tick
	}
	v.peer.Sent.Add(uint64(n))
}

func (v *viewer) push(now time.Time, o *outPacket, p ctrl.Packet) bool {
	v.nextID++
	p.ID = v.nextID
	p.Enqueued = now
	if !v.ctl.Pacer.Push(now, p) {
		return false
	}
	v.pending[p.ID] = o
	return true
}

// step runs one control decision and applies what the forwarder applies
// immediately (a temporal downswitch); the rest waits for keys and
// switching points.
func (v *viewer) step(now time.Time) {
	d := v.ctl.Step(now, v.room.costs(now))
	v.nextStep = now.Add(ctrl.OveruseTime)
	v.expireGroups(now, nil)
	v.decision = d
	v.fec, v.interleave = d.FEC, d.Interleave
	v.want, v.wantTID = d.Desired.Rendition, min(d.Desired.TID, testMaxTID)
	if d.Stop && v.forwarding(v.rend) && v.want != v.rend {
		// A drain episode made the applied rendition obsolete: stop it now,
		// purge its backlog, and restart at the desired rendition's key
		// (C2 §5 A5).
		v.stopped = true
		v.ctl.Pacer.Purge(v.rend)
	}
	if v.hasRend && v.want == v.rend && v.wantTID < v.tid {
		v.tid = v.wantTID
		v.apply(now)
	}
}

// run is the viewer's writer goroutine: it steps the controller at least
// every OveruseTime, pops paced packets and writes them without the lock.
func (v *viewer) run() {
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	var batch []wireWrite
	for {
		v.mu.Lock()
		now := time.Now()
		if !v.joined && v.ready(now) {
			v.joined, v.nextStep = true, now
		}
		wait := ctrl.OveruseTime
		if v.joined {
			if !now.Before(v.nextStep) {
				v.step(now)
			}
			wait = v.nextStep.Sub(now)
		}
		batch = v.drain(now, batch[:0])
		v.writerPending.Add(int64(len(batch)))
		if next := v.ctl.Pacer.NextAt(now); !next.IsZero() {
			wait = min(wait, next.Sub(now))
		}
		v.mu.Unlock()
		for i, w := range batch {
			if _, err := w.writer.WriteRTP(&w.h, w.payload); err == nil {
				v.wireBytes.Add(uint64(w.h.MarshalSize() + len(w.payload) + int(w.h.PaddingSize) + wireOverhead))
			}
			v.writerPending.Add(-1)
			batch[i] = wireWrite{}
		}
		timer.Reset(max(wait, 250*time.Microsecond))
		select {
		case <-v.peer.done:
			return
		case <-v.wake:
		case <-timer.C:
		}
	}
}

func (v *viewer) drain(now time.Time, out []wireWrite) []wireWrite {
	v.ctl.BeforeSend(now, int(v.writerPending.Load()), ctrl.Option{Rendition: v.want, TID: v.wantTID}, v.room.costs(now))
	for {
		p, ok := v.ctl.Pacer.Pop(now)
		if !ok {
			return out
		}
		out = v.prepare(now, p, out)
	}
}

// Wire sizes: RTP fixed header, and IPv4 + UDP + SRTP authentication plus
// typical transport extensions.
const (
	rtpHeader    = 12
	wireOverhead = 50
	maxPadding   = 255 // RTP padding length is one byte
)

// prepare turns a departing ctrl.Packet into wire packets: it assigns
// sequence numbers, stamps the transport-wide sequence, caches media for
// NACK, builds FEC and reports every packet to the controller.
func (v *viewer) prepare(now time.Time, p ctrl.Packet, out []wireWrite) []wireWrite {
	o := v.pending[p.ID]
	delete(v.pending, p.ID)
	vb := v.video.bound()
	if o == nil {
		if p.Class != ctrl.ClassPadding || vb.writer == nil {
			return out
		}
		// The pacer sized this padding for its cluster; one RTP packet carries
		// at most 255 padding bytes, so the bytes go out as several packets.
		for left := p.Size; left > 0; {
			pad := min(maxPadding, max(1, left-rtpHeader-wireOverhead-8))
			v.seq++
			h := rtp.Header{Version: 2, Padding: true, PaddingSize: byte(pad), SSRC: vb.ssrc, PayloadType: vb.mediaPT, SequenceNumber: v.seq, Timestamp: v.rewrite.lastTS}
			out = v.send(now, vb, wireWrite{writer: vb.writer, h: h}, ctrl.KindPadding, p, out)
			left -= rtpHeader + 8 + pad + wireOverhead
		}
		return out
	}
	switch o.kind {
	case outMedia:
		if vb.writer == nil {
			return out
		}
		f := o.frame
		if !f.reserved {
			if f.info.Key && (!v.hasRend || v.rend != f.info.Rendition) {
				v.rend, v.hasRend, v.stopped = f.info.Rendition, true, false
				v.tid = v.ctl.Operating().TID
			}
			v.rewrite.start([]*rtp.Packet{o.pkt}, f.capture, f.source)
			f.base, f.reserved = v.seq+1, true
			v.seq += uint16(f.packets)
			// The pacer extends the deadline of a picture held behind a key.
			if !p.Deadline.IsZero() {
				f.deadline = p.Deadline
			}
		}
		pkt := o.pkt
		v.rewrite.packet(pkt)
		pkt.SequenceNumber, pkt.SSRC, pkt.PayloadType = f.base+uint16(o.index), vb.ssrc, vb.mediaPT
		v.cache.put(cachedPacket{pkt: pkt, at: now, last: now, frame: f, deadline: f.deadline, generation: v.ctl.Generation()})
		v.media.sent(v.tseq, pkt.SequenceNumber, len(pkt.Payload), true)
		out = v.send(now, vb, videoWrite(vb, pkt), ctrl.KindVideo, p, out)
		if v.fec > 0 && vb.red && (f.info.Key || f.info.TID == 0) {
			v.protect(now, vb, pkt, f, p.Last)
		}
		if f.info.Key && p.Last {
			if option, ok := v.ctl.CompletionOption(now, p, v.room.costs(now)); ok {
				v.tid = option.TID
				v.apply(now)
			}
		}
		if p.Last && f.info.Rendition == ctrl.Native && v.rend == ctrl.Native && v.tid == 2 {
			v.media.fullPicture(pkt.Timestamp)
		}
		if p.Last && f.info.TID <= 2 {
			v.departed[f.info.TID].Add(1)
			if f.info.Key {
				v.departedKeys.Add(1)
			}
		}
	case outRTX:
		if vb.writer != nil {
			v.media.sent(v.tseq, o.pkt.SequenceNumber, len(o.pkt.Payload), false)
			out = v.send(now, vb, videoWrite(vb, o.pkt), ctrl.KindVideo, p, out)
		}
	case outFEC:
		if vb.writer != nil {
			v.seq++
			h := rtp.Header{Version: 2, SSRC: vb.ssrc, PayloadType: vb.redPT, SequenceNumber: v.seq, Timestamp: o.pkt.Timestamp}
			out = v.send(now, vb, wireWrite{writer: vb.writer, h: h, payload: append([]byte{vb.fecPT}, o.pkt.Payload...)}, ctrl.KindVideo, p, out)
			v.fecPackets.Add(1)
		}
	case outAudio:
		ab := v.audio.bound()
		if ab.writer == nil {
			return out
		}
		pkt := o.pkt.Clone()
		if v.audioStarted && o.source != v.audioSource {
			v.audioOffset = v.audioLastTS + audioFrameTicks - pkt.Timestamp
		}
		v.audioStarted, v.audioSource = true, o.source
		pkt.Timestamp += v.audioOffset
		v.audioLastTS = pkt.Timestamp
		v.audioSeq++
		pkt.Header = rtp.Header{Version: 2, Marker: pkt.Marker, PayloadType: ab.mediaPT, SequenceNumber: v.audioSeq, Timestamp: pkt.Timestamp, SSRC: ab.ssrc}
		v.audioCache.put(cachedPacket{pkt: pkt, at: now, last: now, deadline: now.Add(audioMaxAge), generation: v.ctl.Generation()})
		out = v.send(now, ab, wireWrite{writer: ab.writer, h: pkt.Header.Clone(), payload: pkt.Payload}, ctrl.KindAudio, p, out)
		v.audioSent.Add(1)
	case outAudioRTX:
		if ab := v.audio.bound(); ab.writer != nil {
			out = v.send(now, ab, wireWrite{writer: ab.writer, h: o.pkt.Header.Clone(), payload: o.pkt.Payload}, ctrl.KindAudio, p, out)
			v.audioReplayed.Add(1)
		}
	}
	return out
}

// videoWrite encapsulates a media packet in RED when RED/ULPFEC is negotiated.
func videoWrite(b binding, pkt *rtp.Packet) wireWrite {
	w := wireWrite{writer: b.writer, h: pkt.Header.Clone(), payload: pkt.Payload}
	if b.red {
		w.h.PayloadType = b.redPT
		w.payload = append([]byte{b.mediaPT}, pkt.Payload...)
	}
	return w
}

// send stamps the transport-wide sequence and reports the packet to ctrl.
func (v *viewer) send(now time.Time, b binding, w wireWrite, kind ctrl.Kind, p ctrl.Packet, out []wireWrite) []wireWrite {
	if b.twccID != 0 {
		_ = w.h.SetExtension(b.twccID, []byte{byte(v.tseq >> 8), byte(v.tseq)})
	}
	size := w.h.MarshalSize() + len(w.payload) + int(w.h.PaddingSize) + wireOverhead
	v.ctl.OnSent(ctrl.Sent{Seq: v.tseq, At: now, Size: size, Kind: kind, Class: p.Class, Cluster: p.Cluster, Generation: v.ctl.Generation()})
	v.tseq++
	if kind == ctrl.KindPadding {
		v.paddingBytes.Add(uint64(size))
	}
	return append(out, w)
}

// protect adds a departed TL0 or key packet to its interleave bucket's FEC
// group and queues parity when the group is complete. Groups stay within the
// 16-packet ULPFEC mask: the depth is bounded by it and a group whose span
// would exceed it is closed early. The picture's last packet flushes its
// partial groups, so parity leaves while it can still repair the picture
// rather than waiting for the next base picture (C2 §5 Wi-Fi: parity chosen
// in packet time); ctrl.Need charges that flush.
func (v *viewer) protect(now time.Time, b binding, pkt *rtp.Packet, f *outFrame, last bool) {
	size, depth := ctrl.FECGroup(v.fec, v.interleave)
	if len(v.groups) != depth {
		v.groups, v.groupFrame = make([][]*rtp.Packet, depth), make([]*outFrame, depth)
	}
	v.expireGroups(now, f)
	i := int(pkt.SequenceNumber) % depth
	if g := v.groups[i]; len(g) > 0 && pkt.SequenceNumber-g[0].SequenceNumber >= 16 {
		v.parity(now, g, v.groupFrame[i])
		v.groups[i] = nil
	}
	view := &rtp.Packet{Header: pkt.Header.Clone(), Payload: pkt.Payload}
	if b.twccID != 0 {
		_ = view.SetExtension(b.twccID, []byte{0, 0})
	}
	v.groups[i] = append(v.groups[i], view)
	v.groupFrame[i] = f
	if len(v.groups[i]) >= size {
		v.parity(now, v.groups[i], f)
		v.groups[i], v.groupFrame[i] = nil, nil
	}
	if last {
		for j, g := range v.groups {
			if len(g) > 0 {
				v.parity(now, g, v.groupFrame[j])
				v.groups[j], v.groupFrame[j] = nil, nil
			}
		}
	}
}

// expireGroups discards open parity groups that can no longer help: the
// deadline of their newest picture passed, or next (the picture about to be
// protected) is another rendition or a newer key. Parity for them would be
// useless, and an idle bucket would otherwise hold its payloads until its
// next protected packet (C2-1).
func (v *viewer) expireGroups(now time.Time, next *outFrame) {
	for i, g := range v.groups {
		f := v.groupFrame[i]
		if len(g) == 0 || f == nil {
			continue
		}
		superseded := next != nil && next != f && (next.info.Rendition != f.info.Rendition || next.info.Key)
		if superseded || !now.Before(f.deadline) {
			v.groups[i], v.groupFrame[i] = nil, nil
		}
	}
}

func (v *viewer) parity(now time.Time, g []*rtp.Packet, f *outFrame) {
	protected := ctrl.ClassTL0
	if f.info.Key {
		protected = ctrl.ClassKey
	}
	payload := ulpParity(g)
	o := &outPacket{kind: outFEC, pkt: &rtp.Packet{Header: rtp.Header{Timestamp: g[len(g)-1].Timestamp}, Payload: payload}}
	v.push(now, o, ctrl.Packet{Size: rtpHeader + 1 + len(payload) + wireOverhead, Kind: ctrl.KindVideo, Class: ctrl.ClassFEC, ProtectedClass: protected,
		Rendition: f.info.Rendition, Frame: f.info.Picture, TID: f.info.TID, Last: true, Packets: 1, Deadline: f.deadline})
}

// arrival uses the pacer's actual next-RTX schedule. A transport batch that
// has not returned from WriteRTP has unknown completion time, not zero cost.
func (v *viewer) arrival(now time.Time, kind ctrl.Kind, size int) (time.Time, bool) {
	at, ok := v.ctl.Pacer.RTXDeparture(now, kind, size, int(v.writerPending.Load()))
	return at.Add(v.ctl.Est.Estimate(now).RTT / 2), ok
}

func (v *viewer) nackVideo(now time.Time, seqs []uint16) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.resendVideo(now, seqs, false)
	v.notify()
}

// resendVideo answers repair triggers for cached video packets: a NACK, or
// (critical) TWCC loss reports, where only key and TL0 packets are resent
// at once (C2 §5 A12/A13: early critical retransmission). The repair policy
// decides each, and deduplicates the two triggers by the age of the
// packet's latest copy. A packet above the forwarded TID belongs to an
// obsolete enhancement picture and is not repaired. Caller holds v.mu.
func (v *viewer) resendVideo(now time.Time, seqs []uint16, critical bool) {
	rtt := v.ctl.Est.Estimate(now).RTT
	for _, seq := range seqs {
		c := v.cache.get(seq)
		if c == nil || c.frame == nil {
			continue
		}
		f := c.frame
		if critical && !f.info.Key && f.info.TID != 0 {
			continue
		}
		arrival, scheduled := v.arrival(now, ctrl.KindVideo, c.pkt.MarshalSize()+wireOverhead)
		if !scheduled {
			continue
		}
		current := v.forwarding(f.info.Rendition) && c.generation == v.ctl.Generation() && f.info.Picture >= v.latestKey[f.info.Rendition] && f.info.TID <= v.tid
		if !v.ctl.Repair.Resend(ctrl.RepairQuery{Age: now.Sub(c.at), Sends: c.sends, Since: now.Sub(c.last), RTT: rtt, PredictedArrival: arrival, Deadline: c.deadline, Current: current}) {
			continue
		}
		c.sends++
		c.last = now
		v.push(now, &outPacket{kind: outRTX, pkt: c.pkt}, ctrl.Packet{Size: c.pkt.MarshalSize() + wireOverhead, Kind: ctrl.KindVideo, Class: ctrl.ClassRTX,
			Rendition: f.info.Rendition, Frame: f.info.Picture, TID: f.info.TID, Last: true, Packets: 1, Deadline: c.deadline})
	}
}

func (v *viewer) nackAudio(now time.Time, seqs []uint16) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, seq := range seqs {
		c := v.audioCache.get(seq)
		if c == nil {
			continue
		}
		arrival, scheduled := v.arrival(now, ctrl.KindAudio, c.pkt.MarshalSize()+wireOverhead)
		if !scheduled {
			continue
		}
		if !v.ctl.Repair.Resend(ctrl.RepairQuery{Age: now.Sub(c.at), Sends: c.sends, Since: now.Sub(c.last), RTT: v.ctl.Est.Estimate(now).RTT, PredictedArrival: arrival, Deadline: c.deadline, Current: c.generation == v.ctl.Generation()}) {
			continue
		}
		c.sends++
		c.last = now
		v.push(now, &outPacket{kind: outAudioRTX, pkt: c.pkt}, ctrl.Packet{Size: c.pkt.MarshalSize() + wireOverhead, Kind: ctrl.KindAudio, Class: ctrl.ClassRTX,
			Last: true, Packets: 1, Deadline: c.deadline})
	}
	v.notify()
}

// pushAudio queues one opaque Opus packet; ctrl paces it ahead of video.
func (v *viewer) pushAudio(now time.Time, pkt *rtp.Packet, source uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	o := &outPacket{kind: outAudio, pkt: pkt, source: source}
	p := ctrl.Packet{Size: rtpHeader + len(pkt.Payload) + wireOverhead, Kind: ctrl.KindAudio, Class: ctrl.ClassAudio, Last: true, Packets: 1, Deadline: now.Add(audioMaxAge)}
	if !v.joined || !v.ready(now) || !v.push(now, o, p) {
		v.audioDropped.Add(1)
		return
	}
	v.notify()
}

// handleRTCP applies one RTCP read from the sender of ssrc (video or audio).
// SRTCP hands a compound packet to every SSRC it names, so report blocks,
// NACKs and PLIs count only for this reader's SSRC; TWCC is transport-wide
// and deduplicated.
func (v *viewer) handleRTCP(now time.Time, ssrc uint32, video bool, pkts []rtcp.Packet) {
	v.peer.noteRTCP(now)
	for _, pkt := range pkts {
		switch fb := pkt.(type) {
		case *rtcp.TransportLayerCC:
			v.feedback(now, fb)
		case *rtcp.ReceiverReport:
			for _, r := range fb.Reports {
				if video && r.SSRC == ssrc {
					v.report(now, r)
				}
			}
		case *rtcp.TransportLayerNack:
			if fb.MediaSSRC != ssrc {
				continue
			}
			var seqs []uint16
			for _, pair := range fb.Nacks {
				seqs = append(seqs, pair.PacketList()...)
			}
			if video {
				v.nackVideo(now, seqs)
			} else {
				v.nackAudio(now, seqs)
			}
		case *rtcp.PictureLossIndication:
			if video && fb.MediaSSRC == ssrc {
				v.peer.requestKeyframe()
			}
		case *rtcp.FullIntraRequest:
			for _, e := range fb.FIR {
				if video && e.SSRC == ssrc {
					v.peer.requestKeyframe()
					break
				}
			}
		}
	}
}

func (v *viewer) report(now time.Time, r rtcp.ReceptionReport) {
	p := v.peer
	rr := ctrl.ReceiverReport{FractionLost: float64(r.FractionLost) / 256}
	if r.LastSenderReport != 0 {
		ntp := uint64(now.Unix()+2208988800)<<32 | uint64(now.Nanosecond())*(1<<32)/1_000_000_000
		if d := uint32(ntp>>16) - r.LastSenderReport - r.Delay; d < 1<<31 {
			rr.RTT = time.Duration(d) * time.Second / 65536
			p.rttNanos.Store(int64(rr.RTT))
		}
	}
	p.residualLoss.Store(uint32(r.FractionLost))
	p.reports.Add(1)
	v.mu.Lock()
	v.ctl.OnReport(now, rr)
	v.mu.Unlock()
}

// feedback converts one TWCC report to ctrl arrivals (unwrapped transport
// sequence, receiver clock; a zero time is a loss) and steps the controller.
func (v *viewer) feedback(now time.Time, fb *rtcp.TransportLayerCC) {
	if !validTransportFeedback(fb) {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	id := uint64(fb.FbPktCount)<<32 | uint64(fb.BaseSequenceNumber)<<16 | uint64(fb.PacketStatusCount)
	for _, seen := range v.feedbacks[:min(v.feedbackN, len(v.feedbacks))] {
		if seen == id {
			return
		}
	}
	v.feedbacks[v.feedbackN%len(v.feedbacks)] = id
	v.feedbackN++
	if v.tseq == 0 {
		return
	}
	last := int64(v.tseq - 1)
	base := last - int64(uint16(uint16(last)-fb.BaseSequenceNumber))
	at := time.Unix(0, 0).Add(time.Duration(fb.ReferenceTime) * 64 * time.Millisecond)
	arrivals := make([]ctrl.Arrival, 0, fb.PacketStatusCount)
	var lost []uint16 // video media sequence numbers TWCC reports lost
	n, delta := 0, 0
	visit := func(symbol uint16) {
		seq := base + int64(n)
		n++
		a := ctrl.Arrival{Seq: uint64(seq)}
		received := symbol == rtcp.TypeTCCPacketReceivedSmallDelta || symbol == rtcp.TypeTCCPacketReceivedLargeDelta
		if received {
			at = at.Add(time.Duration(fb.RecvDeltas[delta].Delta) * time.Microsecond)
			delta++
			a.At = at
		}
		if seq >= 0 && seq <= last {
			arrivals = append(arrivals, a)
			if received {
				v.media.arrived(uint64(seq))
			} else if q, ok := v.media.seqOf(uint64(seq)); ok && len(lost) < maxPicturePackets {
				lost = append(lost, q)
			}
		}
	}
	for _, chunk := range fb.PacketChunks {
		switch c := chunk.(type) {
		case *rtcp.RunLengthChunk:
			for i := 0; i < int(c.RunLength) && n < int(fb.PacketStatusCount); i++ {
				visit(c.PacketStatusSymbol)
			}
		case *rtcp.StatusVectorChunk:
			for _, s := range c.SymbolList {
				if n < int(fb.PacketStatusCount) {
					visit(s)
				}
			}
		}
	}
	v.ctl.OnFeedback(now, ctrl.Feedback{Arrivals: arrivals})
	if len(lost) > 0 {
		v.resendVideo(now, lost, true)
	}
	if v.joined {
		v.step(now)
	}
	v.notify()
}

// validTransportFeedback rejects a malformed TWCC report before any of it is
// used: status counts, symbols and receive deltas must agree.
func validTransportFeedback(f *rtcp.TransportLayerCC) bool {
	if f == nil || f.PacketStatusCount == 0 {
		return false
	}
	remaining, deltas := int(f.PacketStatusCount), 0
	add := func(symbol uint16, n int) bool {
		if symbol > rtcp.TypeTCCPacketReceivedLargeDelta {
			return false
		}
		n = min(n, remaining)
		remaining -= n
		if symbol != rtcp.TypeTCCPacketNotReceived {
			deltas += n
		}
		return true
	}
	for _, chunk := range f.PacketChunks {
		if remaining == 0 {
			break
		}
		switch c := chunk.(type) {
		case *rtcp.RunLengthChunk:
			if c == nil || !add(c.PacketStatusSymbol, int(c.RunLength)) {
				return false
			}
		case *rtcp.StatusVectorChunk:
			if c == nil {
				return false
			}
			for _, symbol := range c.SymbolList {
				if remaining == 0 {
					break
				}
				if !add(symbol, 1) {
					return false
				}
			}
		default:
			return false
		}
	}
	if remaining != 0 || deltas > len(f.RecvDeltas) {
		return false
	}
	for _, delta := range f.RecvDeltas[:deltas] {
		if delta == nil {
			return false
		}
	}
	return true
}

func (v *viewer) stall(now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.joined && v.ctl.OnDecoderStall(now) {
		v.step(now) // as broken: the requested low key may assemble before the next step
	}
}

// leave detaches a closed peer's forwarder before the room's key policy
// forgets it (Keys.Leave): late RTCP or heartbeats of the old PC must not
// re-enter the shared key state after a replacement.
func (v *viewer) leave() {
	v.peer.admitted.Store(false) // never rejoins
	v.mu.Lock()
	v.joined = false
	v.mu.Unlock()
}

func (v *viewer) pathChange(now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ctl.OnPathChange(now)
}

func (v *viewer) handoff(now time.Time) ctrl.RateState {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.ctl.Handoff(now)
}

// inherit starts this replacement PC from a handed-off rate, in a new
// generation and without admission evidence.
func (v *viewer) inherit(s ctrl.RateState) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.setController(ctrl.NewViewerFrom(v.id, v.room.keys, s))
}

func (v *viewer) rate(now time.Time) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.ctl.Est.Estimate(now).Rate
}

// keyTransit estimates the remaining delivery of an admitted key and the
// pictures queued behind it, and of the network debt a drain episode is
// draining ahead of new pictures (C2 §5 A5), bounded by the key envelope.
// The viewer app defers its stall clock while it is positive: replacing the
// PC cannot deliver video sooner than the queue ahead of it drains.
func (v *viewer) keyTransit(now time.Time) time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.keyTransitLocked(now)
}

func (v *viewer) keyTransitLocked(now time.Time) time.Duration {
	e := v.ctl.Est.Estimate(now)
	t := time.Duration(0)
	if v.ctl.Pacer.KeyInFlight() && e.Rate > 0 {
		queued, _ := v.ctl.Pacer.Queued()
		t = max(time.Millisecond, time.Duration(queued*8)*time.Second/time.Duration(e.Rate))
	}
	if e.Service > 0 && e.Debt > 0 {
		t = max(t, time.Millisecond, time.Duration(int64(e.Debt)*8)*time.Second/time.Duration(e.Service))
	}
	return min(ctrl.KeyEnvelope, t)
}

func (v *viewer) diagnostics(now time.Time) map[string]any {
	v.mu.Lock()
	defer v.mu.Unlock()
	e := v.ctl.Est.Estimate(now)
	queued, _ := v.ctl.Pacer.Queued()
	m := map[string]any{
		"wireBudget":            e.Rate,
		"provenWireRate":        e.Proven,
		"estimate":              map[string]any{"averageLoss": e.Loss},
		"desired":               fmt.Sprintf("%s/T%d", v.want, v.wantTID),
		"untenable":             v.decision.Untenable,
		"queuedBytes":           queued,
		"catchupMs":             v.keyTransitLocked(now).Milliseconds(),
		"bootstrapPaddingBytes": v.ctl.Probe.BootstrapUsed(),
		"keyframeBytes":         v.room.costs(now).KeyBytes[ctrl.Native],
		"fecPackets":            v.fecPackets.Load(),
		"pacerDrops":            v.drops.Load(),
		"paddingBytes":          v.paddingBytes.Load(),
		"wireBytes":             v.wireBytes.Load(),
		"forwardedPictures":     []uint64{v.departed[0].Load(), v.departed[1].Load(), v.departed[2].Load()},
		"forwardedKeys":         v.departedKeys.Load(),
		"testBlackholed":        v.blackholed.Load(),
		"audio":                 map[string]uint64{"forwarded": v.audioSent.Load(), "dropped": v.audioDropped.Load(), "replayed": v.audioReplayed.Load()},
		"uniqueMedia":           map[string]any{"at": now.UnixMilli(), "bytes": v.media.unique, "packets": v.media.uniquePkts, "seq": v.media.lastSeq},
		"offeredMedia":          v.media.offered,
		"keyTimes":              map[string]any{"native": keyTimes(v.ctl.KeyTimes(ctrl.Native)), "low": keyTimes(v.ctl.KeyTimes(ctrl.Low))},
	}
	if v.media.inEpisode {
		m["fullLayerTimestamps"] = v.media.fullTimestamps()
		m["fullLayerFirst"] = v.media.first
	}
	return m
}

// keyTimes is the viewer's latest key milestones for one rendition, Unix ms
// (0 = none): its request, the room's PLI emission and key assembly (room
// wide; they may predate the request), the key's last packet sent to this
// viewer, and whether a replacement's exception served it.
func keyTimes(t ctrl.KeyTimes) map[string]any {
	ms := func(at time.Time) int64 {
		if at.IsZero() {
			return 0
		}
		return at.UnixMilli()
	}
	return map[string]any{"requestedAt": ms(t.Requested), "emittedAt": ms(t.Emitted), "assembledAt": ms(t.Assembled), "deliveredAt": ms(t.Delivered), "exception": t.Exception}
}
