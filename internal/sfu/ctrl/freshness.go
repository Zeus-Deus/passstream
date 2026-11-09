package ctrl

import "time"

// Maintain sheds disposable closures under the original age boundary first.
// A necessary old base remains reference/recovery work under its existing
// finite lease. Only a known failure to reach any fresh output before that
// chain breaks abandons it early; future/unarrived frames remain unknown.
func (p *Queue) Maintain(now time.Time, writerPending int) {
	p.expire(now)
	if writerPending != 0 {
		return
	}
	for _, r := range []Rendition{Native, Low} {
		if p.chains[r].broken || p.UncommittedKey(r) {
			continue
		}
		stale := -1
		for i, f := range p.frames {
			if f.id.rendition == r && p.originals[f.id] == f && !f.key && f.tid == 0 && !f.started && !f.freshUntil.IsZero() && now.After(f.freshUntil) {
				stale = i
				break
			}
		}
		if stale < 0 {
			continue
		}
		service := p.PictureService(now, PictureQuery{Rendition: r})
		if service.Known && service.Broken && service.FirstFresh.IsZero() {
			p.remove(stale, true, false, now) // coalesced CauseBroken; descendants cannot leak
		}
	}
}

func (*FIFOPacer) Maintain(time.Time, int) {}
