package sfu

import (
	"time"
)

func feedbackPeer(now time.Time) *Peer {
	r := &Room{}
	r.hasLow.Store(true)
	r.keyBytes.Store(250_000)
	p := &Peer{Room: r, Pacer: &Pacer{}}
	p.decoderReported.Store(true)
	p.Sent.Store(1)
	p.initialRTT.Store(int64(10 * time.Millisecond))
	p.Pacer.sustainedRateAt.Store(now.UnixNano())
	p.Pacer.minimumFeedbackRTT.Store(int64(10 * time.Millisecond))
	p.Pacer.departedBytes.Store(20_001)
	p.Pacer.acknowledgedBytes.Store(1)
	return p
}
