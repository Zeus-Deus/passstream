package sfusim

import (
	"testing"
	"time"
)

// A1: discovery's finite bootstrap must not be spent on an unmeasurable
// fast-key padding burst. The admitted key provides the next experiment.
// Seed 16 reached full native only 3 s after joining before this change.
func TestJoinUsesItsAdmittedKeyAfterTheRenditionRungs(t *testing.T) {
	for _, seed := range []uint64{1, 16, 45, 49, 100} {
		sc := goodJoin().Build(false)
		r := Run(sc, seed, Default)
		v := r.Viewers[0]
		if full := FirstNative(v, Window{v.Join, sc.Duration}, true); full < 0 || full > 2*time.Second {
			t.Errorf("seed %d: full native %v", seed, full)
		}
	}
}

func TestDiscoveryEscapesAStaleCeilingAndRenewsFastKeyEvidence(t *testing.T) {
	p := collapse()
	for _, seed := range []uint64{84, 85, 100} {
		r := Run(p.Build(false), seed, Default)
		if verdict := p.Check(r, nil); !verdict.Pass {
			t.Errorf("seed %d: %s", seed, verdict.Detail)
		}
	}
}
