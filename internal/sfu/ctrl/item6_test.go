package ctrl

import (
	"testing"
	"time"
)

// Regressions for C2 item 6a, "keys and timing" (docs/REPORT.md). Each
// names the Tier A row or diagnosis that exposed it.

// With the sender's key boost the encoder no longer pauses after a key, so
// base pictures captured while a slow key is delivered wait behind it for
// longer than FrameDeadline. Their deadline clock starts when the key
// completes, as FrameAge's does: dropping them breaks the chain the key just
// started and costs a recovery key (A4k b1_3m_560k, A18 native_1500k after
// the boost model).
func TestBaseBehindKeyKeepsDeadlineUntilKeyCompletes(t *testing.T) {
	p := &Queue{}
	var released []Packet
	p.SetRelease(func(k Packet) { released = append(released, k) })
	const keyBytes = 200_000
	p.SetRate(epoch, keyBytes*8) // the key takes one second
	key := Packet{ID: 1, Size: keyBytes / 100, FrameSize: keyBytes, Packets: 100, Kind: KindVideo, Class: ClassKey, Key: true, Frame: 1, Rendition: Native, Deadline: epoch.Add(KeyEnvelope)}
	for i := range 100 {
		key.ID, key.Last = uint64(1+i), i == 99
		p.Push(epoch, key)
	}
	tl0 := Packet{ID: 200, Size: 1_000, FrameSize: 1_000, Packets: 1, Last: true, Kind: KindVideo, Class: ClassTL0, Frame: 2, Rendition: Native, Deadline: epoch.Add(FrameDeadline)}
	p.Push(epoch.Add(10*time.Millisecond), tl0)
	var sent []Packet
	for at := epoch; at.Before(epoch.Add(1500 * time.Millisecond)); at = at.Add(5 * time.Millisecond) {
		// The writer stalls for 300 ms: the key completes after the base
		// picture's original deadline.
		if at.After(epoch.Add(400*time.Millisecond)) && at.Before(epoch.Add(700*time.Millisecond)) {
			continue
		}
		for {
			k, ok := p.Pop(at)
			if !ok {
				break
			}
			sent = append(sent, k)
		}
	}
	if len(released) != 0 {
		t.Fatalf("released %d packets; the base picture behind the key must not expire", len(released))
	}
	if n := len(sent); n != 101 || sent[n-1].Frame != 2 {
		t.Fatalf("sent %d packets", n)
	}
}

// An accepted recovery is an immediate switch intent: OnDecoderStall says
// so, and a Step run at once wants low (A8 outage). An ignored one does not.
func TestAcceptedRecoveryWantsLowAtOnce(t *testing.T) {
	v := NewViewer(1, NewRoomKeys())
	c := Costs{Rate: [2][3]int{{1_000_000, 1_500_000, 2_000_000}, {50_000, 70_000, 100_000}}}
	v.Step(epoch, c)
	v.OnApplied(epoch, Option{Native, 2})
	at := epoch.Add(10 * time.Second)
	if !v.OnDecoderStall(at) {
		t.Fatal("recovery not accepted")
	}
	if d := v.Step(at, c); d.Desired.Rendition != Low {
		t.Fatalf("desired %v right after an accepted recovery", d.Desired)
	}
	if v.OnDecoderStall(at.Add(100 * time.Millisecond)) {
		t.Fatal("a debounced recovery reported a switch intent")
	}
}

// Delivery facts reach Keys when the forwarder steps. An admission older
// than the viewer's request does not serve it: stepping at once after a
// recovery must not erase the request it just made.
func TestOlderAdmissionDoesNotServeANewerRequest(t *testing.T) {
	k := &RoomKeys{}
	k.Request(epoch, 1, Low, CauseJoin)
	k.Due(epoch)
	k.OnKey(epoch.Add(50*time.Millisecond), Low, 5_000, epoch.Add(20*time.Millisecond))
	k.Request(epoch.Add(time.Second), 1, Low, CauseBroken)
	k.OnDelivery(1, KeyEvent{Rendition: Low, Frame: 1, Phase: KeyAdmitted, At: epoch.Add(50 * time.Millisecond)})
	if due := k.Due(epoch.Add(50*time.Millisecond + KeyInterval)); len(due) != 1 || due[0] != Low {
		t.Fatalf("the newer request was dropped: due %v", due)
	}
}

// The replacement low-key exception (C2 review §5, A19 pc_replacement): a
// genuine replacement handed off from an admitted session gets a low key at
// once even though the room's low budget is spent; an initial admission
// does not. The credit lives at the session: a second replacement within
// KeyInterval waits, as does another session within KeyRetry (it shares the
// exception key), and an outstanding low PLI suppresses it.
func TestReplacementLowKeyException(t *testing.T) {
	c := Costs{Rate: [2][3]int{{1_000_000, 1_500_000, 2_000_000}, {50_000, 70_000, 100_000}}}
	keys := &RoomKeys{}
	first := NewViewer(1, keys)
	first.Step(epoch, c) // join: low PLI
	if due := keys.Due(epoch); len(due) != 1 || due[0] != Low {
		t.Fatal("join PLI", due)
	}
	keys.OnKey(epoch.Add(30*time.Millisecond), Low, 5_000, epoch.Add(10*time.Millisecond))
	at := epoch.Add(time.Second) // the low budget is spent until 3.03 s

	fresh := NewViewer(2, keys) // a new admission: no credit
	if d := fresh.Step(at, c); d.Key != KeyPending || len(keys.Due(at)) != 0 {
		t.Fatalf("an initial admission got %v", d.Key)
	}
	state := first.Handoff(at)
	if state.Session != 1 {
		t.Fatalf("handoff session %d", state.Session)
	}
	keys.Leave(1)
	repl := NewViewerFrom(1001, keys, state)
	if d := repl.Step(at, c); d.Key != KeyAccepted || d.Desired.Rendition != Low {
		t.Fatalf("replacement join %v %v", d.Key, d.Desired)
	}
	if due := keys.Due(at); len(due) != 1 || due[0] != Low {
		t.Fatal("no exception PLI", due)
	}
	if kt := repl.KeyTimes(Low); !kt.Exception || !kt.Requested.Equal(at) || !kt.Emitted.Equal(at) {
		t.Fatalf("key times %+v", kt)
	}
	// Another session's replacement within KeyRetry shares that key.
	other := NewViewerFrom(1002, keys, RateState{Rate: 1_000_000, Session: 2})
	if d := other.Step(at.Add(100*time.Millisecond), c); d.Key == KeyAccepted || len(keys.Due(at.Add(100*time.Millisecond))) != 0 {
		t.Fatal("room cap: a second exception PLI within KeyRetry")
	}
	keys.OnKey(at.Add(130*time.Millisecond), Low, 5_000, at.Add(110*time.Millisecond))
	if _, ok := keys.pending[1002]; ok {
		t.Fatal("the coalesced replacement was not served by the exception key")
	}
	// The same session again within KeyInterval: no second credit.
	keys.Leave(1001)
	again := NewViewerFrom(2001, keys, repl.Handoff(at.Add(1500*time.Millisecond)))
	if d := again.Step(at.Add(1500*time.Millisecond), c); d.Key == KeyAccepted || len(keys.Due(at.Add(1500*time.Millisecond))) != 0 {
		t.Fatal("a session spent its credit twice within KeyInterval")
	}
	if n := len(keys.Exceptions()); n != 1 {
		t.Fatalf("%d exceptions", n)
	}
	// After KeyInterval the session has credit again, unless a low PLI is
	// outstanding.
	later := at.Add(KeyInterval + 2*time.Second)
	keys.Request(later, 3, Low, CauseJoin)
	keys.Due(later)
	keys.Leave(2001)
	if s := keys.Replace(later.Add(10*time.Millisecond), 3001, 1); s == KeyAccepted {
		t.Fatal("exception despite an outstanding low PLI")
	}
	keys.OnKey(later.Add(30*time.Millisecond), Low, 5_000, later.Add(20*time.Millisecond))
	if s := keys.Replace(later.Add(KeyRetry), 3002, 1); s != KeyAccepted {
		t.Fatalf("credit not restored after KeyInterval: %v", s)
	}
}

// Eligibility is independent of service preparation. Real Viewer tests own
// the bounded measurement outcome; allocator idleness is not a capacity proof.
func TestNativeEligibilityIsIndependentOfServicePreparation(t *testing.T) {
	c := Costs{Rate: [2][3]int{{1_000_000, 1_400_000, 2_000_000}, {50_000, 70_000, 100_000}}, KeyBytes: [2]int{251_000, 9_000}, KeySamples: [2]int{8, 8}}
	in := func(ceiling int) AllocInput {
		return AllocInput{Estimate: Estimate{Rate: 1_400_000, Proven: 1_400_000, Ceiling: ceiling}, Costs: c, Applied: Option{Low, 2}, NativeKey: true}
	}
	for _, tc := range []struct {
		discovering bool
		ceiling     int
		want        Rendition
	}{{true, 0, Native}, {false, 0, Native}, {true, 1_400_000, Native}} {
		a := &LayerAllocator{}
		a.OnApplied(epoch, Option{Low, 2})
		if d := a.Decide(epoch, in(tc.ceiling)); d.Desired.Rendition != tc.want {
			t.Fatalf("discovering %v ceiling %d: desired %v", tc.discovering, tc.ceiling, d.Desired)
		}
	}
	// A key already requested is not withdrawn while discovery climbs.
	a := &LayerAllocator{}
	a.OnApplied(epoch, Option{Low, 2})
	x := in(0)
	x.Switching = true
	if d := a.Decide(epoch, x); d.Desired.Rendition != Native {
		t.Fatalf("a requested switch was dropped: %v", d.Desired)
	}
}

// A shed picture takes its queued dependants with it, and continuity hears
// of each (A17 tl1_loss seed 20: FrameAge shed a TL1 whose TL2 was already
// queued, and the TL2 went out undecodable). A later base picture and what
// references it stay.
func TestShedPictureTakesQueuedDependants(t *testing.T) {
	p := &Queue{}
	var released []Packet
	p.SetRelease(func(k Packet) { released = append(released, k) })
	p.SetRate(epoch, 1) // nothing departs
	p.tokens = 0        // not even one packet larger than the burst cap
	push := func(frame uint64, tid uint8, at time.Duration) {
		p.Push(epoch.Add(at), Packet{ID: frame, Size: 2 * BurstFloor, Kind: KindVideo, Class: ClassTL0 + Class(tid), Rendition: Native, Frame: frame, TID: tid, Last: true, Packets: 1, FrameSize: 2 * BurstFloor, Deadline: epoch.Add(at + FrameDeadline)})
	}
	push(1, 0, 0)
	push(2, 1, 0)                   // shed by FrameAge at 110 ms
	push(3, 2, 70*time.Millisecond) // references 2: must go with it
	push(4, 0, 75*time.Millisecond) // references 1: stays
	push(5, 2, 80*time.Millisecond) // references 4: stays
	p.Pop(epoch.Add(110 * time.Millisecond))
	var got []uint64
	for _, k := range released {
		got = append(got, k.Frame)
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("released %v, want the shed TL1 2 and its TL2 3", got)
	}
	if !released[1].Last || released[1].Superseded {
		t.Fatal("the dependant must be reported to continuity as lost")
	}
	if _, n := p.Queued(); n != 3 {
		t.Fatalf("%d frames queued, want 1, 4 and 5", n)
	}
}

// An aborted key takes the pictures held behind it (they reference it), so
// nothing is left blocked behind a key that will never complete.
func TestAbortedKeyTakesBlockedPictures(t *testing.T) {
	p := &Queue{}
	var released []Packet
	p.SetRelease(func(k Packet) { released = append(released, k) })
	p.SetRate(epoch, 1)
	p.Push(epoch, Packet{ID: 1, Size: 1000, FrameSize: 1000, Packets: 1, Last: true, Kind: KindVideo, Class: ClassKey, Key: true, Frame: 1, Rendition: Native, Deadline: epoch.Add(KeyEnvelope)})
	p.Push(epoch, Packet{ID: 2, Size: 1000, FrameSize: 1000, Packets: 1, Last: true, Kind: KindVideo, Class: ClassTL0, Frame: 2, Rendition: Native, Deadline: epoch.Add(FrameDeadline)})
	p.Pop(epoch.Add(KeyEnvelope))
	if b, n := p.Queued(); n != 0 || b != 0 || len(released) != 2 {
		t.Fatalf("queued %d B / %d frames, released %d", b, n, len(released))
	}
}
