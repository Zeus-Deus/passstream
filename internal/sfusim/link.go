package sfusim

import (
	"math"
	"math/rand/v2"
	"time"
)

// Packet is anything that crosses a modelled link.
type Packet struct {
	Size    int // wire bytes
	Flow    int // flow id for FQ-CoDel hashing
	deliver func(*Packet)
	payload any
	enq     time.Duration // queue entry time (CoDel sojourn)
}

// RatePoint sets the link rate from At onwards. Bps 0 means unlimited.
type RatePoint struct {
	At  time.Duration
	Bps int
}

// DelayPoint sets the one-way propagation delay from At onwards.
type DelayPoint struct {
	At    time.Duration
	Delay time.Duration
}

// Interval is a half-open time range.
type Interval struct{ From, To time.Duration }

// netemExit scales the bad-state exit probability to netem's measured
// behaviour. test/torture/results/netem-gemodel.json: "loss gemodel 1% 19%
// 100% 0%" lost 5.48% of 60,000 packets in 571 runs of mean 5.76, where an
// exact Gilbert-Elliott chain gives 5.00% in runs of 1/r = 5.26. An exit
// probability of 1/5.76 = 0.914 r reproduces both moments (5.45%, 5.76).
// Measured at this one parameter point, the only GE profile GATES uses.
const netemExit = 1 / 5.76 / 0.19

// Loss models netem loss. GE follows netem's "loss gemodel p r 1-h 1-k":
// each packet first moves state (good→bad with P, bad→good with R), then is
// lost with probability BadLoss in the bad state or GoodLoss in the good one.
type Loss struct {
	Bernoulli         float64
	P, R              float64
	BadLoss, GoodLoss float64
}

// Queue disciplines.
const (
	DropTail = iota
	FQCoDel
)

// LinkConfig describes one impaired qdisc, as in web/torture.mjs.
type LinkConfig struct {
	Rate    []RatePoint  // nil: unlimited, no queue
	Delay   []DelayPoint // propagation; first entry applies from 0
	Jitter  time.Duration
	Reorder bool // netem-style independent jitter (reorders); default in-order
	Queue   int  // DropTail or FQCoDel
	Limit   int  // packets; default 1000
	Loss    Loss
	Outages []Interval    // 100% loss
	Start   time.Duration // impairment applies from Start; before it the link is ideal
}

// Link is one qdisc: loss, then a rate-limited queue, then delay and jitter.
// Several directions may share one Link, as the torture FIFO rows do.
type Link struct {
	sim  *Sim
	cfg  LinkConfig
	rng  *rand.Rand
	bad  bool
	busy bool
	fifo []*Packet
	fq   *fqCodel
	last time.Duration // in-order delivery floor

	Sent, Dropped, QueueDrops int
	runStart                  time.Duration
	run                       int
	Runs                      [][2]time.Duration // diagnostic: loss runs (start, duration) ≥ 300 ms
}

func newLink(s *Sim, stream uint64, c LinkConfig) *Link {
	if c.Limit == 0 {
		c.Limit = 1000
	}
	l := &Link{sim: s, cfg: c, rng: s.Rand(stream)}
	if c.Queue == FQCoDel {
		l.fq = &fqCodel{flows: map[int]*fqFlow{}}
	}
	return l
}

func (l *Link) rate(t time.Duration) int {
	r := 0
	for _, p := range l.cfg.Rate {
		if p.At <= t {
			r = p.Bps
		}
	}
	return r
}

func (l *Link) delay(t time.Duration) time.Duration {
	d := time.Duration(0)
	for _, p := range l.cfg.Delay {
		if p.At <= t {
			d = p.Delay
		}
	}
	return d
}

// inOutage reports whether the link is in one of its 100% loss intervals.
func (l *Link) inOutage() bool {
	now := l.sim.Now()
	for _, o := range l.cfg.Outages {
		if now >= o.From && now < o.To {
			return true
		}
	}
	return false
}

func (l *Link) lost() bool {
	now := l.sim.Now()
	for _, o := range l.cfg.Outages {
		if now >= o.From && now < o.To {
			return true
		}
	}
	c := l.cfg.Loss
	if c.P > 0 || c.R > 0 {
		if l.bad {
			if l.rng.Float64() < c.R*netemExit {
				l.bad = false
			}
		} else if l.rng.Float64() < c.P {
			l.bad = true
		}
		p := c.GoodLoss
		if l.bad {
			p = c.BadLoss
		}
		if p > 0 && l.rng.Float64() < p {
			return true
		}
	}
	return c.Bernoulli > 0 && l.rng.Float64() < c.Bernoulli
}

// Send offers a packet to the link.
func (l *Link) Send(p *Packet) {
	l.Sent++
	if l.sim.Now() < l.cfg.Start {
		l.sim.At(max(l.sim.Now(), l.last), func() { p.deliver(p) })
		return
	}
	if l.lost() {
		l.Dropped++
		if l.run == 0 {
			l.runStart = l.sim.Now()
		}
		l.run++
		return
	}
	if l.run > 0 {
		if d := l.sim.Now() - l.runStart; d >= 300*time.Millisecond {
			l.Runs = append(l.Runs, [2]time.Duration{l.runStart, d})
		}
		l.run = 0
	}
	if l.rate(l.sim.Now()) == 0 && len(l.fifo) == 0 && (l.fq == nil || l.fq.n == 0) && !l.busy {
		l.propagate(p)
		return
	}
	p.enq = l.sim.Now()
	if l.fq != nil {
		if !l.fq.enqueue(p, l.cfg.Limit) {
			l.QueueDrops++
		}
	} else if len(l.fifo) >= l.cfg.Limit {
		l.QueueDrops++
		return
	} else {
		l.fifo = append(l.fifo, p)
	}
	if !l.busy {
		l.serve()
	}
}

// QueueLen is the number of packets waiting for the serializer.
func (l *Link) QueueLen() int {
	if l.fq != nil {
		return l.fq.n
	}
	return len(l.fifo)
}

// QueueDelay is how long the bytes waiting for the serializer take to
// leave at the current rate (FIFO only; zero for an unlimited link).
func (l *Link) QueueDelay() time.Duration {
	r := l.rate(l.sim.Now())
	if r <= 0 {
		return 0
	}
	bytes := 0
	for _, p := range l.fifo {
		bytes += p.Size
	}
	return time.Duration(float64(bytes*8) / float64(r) * float64(time.Second))
}

func (l *Link) serve() {
	var p *Packet
	if l.fq != nil {
		p = l.fq.dequeue(l.sim.Now())
	} else if len(l.fifo) > 0 {
		p = l.fifo[0]
		l.fifo[0] = nil
		l.fifo = l.fifo[1:]
	}
	if p == nil {
		l.busy = false
		return
	}
	l.busy = true
	r := l.rate(l.sim.Now())
	tx := time.Duration(0)
	if r > 0 {
		tx = time.Duration(float64(p.Size*8) / float64(r) * float64(time.Second))
	}
	l.sim.After(tx, func() {
		l.propagate(p)
		l.serve()
	})
}

func (l *Link) propagate(p *Packet) {
	now := l.sim.Now()
	at := now + l.delay(now)
	if j := l.cfg.Jitter; j > 0 {
		at += time.Duration(l.rng.NormFloat64() * float64(j))
		if at < now {
			at = now
		}
	}
	if !l.cfg.Reorder {
		// netem with a rate never schedules a packet before its predecessor.
		at = max(at, l.last)
		l.last = at
	}
	l.sim.At(at, func() { p.deliver(p) })
}

// fqCodel is a simplified FQ-CoDel (RFC 8290): per-flow queues, DRR with a
// 1514-byte quantum, new/old flow lists, CoDel 5 ms target / 100 ms interval
// at dequeue, and fattest-flow drop at the packet limit.
type fqCodel struct {
	flows    map[int]*fqFlow
	order    []*fqFlow // deterministic iteration for the fattest-flow drop
	newFlows []*fqFlow
	oldFlows []*fqFlow
	n        int
}

type fqFlow struct {
	q        []*Packet
	bytes    int
	deficit  int
	active   bool
	firstAbv time.Duration // codel: first time above target (0 = not above)
	dropping bool
	dropNext time.Duration
	count    int
}

const (
	fqQuantum   = 1514
	codelTarget = 5 * time.Millisecond
	codelPeriod = 100 * time.Millisecond
)

func (f *fqCodel) enqueue(p *Packet, limit int) bool {
	fl := f.flows[p.Flow]
	if fl == nil {
		fl = &fqFlow{}
		f.flows[p.Flow] = fl
		f.order = append(f.order, fl)
	}
	fl.q = append(fl.q, p)
	fl.bytes += p.Size
	f.n++
	if !fl.active {
		fl.active = true
		fl.deficit = fqQuantum
		f.newFlows = append(f.newFlows, fl)
	}
	if f.n > limit {
		var fat *fqFlow
		for _, x := range f.order {
			if fat == nil || x.bytes > fat.bytes {
				fat = x
			}
		}
		drop := fat.q[0]
		fat.q = fat.q[1:]
		fat.bytes -= drop.Size
		f.n--
		return drop != p
	}
	return true
}

func (f *fqCodel) dequeue(now time.Duration) *Packet {
	for {
		var list *[]*fqFlow
		if len(f.newFlows) > 0 {
			list = &f.newFlows
		} else if len(f.oldFlows) > 0 {
			list = &f.oldFlows
		} else {
			return nil
		}
		fl := (*list)[0]
		if fl.deficit <= 0 {
			fl.deficit += fqQuantum
			*list = (*list)[1:]
			f.oldFlows = append(f.oldFlows, fl)
			continue
		}
		p := f.codel(fl, now)
		if p == nil {
			// RFC 8290: an emptied new flow moves to the old list; an emptied
			// old flow becomes inactive.
			fromNew := list == &f.newFlows
			*list = (*list)[1:]
			if fromNew {
				f.oldFlows = append(f.oldFlows, fl)
			} else {
				fl.active = false
			}
			continue
		}
		fl.deficit -= p.Size
		return p
	}
}

func (f *fqCodel) take(fl *fqFlow) *Packet {
	if len(fl.q) == 0 {
		return nil
	}
	p := fl.q[0]
	fl.q[0] = nil
	fl.q = fl.q[1:]
	fl.bytes -= p.Size
	f.n--
	return p
}

// codel applies the CoDel control law to one flow's head.
func (f *fqCodel) codel(fl *fqFlow, now time.Duration) *Packet {
	for {
		p := f.take(fl)
		if p == nil {
			fl.firstAbv = 0
			fl.dropping = false
			return nil
		}
		sojourn := now - p.enq
		ok := sojourn < codelTarget || fl.bytes < fqQuantum
		if ok {
			fl.firstAbv = 0
		} else if fl.firstAbv == 0 {
			fl.firstAbv = now + codelPeriod
			ok = true
		} else if now < fl.firstAbv {
			ok = true
		}
		if fl.dropping {
			if ok {
				fl.dropping = false
				return p
			}
			if now >= fl.dropNext {
				fl.count++
				fl.dropNext += time.Duration(float64(codelPeriod) / math.Sqrt(float64(fl.count)))
				continue // drop p
			}
			return p
		}
		if !ok {
			fl.dropping = true
			if fl.count > 2 {
				fl.count -= 2
			} else {
				fl.count = 1
			}
			fl.dropNext = now + time.Duration(float64(codelPeriod)/math.Sqrt(float64(fl.count)))
			continue // drop p
		}
		return p
	}
}
