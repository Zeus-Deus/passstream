package sfu

import (
	"time"

	"github.com/pion/rtcp"
)

const sourceSendTimeURI = "http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time"

// Production's 4Mbit/s cap triples briefly after a key. Leave 25% for the
// independent rendition and libwebrtc's allocation margin; a 4Mbit/s REMB
// ceiling otherwise silently defeats that repayment policy on a clean uplink.
const publisherWireCeiling = 15_000_000

type sourceDelaySample struct{ at, offset time.Duration }

// Receiver-side control keeps repaired random loss out of the publisher's raw
// TWCC loss controller. Absolute send time measures wire departure, so local
// keyframe serialization cannot masquerade as a network queue.
type publisherController struct {
	rate                                        int
	start, clockAt, lastAST, nextProbe, lastCut time.Time
	raw                                         uint32
	ticks                                       int64
	clock                                       bool
	offset                                      time.Duration
	samples                                     [256]sourceDelaySample
	head, count                                 int
	wire                                        deliveryWindow
	received                                    int64
	limited                                     bool
	growthAt, candidateAt                       time.Time
	candidate                                   time.Duration
}

func sourceClock(ticks int64) time.Duration {
	return time.Duration(ticks/(1<<18))*time.Second + time.Duration(ticks%(1<<18))*time.Second/(1<<18)
}

func (c *publisherController) queue(now time.Time) time.Duration {
	minimum := c.offset
	for i := 0; i < c.count; i++ {
		s := c.samples[(c.head-1-i+len(c.samples))%len(c.samples)]
		if now.Sub(c.clockAt)-s.at <= 20*time.Second {
			minimum = min(minimum, s.offset)
		}
	}
	return max(0, c.offset-minimum)
}

func (c *publisherController) observe(now time.Time, extension []byte, bytes int, fresh bool) {
	if c.rate == 0 {
		c.rate = 4_000_000
	}
	if c.start.IsZero() {
		c.start = now
	}
	if rate := c.wire.observe(now.Sub(c.start), int64(bytes)); rate > 0 {
		c.received = rate
	}
	// Cached retransmissions and reordered older packets may retain old send
	// timestamps. Count their wire bytes, but do not let them prove queue growth.
	if !fresh || len(extension) != 3 {
		return
	}
	raw := uint32(extension[0])<<16 | uint32(extension[1])<<8 | uint32(extension[2])
	if !c.clock || now.Sub(c.lastAST) > 20*time.Second {
		c.clock = true
		c.raw = raw
		c.clockAt = now
		c.ticks = 0
		c.head = 0
		c.count = 0
	}
	delta := int32(raw-c.raw) << 8 >> 8
	ticks := c.ticks + int64(delta)
	if delta >= 0 {
		c.raw = raw
		c.ticks = ticks
	}
	c.lastAST = now
	c.offset = now.Sub(c.clockAt) - sourceClock(ticks)
	if c.count == 0 || now.Sub(c.clockAt)-c.samples[(c.head-1+len(c.samples))%len(c.samples)].at >= 100*time.Millisecond {
		if c.count > 0 {
			previous := c.samples[(c.head-1+len(c.samples))%len(c.samples)]
			c.closedDelay(now, previous.offset)
		}
		c.samples[c.head] = sourceDelaySample{now.Sub(c.clockAt), c.offset}
		c.head = (c.head + 1) % len(c.samples)
		c.count = min(c.count+1, len(c.samples))
	} else {
		i := (c.head - 1 + len(c.samples)) % len(c.samples)
		c.samples[i].offset = min(c.samples[i].offset, c.offset)
	}
}

// Compare completed bucket minima, rather than individual arrivals. A fixed
// propagation-delay step must not repeatedly consume the source's capacity.
func (c *publisherController) closedDelay(now time.Time, offset time.Duration) {
	excess := max(0, c.queue(now)-(c.offset-offset))
	if excess < 120*time.Millisecond {
		c.candidateAt = time.Time{}
		return
	}
	if c.candidateAt.IsZero() || now.Sub(c.candidateAt) > 500*time.Millisecond || offset < c.candidate {
		c.candidateAt, c.candidate = now, offset
		return
	}
	if offset-c.candidate < 20*time.Millisecond {
		return
	}
	c.growthAt = now
	c.candidateAt, c.candidate = now, offset
	if c.lastCut.IsZero() || now.Sub(c.lastCut) >= 500*time.Millisecond {
		ceiling := min(c.rate*85/100, int(int64(c.rate)*int64(100*time.Millisecond)/int64(excess+100*time.Millisecond)))
		if c.received > 0 {
			ceiling = min(ceiling, int(c.received)*85/100)
		}
		c.rate = max(128_000, ceiling)
		c.lastCut = now
		c.limited = true
		c.nextProbe = now.Add(2 * time.Second)
	}
}

func (c *publisherController) budget(now time.Time) int {
	if c.rate == 0 {
		c.rate = 4_000_000
	}
	if !c.clock {
		c.rate = 128_000
		return c.rate
	}
	if c.clock && now.Sub(c.lastAST) < 500*time.Millisecond && (c.queue(now) < 60*time.Millisecond || !c.growthAt.IsZero() && now.Sub(c.growthAt) >= 2*time.Second) && !now.Before(c.nextProbe) {
		growth, interval := 25, 100*time.Millisecond
		if c.limited {
			growth, interval = 8, 400*time.Millisecond
		}
		c.rate = min(publisherWireCeiling, c.rate+c.rate*growth/100)
		c.nextProbe = now.Add(interval)
	}
	return c.rate
}

func (p *Peer) publisherFeedback() {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var lastICE time.Time
	for {
		select {
		case <-p.done:
			return
		case now := <-tick.C:
			p.Room.mu.Lock()
			active := p.Room.publisher == p
			ssrcs := make([]uint32, 0, 2)
			if p.Room.ssrc != 0 {
				ssrcs = append(ssrcs, p.Room.ssrc)
			}
			if p.Room.lowSSRC != 0 {
				ssrcs = append(ssrcs, p.Room.lowSSRC)
			}
			p.Room.mu.Unlock()
			if !active {
				return
			}
			p.sourceMu.Lock()
			rate := p.sourceControl.budget(now)
			clock := p.sourceControl.clock
			p.sourceMu.Unlock()
			if clock && now.Sub(lastICE) >= 500*time.Millisecond {
				lastICE = now
				if sctp := p.PC.SCTP(); sctp != nil && sctp.Transport() != nil {
					if stats, ok := sctp.Transport().ICETransport().GetSelectedCandidatePairStats(); ok && stats.CurrentRoundTripTime > 0 {
						rtt := int64(stats.CurrentRoundTripTime * float64(time.Second))
						for {
							old := p.initialRTT.Load()
							if old > 0 && old <= rtt || p.initialRTT.CompareAndSwap(old, rtt) {
								break
							}
						}
					}
				}
			}
			if len(ssrcs) > 0 {
				_ = p.PC.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverEstimatedMaximumBitrate{Bitrate: float32(rate), SSRCs: ssrcs}})
			}
		}
	}
}

// A cheap refresh on an observed fast publisher path costs less than waiting
// a full second for a possibly lost PLI. Unknown and expensive encodings keep the
// conservative unanswered guard; joins still use their 500ms coalescing.
func (r *Room) unansweredSpacing() time.Duration {
	if r.publisher == nil {
		return time.Second
	}
	return r.publisher.refreshSpacing(time.Now(), r.keyBytes.Load())
}

func (p *Peer) refreshSpacing(now time.Time, bytes int64) time.Duration {
	if bytes <= 0 {
		return time.Second
	}
	p.sourceMu.Lock()
	rate := 0
	if p.sourceControl != nil && p.sourceControl.clock && now.Sub(p.sourceControl.lastAST) < 500*time.Millisecond && p.sourceControl.queue(now) < 60*time.Millisecond {
		rate = p.sourceControl.rate
	}
	p.sourceMu.Unlock()
	rtt := time.Duration(p.initialRTT.Load())
	// A key costing less than one 15fps capture interval can repay a lost
	// request promptly. Its encoded size alone says nothing about that cost.
	const cheapRefresh = time.Second / 16
	serialization := keySerialization(bytes, int64(rate))
	if rtt <= 0 || serialization > cheapRefresh {
		return time.Second
	}
	return min(time.Second, max(300*time.Millisecond, rtt+100*time.Millisecond+serialization))
}
