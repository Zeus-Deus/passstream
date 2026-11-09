package ctrl

import "testing"

// C2-4: non-flexible TL0PICIDX must progress with the 0,2,1,2 pattern,
// across missing pictures too; a violation latches the rendition
// unsupported, visible to the forwarder through Unsupported.
func TestNonFlexibleTL0Progression(t *testing.T) {
	pic := func(n uint64, tid, tl0 uint8) FrameInfo {
		return FrameInfo{Rendition: Low, Picture: n, TID: tid, TL0: tl0}
	}
	d := &Dependency{}
	if !d.Allow(FrameInfo{Rendition: Low, Picture: 10, Key: true, TL0: 250}, 2) {
		t.Fatal("key refused")
	}
	// 2,1,2 share the key's TL0PICIDX; the next base advances it (wrapping).
	for _, f := range []FrameInfo{pic(11, 2, 250), pic(12, 1, 250), pic(13, 2, 250), pic(14, 0, 251)} {
		if !d.Allow(f, 2) {
			t.Fatalf("in-pattern picture %+v refused", f)
		}
	}
	// Pictures 15–21 missing: two bases (18 and 22) passed, 22 is the second.
	if !d.Allow(pic(22, 0, 253), 2) || d.Unsupported(Low) {
		t.Fatal("TL0PICIDX across missing pictures refused")
	}
	if d.Allow(pic(23, 2, 254), 2) || !d.Unsupported(Low) {
		t.Fatal("TL0PICIDX ahead of the pattern allowed")
	}
	if d.Allow(FrameInfo{Rendition: Low, Picture: 30, Key: true}, 2) || d.Unsupported(Native) {
		t.Fatal("unsupported state not latched per rendition")
	}
}
