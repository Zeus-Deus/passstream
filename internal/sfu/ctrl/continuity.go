package ctrl

// Dependency shares exact flexible references and measured non-flexible L1T3
// continuity between the wire and simulator (A17 tl1_loss and F-4 rejection).
type Dependency struct{ streams [2]dependencyState }
type dependencyState struct {
	history                                [128]uint64
	unsupported, started, broken, baseOnly bool
	keyPicture                             uint64 // A4 supersession: stale drops cannot break a newer key
	picture                                uint64
	position                               int
	tl0                                    uint8 // non-flexible: TL0PICIDX as of picture
}

func (d *Dependency) Supported(f FrameInfo) bool {
	if f.Rendition > Low || f.SID != 0 || f.TID > 2 || (f.Flexible && !f.Key && len(f.Refs) == 0) {
		return false
	}
	for _, ref := range f.Refs {
		if ref >= f.Picture || f.Picture-ref > 127 {
			return false
		}
	}
	return true
}
func (d *Dependency) Allow(f FrameInfo, maxTID uint8) bool {
	if f.Rendition > Low {
		return false
	}
	s := &d.streams[f.Rendition]
	if !d.Supported(f) {
		s.unsupported = true
		return false
	}
	if s.unsupported {
		return false
	}
	if f.Key {
		*s = dependencyState{started: true, picture: f.Picture, keyPicture: f.Picture, position: 1, tl0: f.TL0}
		s.history[f.Picture%128] = f.Picture + 1
		return true
	}
	if !s.started || s.broken {
		return false
	}
	if !f.Flexible {
		if f.Picture > s.picture {
			// TL0PICIDX advances once per base position passed since the
			// previous picture, so missing pictures keep it checkable.
			gap := f.Picture - s.picture
			bases := uint64(0)
			if first := uint64((4 - s.position) % 4); first < gap {
				bases = (gap-first-1)/4 + 1
			}
			s.position = (s.position + int(gap%4) + 3) % 4
			expected := [4]uint8{0, 2, 1, 2}[s.position]
			s.position = (s.position + 1) % 4
			s.picture = f.Picture
			if f.TID != expected || f.TL0 != s.tl0+uint8(bases) {
				s.unsupported = true
				return false
			}
			s.tl0 = f.TL0
		}
		if f.TID == 0 {
			s.baseOnly = false
		}
		if s.baseOnly && f.TID > 0 {
			return false
		}
	} else {
		for _, ref := range f.Refs {
			if s.history[ref%128] != ref+1 {
				return false
			}
		}
	}
	if f.TID > maxTID {
		return false
	}
	s.history[f.Picture%128] = f.Picture + 1
	return true
}
func (d *Dependency) Lost(f FrameInfo) {
	if f.Rendition > Low {
		return
	}
	s := &d.streams[f.Rendition]
	if f.Picture < s.keyPicture {
		return
	}
	if s.history[f.Picture%128] == f.Picture+1 {
		s.history[f.Picture%128] = 0
	}
	if f.Key || f.TID == 0 {
		s.broken = true
	} else if f.TID == 1 && !f.Flexible {
		s.baseOnly = true
	}
}
func (d *Dependency) Unsupported(r Rendition) bool { return r > Low || d.streams[r].unsupported }
