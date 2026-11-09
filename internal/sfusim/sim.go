// Package sfusim is a deterministic discrete-event model of one PassStream
// room: a VP9 L1T3 publisher with native and low renditions, the SFU forward
// path driving internal/sfu/ctrl, per-viewer links, competing TCP and
// browser-like receivers. It is Tier A of docs/GATES.md.
//
// Time is virtual. Every random draw comes from a stream derived from the
// scenario seed, so a run is a pure function of (scenario, seed, controller).
package sfusim

import (
	"math/rand/v2"
	"time"
)

// epoch is the virtual wall clock origin handed to ctrl as time.Time.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type event struct {
	at  time.Duration
	seq uint64
	fn  func()
}

// Sim is the event loop. Events at the same instant run in scheduling order.
type Sim struct {
	now  time.Duration
	seq  uint64
	heap []event
	seed uint64
}

// traceSim, when set by a diagnostic test, receives each new Sim.
var traceSim func(*Sim)

// traceLinks, when set by a regression test, receives each run's links.
var traceLinks func([]*Link)

func newSim(seed uint64) *Sim {
	s := &Sim{seed: seed}
	if traceSim != nil {
		traceSim(s)
	}
	return s
}

// Now is the current virtual time since the start of the run.
func (s *Sim) Now() time.Duration { return s.now }

// Time converts virtual time to the time.Time the controller sees.
func (s *Sim) Time() time.Time { return epoch.Add(s.now) }

// Rand returns an independent deterministic stream for one model component.
func (s *Sim) Rand(stream uint64) *rand.Rand {
	return rand.New(rand.NewPCG(s.seed, stream*0x9e3779b97f4a7c15+1))
}

// At schedules fn at absolute virtual time t (clamped to now).
func (s *Sim) At(t time.Duration, fn func()) {
	if t < s.now {
		t = s.now
	}
	s.seq++
	s.heap = append(s.heap, event{t, s.seq, fn})
	h := s.heap
	for i := len(h) - 1; i > 0; {
		p := (i - 1) / 2
		if !less(h[i], h[p]) {
			break
		}
		h[i], h[p] = h[p], h[i]
		i = p
	}
}

// After schedules fn d after now.
func (s *Sim) After(d time.Duration, fn func()) { s.At(s.now+d, fn) }

func less(a, b event) bool { return a.at < b.at || a.at == b.at && a.seq < b.seq }

func (s *Sim) pop() event {
	h := s.heap
	top := h[0]
	n := len(h) - 1
	h[0] = h[n]
	h[n] = event{}
	h = h[:n]
	for i := 0; ; {
		l, r, m := 2*i+1, 2*i+2, i
		if l < n && less(h[l], h[m]) {
			m = l
		}
		if r < n && less(h[r], h[m]) {
			m = r
		}
		if m == i {
			break
		}
		h[i], h[m] = h[m], h[i]
		i = m
	}
	s.heap = h
	return top
}

// Run executes events until virtual time until.
func (s *Sim) Run(until time.Duration) {
	for len(s.heap) > 0 && s.heap[0].at <= until {
		e := s.pop()
		s.now = e.at
		e.fn()
	}
	s.now = until
}

// Every runs fn every period starting at first, until the run ends.
func (s *Sim) Every(first, period time.Duration, fn func()) {
	var tick func()
	tick = func() {
		fn()
		s.After(period, tick)
	}
	s.At(first, tick)
}
