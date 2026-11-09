package sfu

import (
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"sync"
	"time"
)

type cachedPacket struct {
	p        *rtp.Packet
	at, last time.Time
	tries    int
	lifetime time.Duration
}

// packetCache serves downstream NACKs. Sparse screen traffic delays loss
// detection (the next packet after a burst may come hundreds of ms later), so
// packets are retained for replayAge and replayTries attempts. Packets sent
// before the latest keyframe are never replayed: the decoder no longer needs
// them, which also bounds stale replay after an outage.
type packetCache struct {
	mu      sync.Mutex
	entries [1024]cachedPacket
	keySeq  uint16
	hasKey  bool
	// Cache lookup outcomes: hit, missing, aged, exhausted, spaced, superseded.
	// A hit may still be suppressed by the pacer deadline; wire counters count sends.
	Outcomes [6]uint64
}

const (
	replayAge   = time.Second
	replayTries = 6
)

// keyframe records the wire sequence number at which a keyframe starts.
func (c *packetCache) keyframe(seq uint16) {
	c.mu.Lock()
	c.keySeq, c.hasKey = seq, true
	c.mu.Unlock()
}

func (c *packetCache) put(p *rtp.Packet, now time.Time) { c.putFor(p, now, replayAge) }
func (c *packetCache) putFor(p *rtp.Packet, now time.Time, lifetime time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[int(p.SequenceNumber)%len(c.entries)] = cachedPacket{p: p.Clone(), at: now, lifetime: lifetime}
}

// The pacer owns this immutable payload until all cache/replay references die.
// Copy only the header: later TWCC stamping mutates extensions, not media.
func (c *packetCache) putOwnedFor(p *rtp.Packet, now time.Time, lifetime time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	packet := &rtp.Packet{Header: p.Header.Clone(), Payload: p.Payload}
	c.entries[int(p.SequenceNumber)%len(c.entries)] = cachedPacket{p: packet, at: now, lifetime: lifetime}
}

func (c *packetCache) outcomes() [6]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Outcomes
}
func (c *packetCache) get(seq uint16, now time.Time) *rtp.Packet {
	p, _ := c.replay(seq, now)
	return p
}
func (c *packetCache) replay(seq uint16, now time.Time) (*rtp.Packet, time.Time) {
	return c.replaySpaced(seq, now, 30*time.Millisecond)
}

func (c *packetCache) replaySpaced(seq uint16, now time.Time, spacing time.Duration) (*rtp.Packet, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := &c.entries[int(seq)%len(c.entries)]
	lifetime := q.lifetime
	if lifetime == 0 {
		lifetime = replayAge
	}
	outcome := 0
	switch {
	case q.p == nil || q.p.SequenceNumber != seq:
		outcome = 1
	case now.Sub(q.at) > lifetime:
		outcome = 2
	case q.tries >= replayTries:
		outcome = 3
	case now.Sub(q.last) < max(30*time.Millisecond, spacing):
		outcome = 4
	case c.hasKey && int16(seq-c.keySeq) < 0:
		outcome = 5
	}
	c.Outcomes[outcome]++
	if outcome != 0 {
		return nil, time.Time{}
	}
	q.last = now
	q.tries++
	return q.p.Clone(), q.at.Add(lifetime)
}

type viewerLiveness struct{ unacked time.Time }

func (v *viewerLiveness) sent(now time.Time) {
	if v.unacked.IsZero() {
		v.unacked = now
	}
}

// dead reports that nothing came back for max(1 s, 4×RTT) after a send. Until
// an RTT is measured the bound is 2 s: at 800 ms RTT the first feedback can
// take about a second, and declaring the viewer dead then drops the deltas
// after its first keyframe.
func (v *viewerLiveness) dead(now time.Time, rtcpAt int64, rtt time.Duration) bool {
	if time.Unix(0, rtcpAt).After(v.unacked) {
		v.unacked = time.Time{}
	}
	limit := 2 * time.Second
	if rtt > 0 {
		limit = max(time.Second, 4*rtt)
	}
	return !v.unacked.IsZero() && now.Sub(v.unacked) > limit
}

// Offsets change only at publisher boundaries; reference deltas within an epoch survive.
type continuity struct {
	source     uint32
	generation uint64
	started    bool
	tsOffset   uint32
	pidOffset  uint16
	tlOffset   uint8
	lastTS     uint32
	lastPID    uint16
	lastTL     uint8
	lastAt     time.Time
}

func (c *continuity) start(f frame) {
	if len(f.packets) == 0 {
		return
	}
	q := f.packets[0]
	var vp codecs.VP9Packet
	if _, err := vp.Unmarshal(q.Payload); err != nil {
		return
	}
	if c.started && (c.source != q.SSRC || c.generation != f.source) {
		step := uint32(6000)
		// Separate encoders have independent RTP origins. Preserve elapsed media
		// time across their switch; compressing a repair gap to one frame makes a
		// fresh key look late on the viewer's existing RTP clock.
		if !c.lastAt.IsZero() && !f.at.IsZero() && f.at.After(c.lastAt) {
			step = uint32(max(int64(step), int64(f.at.Sub(c.lastAt))*90_000/int64(time.Second)))
		}
		c.tsOffset = c.lastTS + step - q.Timestamp
		c.pidOffset = (c.lastPID + 1 - vp.PictureID) & 0x7fff
		c.tlOffset = c.lastTL + 1 - vp.TL0PICIDX
	}
	c.lastAt = f.at
	c.source = q.SSRC
	c.generation = f.source
	c.started = true
}
func (c *continuity) packet(p *rtp.Packet) {
	p.Timestamp += c.tsOffset
	c.lastTS = p.Timestamp
	b := p.Payload
	if len(b) < 2 {
		return
	}
	var references [3]uint8
	vp := codecs.VP9Packet{PDiff: references[:0]}
	if _, err := vp.Unmarshal(b); err != nil {
		return
	}
	i := 1
	if vp.I {
		pid := (vp.PictureID + c.pidOffset) & 0x7fff
		if b[i]&0x80 != 0 {
			if len(b) < 3 {
				return
			}
			b[i] = 0x80 | byte(pid>>8)
			b[i+1] = byte(pid)
			i += 2
		} else {
			b[i] = byte(pid & 0x7f)
			i++
		}
		c.lastPID = pid
	}
	if vp.L && !vp.F && len(b) > i+1 {
		b[i+1] += c.tlOffset
		c.lastTL = b[i+1]
	}
}

// A missing cache entry can be padding, FEC or an old superseded picture. Only
// a known video reference whose repair deadline/attempts ended needs recovery.
func (c *packetCache) unrepairableReference(seq uint16, now time.Time, mediaPT, redPT int32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := &c.entries[int(seq)%len(c.entries)]
	if q.p == nil || q.p.SequenceNumber != seq || c.hasKey && int16(seq-c.keySeq) < 0 {
		return false
	}
	lifetime := q.lifetime
	if lifetime == 0 {
		lifetime = replayAge
	}
	return (now.Sub(q.at) > lifetime || q.tries >= replayTries) && replayReference(q.p, mediaPT, redPT)
}
func replayReference(p *rtp.Packet, mediaPT, redPT int32) bool {
	payload, pt := p.Payload, int32(p.PayloadType)
	if pt == redPT && len(payload) > 0 {
		pt, payload = int32(payload[0]&0x7f), payload[1:]
	}
	if pt != mediaPT {
		return false
	}
	var v codecs.VP9Packet
	if _, err := v.Unmarshal(payload); err != nil {
		return false
	}
	return !(v.L && !v.F && v.SID == 0 && v.TID == 2)
}
