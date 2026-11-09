package ctrl

import (
	"reflect"
	"testing"
	"time"
)

func resetLow(q *Queue, at time.Time, count, size int) {
	for i := range count {
		if !q.Push(at, Packet{ID: 9000 + uint64(i), Kind: KindVideo, Class: ClassKey, Rendition: Low, Frame: 7, Key: true, Size: size, FrameSize: count * size, Packets: count, Last: i == count-1, FreshUntil: at.Add(FrameDeadline), Deadline: at.Add(KeyEnvelope)}) {
			panic("Low reset fixture exceeds bounds")
		}
	}
}

func resetQueue(t *testing.T, partialKey bool) (*Queue, time.Time) {
	t.Helper()
	q := &Queue{}
	q.SetRate(epoch, 128000)
	if !partialKey {
		serviceFrame(q, epoch, 1, true, 0, 1, 100, epoch.Add(FrameDeadline), epoch.Add(KeyEnvelope))
		q.Pop(epoch)
	}
	serviceFrame(q, epoch, 2, partialKey, 0, 24, 1000, epoch.Add(FrameDeadline), epoch.Add(KeyEnvelope))
	if p, ok := q.Pop(epoch); !ok || p.Frame != 2 {
		t.Fatal("old Native picture did not start")
	}
	// A known dependent closure must leave with the superseded picture.
	serviceFrame(q, epoch, 3, false, 0, 2, 500, epoch.Add(FrameDeadline), epoch.Add(KeyEnvelope))
	return q, epoch.Add(100 * time.Millisecond)
}

func TestRequiredLowResetCommitsAtFirstDeparture(t *testing.T) {
	for _, key := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial-delta", true: "partial-key"}[key], func(t *testing.T) {
			q, at := resetQueue(t, key)
			var released []Packet
			q.SetRelease(func(p Packet) { released = append(released, p) })
			resetLow(q, at, 2, 500)
			q.SetCommit(Low, false)
			old := q.originals[frameID{Native, 2}]
			auth, _ := q.Key(Low)
			auth.Option = Option{Low, 2}
			auth.Intent = Required
			forecast := q.PictureService(at, PictureQuery{Rendition: Low, Frame: 7, Commit: true, Authorization: &auth})
			if !forecast.Known || forecast.Key != at || forecast.Broken || len(released) != 0 || q.originals[old.id] != old || old.remaining != 23 {
				t.Fatalf("forecast mutated old work or lost reset service: %+v", forecast)
			}
			if q.requiredLow() {
				t.Fatal("admission alone armed the reset")
			}
			authorizeReset(q)
			if len(released) != 0 {
				t.Fatal("authorization alone purged playback")
			}
			p, ok := q.Pop(at)
			if !ok || p.Rendition != Low || !p.Key || p.Last {
				t.Fatalf("required reset did not depart first: %+v", p)
			}
			if active, known := q.Active(); !known || active != Low || q.originals[old.id] != nil || len(released) != 25 {
				t.Fatalf("old picture/closure survived boundary: active%v/%v released%d", active, known, len(released))
			}
			for _, drop := range released {
				if !drop.Superseded || drop.Rendition != Native {
					t.Fatalf("intentional retirement became broken recovery: %+v", drop)
				}
			}
			p, ok = q.Pop(at)
			if !ok || !p.Last || p.Rendition != Low || len(q.Dropped()) != 0 {
				t.Fatal("Low did not finish or false CauseBroken")
			}
			terminals := 0
			for _, e := range q.KeyEvents() {
				if e.Phase == KeySuperseded {
					terminals++
					if e.Rendition != Native || e.Frame != 2 || e.At != at {
						t.Fatalf("wrong retired delivery: %+v", e)
					}
				}
			}
			if key && terminals != 1 || !key && terminals != 0 {
				t.Fatalf("terminal key ownership count %d", terminals)
			}
		})
	}
}

func TestRequiredLowResetUsesActualOwnershipAndPhysicalService(t *testing.T) {
	for _, arm := range []string{"pending-writer", "insufficient-service", "expired-key", "stale-desire", "ready"} {
		t.Run(arm, func(t *testing.T) {
			q, at := resetQueue(t, true)
			v := NewViewer(1, NewRoomKeys())
			v.Pacer, v.Est, v.Probe = q, &FixedEstimator{Rate: 128000}, NoProber{}
			// The first Native packet left, but its LAST has not applied Native.
			v.OnApplied(epoch, Option{Low, 2})
			count := 2
			if arm == "insufficient-service" {
				count = 100 // beyond the unchanged 2s lease at this actual rate
			}
			resetLow(q, at, count, 500)
			if arm == "expired-key" {
				q.originals[frameID{Low, 7}].deadline = at
			}
			pending, desired := 0, Option{Low, 2}
			if arm == "pending-writer" {
				pending = 3
			}
			if arm == "stale-desire" {
				desired = Option{Native, 0}
			}
			v.preparation.choice = &Allocation{Desired: desired, Intent: Required}
			v.preparation.generation = v.Generation()
			v.BeforeSend(at, pending, desired, Costs{})
			if q.originals[frameID{Native, 2}] == nil {
				t.Fatal("old payload erased before reset departure")
			}
			p, ok := q.Pop(at)
			if !ok {
				t.Fatal("no next packet")
			}
			if (p.Rendition == Low) != (arm == "ready") {
				t.Fatalf("reset without current physical service or ignored actual ownership: %+v", p)
			}
			if arm == "pending-writer" {
				v.BeforeSend(at, 0, desired, Costs{})
				if !q.requiredLow() {
					t.Fatal("completed physical batch did not release reset")
				}
			}
		})
	}
}

func TestRequiredResetForecastMatchesPopWithProtectionAndDebt(t *testing.T) {
	for trial := 0; trial < 60; trial++ {
		q, at := resetQueue(t, trial%2 == 0)
		resetLow(q, at, 4, 500)
		authorizeReset(q)
		q.tokens = float64(-trial * 17)
		if trial%3 == 0 {
			q.SetCluster(&Cluster{Start: at, Until: at.Add(20 * time.Millisecond), Rate: 512000})
		}
		for _, p := range []Packet{
			{Kind: KindAudio, Class: ClassAudio, Size: 100, Last: true},
			{Kind: KindVideo, Class: ClassRTX, Rendition: Native, Frame: 2, Size: 100, Last: true},
			{Kind: KindVideo, Class: ClassFEC, ProtectedClass: ClassKey, Rendition: Native, Frame: 2, Size: 100, Last: true},
		} {
			q.Push(at, p)
		}
		query := PictureQuery{Rendition: Low, Frame: 7, Commit: true}
		forecast := q.PictureService(at, query)
		actual := actualPictureService(q.snapshot(), at, query)
		if !reflect.DeepEqual(forecast, actual) {
			t.Fatalf("trial%d forecast%+v actual%+v", trial, forecast, actual)
		}
	}
}

func TestSupersededDeliveryIsBoundedAndRenditionFrameFenced(t *testing.T) {
	q := &Queue{}
	q.event(KeyEvent{Rendition: Native, Frame: 1, Phase: KeyAborted})
	for id := uint64(2); id < 10000; id++ {
		for _, phase := range []KeyPhase{KeyAdmitted, KeyStarted, KeySuperseded} {
			q.event(KeyEvent{Rendition: Native, Frame: id, Phase: phase})
		}
		if len(q.events) > 4 {
			t.Fatal("unbounded retired delivery events")
		}
	}
	if q.events[0].Phase != KeyAborted {
		t.Fatal("intentional supersession hid genuine earlier abort")
	}
	k := NewRoomKeys().(*RoomKeys)
	k.OnDelivery(1, KeyEvent{Rendition: Native, Frame: 2, Phase: KeyStarted})
	for _, wrong := range []KeyEvent{{Rendition: Low, Frame: 2, Phase: KeySuperseded}, {Rendition: Native, Frame: 1, Phase: KeySuperseded}} {
		k.OnDelivery(1, wrong)
		if !k.delivery[1].active {
			t.Fatal("wrong terminal event cleared live delivery")
		}
	}
	k.OnDelivery(1, KeyEvent{Rendition: Native, Frame: 2, Phase: KeySuperseded})
	if k.delivery[1].active || !k.delivery[1].until.IsZero() {
		t.Fatal("matching retired delivery kept active/tail ownership")
	}
	k.OnDelivery(1, KeyEvent{Rendition: Low, Frame: 2, Phase: KeyStarted})
	k.OnDelivery(1, KeyEvent{Rendition: Native, Frame: 2, Phase: KeySuperseded})
	if !k.delivery[1].active || k.delivery[1].rendition != Low {
		t.Fatal("late old terminal event erased replacement ownership")
	}
}

type resetDeliveryKeys struct {
	Keys
	events   []KeyEvent
	requests int
}

func (k *resetDeliveryKeys) OnDelivery(id int, e KeyEvent) {
	k.events = append(k.events, e)
	k.Keys.OnDelivery(id, e)
}

func (k *resetDeliveryKeys) Request(at time.Time, id int, r Rendition, c Cause) KeyStatus {
	k.requests++
	return k.Keys.Request(at, id, r, c)
}

func TestRequiredResetRetiresDeliveryWithoutRecoveryOrRefusal(t *testing.T) {
	for _, purged := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued-partial", true: "prior-drain-released-payload"}[purged], func(t *testing.T) {
			q, at := resetQueue(t, true)
			keys := &resetDeliveryKeys{Keys: NewRoomKeys()}
			v := NewViewer(1, keys)
			v.Pacer, v.Est, v.Probe, v.Alloc = q, &FixedEstimator{Rate: 128000}, NoProber{}, ReferenceAllocator{}
			v.started = true
			v.OnApplied(epoch, Option{Low, 2})
			costs := Costs{Rate: [2][3]int{{1000000, 1600000, 2000000}, {10000, 20000, 30000}}}
			v.Step(at, costs)
			if !v.delivering[Native] {
				t.Fatal("old partial key never acquired delivery ownership")
			}
			if purged {
				q.Purge(Native)
			}
			resetLow(q, at, 2, 500)
			requests := keys.requests
			v.BeforeSend(at, 0, Option{Low, 2}, costs)
			for i := 0; i < 2; i++ {
				if p, ok := q.Pop(at); !ok || p.Rendition != Low {
					t.Fatal("required Low did not reset")
				}
			}
			for i := 0; i < 3; i++ {
				v.Step(at.Add(time.Duration(i)*time.Millisecond), costs)
			}
			terminals := 0
			for _, e := range keys.events {
				if e.Phase == KeySuperseded && e.Frame == 2 && e.Rendition == Native {
					terminals++
				}
			}
			if terminals != 1 || v.delivering[Native] || v.switching || keys.requests != requests || v.refusedFor != 0 || !v.recoveryAt.IsZero() {
				t.Fatalf("retirement leaked ownership or invented recovery: terminals%d native%v switching%v requests%d→%d refused%v recovery%v", terminals, v.delivering[Native], v.switching, requests, keys.requests, v.refusedFor, v.recoveryAt)
			}
			delivery := keys.Keys.(*RoomKeys).delivery[1]
			if delivery.active || delivery.rendition != Low || delivery.frame != 7 || delivery.until != at.Add(KeyEnvelope) {
				t.Fatalf("old event erased Low completed-tail guard: %+v", delivery)
			}
		})
	}
}

// Queue-only fixtures supply the explicit Required authorization whose
// acquisition/selection is covered independently by low_demand and intent tests.
func authorizeReset(q *Queue) {
	a, ok := q.Key(Low)
	if !ok {
		panic("missing reset fixture")
	}
	a.Intent, a.Option = Required, Option{Low, 2}
	if !q.Authorize(a) {
		panic("invalid reset fixture")
	}
}
