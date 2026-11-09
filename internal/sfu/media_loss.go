package sfu

import "time"

type mediaLossSample struct {
	at             time.Time
	expected, lost int
}

type mediaLossWindow struct {
	samples     [32]mediaLossSample
	head, count int
}

func (w *mediaLossWindow) observe(now time.Time, expected, lost int) uint32 {
	w.samples[w.head] = mediaLossSample{now, expected, lost}
	w.head = (w.head + 1) % len(w.samples)
	w.count = min(w.count+1, len(w.samples))
	var total, missing int
	for i := 0; i < w.count; i++ {
		s := w.samples[(w.head-1-i+len(w.samples))%len(w.samples)]
		if now.Sub(s.at) < 500*time.Millisecond {
			total += s.expected
			missing += s.lost
		}
	}
	if total == 0 {
		return 0
	}
	return uint32(int64(missing) * 1_000_000 / int64(total))
}

func (p *Pacer) mediaLoss(now time.Time) float64 {
	if at := p.mediaLossAt.Load(); at > 0 && now.Sub(time.Unix(0, at)) < 500*time.Millisecond {
		return float64(p.mediaLossPPM.Load()) / 1_000_000
	}
	return 0
}
