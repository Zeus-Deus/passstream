package ctrl

import (
	"testing"
	"time"
)

func TestRTXDepartureMatchesTheRealPacer(t *testing.T) {
	for _, name := range []string{"token-deficit", "audio-and-video-rtx", "audio-rtx-overtakes-video-rtx", "ordinary-backlog", "cluster-tail", "oversize-packet", "expired-audio"} {
		t.Run(name, func(t *testing.T) {
			q := &Queue{}
			q.SetRate(epoch, 1_000_000)
			q.tokens = 0
			kind, size := KindVideo, 1200
			add := func(id uint64, kind Kind, class Class, size int, deadline time.Time) {
				if !q.Push(epoch, Packet{ID: id, Kind: kind, Class: class, Size: size, Packets: 1, Last: true, Frame: id, Deadline: deadline}) {
					t.Fatal("fixture admission")
				}
			}
			switch name {
			case "audio-and-video-rtx", "audio-rtx-overtakes-video-rtx":
				add(1, KindAudio, ClassAudio, 400, time.Time{})
				add(2, KindVideo, ClassRTX, 1000, time.Time{})
				add(3, KindAudio, ClassRTX, 300, time.Time{})
				if name == "audio-rtx-overtakes-video-rtx" {
					kind = KindAudio
				}
			case "ordinary-backlog":
				if !q.Push(epoch, Packet{ID: 1, Kind: KindVideo, Class: ClassKey, Key: true, Frame: 1, Size: 1200, FrameSize: 400000, Packets: 400, Deadline: epoch.Add(KeyEnvelope)}) {
					t.Fatal("key admission")
				}
				add(2, KindVideo, ClassTL0, 40000, epoch.Add(FrameDeadline))
			case "cluster-tail":
				q.SetCluster(&Cluster{Rate: 5_000_000, Start: epoch, Until: epoch.Add(time.Millisecond)})
				size = 2000
			case "oversize-packet":
				q.SetRate(epoch, 128_000)
				size = 1500
			case "expired-audio":
				add(1, KindAudio, ClassAudio, 1200, epoch.Add(time.Millisecond))
			}
			released := 0
			q.SetRelease(func(Packet) { released++ })
			tokens, at, bytes, frames := q.tokens, q.at, q.bytes, len(q.frames)
			want, ok := q.RTXDeparture(epoch, kind, size, 0)
			if !ok {
				t.Fatal("unknown schedule")
			}
			if q.tokens != tokens || q.at != at || q.bytes != bytes || len(q.frames) != frames || released != 0 {
				t.Fatal("query mutated the live pacer")
			}
			if !q.Push(epoch, Packet{ID: 999, Kind: kind, Class: ClassRTX, Size: size, Packets: 1, Last: true}) {
				t.Fatal("candidate admission")
			}
			when := epoch
			found := false
			for steps := 0; steps < 100; steps++ {
				k, sent := q.Pop(when)
				if sent && k.ID == 999 {
					found = true
					break
				}
				next := q.NextAt(when)
				if next.IsZero() {
					t.Fatal("candidate disappeared")
				}
				if !sent && !next.After(when) {
					next = when.Add(time.Nanosecond)
				}
				when = next
			}
			if !found || when.Sub(want) > time.Nanosecond || want.Sub(when) > time.Nanosecond {
				t.Fatalf("forecast %v, actual %v, found %v", want.Sub(epoch), when.Sub(epoch), found)
			}
		})
	}
}

func TestRTXDepartureUnknownTransportOrRateAndQueueBounds(t *testing.T) {
	q := &Queue{}
	q.SetRate(epoch, 1_000_000)
	for _, pending := range []int{1, 10000, -1} {
		if _, ok := q.RTXDeparture(epoch, KindVideo, 1200, pending); ok {
			t.Fatal("unacknowledged writer batch has no measured completion time")
		}
	}
	q.SetRate(epoch, 0)
	q.tokens = 0
	if _, ok := q.RTXDeparture(epoch, KindVideo, 1200, 0); ok {
		t.Fatal("zero-rate schedule invented")
	}
	q.SetRate(epoch, 1_000_000)
	for i := 0; i < QueueFrames; i++ {
		if !q.Push(epoch, Packet{Size: 100, Kind: KindAudio, Class: ClassAudio, Last: true, Packets: 1}) {
			t.Fatal("fixture admission")
		}
	}
	if _, ok := q.RTXDeparture(epoch, KindVideo, 1200, 0); ok {
		t.Fatal("candidate ignored queue frame bound")
	}
}
