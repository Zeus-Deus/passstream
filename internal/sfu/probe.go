package sfu

import (
	"time"
)

const capacityTrialDuration = 750 * time.Millisecond

// Stopping padding does not stop transport feedback. Keep the trial identity
// and its final planned deadline until another experiment actually starts.
func (p *Pacer) startTrial(until int64) {
	p.probeID.Store(until)
	p.probeEnd.Store(until)
	p.probeUntil.Store(until)
}

type capacityTrials struct {
	started, next, stable time.Time
	backoff, interval     time.Duration
	pending, failed       bool
	offered               int64
}

func (t *capacityTrials) observe(now time.Time, p *Pacer, rtt time.Duration, goal int64, stable bool) {
	if t.backoff == 0 {
		t.backoff = minBackoff
	}
	complete := t.pending && trialFeedbackComplete(now, p.probeEnd.Load(), rtt)
	rate := p.trialRate(now)
	if t.offered > 0 {
		// Numerical congestion cuts may fund only an intermediate discovery
		// step. Test what was actually offered, with the same 10% wire margin
		// as admission; a partial step cannot prove the higher native goal.
		goal = min(goal, t.offered*100/110)
	}
	// The rate controller already handles delay. A trial fails on its own
	// loss or completed delivery window, not an older competing queue ramp.
	// An aborted experiment without a full arrival window is inconclusive.
	failed := !p.trialClear(now) || complete && rate > 0 && rate < goal
	if p.probeID.Load() > 0 && failed && !t.failed {
		t.failed = true
		p.failedTrial.Store(p.probeID.Load())
		p.probeUntil.Store(0)
		if !t.pending {
			// Reordered feedback can arrive after the estimated tail, before
			// the next trial. It still belongs to this experiment.
			t.backoff = min(2*t.backoff, maxBackoff)
			if retry := t.started.Add(t.backoff); retry.After(t.next) {
				t.next = retry
			}
			t.stable = time.Time{}
		}
	}
	if t.pending {
		if complete {
			t.pending = false
			if t.failed {
				t.backoff = min(2*t.backoff, maxBackoff)
				if retry := t.started.Add(t.backoff); retry.After(t.next) {
					t.next = retry
				}
				t.stable = time.Time{}
			} else if goal == 0 || rate >= goal {
				t.backoff = minBackoff
				t.next = t.started.Add(min(t.interval, minBackoff))
			}
		}
	}
	if stable && (!t.pending || !t.failed) {
		if t.stable.IsZero() {
			t.stable = now
		}
		if now.Sub(t.stable) >= stableWindow {
			t.backoff = minBackoff
			if t.next.After(now.Add(minBackoff)) {
				t.next = now.Add(minBackoff)
			}
		}
	} else {
		t.stable = time.Time{}
	}
}
func (t *capacityTrials) ready(now time.Time) bool {
	return !t.pending && !now.Before(t.next)
}
func (t *capacityTrials) start(now time.Time, p *Pacer, until int64, wait time.Duration) {
	p.startTrial(until)
	t.started, t.interval = now, wait
	t.offered = p.probeCeiling.Load()
	t.pending, t.failed = true, false
	t.next = now.Add(max(wait, t.backoff))
	if wait < minBackoff && t.backoff <= minBackoff {
		t.next = now.Add(wait)
	}
}
