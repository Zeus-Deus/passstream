package ctrl

import "time"

// RTXDeparture forecasts one candidate using the real priority/token/cluster
// scheduler on a private, bounded metadata snapshot. Ordinary key/delta work
// cannot sit ahead of RTX; audio and earlier RTX do. No payload is copied.
// writerPending is already popped work whose transport write has not returned:
// its completion time is unknown, so it cannot support a deadline prediction.
// Owning cases: key repair behind warm deltas, batched repair, blocked writer.
func (p *Queue) RTXDeparture(now time.Time, kind Kind, size, writerPending int) (time.Time, bool) {
	if size <= 0 || (kind != KindVideo && kind != KindAudio) || writerPending != 0 || p.bytes+size > QueueBytes || len(p.frames) >= QueueFrames {
		return time.Time{}, false
	}
	q := Queue{rate: p.rate, tokens: p.tokens, at: p.at, cluster: p.cluster}
	packets := 1
	for _, f := range p.frames {
		if f.class > ClassRTX {
			continue
		}
		copy := *f // Pop changes slice headers/counters, never packet values.
		q.frames = append(q.frames, &copy)
		q.bytes += copy.bytes
		packets += len(copy.packets)
	}
	candidate := &queuedFrame{class: ClassRTX, kind: kind, packets: []Packet{{Size: size, Kind: kind, Class: ClassRTX, Last: true, Packets: 1}}, remaining: 1, bytes: size, at: now}
	q.frames = append(q.frames, candidate)
	q.bytes += size
	at := now
	// Each successful pop removes a packet; waits use the same NextAt
	// transitions as the writer, including the active cluster ending.
	for steps := 0; steps < packets*2+4; steps++ {
		next := q.NextAt(at)
		if next.IsZero() {
			return time.Time{}, false
		}
		at = next
		// NextAt expired any no-longer-useful work before this priority check.
		q.refill(at)
		q.expire(at)
		i := q.best()
		target := i >= 0 && q.frames[i] == candidate
		if _, ok := q.Pop(at); ok && target {
			return at, true
		}
	}
	return time.Time{}, false
}

// The old FIFO reference retains its own scheduling semantics, rather than
// borrowing the production queue's priorities for archived diagnostics.
func (p *FIFOPacer) RTXDeparture(now time.Time, kind Kind, size, writerPending int) (time.Time, bool) {
	if size <= 0 || writerPending != 0 || p.current(now) <= 0 {
		return time.Time{}, false
	}
	q := *p
	q.queue = append(append([]Packet(nil), p.queue...), Packet{Size: size, Kind: kind, Class: ClassRTX})
	at := now
	limit := 2*len(q.queue) + 4
	for steps := 0; steps < limit; steps++ {
		last := len(q.queue) == 1
		if _, ok := q.Pop(at); ok && last {
			return at, true
		}
		next := q.NextAt(at)
		if next.IsZero() {
			return time.Time{}, false
		}
		if !next.After(at) {
			next = at.Add(time.Nanosecond)
		}
		at = next
	}
	return time.Time{}, false
}
