package ctrl

import (
	"testing"
	"time"
)

func TestKeyExpiryUsesCommittedDecoderOwnership(t *testing.T) {
	for _, arm := range []string{"initial-native", "join-low", "refresh-low", "optional-before-writer", "optional-deferred", "partial-target", "broken-applied-low"} {
		t.Run(arm, func(t *testing.T) {
			q := &Queue{}
			q.SetRate(epoch, 5_000_000)
			pushKey := func(at time.Time, r Rendition, id uint64) {
				for i := 0; i < 4; i++ {
					if !q.Push(at, Packet{Kind: KindVideo, Class: ClassKey, Rendition: r, Frame: id, Key: true, Size: 1000, FrameSize: 4000, Packets: 4, Last: i == 3, Deadline: at.Add(KeyEnvelope)}) {
						t.Fatal("key fixture admission")
					}
				}
			}
			now := epoch
			if arm != "initial-native" && arm != "join-low" {
				pushKey(now, Low, 1)
				for !q.chains[Low].completed {
					if _, ok := q.Pop(now); !ok {
						now = q.NextAt(now)
					}
				}
				q.KeyEvents()
			}
			if arm == "broken-applied-low" {
				q.Push(now, Packet{Kind: KindVideo, Class: ClassTL0, Rendition: Low, Frame: 2, Size: 1000, FrameSize: 1000, Packets: 1, Last: true, Deadline: now.Add(FrameDeadline)})
				now = now.Add(FrameDeadline)
				q.Maintain(now, 0)
				if !q.chains[Low].broken || len(q.Dropped()) != 1 {
					t.Fatal("applied low was not genuinely broken")
				}
			}
			r := Native
			if arm == "refresh-low" || arm == "join-low" {
				r = Low
			}
			pushKey(now, r, 7)
			expires := now.Add(KeyEnvelope)
			if arm == "optional-deferred" {
				q.SetCommit(r, false)
			}
			if arm == "partial-target" {
				if _, ok := q.Pop(now); !ok {
					now = q.NextAt(now)
					if _, ok := q.Pop(now); !ok {
						t.Fatal("target never started")
					}
				}
			}
			q.KeyEvents()
			q.Maintain(expires.Add(-time.Nanosecond), 1)
			if left, _ := q.QueuedKey(r); left == 0 {
				t.Fatal("key discarded before unchanged hard deadline")
			}
			q.Maintain(expires, 1)
			if left, _ := q.QueuedKey(r); left != 0 {
				t.Fatal("key outlived unchanged hard deadline")
			}
			optional := arm == "optional-before-writer" || arm == "optional-deferred"
			phase := KeyAborted
			if optional {
				phase = KeyCancelled
			}
			events := q.KeyEvents()
			if len(events) != 1 || events[0].Frame != 7 || events[0].Rendition != r || events[0].Phase != phase {
				t.Fatalf("ownership classification %s: %+v", arm, events)
			}
			drops := q.Dropped()
			if optional && len(drops) != 0 || !optional && len(drops) != 1 {
				t.Fatalf("false/missing broken chain %s: %+v", arm, drops)
			}
		})
	}
}
