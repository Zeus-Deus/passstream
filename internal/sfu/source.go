package sfu

import "time"

// Encoder RTP clocks share capture time, not key assembly or retransmission
// delay. Anchor each clock to its least observed arrival offset; the same
// finite path window absorbs clock drift without timing media from repair.
type frameClock struct {
	at        time.Time
	timestamp uint32
	minimum   pathMinimum
}

func (c *frameClock) observe(timestamp uint32, now time.Time) time.Time {
	if c.at.IsZero() {
		c.at = now
	} else {
		c.at = c.at.Add(time.Duration(int32(timestamp-c.timestamp)) * time.Second / 90_000)
	}
	c.timestamp = timestamp
	return c.at.Add(c.minimum.observe(now, now.Sub(c.at)))
}

// pathMinimum is the least offset observed within clockWindow.
type pathMinimum struct{ samples []offsetSample }
type offsetSample struct {
	at     time.Time
	offset time.Duration
}

const clockWindow = 30 * time.Second

func (m *pathMinimum) observe(now time.Time, offset time.Duration) time.Duration {
	kept := m.samples[:0]
	least := offset
	for _, s := range m.samples {
		if now.Sub(s.at) < clockWindow {
			kept = append(kept, s)
			least = min(least, s.offset)
		}
	}
	m.samples = append(kept, offsetSample{now, offset})
	return least
}

// Publisher-only diagnostics distinguish encoded-source gaps from forwarding
// stalls without inspecting captured pixels or media content.
func (r *Room) sourceFrameTimes() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	times := make([]int64, r.frameN)
	for i := range times {
		times[i] = r.frameTimes[(r.frameHead-r.frameN+i+len(r.frameTimes))%len(r.frameTimes)]
	}
	return times
}
