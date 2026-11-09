package ctrl

import (
	"math"
	"time"
)

// Ingress meters publisher ingress for Costs (docs/CONTROLLER.md "Video
// need"). internal/sfu and internal/sfusim both feed it every received
// packet.
//
// Video rates are measured over complete temporal cycles: a cycle opens at
// the first packet of a base picture (TL0 or key) and closes when the next
// base picture begins, so every cycle holds one TL0 and its TL1/TL2
// pictures. A rendition's rate is the bytes of its newest CostCycles complete
// cycles over their span. Fixed time buckets sampled the 3.75 base
// pictures/s of an L1T3 stream at an arbitrary phase, so T0's cost swung by
// ±20% from one 100 ms step to the next; and a 1 s estimate rests on ≈3.75
// base pictures, whose sizes vary by ≈25% each, so it jittered around the
// hold and admission margins, revoking held options and moving probe
// targets (C2 §5 A12/A13 meter; A1 good_join seed 49: four probes chased
// one rung; A18 native_1500k).
//
// A cycle that contains a key is not steady demand: its span includes the
// encoder's post-key pause (up to ≈0.8 s without the sender's key boost),
// and treating that as a lower rate admits options the stream cannot hold
// once the encoder resumes (A18 native_1500k). Such a cycle is recorded only
// while no steady one is, and nothing decays while the source is silent. Key
// bytes are excluded from the TID rates; their size feeds Keys.Size.
//
// Audio is a continuous-time EWMA over TrendWindow of every packet's bytes,
// with bias correction: a steady 50 packets/s needs no buckets, and Opus
// DTX silence is real lower demand.
type Ingress struct {
	video     [2]cycleMeter
	audioAt   time.Time
	audioRate float64 // bps
	audioFrom time.Time
}

// meterCycle is one complete temporal cycle of a rendition.
type meterCycle struct {
	span     time.Duration
	bytes    [3]int // delta bytes per TID
	prot     int    // TL0 delta bytes (FEC-protected)
	packets  int    // TL0 delta packets
	pictures int    // TL0 delta pictures
	key      bool   // contains a key: its span includes the post-key pause
}

// cycleMeter is one rendition's open cycle and a ring of its newest
// complete cycles.
type cycleMeter struct {
	open    bool
	start   time.Time
	picture uint64 // picture of the newest packet
	cur     meterCycle
	ring    [CostCycles]meterCycle
	n       int  // complete cycles recorded
	steady  bool // the ring holds a cycle without a key
}

func (c *cycleMeter) video(now time.Time, tid uint8, key bool, picture uint64, bytes int) {
	if base := key || tid == 0; base && (!c.open || picture != c.picture) {
		if c.open && now.After(c.start) && !(c.cur.key && c.steady) {
			c.cur.span = now.Sub(c.start)
			if !c.cur.key && !c.steady {
				c.n = 0 // the first steady cycle replaces key cycles
			}
			c.steady = c.steady || !c.cur.key
			c.ring[c.n%len(c.ring)] = c.cur
			c.n++
		}
		c.open, c.start, c.cur = true, now, meterCycle{}
	}
	c.picture = picture
	if !c.open {
		return // the first base picture opens the first cycle
	}
	if key {
		c.cur.key = true
		return
	}
	c.cur.bytes[tid] += bytes
	if tid == 0 {
		if c.cur.packets == 0 {
			c.cur.pictures++ // a cycle holds one TL0 picture
		}
		c.cur.prot += bytes
		c.cur.packets++
	}
}

// rates returns the cumulative per-TID, protected and parity-flush wire
// rates over the recorded cycles; zero before the first complete cycle.
// The flush rate is one parity packet per TL0 picture: the protected
// packets' mean wire size plus the ULPFEC and RED headers (15 bytes), the
// size internal/sfu's parity takes for packets of that size.
func (c *cycleMeter) rates() (rate, prot, flush [3]int) {
	var bytes [3]int
	pbytes, packets, pictures := 0, 0, 0
	span := time.Duration(0)
	for i := range min(c.n, len(c.ring)) {
		x := c.ring[i]
		span += x.span
		for t := range 3 {
			bytes[t] += x.bytes[t]
		}
		pbytes += x.prot
		packets += x.packets
		pictures += x.pictures
	}
	if span <= 0 {
		return
	}
	f := 0.0
	if packets > 0 {
		f = float64(pictures) * (float64(pbytes)/float64(packets) + 15) * 8 / span.Seconds()
	}
	sum := 0
	for t := range 3 {
		sum += bytes[t]
		rate[t] = int(float64(sum*8) / span.Seconds())
		prot[t] = int(float64(pbytes*8) / span.Seconds())
		flush[t] = int(f)
	}
	return
}

// Video records one video packet's wire bytes. picture identifies its
// picture within the rendition (internal/sfu passes the RTP timestamp).
func (m *Ingress) Video(now time.Time, r Rendition, tid uint8, key bool, picture uint64, bytes int) {
	if !key && tid > 2 {
		return
	}
	m.video[r].video(now, tid, key, picture, bytes)
}

// Audio records one audio packet's wire bytes, including RED.
func (m *Ingress) Audio(now time.Time, bytes int) {
	m.decay(now)
	if m.audioFrom.IsZero() {
		m.audioFrom = now
	}
	m.audioRate += float64(bytes*8) / TrendWindow.Seconds()
}

// decay ages the audio EWMA to now.
func (m *Ingress) decay(now time.Time) {
	if !m.audioAt.IsZero() && now.After(m.audioAt) {
		m.audioRate *= math.Exp(-now.Sub(m.audioAt).Seconds() / TrendWindow.Seconds())
	}
	if now.After(m.audioAt) {
		m.audioAt = now
	}
}

// Costs returns cumulative per-option rates, the audio wire rate and the
// room's key-size estimates.
func (m *Ingress) Costs(now time.Time, keys Keys) Costs {
	m.decay(now)
	var c Costs
	for r := range 2 {
		c.Rate[r], c.Protected[r], c.Flush[r] = m.video[r].rates()
		c.KeyBytes[r], c.KeySamples[r] = keys.Size(now, Rendition(r))
	}
	// Bias correction: the EWMA started from zero at the first packet.
	if !m.audioFrom.IsZero() && now.After(m.audioFrom) {
		c.AudioWire = int(m.audioRate / (1 - math.Exp(-now.Sub(m.audioFrom).Seconds()/TrendWindow.Seconds())))
	}
	return c
}
