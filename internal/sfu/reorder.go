package sfu

import (
	"github.com/pion/rtp"
	"slices"
	"time"
)

// Reorder gives upstream NACK repair reorderDeadline to arrive before
// abandoning a gap: eleven requests in 20 ms slots, enough for heavy random loss on
// both the NACK and the retransmission. Contiguous packets are not delayed.
// It adds no delay when packets are contiguous. Both packet count and lifetime
// are bounded so a malicious or broken publisher cannot grow memory indefinitely.
type queued struct {
	p  *rtp.Packet
	at time.Time
}
type reorder struct {
	next        uint16
	started     bool
	packets     map[uint16]queued
	missing     map[uint16]*nackAttempt
	high        uint16
	lastArrival time.Time
}

func newReorder() *reorder {
	return &reorder{packets: map[uint16]queued{}, missing: map[uint16]*nackAttempt{}}
}

// A new independent keyframe supersedes missing older pictures. Keep any of
// its packets that arrived first, but do not wait 300ms to repair obsolete refs.
func (r *reorder) keyframe(sequence uint16) bool {
	if !r.started || int16(sequence-r.next) <= 0 {
		return false
	}
	for seq := range r.packets {
		if int16(seq-sequence) < 0 {
			delete(r.packets, seq)
		}
	}
	for seq := range r.missing {
		if int16(seq-sequence) < 0 {
			delete(r.missing, seq)
		}
	}
	r.next = sequence
	if int16(sequence-r.high) > 0 {
		r.high = sequence - 1
	}
	return true
}
func (r *reorder) push(p *rtp.Packet, now time.Time) {
	if !r.started {
		r.next = p.SequenceNumber
		r.started = true
		r.high = p.SequenceNumber
	}
	if int16(p.SequenceNumber-r.next) < 0 {
		return
	}
	if d := int16(p.SequenceNumber - r.high); d > 0 {
		if !r.lastArrival.IsZero() && now.Sub(r.lastArrival) > reorderDeadline && d > 1 {
			// The first packet after an outage reveals old gaps. Their age is
			// not zero just because we have only now learned their numbers.
			clear(r.missing)
			clear(r.packets)
			r.next = p.SequenceNumber
		} else {
			// A large jump is an outage, not an invitation to replay the entire gap.
			for n := max(1, int(d)-255); n < int(d); n++ {
				seq := r.high + uint16(n)
				r.missing[seq] = &nackAttempt{at: now}
			}
		}
		r.high = p.SequenceNumber
	}
	r.lastArrival = now
	delete(r.missing, p.SequenceNumber)
	if len(r.packets) < 2048 {
		r.packets[p.SequenceNumber] = queued{p, now}
	}
}
func (r *reorder) pop(now time.Time) []*rtp.Packet {
	var out []*rtp.Packet
	for {
		if q, ok := r.packets[r.next]; ok {
			delete(r.packets, r.next)
			r.next++
			out = append(out, q.p)
			continue
		}
		var earliest uint16
		distance := 65536
		expired := false
		for seq, q := range r.packets {
			if now.Sub(q.at) > reorderDeadline {
				expired = true
			}
			if d := int(uint16(seq - r.next)); d < distance {
				distance = d
				earliest = seq
			}
		}
		if !expired {
			break
		}
		r.next = earliest
	}
	return out
}

const (
	reorderDeadline = 300 * time.Millisecond
	nackTries       = 11
	nackSpacing     = 20 * time.Millisecond
	nackDelay       = 0 * time.Millisecond
)

type nackAttempt struct {
	at, last time.Time
	tries    int
}

func (r *reorder) nacks(now time.Time) []uint16 {
	var out []uint16
	for seq, a := range r.missing {
		if now.Sub(a.at) >= reorderDeadline || int16(seq-r.next) < 0 || uint16(r.high-seq) >= 256 {
			delete(r.missing, seq)
			continue
		}
		// Anchor retry slots to discovery so scheduler drift cannot consume
		// the last repair opportunity; keep late catch-up feedback bounded.
		if a.tries < nackTries && now.Sub(a.at) >= nackDelay+time.Duration(a.tries)*nackSpacing && now.Sub(a.last) >= 10*time.Millisecond {
			out = append(out, seq)
			a.tries++
			a.last = now
		}
	}
	slices.SortFunc(out, func(a, b uint16) int { return int(uint16(a-r.next)) - int(uint16(b-r.next)) })
	return out
}
