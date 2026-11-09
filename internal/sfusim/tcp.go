package sfusim

import (
	"math"
	"time"
)

// TCP is one bulk Cubic flow (NewReno loss recovery, delayed ACKs, RTO) with
// a receive-window cap. The cap is calibrated against the browser
// fifo_tcp_only_512kbit queue probe (docs/REPORT.md, simulator calibration).
type TCP struct {
	sim      *Sim
	data     *Link // sender → receiver
	ack      *Link // receiver → sender
	flow     int
	rwnd     float64 // segments
	cwnd     float64
	ssthresh float64

	una, nxt   int64
	dupacks    int
	recovery   bool
	recover    int64
	retrans    map[int64]bool
	sentAt     map[int64]time.Duration
	srtt, var_ time.Duration
	rto        time.Duration
	rtoGen     int
	wmax       float64
	epoch      time.Duration
	minRTT     time.Duration

	rcvNxt   int64
	ooo      map[int64]bool
	unacked  int
	delayGen int

	// Delivered counts in-order payload bytes at the receiver.
	Delivered         int64
	Timeouts, FastRtx int
}

const (
	tcpMSS      = 1448
	tcpWire     = 1500
	tcpAckWire  = 52
	cubicC      = 0.4
	cubicBeta   = 0.7
	tcpAckEvery = 2 // Linux delayed ACK for bulk transfer
)

type tcpSeg struct{ n int64 }
type tcpAck struct{ ack int64 }

func newTCP(s *Sim, data, ack *Link, flow int, rwndBytes int) *TCP {
	return &TCP{sim: s, data: data, ack: ack, flow: flow, rwnd: float64(rwndBytes) / tcpMSS,
		cwnd: 10, ssthresh: math.Inf(1), retrans: map[int64]bool{}, sentAt: map[int64]time.Duration{},
		rto: time.Second, ooo: map[int64]bool{}}
}

// Start begins the bulk transfer at t.
func (c *TCP) Start(t time.Duration) { c.sim.At(t, c.pump) }

func (c *TCP) pump() {
	for float64(c.nxt-c.una) < min(c.cwnd, c.rwnd) {
		c.send(c.nxt)
		c.nxt++
	}
}

func (c *TCP) send(n int64) {
	if _, again := c.sentAt[n]; again {
		c.retrans[n] = true
	}
	c.sentAt[n] = c.sim.Now()
	c.data.Send(&Packet{Size: tcpWire, Flow: c.flow, payload: tcpSeg{n}, deliver: c.receive})
	c.arm()
}

func (c *TCP) arm() {
	c.rtoGen++
	gen := c.rtoGen
	c.sim.After(c.rto, func() {
		if gen == c.rtoGen && c.nxt > c.una {
			c.timeout()
		}
	})
}

func (c *TCP) timeout() {
	c.Timeouts++
	c.ssthresh = max(float64(c.nxt-c.una)/2, 2)
	c.wmax = c.cwnd
	c.cwnd = 1
	c.epoch = 0
	c.recovery = false
	c.dupacks = 0
	c.nxt = c.una
	c.rto = min(2*c.rto, 60*time.Second)
	c.pump()
}

// receive runs at the receiver.
func (c *TCP) receive(p *Packet) {
	n := p.payload.(tcpSeg).n
	switch {
	case n == c.rcvNxt:
		c.rcvNxt++
		c.Delivered += tcpMSS
		for c.ooo[c.rcvNxt] {
			delete(c.ooo, c.rcvNxt)
			c.rcvNxt++
			c.Delivered += tcpMSS
		}
		c.unacked++
		if c.unacked >= tcpAckEvery || len(c.ooo) > 0 {
			c.sendAck()
			return
		}
		c.delayGen++
		gen := c.delayGen
		c.sim.After(40*time.Millisecond, func() {
			if gen == c.delayGen && c.unacked > 0 {
				c.sendAck()
			}
		})
	case n > c.rcvNxt:
		c.ooo[n] = true
		c.sendAck()
	default:
		c.sendAck()
	}
}

func (c *TCP) sendAck() {
	c.unacked = 0
	c.delayGen++
	c.ack.Send(&Packet{Size: tcpAckWire, Flow: c.flow, payload: tcpAck{c.rcvNxt}, deliver: c.onAck})
}

// onAck runs at the sender.
func (c *TCP) onAck(p *Packet) {
	a := p.payload.(tcpAck).ack
	now := c.sim.Now()
	if a > c.una {
		if at, ok := c.sentAt[a-1]; ok && !c.retrans[a-1] {
			c.sample(now - at)
		}
		for n := c.una; n < a; n++ {
			delete(c.sentAt, n)
			delete(c.retrans, n)
		}
		acked := float64(a - c.una)
		c.una = a
		if c.nxt < c.una {
			c.nxt = c.una
		}
		c.dupacks = 0
		if c.recovery {
			if a > c.recover {
				c.recovery = false
				c.cwnd = c.ssthresh
			} else {
				c.send(c.una) // NewReno partial ACK
			}
		} else if c.cwnd >= c.rwnd {
			// Not cwnd-limited: Linux does not grow the window (RFC 7661).
		} else if c.cwnd < c.ssthresh {
			c.cwnd += acked
		} else {
			c.cubic(now, acked)
		}
		if c.nxt > c.una {
			c.arm()
		} else {
			c.rtoGen++
		}
	} else if a == c.una && c.nxt > c.una {
		c.dupacks++
		if c.dupacks == 3 && !c.recovery {
			c.wmax = c.cwnd
			c.ssthresh = max(c.cwnd*cubicBeta, 2)
			c.cwnd = c.ssthresh
			c.epoch = 0
			c.recovery = true
			c.recover = c.nxt - 1
			c.FastRtx++
			c.send(c.una)
		}
	}
	c.pump()
}

func (c *TCP) sample(rtt time.Duration) {
	if c.minRTT == 0 || rtt < c.minRTT {
		c.minRTT = rtt
	}
	if c.srtt == 0 {
		c.srtt, c.var_ = rtt, rtt/2
	} else {
		d := c.srtt - rtt
		if d < 0 {
			d = -d
		}
		c.var_ = (3*c.var_ + d) / 4
		c.srtt = (7*c.srtt + rtt) / 8
	}
	c.rto = max(200*time.Millisecond, c.srtt+4*c.var_)
}

func (c *TCP) cubic(now time.Duration, acked float64) {
	if c.epoch == 0 {
		c.epoch = now
		if c.wmax < c.cwnd {
			c.wmax = c.cwnd
		}
	}
	rtt := c.srtt
	if rtt == 0 {
		rtt = 100 * time.Millisecond
	}
	k := math.Cbrt(c.wmax * (1 - cubicBeta) / cubicC)
	t := (now - c.epoch + rtt).Seconds()
	target := cubicC*math.Pow(t-k, 3) + c.wmax
	reno := c.wmax*cubicBeta + 3*(1-cubicBeta)/(1+cubicBeta)*(now-c.epoch).Seconds()/rtt.Seconds()
	target = max(target, reno)
	if target > c.cwnd {
		c.cwnd += acked * (target - c.cwnd) / c.cwnd
	} else {
		c.cwnd += acked * 0.01 / c.cwnd
	}
}
