package ctrl

import (
	"testing"
	"time"
)

func TestOriginalFreshnessShedsBlockedEnhancementClosure(t *testing.T) {
	q := &Queue{}
	q.SetRate(epoch, 1_500_000)
	serviceFrame(q, epoch, 1, true, 0, 200, 1242, epoch.Add(FrameDeadline), epoch.Add(KeyEnvelope))
	serviceFrame(q, epoch, 2, false, 1, 2, 1200, epoch.Add(100*time.Millisecond), epoch.Add(FrameDeadline))
	serviceFrame(q, epoch, 3, false, 2, 2, 1200, epoch.Add(FrameDeadline), epoch.Add(FrameDeadline))
	serviceFrame(q, epoch, 4, false, 0, 2, 1200, epoch.Add(FrameDeadline), epoch.Add(FrameDeadline))
	for _, f := range q.frames {
		f.flexible = true
		if !f.key {
			f.refCount = 1
			f.refs[0] = f.id.picture - 1
		}
		if f.id.picture == 4 {
			f.refs[0] = 1
		}
	}
	q.Maintain(epoch.Add(101*time.Millisecond), 0)
	if len(q.frames) != 2 || q.frames[0].id.picture != 1 || q.frames[1].id.picture != 4 || len(q.Dropped()) != 0 {
		t.Fatal("enhancement closure leaked or useful base was blindly dropped")
	}
}

func TestOldBaseCanFundAKnownFreshDescendantWithoutAgeReset(t *testing.T) {
	q := &Queue{}
	q.SetRate(epoch, 5_000_000)
	serviceFrame(q, epoch, 1, true, 0, 200, 1242, epoch.Add(FrameDeadline), epoch.Add(KeyEnvelope))
	serviceFrame(q, epoch.Add(66*time.Millisecond), 2, false, 0, 130, 1231, epoch.Add(1066*time.Millisecond), epoch.Add(1066*time.Millisecond))
	serviceFrame(q, epoch.Add(900*time.Millisecond), 3, false, 0, 2, 1200, epoch.Add(1900*time.Millisecond), epoch.Add(1900*time.Millisecond))
	q.Maintain(epoch.Add(1100*time.Millisecond), 0)
	forecast := q.PictureService(epoch.Add(1100*time.Millisecond), PictureQuery{Frame: 1})
	if !forecast.Known || forecast.Broken || forecast.FirstFresh.IsZero() || len(forecast.Pictures) != 3 {
		t.Fatalf("useful closure refused: %+v", forecast)
	}
	old := forecast.Pictures[1]
	if !old.At.After(old.FreshUntil) || old.FreshUntil != epoch.Add(1066*time.Millisecond) || !old.Lease.After(old.FreshUntil) {
		t.Fatalf("old base age was relabelled: %+v", old)
	}
	actual := actualPictureService(q, epoch.Add(1100*time.Millisecond), PictureQuery{Frame: 1})
	if actual.FirstFresh != forecast.FirstFresh || len(q.Dropped()) != 0 {
		t.Fatal("reference catch-up did not match real departure")
	}
}

func TestUncatchableReferenceChainAbandonsOnceAndKeepsRoomBudget(t *testing.T) {
	keys := NewRoomKeys()
	keys.Request(epoch, 1, Low, CauseJoin)
	keys.Due(epoch)
	keys.OnKey(epoch, Low, 1000, epoch)
	v := NewViewer(1, keys)
	v.started = true
	v.OnApplied(epoch, Option{Native, 0})
	v.Est = &FixedEstimator{Rate: 128000}
	v.Probe = NoProber{}
	q := v.Pacer.(*Queue)
	q.SetRate(epoch, 5_000_000)
	serviceFrame(q, epoch, 1, true, 0, 2, 1200, epoch.Add(FrameDeadline), epoch.Add(KeyEnvelope))
	serviceFrame(q, epoch, 2, false, 0, 130, 1231, epoch.Add(10*time.Millisecond), epoch.Add(FrameDeadline))
	// Complete the key only. Its base keeps a reference lease despite its
	// old freshness boundary; a later collapse makes that closure impossible.
	for at := epoch; !q.chains[Native].completed; {
		if _, ok := q.Pop(at); !ok {
			at = q.NextAt(at)
		}
	}
	q.KeyEvents()
	now := epoch.Add(20 * time.Millisecond)
	q.SetRate(now, 128000)
	q.Maintain(now, 0)
	if !q.chains[Native].broken || len(q.frames) != 0 {
		t.Fatal("known impossible base was retained")
	}
	q.Maintain(now.Add(time.Millisecond), 0)
	if drops := q.drops; len(drops) != 1 || drops[0].TID != 0 {
		t.Fatalf("abandonment not coalesced: %+v", drops)
	}
	if d := v.Step(now, Costs{Rate: [2][3]int{{1000000, 1400000, 2000000}, {50000, 70000, 100000}}}); d.Desired.Rendition != Low {
		t.Fatal("broken reference did not recover low")
	}
	if due := keys.Due(now); len(due) != 0 {
		t.Fatal("freshness recovery bypassed room budget")
	}
	if due := keys.Due(epoch.Add(KeyInterval)); len(due) != 1 || due[0] != Low {
		t.Fatal("recovery lost its bounded low request")
	}
}

func TestCancellationCannotClearAnotherRenditionsSuppression(t *testing.T) {
	k := NewRoomKeys()
	k.Request(epoch, 1, Low, CauseJoin)
	k.OnDelivery(1, KeyEvent{Rendition: Low, Frame: 7, Phase: KeyAdmitted, At: epoch})
	k.OnDelivery(1, KeyEvent{Rendition: Native, Frame: 7, Phase: KeyCancelled, At: epoch.Add(time.Millisecond)})
	if k.Request(epoch.Add(2*time.Millisecond), 1, Low, CauseRecovery) != KeyIgnored {
		t.Fatal("foreign cancellation cleared active guard")
	}
	k.OnDelivery(1, KeyEvent{Rendition: Low, Frame: 7, Phase: KeyCompleted, At: epoch.Add(10 * time.Millisecond)})
	if k.Request(epoch.Add(100*time.Millisecond), 1, Low, CauseRecovery) != KeyIgnored {
		t.Fatal("completed tail guard changed")
	}
}
