package ctrl

import (
	"math"
	"time"
)

type frameID struct {
	rendition Rendition
	picture   uint64
}
type queuedFrame struct {
	generation            uint64
	id                    frameID
	packets               []Packet
	class                 Class
	remaining, bytes      int
	at, deadline          time.Time
	freshUntil            time.Time // original age is never extended behind a key
	kind                  Kind
	key, started, blocked bool
	parity                bool
	tid                   uint8
	flexible              bool
	refs                  [3]uint64
	refCount              uint8
}

type queuedChain struct {
	frame                    uint64
	at                       time.Time
	known, completed, broken bool
	history                  [128]uint64 // same bounded VP9 reference horizon as Dependency
}

// Queue enforces the wire envelope and hard reservation bounds, including
// not-yet-arrived tails and pictures held behind a key (A4k/A16 collapse).
type Queue struct {
	rate                 int
	tokens               float64
	at                   time.Time
	cluster              *Cluster
	frames               []*queuedFrame
	originals            map[frameID]*queuedFrame
	bytes                int
	drops                []Packet // coalesced chain-breaking drops, one per rendition
	events               []KeyEvent
	padding              func(time.Time, int) bool
	release              func(Packet)
	serial               uint64
	transition, deferred [2]bool        // unstarted switch stays on the existing single-decoder boundary
	chains               [2]queuedChain // post-key reference catch-up/freshness, never a payload cache
	active               Rendition      // first-key departure, not merely requested/applied (R3 optional expiry)
	hasActive            bool
	generation           uint64
	authorization        *KeyAuthorization // one unstarted key-bound decision (D1/D2)
	operating            *KeyAuthorization // actual FIRST owner/cap; never completed dwell
}

func (p *Queue) current(now time.Time) int {
	if p.cluster != nil && !now.Before(p.cluster.Start) && now.Before(p.cluster.Until) {
		return p.cluster.Rate
	}
	return p.rate
}
func (p *Queue) refill(now time.Time) {
	if p.at.IsZero() {
		p.tokens = float64(BurstFloor)
	} else {
		dt := now.Sub(p.at).Seconds() * float64(p.rate)
		if c := p.cluster; c != nil {
			start, end := p.at, now
			if start.Before(c.Start) {
				start = c.Start
			}
			if end.After(c.Until) {
				end = c.Until
			}
			if end.After(start) {
				dt += end.Sub(start).Seconds() * float64(c.Rate-p.rate)
			}
		}
		p.tokens += max(0, dt) / 8
	}
	p.at = now
	p.tokens = min(p.tokens, p.burst(now))
}

// burst is the token cap: the burst depth at the current rate, at least one
// MTU-size packet.
func (p *Queue) burst(now time.Time) float64 {
	return max(float64(BurstFloor), float64(p.current(now))*PacerBurst.Seconds()/8)
}

// ready reports whether a packet of size bytes may leave: the tokens cover
// it, or they are at the cap and it is larger than any burst. Parity is a
// few bytes longer than the longest packet it protects, so it can exceed
// BurstFloor; waiting for tokens the cap never allows stalled the pacer
// for good at rates below ≈1 Mbit/s (found with C2 item 6b's
// longest-packet parity sizing). The deficit is repaid from later refills.
func (p *Queue) ready(now time.Time, size int) bool {
	return p.tokens >= min(float64(size), p.burst(now))
}
func (p *Queue) SetRate(now time.Time, bps int) { p.refill(now); p.rate = bps }
func (p *Queue) SetCluster(c *Cluster)          { p.cluster = c }
func (p *Queue) SetRelease(fn func(Packet))     { p.release = fn }

// drop discards one packet inside the mutation that decided it: the caller's
// release hook frees its payload now, under the caller's lock, whether or not
// the writer that runs Step is making progress. Only the fact Step needs (a
// non-superseded original base picture broke a chain) is kept, at most once
// per rendition (C2-1).
func (p *Queue) drop(k Packet) {
	if p.release != nil {
		p.release(k)
	}
	if !breaksChain(k) {
		return
	}
	for _, x := range p.drops {
		if x.Rendition == k.Rendition {
			return
		}
	}
	p.drops = append(p.drops, k)
}

// breaksChain: a dropped original key or TL0 picture that was not superseded
// leaves the viewer without a decodable reference (CauseBroken).
func breaksChain(k Packet) bool {
	return !k.Superseded && k.Kind == KindVideo && (k.Key || k.TID == 0) && k.Class != ClassRTX && k.Class != ClassFEC && k.Class != ClassPadding
}

// event records a key-delivery fact for Step, coalesced per rendition so the
// facts stay bounded while the writer (which runs Step) is blocked: a newly
// admitted key replaces its rendition's earlier events except an abort, and a
// later abort replaces an earlier one. What Keys.OnDelivery and Step derive
// (the latest key's phase, and that an admitted key was aborted) survives.
func (p *Queue) event(e KeyEvent) {
	id := KeyAuthorization{Generation: e.Generation, Rendition: e.Rendition, Frame: e.Frame}
	if p.operating != nil && sameKey(*p.operating, id) {
		p.operating.Phase = e.Phase
	}
	if p.authorization != nil && sameKey(*p.authorization, id) && e.Phase >= KeyCompleted {
		p.authorization = nil
	}
	if e.Phase == KeySuperseded {
		keep := p.events[:0]
		for _, x := range p.events {
			if x.Rendition != e.Rendition || x.Phase == KeyAborted || x.Frame == e.Frame && (x.Phase == KeyAdmitted || x.Phase == KeyStarted) {
				keep = append(keep, x)
			}
		}
		p.events = keep
	}
	if e.Phase == KeyAdmitted || e.Phase == KeyAborted || e.Phase == KeyCancelled {
		keep := p.events[:0]
		for _, x := range p.events {
			if x.Rendition != e.Rendition || (e.Phase == KeyAdmitted) == (x.Phase == KeyAborted) {
				keep = append(keep, x)
			}
		}
		p.events = keep
	}
	p.events = append(p.events, e)
}
func (p *Queue) Push(now time.Time, k Packet) bool {
	k.Generation = p.generation
	original := k.Kind == KindVideo && k.Class != ClassRTX && k.Class != ClassFEC && k.Class != ClassPadding
	id := frameID{k.Rendition, k.Frame}
	var f *queuedFrame
	if original {
		f = p.originals[id]
	}
	if f == nil {
		if k.Key && original {
			p.purge(k.Rendition, true)
		}
		size := max(k.Size, k.FrameSize)
		count := max(1, k.Packets)
		if p.bytes+size > QueueBytes || len(p.frames) >= QueueFrames {
			k.Last = true
			p.drop(k)
			return false
		}
		f = &queuedFrame{generation: p.generation, id: id, class: k.Class, remaining: count, bytes: size, at: now, deadline: k.Deadline, kind: k.Kind, key: k.Key, tid: k.TID}
		f.flexible, f.refs, f.refCount = k.Flexible, k.Refs, k.RefCount
		f.freshUntil = k.FreshUntil
		if f.freshUntil.IsZero() && !k.Key {
			f.freshUntil = k.Deadline
		}
		if original && !k.Key {
			for _, x := range p.frames {
				if x.key && x.id.rendition == k.Rendition {
					f.blocked = true
					break
				}
			}
		}
		if k.Class == ClassFEC && k.ProtectedClass > 0 {
			f.class = k.ProtectedClass
			f.parity = true
		}
		p.frames = append(p.frames, f)
		p.bytes += size
		if original {
			if p.originals == nil {
				p.originals = map[frameID]*queuedFrame{}
			}
			p.originals[id] = f
		}
		if f.key {
			p.chains[k.Rendition] = queuedChain{frame: k.Frame, known: true}
			p.keyEvent(f, KeyAdmitted, now)
		}
	}
	f.packets = append(f.packets, k)
	return true
}
func (p *Queue) remove(index int, drop, superseded bool, now time.Time) {
	f := p.frames[index]
	original := p.originals[f.id] == f
	if original {
		delete(p.originals, f.id)
		if p.authorization != nil && sameKey(*p.authorization, KeyAuthorization{Generation: f.generation, Rendition: f.id.rendition, Frame: f.id.picture}) {
			p.authorization = nil
		}
	}
	p.bytes -= f.bytes
	p.frames = append(p.frames[:index], p.frames[index+1:]...)
	if drop {
		if !superseded && original && (f.key || f.tid == 0) {
			p.chains[f.id.rendition].broken = true
		}
		for i, k := range f.packets {
			k.Superseded = superseded
			k.Last = i == len(f.packets)-1
			p.drop(k)
		}
		if f.key && !superseded {
			p.keyEvent(f, KeyAborted, now)
		}
		if !superseded && f.kind == KindVideo && !f.parity && f.class != ClassRTX {
			p.dependants(index, f, now)
		}
	}
}

// dependants drops the queued pictures of f's rendition that reference the
// dropped picture f, each reported like any other drop so continuity learns
// of it (A17 tl1_loss: a shed TL1 left its queued TL2 to go out
// undecodable). In the supported L1T3 structures (docs/CONTROLLER.md) a key
// or TL0 is referenced by everything up to the next key, and a TL1 or TL2
// by the later pictures of a higher TID up to the next one of its TID or
// lower. from is the index f occupied: later pictures follow it.
func (p *Queue) dependants(from int, f *queuedFrame, now time.Time) {
	base := f.key || f.tid == 0
	if f.flexible && !base {
		for i := from; i < len(p.frames); {
			x := p.frames[i]
			if x.kind != KindVideo || x.parity || x.id.rendition != f.id.rendition || p.originals[x.id] != x || x.started {
				i++
				continue
			}
			if x.key {
				return
			}
			depends := false
			for _, ref := range x.refs[:min(int(x.refCount), len(x.refs))] {
				depends = depends || ref == f.id.picture
			}
			if depends {
				p.remove(i, true, false, now)
			} else {
				i++
			}
		}
		return
	}
	for i := from; i < len(p.frames); {
		x := p.frames[i]
		if x.kind != KindVideo || x.parity || x.id.rendition != f.id.rendition || p.originals[x.id] != x || x.started {
			i++
			continue
		}
		if x.key || !base && x.tid <= f.tid {
			return
		}
		p.remove(i, true, false, now)
	}
}
func (p *Queue) expire(now time.Time) {
	for i := 0; i < len(p.frames); {
		f := p.frames[i]
		// A picture held behind a key is bounded by the key's own abort;
		// its deadline clock starts when the key completes (Pop).
		expired := !f.blocked && !f.deadline.IsZero() && !now.Before(f.deadline)
		// Disposable closures never gain freshness by waiting behind a key.
		// Required bases keep their bounded reference lease, not a new age.
		if !f.key && f.tid > 0 && p.originals[f.id] == f && !f.started && !f.freshUntil.IsZero() && !now.Before(f.freshUntil) {
			expired = true
		}
		// FrameAge sheds disposable layers under a transient backlog (the
		// frames produced while a key was delivered). A base picture is
		// kept until its useful deadline: dropping it breaks the chain and
		// costs a recovery key (A18 native_1500k).
		if !f.key && f.tid > 0 && p.originals[f.id] == f && !f.blocked && !f.started && now.Sub(f.at) > FrameAge {
			expired = true
		}
		if expired {
			if f.key && !f.started && p.hasActive && f.id.rendition != p.active && p.chains[p.active].completed && !p.chains[p.active].broken {
				// No target byte interrupted the intact old decoder. The
				// same hard deadline discards this optional transaction and
				// its closure, without inventing a broken reference chain.
				p.CancelKey(now, f.id.rendition)
				i = 0
				continue
			}
			p.remove(i, true, false, now)
		} else {
			i++
		}
	}
}

// best picks the next frame to send. Original pictures of one rendition
// leave in order: only the oldest queued one is eligible, so priority never
// delivers a newer picture ahead of an older one (the receiver drops the late
// picture and its SFrame counter goes backwards; e2ee.mjs). Class priority
// orders kinds and renditions against each other; TL1/TL2 still give way
// through FrameAge shedding.
func (p *Queue) best() int {
	best := -1
	var head [2]bool
	reset := p.requiredLow()
	for i, f := range p.frames {
		if f.kind == KindVideo && !f.parity && p.originals[f.id] == f {
			if reset && f.id.rendition == Native {
				continue
			}
			r := f.id.rendition
			if head[r] {
				continue
			}
			head[r] = true
		}
		if len(f.packets) == 0 || f.blocked {
			continue
		}
		if f.key && !f.started && p.transition[f.id.rendition] {
			if p.deferred[f.id.rendition] {
				continue
			}
			partial := false
			for _, old := range p.frames {
				if old.kind == KindVideo && !old.parity && p.originals[old.id] == old && old.started && old.id.rendition != f.id.rendition {
					partial = true
					break
				}
			}
			if partial && !reset {
				continue
			}
		}
		if best < 0 || f.class < p.frames[best].class || (f.class == p.frames[best].class && f.parity && !p.frames[best].parity) || (f.class == ClassRTX && f.kind == KindAudio && p.frames[best].kind != KindAudio) {
			best = i
		}
	}
	return best
}

// Only a currently authorized, complete Low key can reset an active native
// decoder. BeforeSend grants the transaction after checking the current desire,
// physical writer state and actual service. Arrival alone leaves it unarmed.
func (p *Queue) requiredLow() bool {
	if !p.hasActive || p.active != Native || !p.transition[Low] || p.deferred[Low] || p.authorization == nil || p.authorization.Intent != Required {
		return false
	}
	for _, f := range p.frames {
		if f.key && f.id.rendition == Low && !f.started && len(f.packets) > 0 && f.remaining == len(f.packets) && sameKey(*p.authorization, KeyAuthorization{Generation: f.generation, Rendition: Low, Frame: f.id.picture}) {
			return true
		}
	}
	return false
}
func (p *Queue) Pop(now time.Time) (Packet, bool) {
	p.refill(now)
	p.expire(now)
	i := p.best()
	if i < 0 {
		if p.cluster == nil || p.cluster.MediaOnly || !now.Before(p.cluster.Until) || now.Before(p.cluster.Start) || p.tokens < float64(p.paddingSize()) || (p.padding != nil && !p.padding(now, p.paddingSize())) {
			return Packet{}, false
		}
		size := p.paddingSize()
		if p.tokens < float64(size) || (p.padding != nil && !p.padding(now, size)) {
			return Packet{}, false
		}
		p.serial++
		p.tokens -= float64(size)
		return Packet{ID: p.serial, Size: size, Kind: KindPadding, Class: ClassPadding, Cluster: p.cluster.ID, Last: true, Packets: 1}, true
	}
	f := p.frames[i]
	k := f.packets[0]
	k.Generation = f.generation
	k.FreshUntil = f.freshUntil
	if !p.ready(now, k.Size) {
		return Packet{}, false
	}
	if f.key && !f.started && p.transition[k.Rendition] {
		// The first target packet is the commit. Optional upgrades finish
		// old pictures; a required Low reset retires their partial closure.
		// Neither permits an old tail after this decoder boundary.
		p.supersede(now, 1-k.Rendition)
		p.transition[k.Rendition], p.deferred[k.Rendition] = false, false
		for j, candidate := range p.frames {
			if candidate == f {
				i = j
				break
			}
		}
	}
	if !f.deadline.IsZero() {
		k.Deadline = f.deadline // as extended behind a key: repair uses it
	}
	p.tokens -= float64(k.Size)
	if p.cluster != nil && !now.Before(p.cluster.Start) && now.Before(p.cluster.Until) {
		k.Cluster = p.cluster.ID
	}
	if !f.started {
		f.started = true
		if f.key {
			previous := p.active
			a := KeyAuthorization{Generation: f.generation, Rendition: k.Rendition, Frame: k.Frame, Option: Option{k.Rendition, 0}, Previous: previous, Phase: KeyStarted}
			if p.authorization != nil && sameKey(*p.authorization, a) {
				a = *p.authorization
				a.Phase = KeyStarted
			} else if p.hasActive && previous == Low && k.Rendition == Native {
				a.Intent = Upgrade
			}
			p.operating, p.authorization = &a, nil
			p.active, p.hasActive = k.Rendition, true
			p.keyEvent(f, KeyStarted, now)
		}
	}
	f.packets = f.packets[1:]
	f.remaining--
	f.bytes -= k.Size
	p.bytes -= k.Size
	if f.remaining == 0 {
		if p.originals[f.id] == f {
			chain := &p.chains[k.Rendition]
			chain.history[k.Frame%128] = k.Frame + 1
			if f.key {
				chain.completed, chain.at = true, now
			}
		}
		if f.bytes != 0 {
			p.bytes -= f.bytes
			f.bytes = 0
		}
		p.remove(i, false, false, now)
		if f.key {
			p.keyEvent(f, KeyCompleted, now)
			for _, x := range p.frames {
				if x.id.rendition == k.Rendition && x.blocked {
					// The sender's key boost leaves no encoder pause, so
					// base pictures can wait behind a slow key past their
					// deadline; the time spent behind it does not count
					// (A4k b1_3m_560k, A18 native_1500k).
					if !x.deadline.IsZero() {
						x.deadline = x.deadline.Add(now.Sub(x.at))
					}
					x.blocked = false
					x.at = now
				}
			}
		}
	}
	return k, true
}
func (p *Queue) NextAt(now time.Time) time.Time {
	p.refill(now)
	p.expire(now)
	size := p.paddingSize()
	if i := p.best(); i >= 0 {
		size = p.frames[i].packets[0].Size
	} else if p.cluster == nil || p.cluster.MediaOnly || !now.Before(p.cluster.Until) || (p.padding != nil && !p.padding(now, size)) {
		return time.Time{}
	}
	rate := p.current(now)
	if rate <= 0 {
		return time.Time{}
	}
	at := now.Add(time.Duration(math.Ceil(max(0, min(float64(size), p.burst(now))-p.tokens) * 8 / float64(rate) * float64(time.Second))))
	if p.cluster != nil && now.Before(p.cluster.Until) && at.After(p.cluster.Until) {
		return p.cluster.Until
	}
	return at
}
func (p *Queue) Dropped() []Packet     { d := p.drops; p.drops = nil; return d }
func (p *Queue) KeyEvents() []KeyEvent { e := p.events; p.events = nil; return e }
func (p *Queue) KeyInFlight() bool {
	for _, f := range p.frames {
		if f.key {
			return true
		}
	}
	return false
}
func (p *Queue) Queued() (int, int) { return p.bytes, len(p.frames) }
func (p *Queue) QueuedKey(r Rendition) (remaining, total int) {
	for _, f := range p.frames {
		if f.key && f.id.rendition == r && p.originals[f.id] == f && len(f.packets) > 0 {
			return f.bytes, f.packets[0].FrameSize
		}
	}
	return 0, 0
}
func (p *Queue) purge(r Rendition, superseded bool) {
	for i := 0; i < len(p.frames); {
		if f := p.frames[i]; f.kind == KindVideo && f.id.rendition == r {
			p.remove(i, true, superseded, p.at)
		} else {
			i++
		}
	}
}
func (p *Queue) Purge(r Rendition) { p.purge(r, true) }

// supersede is the commit boundary: release every queued old video kind and
// its key-delivery ownership exactly once. An intentional decoder reset has
// its replacement key already admitted; it does not create a broken-chain PLI.
func (p *Queue) supersede(now time.Time, r Rendition) {
	chain := p.chains[r]
	frame, retire := chain.frame, chain.known && !chain.completed && !chain.broken
	generation := p.generation
	if p.operating != nil && p.operating.Rendition == r {
		generation = p.operating.Generation
	}
	for _, f := range p.frames {
		if f.key && f.id.rendition == r && p.originals[f.id] == f {
			frame, retire = f.id.picture, true
			generation = f.generation
			break
		}
	}
	p.purge(r, true)
	if retire {
		// A prior drain may already have released the payload. The chain
		// still owns its admitted delivery until a terminal event retires it.
		p.event(KeyEvent{Rendition: r, Frame: frame, Phase: KeySuperseded, At: now, Generation: generation})
	}
	p.chains[r] = queuedChain{}
	p.transition[r], p.deferred[r] = false, false
}

func (p *Queue) Active() (Rendition, bool) { return p.active, p.hasActive }

func (p *Queue) UncommittedKey(r Rendition) bool {
	for _, f := range p.frames {
		if f.key && f.id.rendition == r && !f.started {
			return true
		}
	}
	return false
}
func (p *Queue) SetCommit(r Rendition, allow bool) { p.transition[r], p.deferred[r] = true, !allow }
func (p *Queue) CancelKey(now time.Time, r Rendition) {
	for _, f := range p.frames {
		if f.key && f.id.rendition == r && !f.started {
			id := f.id
			p.purge(r, true)
			p.event(KeyEvent{Rendition: r, Frame: id.picture, Phase: KeyCancelled, At: now, Generation: f.generation})
			p.transition[r], p.deferred[r] = false, false
			p.chains[r] = queuedChain{}
			return
		}
	}
}

// Low-rate A17 clock probes must still contain ProbeMinPackets, rather than
// being forced to a packet-size-imposed rate floor.
func (p *Queue) paddingSize() int {
	if p.cluster == nil {
		return BurstFloor
	}
	return max(1, min(BurstFloor, int(float64(p.cluster.Rate)*ProbeLength.Seconds()/8/ProbeMinPackets)))
}

// Retained counts the drop and key-delivery facts waiting for Step (C2-1).
// Dropped payloads are not among them: they were released when dropped.
func (p *Queue) Retained() (drops, events int) { return len(p.drops), len(p.events) }
