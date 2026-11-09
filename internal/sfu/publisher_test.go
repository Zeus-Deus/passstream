package sfu

import (
	"testing"
	"time"
)

func sourceExtension(ticks int64) []byte {
	return []byte{byte(ticks >> 16), byte(ticks >> 8), byte(ticks)}
}

func TestPublisherMeasuresHighPacketRateAcrossACompleteReceiveWindow(t *testing.T) {
	now := time.Now()
	c := publisherController{}
	for i := 0; i <= 120; i++ {
		at := time.Duration(i) * 2 * time.Millisecond
		c.observe(now.Add(at), sourceExtension(int64(at)*(1<<18)/int64(time.Second)), 1000, true)
	}
	if c.received < 3_900_000 || c.received > 4_100_000 {
		t.Fatal("a short packet ring could not measure the detailed source", c.received)
	}
}

func TestPublisherIgnoresLossAndOldRetransmissionClocks(t *testing.T) {
	now := time.Now()
	c := publisherController{rate: 4_000_000}
	for i := 0; i < 50; i++ {
		sent := time.Duration(i) * 40 * time.Millisecond
		c.observe(now.Add(sent), sourceExtension(int64(sent)*(1<<18)/int64(time.Second)), 1200, true)
		c.budget(now.Add(sent))
	}
	c.observe(now.Add(2*time.Second), sourceExtension(1), 1200, false)
	if c.rate != publisherWireCeiling || c.limited {
		t.Fatal("raw loss or cached retransmission clock cut productive source capacity", c.rate)
	}
}

func TestPublisherCutsARealSerializationQueue(t *testing.T) {
	now := time.Now()
	c := publisherController{rate: 4_000_000}
	for i := 0; i < 32; i++ {
		sent := time.Duration(i) * 2400 * time.Microsecond
		arrival := time.Duration(i) * 37500 * time.Microsecond
		c.observe(now.Add(arrival), sourceExtension(int64(sent)*(1<<18)/int64(time.Second)), 1200, true)
	}
	if c.rate > 400_000 || !c.limited {
		t.Fatal("publisher ignored an actual network queue", c.rate)
	}
}

func TestPublisherClockWrapAndShortOutageRetainCapacity(t *testing.T) {
	now := time.Now()
	c := publisherController{rate: 2_000_000}
	start := int64((1 << 24) - 32)
	c.observe(now, sourceExtension(start), 1200, true)
	d := 2 * time.Second
	c.observe(now.Add(d), sourceExtension(start+2*(1<<18)), 1200, true)
	if q := c.queue(now.Add(d)); q > time.Millisecond {
		t.Fatal("clock wrap or short outage manufactured network queue", q)
	}
	if c.budget(now.Add(d)) < 2_000_000 {
		t.Fatal("short outage restarted source slow start")
	}
}

func TestPublisherWithoutSendClockCannotClaimCapacity(t *testing.T) {
	c := publisherController{}
	if rate := c.budget(time.Now()); rate != 128_000 {
		t.Fatal("unknown source clock retained optimistic capacity", rate)
	}
}

func TestPublisherFixedPropagationStepDoesNotCollapseCapacity(t *testing.T) {
	now := time.Now()
	c := publisherController{rate: 4_000_000}
	for i := 0; i < 600; i++ {
		sent := time.Duration(i) * 40 * time.Millisecond
		arrival := sent
		if i >= 50 {
			arrival += 400 * time.Millisecond
		}
		c.observe(now.Add(arrival), sourceExtension(int64(sent)*(1<<18)/int64(time.Second)), 1200, true)
		c.budget(now.Add(arrival))
		if c.rate < 4_000_000 || c.limited {
			t.Fatal("fixed propagation step was mistaken for a growing queue", i, c.rate)
		}
	}
}

func TestSmallFastPublisherCanRetryALostPLI(t *testing.T) {
	p := &Peer{sourceControl: &publisherController{rate: 4_000_000, clock: true, lastAST: time.Now()}}
	p.initialRTT.Store(int64(80 * time.Millisecond))
	r := &Room{publisher: p}
	r.keyBytes.Store(16_000)
	if delay := r.unansweredSpacing(); delay != 300*time.Millisecond {
		t.Fatal("small fast refresh retained one-second unanswered wait", delay)
	}
	r.keyBytes.Store(250_000)
	if delay := r.unansweredSpacing(); delay != time.Second {
		t.Fatal("large frame lost in-flight protection", delay)
	}
	r.keyBytes.Store(16_000)
	p.sourceControl.rate = 128_000
	if delay := r.unansweredSpacing(); delay != time.Second {
		t.Fatal("slow publisher got repeated expensive keyframes", delay)
	}
	p.sourceControl.rate = 4_000_000
	p.sourceControl.clock = false
	if delay := r.unansweredSpacing(); delay != time.Second {
		t.Fatal("unmeasured publisher got fast keyframe retries", delay)
	}
}
