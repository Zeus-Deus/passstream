package sfusim

import (
	"math/rand/v2"
	"testing"
	"time"
)

// TestOversizeFirstKeyAt1500k is the sim regression for the orchestrator's
// C0 r3 admission counterexample: on 1.5 Mbit/s, native T0 holds at ≈1.24
// Mbit/s and a typical key admits at ≈1.42 Mbit/s, but the first native key
// the room sees is 588 KB. Native pixels must still be reached within 20 s of
// join on every seed, and native keys stay bounded by the room budget.
func TestOversizeFirstKeyAt1500k(t *testing.T) {
	seeds := 100
	if testing.Short() {
		seeds = 10
	}
	for seed := uint64(1); seed <= uint64(seeds); seed++ {
		prof := Detailed
		first := true
		prof.KeyBytes = func(r *rand.Rand) int {
			if first {
				first = false
				return 588_020
			}
			return archivedKey(r)
		}
		sc := &Scenario{Name: "oversize_first_key_1500k", Duration: 52 * time.Second, Native: prof, Audio: true,
			Links:   []LinkConfig{{Rate: rate(1_500_000), Delay: delay(40 * ms), Limit: 1000}},
			Viewers: []ViewerConfig{{Join: time.Second, Fwd: 0, Rev: Ideal, NominalRTT: 80 * ms}}}
		r := Run(sc, seed, Default)
		v := r.Viewers[0]
		if r.Keys[1].Bytes != 588_020 && r.Keys[0].Bytes != 588_020 {
			t.Fatalf("seed %d: first native key was not oversized: %+v", seed, r.Keys[:2])
		}
		if f := FirstNative(v, Window{time.Second, 21 * time.Second}, false); f < 0 {
			t.Fatalf("seed %d: no native pixels within 20 s of join", seed)
		}
		native := 0
		for _, k := range r.Keys {
			if k.Rend == 0 {
				native++
			}
		}
		if limit := int(sc.Duration/(3*time.Second)) + 1; native > limit {
			t.Fatalf("seed %d: %d native keys > budget %d", seed, native, limit)
		}
	}
}
