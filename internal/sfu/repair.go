package sfu

import (
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// cachedPacket is one packet a viewer was sent, as the repair policy needs
// it: the packet before RED encapsulation and its frame's deadline, rendition
// and generation. Whether a NACK is answered is ctrl.Repair's decision.
type cachedPacket struct {
	pkt        *rtp.Packet
	at         time.Time
	last       time.Time // latest copy sent (original or retransmission)
	frame      *outFrame // nil for audio
	deadline   time.Time
	sends      int
	generation uint64
}

// packetCache is indexed by the viewer-side sequence number. The viewer's
// lock guards it; the cached packet is immutable once sent.
type packetCache struct{ entries []cachedPacket }

func newPacketCache(n int) packetCache { return packetCache{entries: make([]cachedPacket, n)} }

func (c *packetCache) put(e cachedPacket) {
	c.entries[int(e.pkt.SequenceNumber)%len(c.entries)] = e
}

func (c *packetCache) get(seq uint16) *cachedPacket {
	e := &c.entries[int(seq)%len(c.entries)]
	if e.pkt == nil || e.pkt.SequenceNumber != seq {
		return nil
	}
	return e
}

// continuity rewrites RTP timestamps, picture IDs and TL0PICIDX so the
// viewer's single video receiver sees one stream across rendition switches
// and publisher changes. Offsets change only at those boundaries; reference
// deltas within an encoding survive.
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

func (c *continuity) start(packets []*rtp.Packet, at time.Time, source uint64) {
	if len(packets) == 0 {
		return
	}
	q := packets[0]
	var vp codecs.VP9Packet
	if _, err := vp.Unmarshal(q.Payload); err != nil {
		return
	}
	if c.started && (c.source != q.SSRC || c.generation != source) {
		step := uint32(6000)
		// Separate encoders have independent RTP origins. Preserve elapsed media
		// time across their switch; compressing a repair gap to one frame makes a
		// fresh key look late on the viewer's existing RTP clock.
		if !c.lastAt.IsZero() && !at.IsZero() && at.After(c.lastAt) {
			step = uint32(max(int64(step), int64(at.Sub(c.lastAt))*90_000/int64(time.Second)))
		}
		c.tsOffset = c.lastTS + step - q.Timestamp
		c.pidOffset = (c.lastPID + 1 - vp.PictureID) & 0x7fff
		c.tlOffset = c.lastTL + 1 - vp.TL0PICIDX
	}
	c.lastAt = at
	c.source = q.SSRC
	c.generation = source
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
