package ctrl

import (
	"math"
	"slices"
	"time"
)

type keyRequest struct {
	at        time.Time
	rendition Rendition
}
type keySize struct {
	at    time.Time
	bytes int
}
type keyDelivery struct {
	generation uint64
	frame      uint64
	rendition  Rendition // a cancelled candidate cannot clear another rendition's delivery
	active     bool
	until      time.Time
}

// RoomKeys shares requests across viewers without first-join exemptions.
// A9 spam/A14 join_storm and A4k delivery events own this state.
type RoomKeys struct {
	pending    map[int]keyRequest
	recovery   map[int]time.Time // last accepted recovery (debounce)
	asked      map[int]time.Time // last recovery request of any status (upgrade eligibility)
	delivery   map[int]keyDelivery
	last, sent [2]time.Time
	due        [2]bool
	sizes      [2][]keySize
	// Replacement low-key exception (C2 review §5, A19 pc_replacement):
	// credit spent per admitted session, kept only for KeyInterval, and the
	// room's last exception PLI.
	spent       map[int]time.Time
	exceptionAt time.Time
	exceptions  [16]Exception // ring of the latest exceptions granted (diagnostics, A19)
	exceptionN  int
	emitted     [2]time.Time
	assembled   [2]time.Time
}

// Exception is one replacement low-key exception the room granted.
type Exception struct {
	At      time.Time
	Session int
}

func (k *RoomKeys) Request(now time.Time, v int, r Rendition, c Cause) KeyStatus {
	if k.pending == nil {
		k.pending = map[int]keyRequest{}
		k.recovery = map[int]time.Time{}
		k.asked = map[int]time.Time{}
		k.delivery = map[int]keyDelivery{}
	}
	status := KeyPending
	if c == CauseRecovery || c == CauseBroken {
		// Every request is evidence that the viewer is still asking, before
		// any suppression below: a request ignored while a key is delivered
		// or debounced still blocks a native upgrade (C2-5, A9 spam).
		k.asked[v] = now
		d := k.delivery[v]
		if c == CauseRecovery && (d.active || now.Before(d.until)) {
			return KeyIgnored
		}
		// Debounce against the last accepted recovery: a stalled viewer
		// whose heartbeat repeats every second must still be served once
		// per RecoveryQuiet (A8 outage); a spammer gets no more (A9).
		previous := k.recovery[v]
		if c == CauseRecovery && !previous.IsZero() && now.Sub(previous) < RecoveryQuiet {
			return KeyIgnored
		}
		k.recovery[v] = now
		r, status = Low, KeyRedirected
	}
	if old, ok := k.pending[v]; !ok || old.rendition != r {
		k.pending[v] = keyRequest{now, r}
	}
	if (k.last[r].IsZero() || now.Sub(k.last[r]) >= KeyInterval) && !k.due[r] && k.sent[r].IsZero() {
		k.due[r] = true
		if status != KeyRedirected {
			status = KeyAccepted
		}
	}
	return status
}

// Replace is the first request of a replacement PC handed off from the
// admitted session's previous PC (C2 review §5): low, served at once by an
// exception PLI instead of waiting for the room's KeyInterval when
//   - the session has not spent its credit within KeyInterval (the credit
//     lives at the session, not the PC, so repeated replacements share it;
//     a replacement the room's own budget served at once spends it too);
//   - the room sent no exception PLI within KeyRetry (concurrent
//     replacements of other sessions coalesce into that key: their requests
//     predate its capture);
//   - no low PLI is already outstanding (retry and in-flight suppression).
//
// Native never gets an exception, and only NewViewerFrom loops call this:
// initial and new admissions join through Request like everyone else. The
// assembled key is still rechecked against the viewer's rate (KeyFits).
func (k *RoomKeys) Replace(now time.Time, v, session int) KeyStatus {
	for s, at := range k.spent {
		if now.Sub(at) >= KeyInterval {
			delete(k.spent, s)
		}
	}
	if k.spent == nil {
		k.spent = map[int]time.Time{}
	}
	_, spent := k.spent[session]
	status := k.Request(now, v, Low, CauseJoin)
	if status == KeyAccepted {
		// Served at once by the room's own budget: that is the session's
		// immediate service for this KeyInterval too.
		if !spent {
			k.spent[session] = now
		}
		return status
	}
	if spent || k.due[Low] || !k.sent[Low].IsZero() || !k.exceptionAt.IsZero() && now.Sub(k.exceptionAt) < KeyRetry {
		return status
	}
	k.spent[session], k.exceptionAt, k.due[Low] = now, now, true
	k.exceptions[k.exceptionN%len(k.exceptions)] = Exception{now, session}
	k.exceptionN++
	return KeyAccepted
}

// Exceptions returns the latest replacement exceptions granted, oldest
// first (at most the ring's 16).
func (k *RoomKeys) Exceptions() []Exception {
	n := min(k.exceptionN, len(k.exceptions))
	out := make([]Exception, n)
	for i := range out {
		out[i] = k.exceptions[(k.exceptionN-n+i)%len(k.exceptions)]
	}
	return out
}

// Times reports when r's latest PLI was emitted and its latest key
// assembled (diagnostics).
func (k *RoomKeys) Times(r Rendition) (emitted, assembled time.Time) {
	return k.emitted[r], k.assembled[r]
}

// Allowed: a viewer still asking for recovery, even when debounced, does
// not qualify for a native upgrade (A9 spam).
func (k *RoomKeys) Allowed(now time.Time, v int, _ Rendition, _ Cause) bool {
	t := k.asked[v]
	return t.IsZero() || now.Sub(t) >= RecoveryQuiet
}
func (k *RoomKeys) Due(now time.Time) []Rendition {
	var out []Rendition
	for r := range 2 {
		pending := false
		for _, q := range k.pending {
			if int(q.rendition) == r {
				pending = true
				break
			}
		}
		if !pending {
			continue
		}
		retry := !k.sent[r].IsZero() && now.Sub(k.sent[r]) >= KeyRetry
		ready := k.sent[r].IsZero() && (k.last[r].IsZero() || now.Sub(k.last[r]) >= KeyInterval)
		if k.due[r] || retry || ready {
			out = append(out, Rendition(r))
			k.sent[r], k.emitted[r] = now, now
			if !retry {
				k.last[r] = now
			}
			k.due[r] = false
		}
	}
	return out
}
func (k *RoomKeys) OnKey(now time.Time, r Rendition, bytes int, captured time.Time) {
	k.assembled[r] = now
	k.sizes[r] = append(k.sizes[r], keySize{now, bytes})
	if n := len(k.sizes[r]); n > KeySizeSamples {
		k.sizes[r] = k.sizes[r][n-KeySizeSamples:]
	}
	// A1 good_join: startup pictures before any request do not consume a
	// viewer-driven budget. Security keys with admitted viewers still do.
	if k.pending == nil {
		return
	}
	k.last[r], k.sent[r] = now, time.Time{}
	k.due[r] = false
	for v, q := range k.pending {
		if q.rendition == r && !captured.Before(q.at) {
			delete(k.pending, v)
		}
	}
}
func (k *RoomKeys) Size(now time.Time, r Rendition) (int, int) {
	var recent []int
	for _, s := range k.sizes[r] {
		if now.Sub(s.at) < KeySizeAge {
			recent = append(recent, s.bytes)
		}
	}
	if len(recent) == 0 {
		return 0, 0
	}
	slices.Sort(recent)
	return recent[int(math.Ceil(KeySizeQuantile*float64(len(recent))))-1], len(recent)
}
func (k *RoomKeys) OnDelivery(v int, e KeyEvent) {
	if k.delivery == nil {
		k.delivery = map[int]keyDelivery{}
	}
	d := k.delivery[v]
	if e.Generation < d.generation {
		return
	}
	switch e.Phase {
	case KeyAdmitted, KeyStarted:
		// The viewer's request for this rendition is being served, unless
		// it was made after this key was admitted: facts reach Keys when
		// the forwarder next steps, possibly after a newer request.
		if q, ok := k.pending[v]; ok && q.rendition == e.Rendition && !q.at.After(e.At) {
			delete(k.pending, v)
		}
		d = keyDelivery{generation: e.Generation, frame: e.Frame, rendition: e.Rendition, active: true}
	case KeyCompleted:
		if d.frame == e.Frame && d.rendition == e.Rendition && d.generation == e.Generation {
			d.active = false
			d.until = e.At.Add(KeyEnvelope)
		}
	case KeyAborted, KeyCancelled, KeySuperseded:
		if d.frame == e.Frame && d.rendition == e.Rendition && d.generation == e.Generation {
			d.active = false
			d.until = time.Time{}
		}
	}
	k.delivery[v] = d
}
func (k *RoomKeys) Leave(v int) {
	delete(k.pending, v)
	delete(k.recovery, v)
	delete(k.asked, v)
	delete(k.delivery, v)
}
