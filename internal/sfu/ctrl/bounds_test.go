package ctrl

import (
	"testing"
	"time"
)

// C2-1: key-delivery facts coalesce per rendition without losing what Step
// and Keys.OnDelivery derive: the latest key's phases and an earlier abort.
func TestKeyEventsCoalesceKeepingTheLatestKeyAndAbort(t *testing.T) {
	p := &Queue{}
	key := func(frame uint64) Packet {
		return Packet{ID: frame, Size: 1000, FrameSize: 1000, Packets: 1, Last: true, Kind: KindVideo, Class: ClassKey, Key: true, Frame: frame, Rendition: Native, Deadline: epoch.Add(KeyEnvelope)}
	}
	p.SetRate(epoch, 1)
	p.Push(epoch, key(1))
	p.Pop(epoch.Add(KeyEnvelope)) // aborted at its deadline
	for f := uint64(2); f < 100; f++ {
		p.Push(epoch.Add(KeyEnvelope), key(f)) // each supersedes the last
	}
	p.Push(epoch.Add(KeyEnvelope), Packet{ID: 500, Size: 100, FrameSize: 100, Packets: 1, Last: true, Kind: KindVideo, Class: ClassKey, Key: true, Frame: 7, Rendition: Low})
	ev := p.KeyEvents()
	want := []KeyEvent{{Native, 1, KeyAborted, epoch.Add(KeyEnvelope), 0}, {Native, 99, KeyAdmitted, epoch.Add(KeyEnvelope), 0}, {Low, 7, KeyAdmitted, epoch.Add(KeyEnvelope), 0}}
	if len(ev) != len(want) {
		t.Fatalf("events %+v", ev)
	}
	for i := range want {
		if ev[i] != want[i] {
			t.Fatalf("events %+v, want %+v", ev, want)
		}
	}
	if d := p.Dropped(); len(d) != 1 || d[0].Frame != 1 {
		t.Fatalf("chain-breaking drops %+v, want the aborted key only", d)
	}
}

// C2-1: the receiver controls its clock. An honest clock is evidence; one
// that stops, jumps back beyond PathWindow or implies a transit beyond
// PathWindow from the path minimum is not, and adds no history.
func TestImplausibleReceiverClockIsNotEvidence(t *testing.T) {
	b := &Bandwidth{}
	seq := uint64(0)
	report := func(now, arrival time.Time) {
		b.OnSent(Sent{Seq: seq, At: now, Size: 1200, Kind: KindVideo})
		b.OnFeedback(now, Feedback{Arrivals: []Arrival{{Seq: seq, At: arrival}}})
		seq++
	}
	remote := time.Unix(5000, 0)
	for i := range 100 {
		now := epoch.Add(time.Duration(i) * 10 * time.Millisecond)
		report(now, remote.Add(now.Sub(epoch)+20*time.Millisecond))
	}
	if b.acked == 0 {
		t.Fatal("an honest receiver clock produced no acked rate")
	}
	honest := len(b.ack)
	now := epoch.Add(time.Second)
	for i, at := range []time.Time{
		b.clockArrival,                      // stopped while sends continue
		b.clockArrival.Add(-2 * PathWindow), // far behind
		b.clockArrival.Add(2 * PathWindow),  // transit far above the minimum
	} {
		before := b.bytes
		report(now.Add(time.Duration(i)*time.Millisecond), at)
		if b.bytes != before || len(b.ack) > honest {
			t.Fatalf("case %d: implausible arrival %v accepted", i, at)
		}
	}
}

// C2-1/A12: interleaving never allocates more buckets than a ULPFEC mask
// can protect, whatever loss run the receiver reports.
func TestFECGroupStaysWithinTheMask(t *testing.T) {
	for _, ratio := range []float64{0, 0.05, 0.25, 0.5, 1} {
		for _, run := range []int{0, 1, 3, 16, 1 << 20} {
			size, depth := FECGroup(ratio, run)
			if depth < 1 || depth > 16 || size > 1 && depth*(size-1) >= 16 && depth > 1 {
				t.Fatalf("ratio %v run %d: group %d depth %d", ratio, run, size, depth)
			}
		}
	}
	if _, depth := FECGroup(2*FECRatio, 3); depth != 3 {
		t.Fatalf("depth %d, want the loss run within the mask", depth)
	}
}
