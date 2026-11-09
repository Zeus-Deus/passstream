package sfusim

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

func firstDeltaAfterKey(p Profile, replay *Replay) time.Duration {
	s := newSim(1)
	gap := time.Duration(-1)
	src := newSource(s, 1, ctrl.Native, p, func(f *Frame, i int) {
		if i == 0 && !f.Key && gap < 0 {
			gap = f.Capture
		}
	})
	src.replay = replay
	src.Start(0)
	s.Run(4 * time.Second)
	return gap
}

// The real clock has a 4 Mbit/s configured cap while its simple scene
// emits only ~202 kbit/s. Sparse output must not become a smaller encoder
// repayment cap. Archived clock keys resume within 133 ms, not ~300 ms.
func TestEncoderRepaymentCapIsIndependentOfSparseOutput(t *testing.T) {
	for _, output := range []float64{20_000, 201_690} {
		p := Profile{FPS: 15, Layer: [3]float64{output, 0, 0}, EncoderBitrate: 4_000_000,
			KeyBytes: func(*rand.Rand) int { return 9000 }}
		if gap := firstDeltaAfterKey(p, nil); gap < 0 || gap > 2*time.Second/15 {
			t.Errorf("output %.0f bps changed the configured 4M encoder's key repayment: %v", output, gap)
		}
	}
}

func TestCustomAndArchivedReplayKeepTheirDocumentedLegacyFallback(t *testing.T) {
	p := Profile{FPS: 15, Layer: [3]float64{20_000, 0, 0}, KeyBytes: func(*rand.Rand) int { return 9000 }}
	if gap := firstDeltaAfterKey(p, nil); gap < time.Second {
		t.Fatalf("unspecified custom cap no longer uses output fallback: %v", gap)
	}
	p.EncoderBitrate = 4_000_000
	replay := &Replay{At: []time.Duration{0}, FPS: []float64{15}, Layer: [][3]float64{{20_000, 0, 0}}}
	if gap := firstDeltaAfterKey(p, replay); gap < time.Second {
		t.Fatalf("legacy replay without configured-cap evidence silently acquired a modern cap: %v", gap)
	}
}

func TestCanonicalEncoderCapsMatchTheProductionSender(t *testing.T) {
	for _, x := range []struct {
		p    Profile
		want int
	}{{Detailed, 2_000_000}, {Production, 4_000_000}, {LowProfile, 100_000}, {Clock, 4_000_000}} {
		if x.p.EncoderBitrate != x.want {
			t.Errorf("configured encoder cap %d, want %d", x.p.EncoderBitrate, x.want)
		}
	}
}
