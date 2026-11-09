package ctrl

import "time"

// snapshot copies bounded scheduling metadata, never payloads or release hooks.
// Pop mutates frame/slice headers and maps, so both frame order and the originals
// index must belong to the copy. Packet values and the live pacer stay unchanged.
func (p *Queue) snapshot() *Queue {
	q := &Queue{rate: p.rate, tokens: p.tokens, at: p.at, bytes: p.bytes, originals: map[frameID]*queuedFrame{}, padding: func(time.Time, int) bool { return false }}
	q.transition, q.deferred = p.transition, p.deferred
	q.chains = p.chains
	q.active, q.hasActive = p.active, p.hasActive
	q.generation = p.generation
	if p.authorization != nil {
		a := *p.authorization
		q.authorization = &a
	}
	if p.operating != nil {
		a := *p.operating
		q.operating = &a
	}
	if p.cluster != nil {
		c := *p.cluster
		q.cluster = &c
	}
	for _, f := range p.frames {
		copy := *f
		q.frames = append(q.frames, &copy)
		if p.originals[f.id] == f {
			q.originals[f.id] = &copy
		}
	}
	return q
}

// PictureService executes the real Pop/NextAt scheduler on observed metadata.
// In particular a cluster's peak is not extrapolated past its tail. Complete
// original pictures stay ordered, so returned post-key pictures include their
// already-admitted reference work. Unknown source tails or writer completion
// cannot establish a fresh output. Generation is echoed for the caller's fence.
func (p *Queue) PictureService(now time.Time, in PictureQuery) PictureService {
	out := PictureService{Generation: in.Generation}
	if in.Rendition > Low || in.WriterPending != 0 || len(in.Offer) > QueueBytes {
		return out
	}
	q := p.snapshot()
	if in.Commit {
		q.SetCommit(in.Rendition, true)
	}
	if in.Frame == 0 {
		if q.chains[in.Rendition].known {
			in.Frame = q.chains[in.Rendition].frame
		}
		for _, f := range q.frames {
			if f.key && f.id.rendition == in.Rendition {
				in.Frame = f.id.picture
				break
			}
		}
	}
	for _, k := range in.Offer {
		if k.Size <= 0 || !q.Push(now, k) {
			return out
		}
	}
	if in.Authorization != nil && !q.Authorize(*in.Authorization) {
		return out
	}
	chain := q.chains[in.Rendition]
	packets, found := 0, chain.known && chain.completed && chain.frame == in.Frame
	for _, f := range q.frames {
		packets += len(f.packets)
		if f.remaining != len(f.packets) {
			return out // a reserved but unarrived tail has unknown availability
		}
		found = found || f.id == (frameID{in.Rendition, in.Frame}) && f.key
	}
	if !found {
		return out
	}
	out.Known = true
	completed := map[uint64]bool{}
	if chain.known && chain.completed && chain.frame == in.Frame {
		out.Key, out.Broken = chain.at, chain.broken
		for _, id := range chain.history {
			if id > 0 {
				completed[id-1] = true
			}
		}
	}
	at := now
	for steps := 0; len(q.frames) > 0 && steps < 2*packets+2*QueueFrames+4; steps++ {
		next := q.NextAt(at)
		if next.IsZero() && len(q.frames) > 0 {
			out.Known = false
			return out
		}
		if len(q.frames) == 0 {
			break
		}
		at = next
		k, ok := q.Pop(at)
		for _, d := range q.Dropped() {
			if d.Rendition == in.Rendition && d.Frame >= in.Frame {
				out.Broken = true
			}
		}
		if !ok {
			continue
		}
		if k.Kind != KindVideo || k.Class == ClassRTX || k.Class == ClassFEC || !k.Last || k.Rendition != in.Rendition || k.Frame < in.Frame {
			continue
		}
		out.Pictures = append(out.Pictures, PictureDeparture{Frame: k.Frame, At: at, FreshUntil: k.FreshUntil, Lease: k.Deadline, Key: k.Key})
		if k.Key && k.Frame == in.Frame {
			out.Key = at
		}
		closed := !out.Broken && !out.Key.IsZero()
		if k.Flexible && !k.Key {
			if k.RefCount == 0 || int(k.RefCount) > len(k.Refs) {
				closed = false
			} else {
				for _, ref := range k.Refs[:k.RefCount] {
					closed = closed && completed[ref]
				}
			}
			out.Broken = out.Broken || !closed
		}
		completed[k.Frame] = closed
		if closed && out.FirstFresh.IsZero() && !k.FreshUntil.IsZero() && !at.After(k.FreshUntil) {
			out.FirstFresh = at
		}
	}
	for _, d := range q.Dropped() {
		if d.Rendition == in.Rendition && d.Frame >= in.Frame {
			out.Broken = true
		}
	}
	if len(q.frames) != 0 {
		out.Known = false
	}
	return out
}

// The archived FIFO diagnostic has no whole-picture reservation/expiry contract.
// It must not borrow the production scheduler's service proof.
func (*FIFOPacer) PictureService(_ time.Time, in PictureQuery) PictureService {
	return PictureService{Generation: in.Generation}
}
func (*FIFOPacer) UncommittedKey(Rendition) bool  { return false }
func (*FIFOPacer) Active() (Rendition, bool)      { return Low, false }
func (*FIFOPacer) SetCommit(Rendition, bool)      {}
func (*FIFOPacer) CancelKey(time.Time, Rendition) {}
