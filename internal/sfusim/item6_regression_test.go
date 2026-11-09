package sfusim

import (
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Regressions for C2 item 6a, "keys and timing" (docs/REPORT.md).

// viewer_flap diagnosis: behind the media barrier (Consent lost 2 s into the
// outage) a missed base must not request a low key that no one receives;
// before the fix the room's low budget was spent ≈0.2 s before the restore
// and every later low request waited KeyInterval from it.
func TestNoLowKeyBehindTheBarrier(t *testing.T) {
	p := pcReplacement("")
	for seed := uint64(1); seed <= 20; seed++ {
		r := Run(p.Build(false), seed, Default)
		for _, at := range r.PLIs[ctrl.Low] {
			if at > 20*time.Second && at < 22500*ms {
				t.Fatalf("seed %d: low PLI at %v while the subject was unreachable", seed, at)
			}
		}
	}
}

// A8 outage: the low key the post-restore recovery requested assembles
// ≈20 ms after the stall, before the next periodic step. The forwarder must
// take it (the recovery is an immediate switch intent), not skip it and wait
// KeyInterval for the next one (seed 2 and seed 3 traces).
func TestRecoveryLowKeyIsForwarded(t *testing.T) {
	p := outage()
	for seed := uint64(1); seed <= 30; seed++ {
		r := Run(p.Build(false), seed, Default)
		var key *KeyRecord
		for i, k := range r.Keys {
			if k.Rend == ctrl.Low && k.At >= 22500*ms {
				key = &r.Keys[i]
				break
			}
		}
		if key == nil {
			t.Fatalf("seed %d: no low key after the restore", seed)
		}
		shown := false
		for _, f := range r.Viewers[0].Timeline {
			if f.Key && f.Rend == ctrl.Low && f.At >= key.At && f.At <= key.At+500*ms {
				shown = true
				break
			}
		}
		if !shown {
			t.Fatalf("seed %d: the first low key after the restore (assembled %v) was not displayed", seed, key.At)
		}
	}
}
