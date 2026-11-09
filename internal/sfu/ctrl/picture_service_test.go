package ctrl

import (
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func serviceFrame(q *Queue, at time.Time, id uint64, key bool, tid uint8, count, size int, fresh, lease time.Time) {
	class := ClassTL0 + Class(tid)
	if key {
		class = ClassKey
	}
	for i := 0; i < count; i++ {
		if !q.Push(at, Packet{ID: id*1000 + uint64(i), Frame: id, Rendition: Native, Kind: KindVideo, Class: class, Key: key, TID: tid, Size: size, FrameSize: count * size, Packets: count, Last: i == count-1, FreshUntil: fresh, Deadline: lease}) {
			panic("service fixture exceeds queue bounds")
		}
	}
}

func actualPictureService(q *Queue, at time.Time, in PictureQuery) PictureService {
	out := PictureService{Known: true, Generation: in.Generation}
	for _, p := range in.Offer {
		q.Push(at, p)
	}
	for steps := 0; len(q.frames) > 0 && steps < 100000; steps++ {
		p, ok := q.Pop(at)
		for _, d := range q.Dropped() {
			if d.Rendition == in.Rendition && d.Frame >= in.Frame {
				out.Broken = true
			}
		}
		if ok {
			if p.Kind == KindVideo && p.Class != ClassRTX && p.Class != ClassFEC && p.Last && p.Rendition == in.Rendition && p.Frame >= in.Frame {
				out.Pictures = append(out.Pictures, PictureDeparture{p.Frame, at, p.FreshUntil, p.Deadline, p.Key})
				if p.Key && p.Frame == in.Frame {
					out.Key = at
				}
				if !out.Broken && !out.Key.IsZero() && out.FirstFresh.IsZero() && !p.FreshUntil.IsZero() && !at.After(p.FreshUntil) {
					out.FirstFresh = at
				}
			}
			continue
		}
		if len(q.frames) == 0 {
			break
		}
		next := q.NextAt(at)
		if next.IsZero() {
			out.Known = false
			break
		}
		if !next.After(at) {
			next = at.Add(time.Nanosecond)
		}
		at = next
	}
	return out
}

func TestPictureServiceDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(31842))
	for trial := 0; trial < 600; trial++ {
		build := func() *Queue { return &Queue{} }
		q, real := build(), build()
		rate := 128000 + rng.Intn(6000000)
		tokens := float64(rng.Intn(10000) - 5000)
		count, size := 2+rng.Intn(100), 500+rng.Intn(1000)
		var cluster *Cluster
		if trial%2 == 0 {
			cluster = &Cluster{Rate: rate * 3, Start: epoch.Add(time.Duration(trial%5) * time.Millisecond), Until: epoch.Add(time.Duration(1+trial%50) * time.Millisecond)}
		}
		for _, p := range []*Queue{q, real} {
			p.SetRate(epoch, rate)
			p.tokens = tokens
			p.SetCluster(cluster)
			serviceFrame(p, epoch, 1, true, 0, count, size, epoch.Add(FrameDeadline), epoch.Add(KeyEnvelope))
			serviceFrame(p, epoch, 2, false, 0, 4, size, epoch.Add(FrameDeadline), epoch.Add(FrameDeadline))
			serviceFrame(p, epoch, 3, false, 1, 3, size, epoch.Add(FrameDeadline), epoch.Add(FrameDeadline))
			for i := 0; i < trial%9; i++ {
				kind, class := KindAudio, ClassAudio
				if i%2 == 0 {
					kind, class = KindVideo, ClassRTX
				}
				p.Push(epoch, Packet{ID: uint64(10000 + i), Kind: kind, Class: class, Size: 100 + size/2, Packets: 1, Last: true, Deadline: epoch.Add(time.Duration(1+trial%80) * time.Millisecond)})
			}
		}
		query := PictureQuery{Rendition: Native, Frame: 1, Generation: 7}
		beforeTokens, beforeBytes, beforeAt := q.tokens, q.bytes, q.at
		frames := make([]queuedFrame, len(q.frames))
		for i, f := range q.frames {
			frames[i] = *f
			frames[i].packets = append([]Packet(nil), f.packets...)
		}
		released := 0
		q.SetRelease(func(Packet) { released++ })
		got := q.PictureService(epoch, query)
		unchanged := len(frames) == len(q.frames)
		for i, f := range q.frames {
			unchanged = unchanged && i < len(frames) && reflect.DeepEqual(frames[i], *f)
		}
		if q.tokens != beforeTokens || q.bytes != beforeBytes || q.at != beforeAt || len(frames) != len(q.frames) || !unchanged || released != 0 {
			t.Fatal("forecast changed live queue")
		}
		want := actualPictureService(real, epoch, query)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("trial%d R%d tokens%v: forecast%+v actual%+v", trial, rate, tokens, got, want)
		}
	}
}

func TestPictureServiceRequiresTheExplicitReferenceClosure(t *testing.T) {
	for _, missing := range []bool{false, true} {
		q := &Queue{}
		q.SetRate(epoch, 5_000_000)
		serviceFrame(q, epoch, 1, true, 0, 2, 1200, time.Time{}, epoch.Add(KeyEnvelope))
		serviceFrame(q, epoch, 2, false, 0, 1, 1200, epoch, epoch.Add(FrameDeadline))
		serviceFrame(q, epoch, 3, false, 0, 1, 1200, epoch.Add(FrameDeadline), epoch.Add(FrameDeadline))
		for _, f := range q.frames {
			for i := range f.packets {
				info := FrameInfo{Flexible: true, Key: f.key}
				if !f.key {
					info.Refs = []uint64{f.id.picture - 1}
				}
				if missing && f.id.picture == 3 {
					info.Refs = []uint64{99}
				}
				f.packets[i].SetReferences(info)
			}
		}
		result := q.PictureService(epoch, PictureQuery{Frame: 1})
		if missing && (!result.Broken || !result.FirstFresh.IsZero()) {
			// The key's freshness is deliberately unknown: only the valid
			// final descendant can establish a fresh post-key output.
			t.Fatalf("missing reference granted fresh output: %+v", result)
		}
		if !missing && (result.Broken || result.FirstFresh.IsZero()) {
			t.Fatalf("necessary old base lost: %+v", result)
		}
	}
}

func TestPictureServiceOldReferenceFreshDescendantAndBrokenBase(t *testing.T) {
	for _, broken := range []bool{false, true} {
		q := &Queue{}
		q.SetRate(epoch, 5_000_000)
		lease := epoch.Add(KeyEnvelope)
		if broken {
			lease = epoch.Add(901 * time.Millisecond)
		}
		serviceFrame(q, epoch, 1, true, 0, 200, 1242, epoch.Add(FrameDeadline), lease)
		serviceFrame(q, epoch, 2, false, 0, 2, 1000, epoch.Add(FrameDeadline), epoch.Add(FrameDeadline))
		serviceFrame(q, epoch.Add(900*time.Millisecond), 3, false, 0, 2, 1000, epoch.Add(1900*time.Millisecond), epoch.Add(1900*time.Millisecond))
		f := q.PictureService(epoch.Add(900*time.Millisecond), PictureQuery{Frame: 1})
		if broken {
			if !f.Known || !f.Broken || !f.FirstFresh.IsZero() {
				t.Fatalf("broken base invented fresh closure: %+v", f)
			}
		} else {
			if !f.Known || f.Broken || len(f.Pictures) != 3 || !f.Pictures[1].At.After(f.Pictures[1].FreshUntil) || !f.Pictures[1].Lease.After(f.Pictures[1].FreshUntil) || f.FirstFresh != f.Pictures[2].At {
				t.Fatalf("old reference/fresh descendant conflated: %+v", f)
			}
		}
	}
}

func TestPictureServiceUnknownInputsOfferAndGeneration(t *testing.T) {
	v := NewViewer(1, NewRoomKeys())
	q := v.Pacer.(*Queue)
	q.SetRate(epoch, 1_500_000)
	offer := []Packet{{Kind: KindVideo, Class: ClassKey, Key: true, Size: 1200, Frame: 4, FrameSize: 1200, Packets: 1, Last: true, Deadline: epoch.Add(KeyEnvelope)}}
	in := PictureQuery{Frame: 4, Offer: offer}
	if f := v.PictureService(epoch, in); !f.Known || f.Key.IsZero() || !f.FirstFresh.IsZero() {
		t.Fatalf("unarrived fresh descendant invented: %+v", f)
	}
	if b, _ := q.Queued(); b != 0 {
		t.Fatal("hypothetical offer changed live queue")
	}
	for _, n := range []int{-1, 1, 100} {
		in.WriterPending = n
		if v.PictureService(epoch, in).Known {
			t.Fatal("unknown physical writer time invented")
		}
	}
	in.WriterPending = 0
	v.OnPathChange(epoch)
	if v.PictureService(epoch, in).Known {
		t.Fatal("old generation forecast reused")
	}
	in.Generation = v.Generation()
	if !v.PictureService(epoch, in).Known {
		t.Fatal("current generation rejected")
	}
	q.Push(epoch, Packet{Kind: KindVideo, Class: ClassKey, Key: true, Size: 1200, Frame: 4, FrameSize: 2400, Packets: 2, Deadline: epoch.Add(KeyEnvelope)})
	if q.PictureService(epoch, PictureQuery{Frame: 4}).Known {
		t.Fatal("unarrived reserved tail invented")
	}
}
