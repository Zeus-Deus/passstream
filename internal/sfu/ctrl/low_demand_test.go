package ctrl

import (
	"testing"
	"time"
)

// Reconstructed owning input from the read-only FQ boundary; not a source or
// gate replay. Queue has real first departures; Allocator and RoomKeys are real.
func TestRequiredLowDemandUsesDepartureOwner(t *testing.T) {
	for _, arm := range []string{"partial-native-applied-low", "native-refresh-applied-native", "native-refresh-room-budget-pending", "unstarted-optional-native", "partial-native-room-budget-pending"} {
		t.Run(arm, func(t *testing.T) {
			now := epoch.Add(10 * time.Second)
			keys := NewRoomKeys().(*RoomKeys)
			keys.Request(now.Add(-4*time.Second), 1, Low, CauseJoin)
			keys.Due(now.Add(-4 * time.Second))
			keys.OnKey(now.Add(-4*time.Second), Low, 4470, now.Add(-4*time.Second))
			v := NewViewer(1, keys)
			v.started = true
			v.requested = Native
			v.requestAt = now.Add(-550 * time.Millisecond)
			v.preparation = switchPreparation{at: now.Add(-550 * time.Millisecond), outcome: SwitchUnmeasured}
			c := Costs{Rate: [2][3]int{{52872, 108276, 213462}, {56086, 79209, 112256}}, KeyBytes: [2]int{9640, 4470}, KeySamples: [2]int{4, 4}, AudioWire: 65198}
			v.Est = &commitEstimate{Estimate{Rate: 129212, Proven: 152015, Sustained: 101895, Overuse: true, RTT: 42718750 * time.Nanosecond, CongestedAt: now, LostAt: now.Add(-3550 * time.Millisecond)}}
			q := v.Pacer.(*Queue)
			q.SetRate(now.Add(-time.Second), 166278)
			first := Low
			if arm == "native-refresh-applied-native" || arm == "native-refresh-room-budget-pending" {
				first = Native
			}
			q.Push(now.Add(-time.Second), Packet{Kind: KindVideo, Class: ClassKey, Rendition: first, Frame: 1, Key: true, Size: 100, FrameSize: 100, Packets: 1, Last: true, Deadline: now.Add(time.Second)})
			if p, ok := q.Pop(now.Add(-time.Second)); !ok || !p.Last {
				t.Fatal("initial owner did not complete")
			}
			q.KeyEvents()
			v.OnApplied(now.Add(-time.Second), Option{first, 0})
			for i := 0; i < 8; i++ {
				q.Push(now.Add(-300*time.Millisecond), Packet{Kind: KindVideo, Class: ClassKey, Rendition: Native, Frame: 314, Key: true, Size: 1200, FrameSize: 9600, Packets: 8, Last: i == 7, Deadline: now.Add(1700 * time.Millisecond)})
			}
			if arm != "unstarted-optional-native" {
				if p, ok := q.Pop(now.Add(-300 * time.Millisecond)); !ok || p.Last {
					t.Fatal("native did not become partial")
				}
			}
			if arm == "partial-native-room-budget-pending" || arm == "native-refresh-room-budget-pending" {
				keys.OnKey(now.Add(-time.Second), Low, 4470, now.Add(-time.Second))
			}
			// Positive control is a completed Native decoder with a genuine persistent
			// Native hold excess; the real allocator's ordinary down timer expires.
			if arm == "native-refresh-applied-native" || arm == "native-refresh-room-budget-pending" {
				c.Rate[Native] = [3]int{200000, 220000, 240000}
				v.Alloc.Decide(now.Add(-TrendWindow), AllocInput{Estimate: v.Est.(*commitEstimate).value, Costs: c, Applied: v.Applied()})
			}
			d := v.Step(now, c)
			active, known := q.Active()
			t.Logf("active=%v/%v applied=%+v desired=%+v request=%v status=%v nativeDelivering=%v lowDelivering=%v lowPending=%v", active, known, v.Applied(), d.Desired, d.Requested, d.Key, v.delivering[Native], v.delivering[Low], keys.pending[1])
			if d.Desired.Rendition != Low {
				t.Fatal("not the real controller Low desire")
			}
			if arm == "unstarted-optional-native" {
				if d.Requested {
					t.Error("intact Low manufactured reset request")
				}
				return
			}
			if !d.Requested {
				t.Error("required Low key demand suppressed by lagging Applied despite actual Native departure")
				return
			}
			if d.Key != KeyAccepted && d.Key != KeyPending {
				t.Error("required reset escaped ordinary room statuses")
			}
			if (arm == "partial-native-room-budget-pending" || arm == "native-refresh-room-budget-pending") && d.Key != KeyPending {
				t.Error("required reset bypassed room budget")
			}
		})
	}
}

func lowDemandState(t *testing.T) (*Viewer, *RoomKeys, Costs, time.Time) {
	t.Helper()
	now := epoch.Add(10 * time.Second)
	keys := NewRoomKeys().(*RoomKeys)
	keys.Request(now.Add(-4*time.Second), 1, Low, CauseJoin)
	keys.Due(now.Add(-4 * time.Second))
	keys.OnKey(now.Add(-4*time.Second), Low, 4470, now.Add(-4*time.Second))
	v := NewViewer(1, keys)
	v.started, v.requested, v.requestAt = true, Native, now.Add(-550*time.Millisecond)
	v.preparation = switchPreparation{at: v.requestAt, outcome: SwitchUnmeasured}
	c := Costs{Rate: [2][3]int{{52872, 108276, 213462}, {56086, 79209, 112256}}, KeyBytes: [2]int{9640, 4470}, KeySamples: [2]int{4, 4}, AudioWire: 65198}
	v.Est = &commitEstimate{Estimate{Rate: 129212, Proven: 152015, Sustained: 101895, Overuse: true, RTT: 42718750 * time.Nanosecond, CongestedAt: now}}
	q := v.Pacer.(*Queue)
	q.SetRate(now.Add(-time.Second), 166278)
	if !q.Push(now.Add(-time.Second), Packet{Kind: KindVideo, Class: ClassKey, Rendition: Low, Frame: 1, Key: true, Size: 100, FrameSize: 100, Packets: 1, Last: true, Deadline: now.Add(time.Second)}) {
		t.Fatal("initial Low fixture")
	}
	if p, ok := q.Pop(now.Add(-time.Second)); !ok || !p.Last {
		t.Fatal("initial Low did not depart")
	}
	q.KeyEvents()
	v.OnApplied(now.Add(-time.Second), Option{Low, 0})
	serviceFrame(q, now.Add(-300*time.Millisecond), 314, true, 0, 8, 1200, now.Add(FrameDeadline), now.Add(1700*time.Millisecond))
	if p, ok := q.Pop(now.Add(-300 * time.Millisecond)); !ok || p.Last {
		t.Fatal("Native did not start")
	}
	return v, keys, c, now
}

func TestRequiredLowDemandPreservesAcquisitionBounds(t *testing.T) {
	for _, arm := range []string{"available", "pending", "already-delivering-low", "retry-pending", "undeliverable"} {
		t.Run(arm, func(t *testing.T) {
			v, keys, costs, now := lowDemandState(t)
			q := v.Pacer.(*Queue)
			if arm == "pending" {
				keys.OnKey(now.Add(-time.Second), Low, 4470, now.Add(-time.Second))
			}
			if arm == "already-delivering-low" {
				resetLow(q, now, 8, 500)
				q.SetCommit(Low, false)
			}
			if arm == "retry-pending" {
				keys.Request(now.Add(-KeyRetry/2), 1, Low, CauseUpgrade)
				v.requested, v.requestAt = Low, now.Add(-KeyRetry/2)
			}
			if arm == "undeliverable" {
				v.Est.(*commitEstimate).value.Service = 128000
				costs.KeyBytes[Low] = 30000
			}
			d := v.Step(now, costs)
			wantRequest := arm == "available" || arm == "pending"
			if d.Desired.Rendition != Low || d.Requested != wantRequest || v.Applied() != (Option{Low, 0}) {
				t.Fatalf("actual ownership bypassed desire/delivery/retry/service bounds: %+v", d)
			}
			if wantRequest {
				want := KeyAccepted
				if arm == "pending" {
					want = KeyPending
				}
				if d.Key != want || keys.pending[1].rendition != Low || keys.pending[1].at != now {
					t.Fatal("required reset bypassed normal room status/coalescing")
				}
			}
			requestedAt, pending := v.requestAt, keys.pending[1]
			for i := 1; i <= 3; i++ {
				if d := v.Step(now.Add(time.Duration(i)*OveruseTime), costs); d.Requested {
					t.Fatal("repeated Step spent an extra request inside KeyRetry")
				}
			}
			oldGeneration := v.Generation()
			v.OnPathChange(now.Add(4 * OveruseTime))
			if d := v.Step(now.Add(4*OveruseTime), costs); d.Requested || v.requestAt != requestedAt || keys.pending[1] != pending {
				t.Fatal("path change reset retry/pending ownership")
			}
			if service := v.PictureService(now, PictureQuery{Rendition: Native, Frame: 314, Generation: oldGeneration}); service.Known {
				t.Fatal("old-generation service survived path fence")
			}
			if v.refusedFor != 0 || !v.recoveryAt.IsZero() || len(keys.asked) != 0 || len(keys.recovery) != 0 {
				t.Fatal("ordinary Low acquisition manufactured refusal/broken recovery")
			}
			due := keys.Due(now.Add(4 * OveruseTime))
			if (len(due) == 1 && due[0] == Low) != (arm == "available" || arm == "retry-pending") || len(keys.Due(now.Add(4*OveruseTime))) != 0 {
				t.Fatal("room budget/duplicate PLI behavior changed")
			}
			if arm == "pending" {
				if retry := v.Step(now.Add(KeyRetry), costs); !retry.Requested || retry.Key != KeyPending || keys.pending[1] != pending || len(keys.Due(now.Add(KeyRetry))) != 0 {
					t.Fatal("normal retry bypassed room budget or moved original demand")
				}
				if due := keys.Due(now.Add(2 * time.Second)); len(due) != 1 || due[0] != Low {
					t.Fatal("blocked demand lost eventual ordinary budget service")
				}
			}
		})
	}
}

func TestRequiredLowDemandCancelsUncommittedStaleDesire(t *testing.T) {
	v, keys, costs, now := lowDemandState(t)
	d := v.Step(now, costs)
	if !d.Requested || d.Key != KeyAccepted {
		t.Fatal("no ordinary Low demand")
	}
	// The requested key arrives, but new current proof now admits Native.
	// This exercises real allocation, rather than forcing a desired option.
	resetLow(v.Pacer.(*Queue), now, 8, 500)
	v.Est.(*commitEstimate).value = Estimate{Rate: 1000000, Proven: 1000000, Sustained: 1000000}
	d = v.Step(now.Add(OveruseTime), costs)
	if d.Desired.Rendition != Native || d.Requested {
		t.Fatalf("not a real stale-Low-desire control: %+v", d)
	}
	v.BeforeSend(now.Add(OveruseTime), 0, d.Desired, costs)
	if v.Pacer.UncommittedKey(Low) || v.activeRendition() != Native || v.Applied().Rendition != Low {
		t.Fatal("stale request/arrival reset the current Native decoder")
	}
	v.Step(now.Add(2*OveruseTime), costs)
	if v.delivering[Low] || v.refusedFor != 0 || len(keys.recovery) != 0 {
		t.Fatal("cancelled stale key leaked delivery/refusal/broken ownership")
	}
}
