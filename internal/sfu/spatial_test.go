package sfu

import (
	"github.com/pion/rtp"
	"testing"
	"time"
)

func TestSpatialAdmissionIncludesAllLowerLayersAndRepair(t *testing.T) {
	r := &Room{}
	r.spatialMax.Store(2)
	for tid := 0; tid < 3; tid++ {
		r.tidRate[tid].Store(20_000)
		r.spatialRate[0][tid].Store(80_000)
		r.spatialRate[1][tid].Store(300_000)
	}
	if rates := r.layerRates(2); rates != [3]int{400_000, 400_000, 400_000} {
		t.Fatal("spatial rates omitted lower-layer dependencies", rates)
	}
	for _, tc := range []struct {
		budget  int
		reserve float64
		want    uint8
	}{{256_000, .1, 0}, {500_000, .1, 1}, {2_000_000, .1, 2}, {2_000_000, .5, 1}} {
		if got := r.admittedSpatial(tc.budget, tc.reserve); got != tc.want {
			t.Fatal("spatial admission exceeded wire headroom", got, tc)
		}
	}
}
func TestCompleteLowerPictureSurvivesMissingHighestMarker(t *testing.T) {
	packets := []*rtp.Packet{{Payload: []byte{0x3c, 0, 0xaa}}, {Payload: []byte{0x3c, 2, 0xaa}}, {Payload: []byte{0x3c, 4, 0xaa}}}
	if !completePicture(packets) {
		t.Fatal("complete E picture required the highest marker")
	}
	for sid := uint8(0); sid < 3; sid++ {
		if got := spatialEnd(packets, sid); got != int(sid) {
			t.Fatal("wrong outgoing spatial marker", sid, got)
		}
	}
	packets[2].Payload[0] &= ^byte(4)
	if completePicture(packets) {
		t.Fatal("incomplete picture was accepted")
	}
	if completePicture(nil) {
		t.Fatal("empty picture was accepted")
	}
}

func TestFirstKeyframeDiscoversSpatialLayersBeforeForwarding(t *testing.T) {
	r := &Room{viewers: map[*Peer]bool{}}
	p := &Peer{Room: r}
	r.publisher = p
	f := frame{key: true, packets: []*rtp.Packet{
		{Payload: []byte{0x3c, 0, 0xaa}},
		{Payload: []byte{0x3c, 2, 0xaa}},
		{Payload: []byte{0x3c, 4, 0xaa}},
	}}
	p.distribute(f)
	if r.spatialMax.Load() != 2 {
		t.Fatal("first keyframe would bypass spatial filtering")
	}
	if r.tidBytes[0].Load() != 0 || r.spatialBytes[0][0].Load() != 0 || r.spatialBytes[1][0].Load() != 0 {
		t.Fatal("keyframe polluted steady delta-rate admission")
	}
	f.key = false
	p.distribute(f)
	if r.tidBytes[0].Load() == 0 || r.spatialBytes[0][0].Load() == 0 || r.spatialBytes[1][0].Load() == 0 {
		t.Fatal("delta pictures did not measure all spatial layers")
	}
}

func TestIndependentRenditionDamageDoesNotStopOtherViewers(t *testing.T) {
	r := &Room{viewers: map[*Peer]bool{}}
	native, low := &Peer{}, &Peer{}
	low.Low.Store(true)
	r.viewers[native], r.viewers[low] = true, true
	r.damage(frame{low: true})
	if native.Epoch.Load() != 0 || low.Epoch.Load() != 1 {
		t.Fatal("thumbnail damage invalidated native references")
	}
	r.damage(frame{})
	if native.Epoch.Load() != 1 || low.Epoch.Load() != 1 {
		t.Fatal("native damage invalidated thumbnail references")
	}
}

func TestThumbnailViewerDoesNotQueueNativeKeyframes(t *testing.T) {
	p := &Peer{Room: &Room{}, Track: &repairTrack{}, frames: make(chan frame), done: make(chan struct{})}
	p.Low.Store(true)
	p.Room.hasLow.Store(true)
	p.Admit()
	go p.forward()
	defer close(p.done)
	f := frame{at: time.Now(), key: true, packets: []*rtp.Packet{{Header: rtp.Header{Version: 2, Marker: true}, Payload: []byte{8, 0}}}}
	p.frames <- f
	p.frames <- f
	if p.Sent.Load() != 0 {
		t.Fatal("native keyframe entered thumbnail budget")
	}
	f.low = true
	p.frames <- f
	p.frames <- f
	if p.Sent.Load() == 0 {
		t.Fatal("selected thumbnail keyframe was not forwarded")
	}
}
