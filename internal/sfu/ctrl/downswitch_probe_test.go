package ctrl

import (
	"os"
	"testing"
	"time"
)

// Retained diagnostic contract counterexample, now with its required-reset
// correction. Before-reset timings remain archived verbatim; these are
// scheduler metadata, not browser decode evidence.
func TestRequiredDownswitchBoundaryCounterexample(t *testing.T) {
	if os.Getenv("SFU_SWITCH_RECOVERY_PROBE") != "1" {
		t.Skip("set SFU_SWITCH_RECOVERY_PROBE=1 for the retained recovery-boundary counterexample")
	}
	q := &Queue{}
	q.SetRate(epoch, 128000)
	serviceFrame(q, epoch, 1, true, 0, 24, 1000, epoch.Add(FrameDeadline), epoch.Add(KeyEnvelope))
	if p, ok := q.Pop(epoch); !ok || !p.Key {
		t.Fatal("native key did not start")
	}
	admitted := epoch.Add(100 * time.Millisecond)
	for i := 0; i < 2; i++ {
		if !q.Push(admitted, Packet{ID: uint64(100 + i), Kind: KindVideo, Class: ClassKey, Rendition: Low, Frame: 1, Key: true, Size: 500, FrameSize: 1000, Packets: 2, Last: i == 1, Deadline: admitted.Add(KeyEnvelope), FreshUntil: admitted.Add(FrameDeadline)}) {
			t.Fatal("low key fixture")
		}
	}
	authorizeReset(q)
	query := PictureQuery{Rendition: Low, Frame: 1, Commit: true}
	wait := q.PictureService(admitted, query)
	control := q.snapshot()
	control.Purge(Native)
	reset := control.PictureService(admitted, query)
	if !wait.Known || !reset.Known || reset.Key.IsZero() || reset.Key.After(admitted.Add(FrameDeadline)) {
		t.Fatal("owning control invalid")
	}
	t.Logf("required Low reset: low complete %s after admission, original fresh deadline %s; existing-purge control low complete %s; no interleaved old tail", wait.Key.Sub(admitted), FrameDeadline, reset.Key.Sub(admitted))
	if wait.Key.After(admitted.Add(FrameDeadline)) {
		t.Errorf("required fresh low reset misses existing freshness boundary solely waiting for an old partial key")
	}
}
