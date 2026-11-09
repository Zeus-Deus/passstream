package ctrl

import "time"

func sameKey(a, b KeyAuthorization) bool {
	return a.Generation == b.Generation && a.Rendition == b.Rendition && a.Frame == b.Frame
}

func (p *Queue) Operating() (KeyAuthorization, bool) {
	if p.operating == nil {
		return KeyAuthorization{}, false
	}
	return *p.operating, true
}

func (p *Queue) Key(r Rendition) (KeyAuthorization, bool) {
	for _, f := range p.frames {
		if f.key && f.id.rendition == r && p.originals[f.id] == f {
			a := KeyAuthorization{Generation: f.generation, Rendition: r, Frame: f.id.picture, Option: Option{r, 0}, Previous: p.active, Phase: KeyAdmitted}
			if p.authorization != nil && sameKey(a, *p.authorization) {
				return *p.authorization, true
			}
			if p.operating != nil && sameKey(a, *p.operating) {
				return *p.operating, true
			}
			return a, true
		}
	}
	return KeyAuthorization{}, false
}

// Authorize binds only existing, complete key metadata. It never releases
// payload, grants tokens or changes physical ownership before FIRST.
func (p *Queue) Authorize(a KeyAuthorization) bool {
	if a.Generation != p.generation || a.Rendition != a.Option.Rendition || a.Option.TID > 2 {
		return false
	}
	if p.operating != nil && sameKey(a, *p.operating) {
		p.operating.Option = a.Option
		return true
	}
	f := p.originals[frameID{a.Rendition, a.Frame}]
	if f == nil || !f.key || f.started || f.remaining != len(f.packets) || f.remaining == 0 {
		return false
	}
	a.Previous, a.Phase = p.active, KeyAdmitted
	f.generation = a.Generation
	p.authorization = &a
	p.SetCommit(a.Rendition, true)
	return true
}

func (p *Queue) Apply(o Option) {
	if p.operating != nil && p.operating.Phase == KeyCompleted && p.operating.Rendition == o.Rendition {
		p.operating.Option = o
	}
}

func (p *Queue) OnGeneration(now time.Time, g uint64) {
	p.generation, p.authorization = g, nil
	// An optional native candidate never interrupted Low. Cancel its old
	// proof; the active key stays a physical fact until reset/abort.
	if p.hasActive && p.active == Low {
		p.CancelKey(now, Native)
	}
	for _, r := range []Rendition{Native, Low} {
		if p.UncommittedKey(r) {
			p.SetCommit(r, false)
		}
	}
}

func (p *Queue) keyEvent(f *queuedFrame, phase KeyPhase, at time.Time) {
	p.event(KeyEvent{Rendition: f.id.rendition, Frame: f.id.picture, Phase: phase, At: at, Generation: f.generation})
}

func (*FIFOPacer) Operating() (KeyAuthorization, bool)    { return KeyAuthorization{}, false }
func (*FIFOPacer) Key(Rendition) (KeyAuthorization, bool) { return KeyAuthorization{}, false }
func (*FIFOPacer) Authorize(KeyAuthorization) bool        { return false }
func (*FIFOPacer) Apply(Option)                           {}
func (*FIFOPacer) OnGeneration(time.Time, uint64)         {}
