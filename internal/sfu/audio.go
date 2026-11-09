package sfu

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Audio is forwarded opaquely. Under E2EE every Opus frame is a complete
// SFrame ciphertext (track 1, no clear prefix), so nothing here parses an
// audio payload: packets are copied byte for byte, with only the RTP header
// rewritten (sequence numbers and a timestamp offset across publisher
// changes, like the video continuity).
//
// Per viewer: the publisher's reader offers each packet to a bounded queue
// (audioQueueLen, oldest dropped first) and never blocks; the viewer's
// goroutine writes it to the viewer track, whose packets the viewer's pacer
// sends ahead of queued video (audio is a few tens of kbit/s and is useless
// late) while charging them to the same wire budget. Layer admission
// subtracts the measured audio rate, so audio takes its share from the video
// enhancement layers first. Audio NACKs are answered from a small cache when
// the viewer negotiated them. RED is not used: Chrome splits RED inside the
// audio decoder (after the receive transform), so a RED payload would reach
// the SFrame decryptor as one undecryptable frame.

const (
	audioPayloadType = 111
	// 1 s of 20 ms Opus frames. A slow viewer loses old audio, never the publisher's time.
	audioQueueLen = 50
	// Packets older than this when they reach the pacer are dropped: the
	// receiver's jitter buffer has played past them.
	audioMaxAge = 200 * time.Millisecond
	// Opus RTP clock: 20 ms.
	audioFrameTicks = 960
	// NACK replay bounds per cached packet: never older than audioMaxAge
	// (the jitter buffer is past it), at most audioReplayMax replays, at
	// least audioReplaySpacing apart (a repeated NACK within one interval is
	// the same loss, not a new one).
	audioReplayMax     = 2
	audioReplaySpacing = 20 * time.Millisecond
	// The audio lane goes ahead of video only up to 1.5x the publisher's
	// measured audio rate (at least audioPriorityFloor before it is
	// measured), and never above half the viewer's wire budget. Audio beyond
	// that waits behind video and ages out, so a flood or a NACK storm can't
	// starve video.
	audioPriorityFloor = 64_000
	// Priority credit holds 100 ms of the priority rate (at least the next
	// packet), and a packet goes first only if it fits in that credit, so a
	// burst can't take video's share either.
	audioPriorityWindow = 100 * time.Millisecond
)

var opusCapability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}

func registerAudio(m *webrtc.MediaEngine) error {
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: opusCapability, PayloadType: audioPayloadType}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}
	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeAudio)
	return nil
}

// roomAudio is the publisher side: counters and the measured audio rate.
type roomAudio struct {
	Packets atomic.Uint64
	bytes   atomic.Uint64
	rate    atomic.Int64 // wire bits/s, smoothed
	rateAt  atomic.Int64
}

// Rate is the smoothed audio wire rate in bits/s (0 without audio).
func (a *roomAudio) Rate() int { return int(a.rate.Load()) }

func (a *roomAudio) count(n int, now time.Time) {
	a.Packets.Add(1)
	a.bytes.Add(uint64(n + wireOverhead))
	last := a.rateAt.Load()
	if last == 0 {
		a.rateAt.Store(now.UnixNano())
		return
	}
	if d := now.Sub(time.Unix(0, last)); d >= 500*time.Millisecond && a.rateAt.CompareAndSwap(last, now.UnixNano()) {
		cur := int64(float64(a.bytes.Swap(0)) * 8 / d.Seconds())
		a.rate.Store((a.rate.Load()*3 + cur*2) / 5)
	}
}

// readAudio forwards the publisher's audio track to every viewer's queue.
func (p *Peer) readAudio(t *webrtc.TrackRemote) {
	r := p.Room
	for {
		pkt, _, err := t.ReadRTP()
		if err != nil {
			return
		}
		now := time.Now()
		r.audio.count(pkt.MarshalSize(), now)
		r.mu.Lock()
		if r.publisher != p {
			r.mu.Unlock()
			return
		}
		src := r.source
		for v := range r.viewers {
			if v.audio != nil {
				v.audio.offer(audioPacket{pkt: pkt, at: now, source: src})
			}
		}
		r.mu.Unlock()
	}
}

type audioPacket struct {
	pkt    *rtp.Packet // shared, read-only: the viewer leg clones before rewriting
	at     time.Time
	source uint64
}

// audioLeg is one viewer's audio: a bounded queue, header continuity and a
// small replay cache.
type audioLeg struct {
	track     *webrtc.TrackLocalStaticRTP
	queue     chan audioPacket
	mu        sync.Mutex // guards cache
	cache     [128]cachedAudio
	Forwarded atomic.Uint64
	Dropped   atomic.Uint64
	Replayed  atomic.Uint64
	Refused   atomic.Uint64 // NACKed replays refused by age, count or spacing
	now       func() time.Time
}

type cachedAudio struct {
	pkt        *rtp.Packet
	sent, last time.Time
	replays    int
}

func newAudioLeg() (*audioLeg, error) {
	t, err := webrtc.NewTrackLocalStaticRTP(opusCapability, "audio", "passstream")
	if err != nil {
		return nil, err
	}
	return &audioLeg{track: t, queue: make(chan audioPacket, audioQueueLen), now: time.Now}, nil
}

// offer never blocks: a full queue loses its oldest packet.
func (l *audioLeg) offer(x audioPacket) {
	for {
		select {
		case l.queue <- x:
			return
		default:
		}
		select {
		case <-l.queue:
			l.Dropped.Add(1)
		default:
		}
	}
}

// run writes queued audio to the viewer track once the viewer is admitted
// and connected (the same media barrier as video).
func (l *audioLeg) run(p *Peer) {
	var seq uint16
	var started bool
	var source uint64
	var tsOffset, lastTS uint32
	for {
		select {
		case <-p.done:
			return
		case x := <-l.queue:
			if !p.admitted.Load() || (p.PC != nil && p.PC.ConnectionState() != webrtc.PeerConnectionStateConnected) || time.Since(x.at) > audioMaxAge {
				l.Dropped.Add(1)
				continue
			}
			pkt := x.pkt.Clone()
			if started && x.source != source {
				tsOffset = lastTS + audioFrameTicks - pkt.Timestamp
			}
			started, source = true, x.source
			pkt.Timestamp += tsOffset
			lastTS = pkt.Timestamp
			seq++
			pkt.SequenceNumber = seq
			// Upstream extension ids belong to its SDP; the viewer's interceptors add TWCC.
			pkt.Extension = false
			pkt.Extensions = nil
			pkt.ExtensionProfile = 0
			l.mu.Lock()
			l.cache[int(seq)%len(l.cache)] = cachedAudio{pkt: pkt.Clone(), sent: l.now()}
			l.mu.Unlock()
			if l.track.WriteRTP(pkt) == nil {
				l.Forwarded.Add(1)
			}
		}
	}
}

// replay re-sends cached audio packets a viewer NACKed, within the replay
// bounds (age, count, spacing; see the constants).
func (l *audioLeg) replay(seqs []uint16) {
	for _, s := range seqs {
		now := l.now()
		l.mu.Lock()
		c := &l.cache[int(s)%len(l.cache)]
		if c.pkt == nil || c.pkt.SequenceNumber != s {
			l.mu.Unlock()
			continue
		}
		if now.Sub(c.sent) > audioMaxAge || c.replays >= audioReplayMax || (c.replays > 0 && now.Sub(c.last) < audioReplaySpacing) {
			l.mu.Unlock()
			l.Refused.Add(1)
			continue
		}
		c.replays++
		c.last = now
		pkt := c.pkt.Clone()
		l.mu.Unlock()
		if l.track.WriteRTP(pkt) == nil {
			l.Replayed.Add(1)
		}
	}
}

// readRTCP serves the viewer's audio NACKs; other audio RTCP is consumed by
// the interceptors (reports, TWCC).
func (l *audioLeg) readRTCP(sender *webrtc.RTPSender) {
	ssrc := uint32(sender.GetParameters().Encodings[0].SSRC)
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		l.handleRTCP(ssrc, pkts)
	}
}

// handleRTCP replays NACKed audio. SRTCP hands a compound packet to every
// SSRC it names, so video NACKs arrive here too: only NACKs for the audio
// SSRC count (video sequence numbers must never replay audio packets).
func (l *audioLeg) handleRTCP(ssrc uint32, pkts []rtcp.Packet) {
	for _, pkt := range pkts {
		if n, ok := pkt.(*rtcp.TransportLayerNack); ok && n.MediaSSRC == ssrc {
			var seqs []uint16
			for _, pair := range n.Nacks {
				seqs = append(seqs, pair.PacketList()...)
			}
			l.replay(seqs)
		}
	}
}

// addAudio gives a viewer PC its audio track.
func (p *Peer) addAudio(pc *webrtc.PeerConnection) error {
	leg, err := newAudioLeg()
	if err != nil {
		return err
	}
	wrapped := &audioTrack{TrackLocalStaticRTP: leg.track, pacer: p.Pacer}
	sender, err := pc.AddTrack(wrapped)
	if err != nil {
		return err
	}
	p.audio = leg
	go leg.readRTCP(sender)
	go leg.run(p)
	return nil
}

// audioTrack tells the viewer's pacer which SSRC carries audio.
type audioTrack struct {
	*webrtc.TrackLocalStaticRTP
	pacer *Pacer
}

func (t *audioTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codec, err := t.TrackLocalStaticRTP.Bind(ctx)
	if err == nil && t.pacer != nil {
		t.pacer.audio.ssrc.Store(uint32(ctx.SSRC()))
	}
	return codec, err
}

// ---------------------------------------------------------------- pacer side

// pacerAudio is the pacer's audio lane: served ahead of the video queue,
// charged to the same budget, bounded by length and age.
type pacerAudio struct {
	ssrc     atomic.Uint32
	measured atomic.Int64 // publisher's audio wire rate, bits/s (SetAudioRate)
	queue    []pacedPacket
	priority wireBudget // credit for going ahead of video
	Sent     atomic.Uint64
	Late     atomic.Uint64
	Spare    atomic.Uint64 // audio sent with priority spent, while no video waited
}

// SetAudioRate tells the pacer the publisher's measured audio rate, which
// bounds the audio lane's priority (see audioPriorityFloor).
func (p *Pacer) SetAudioRate(bps int) { p.audio.measured.Store(int64(bps)) }

// audioPriorityRate is the rate at which audio may go ahead of video.
func (p *Pacer) audioPriorityRate(budget int64) int64 {
	r := max(p.audio.measured.Load()*3/2, audioPriorityFloor)
	return min(r, budget/2)
}

// accrueAudio adds audio priority credit, holding at most burst bytes. Holds p.mu.
func (p *Pacer) accrueAudio(now time.Time, budget int64, next int) {
	a := &p.audio.priority
	rate := p.audioPriorityRate(budget)
	burst := max(float64(rate)*audioPriorityWindow.Seconds()/8, float64(next))
	if a.last.IsZero() {
		a.last = now
		a.credit = burst
		return
	}
	a.credit = min(a.credit+float64(rate)*now.Sub(a.last).Seconds()/8, burst)
	a.last = now
}

const pacerAudioLen = 32

func (a *pacerAudio) is(h *rtp.Header) bool {
	s := a.ssrc.Load()
	return s != 0 && h.SSRC == s
}

// writeAudio queues one audio packet (Pacer.Write for the audio SSRC).
func (p *Pacer) writeAudio(h *rtp.Header, payload []byte, attrs interceptor.Attributes) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.notify()
	select {
	case <-p.done:
		return 0, io.ErrClosedPipe
	default:
	}
	item := pacedPacket{kind: audioKind, h: h.Clone(), p: append([]byte(nil), payload...), a: attrs, at: time.Now(), end: true}
	for _, v := range attrs {
		if r, ok := v.(retained); ok {
			r.Retain()
			item.refs = append(item.refs, r)
		}
	}
	if len(p.audio.queue) >= pacerAudioLen {
		p.dropAudioHead()
	}
	p.audio.queue = append(p.audio.queue, item)
	return h.MarshalSize() + len(payload), nil
}

func (p *Pacer) dropAudioHead() {
	x := p.audio.queue[0]
	p.audio.queue[0] = pacedPacket{}
	p.audio.queue = p.audio.queue[1:]
	for _, r := range x.refs {
		r.Release(false)
	}
	p.audio.Late.Add(1)
}

// audioReady drops stale audio and reports whether a packet waits. Holds p.mu.
func (p *Pacer) audioReady(now time.Time) bool {
	for len(p.audio.queue) > 0 && now.Sub(p.audio.queue[0].at) > audioMaxAge {
		p.dropAudioHead()
	}
	return len(p.audio.queue) > 0
}

// audioFirst reports whether queued audio goes before the video queue now:
// when its head fits in the priority credit, or when no video waits. Holds p.mu.
func (p *Pacer) audioFirst(now time.Time, budget int64) bool {
	if !p.audioReady(now) {
		return false
	}
	next := wireSize(p.audio.queue[0])
	p.accrueAudio(now, budget, next)
	return p.audio.priority.credit >= float64(next) || len(p.queue) == 0
}

// popAudio takes the head and its writer. Holds p.mu.
func (p *Pacer) popAudio() (pacedPacket, interceptor.RTPWriter) {
	x := p.audio.queue[0]
	p.audio.queue[0] = pacedPacket{}
	p.audio.queue = p.audio.queue[1:]
	return x, p.writers[x.h.SSRC]
}

// sendAudio writes one audio packet without the pacer lock and returns its wire size.
func (p *Pacer) sendAudio(w interceptor.RTPWriter, x pacedPacket) int {
	n := wireSize(x)
	var err error
	if w != nil {
		p.stampDeparture(&x, n)
		_, err = w.Write(&x.h, x.p, x.a)
		if err == nil {
			p.wireBytes.Add(uint64(n))
			p.audioWireBytes.Add(uint64(n))
			p.audio.Sent.Add(1)
		}
	}
	for _, r := range x.refs {
		r.Release(w != nil && err == nil)
	}
	return n
}

func (p *Pacer) clearAudio() {
	for len(p.audio.queue) > 0 {
		p.dropAudioHead()
	}
}
