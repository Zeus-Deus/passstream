package sfusim

import (
	"testing"
	"time"
)

// Owning regressions promoted from independent review: real Default
// components and unchanged Properties/Check/controls. This is not full TierA.
func TestSelectedSwitchRegressions(t *testing.T) {
	cases := []struct {
		name string
		seed uint64
	}{
		{"fifo_2m_1v", 1}, {"fq_512k_2v", 7}, {"pc_replacement", 85}, {"native_1500k_return", 1},
		{"good_join", 33}, {"loss_prejoin_5", 53},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, p := range Properties() {
				sc := p.Build(false)
				if sc.Name != tc.name {
					continue
				}
				r := Run(sc, tc.seed, Default)
				var control *Result
				if p.Control != nil {
					control = Run(p.Control(false), tc.seed, Default)
				}
				got := p.Check(r, control)
				t.Logf("seed=%d verdict=%v detail=%s", tc.seed, got.Pass, got.Detail)
				for _, k := range r.Keys {
					t.Logf("key rend=%v at=%s bytes=%d", k.Rend, k.At, k.Bytes)
				}
				for i, v := range r.Viewers {
					prev := v.Join
					for _, f := range v.Timeline {
						if f.At-prev > time.Second {
							t.Logf("v%d gap=%s ends=%s rend=%v tid=%d capture=%s", i, f.At-prev, f.At, f.Rend, f.TID, f.Capture)
						}
						prev = f.At
					}
				}
				if !got.Pass {
					t.Fail()
				}
				return
			}
			t.Fatal("property not found")
		})
	}
}
