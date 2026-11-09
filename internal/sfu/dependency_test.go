package sfu

import (
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"testing"
)

func TestWholeBasePictureLossInvalidatesItsReferences(t *testing.T) {
	var b baseContinuity
	p := codecs.VP9Packet{B: true, L: true, TL0PICIDX: 255}
	if b.missing(&p) {
		t.Fatal("keyframe invalidated itself")
	}
	p.P = true
	p.TL0PICIDX = 0
	p.TID = 2
	b.gap = true
	if !b.missing(&p) {
		t.Fatal("missing whole base picture did not invalidate dependent pictures")
	}
	if b.missing(&p) {
		t.Fatal("same missing base was reported twice")
	}
	p.TID = 0
	p.TL0PICIDX = 1
	if b.missing(&p) {
		t.Fatal("next base picture mistaken for a gap")
	}
	p.TL0PICIDX = 3
	b.gap = true
	if !b.missing(&p) {
		t.Fatal("base-to-base gap was missed")
	}
}

func TestEncoderBaseIndexSkipWithoutPacketLossKeepsReferences(t *testing.T) {
	var b baseContinuity
	p := codecs.VP9Packet{B: true, L: true, TL0PICIDX: 4}
	b.missing(&p)
	p.P = true
	p.TID = 1
	p.TL0PICIDX = 6
	if b.missing(&p) {
		t.Fatal("encoder index skip with contiguous RTP requested an unnecessary keyframe")
	}
	b.gap = true
	p.TL0PICIDX = 7
	if !b.missing(&p) {
		t.Fatal("actual whole-base packet loss was ignored")
	}
}
func TestDisposablePictureLossKeepsBaseReferences(t *testing.T) {
	var b baseContinuity
	p := codecs.VP9Packet{B: true, L: true, TL0PICIDX: 4}
	b.missing(&p)
	p.P = true
	p.TID = 1
	if b.missing(&p) {
		t.Fatal("temporal gap invalidated an intact base")
	}
	p.TID = 0
	p.TL0PICIDX = 5
	if b.missing(&p) {
		t.Fatal("contiguous base counter mistaken for missing media")
	}
}

func TestDisposableTailDoesNotBlockTheNextPicture(t *testing.T) {
	f := frame{tid: 2, packets: []*rtp.Packet{{Header: rtp.Header{Timestamp: 100}, Payload: []byte{0xe8, 0x80, 10, 0x40, 1, 0}}}}
	next := codecs.VP9Packet{B: true, L: true, I: true, PictureID: 11}
	if !disposableRepair(f, &next, 200) {
		t.Fatal("disposable tail blocked a newer picture")
	}
	for _, tid := range []uint8{0, 1} {
		f.tid = tid
		if disposableRepair(f, &next, 200) {
			t.Fatal("reference repair was abandoned")
		}
	}
	f.tid = 2
	if disposableRepair(f, &next, 99) {
		t.Fatal("old retransmission moved repair backward")
	}
	next.F = true
	if disposableRepair(f, &next, 200) {
		t.Fatal("flexible references were assumed disposable")
	}
	next.F = false
	next.PictureID = 12
	if disposableRepair(f, &next, 200) {
		t.Fatal("disposable tail shortcut abandoned a possibly missing base picture")
	}
}

func TestTemporalReferenceLossRecoversAtAnIntactBase(t *testing.T) {
	v := &Peer{}
	r := &Room{viewers: map[*Peer]bool{v: true}}
	r.damage(frame{tid: 1, temporalRecovery: true})
	if v.Epoch.Load() != 0 || v.temporalEpoch.Load() != 1 || r.PLIsSent.Load() != 0 {
		t.Fatal("temporal reference loss unnecessarily required a room-wide keyframe")
	}
	var continuity temporalContinuity
	for _, tid := range []uint8{2, 1, 2} {
		if continuity.allow(frame{tid: tid, temporalEpoch: 1}) {
			t.Fatal("forwarded an enhancement with a missing reference")
		}
	}
	if !continuity.allow(frame{tid: 0, temporalEpoch: 1}) || continuity.allow(frame{tid: 1, temporalEpoch: 1}) || continuity.allow(frame{tid: 1, up: true, temporalEpoch: 1}) {
		t.Fatal("U on a missing temporal layer resumed that layer's broken references")
	}
	if !continuity.allow(frame{tid: 0, up: true, temporalEpoch: 1}) || !continuity.allow(frame{tid: 1, temporalEpoch: 1}) {
		t.Fatal("base and temporal switching point did not safely restore references")
	}
	r.damage(frame{tid: 0, temporalRecovery: true})
	if v.Epoch.Load() != 1 {
		t.Fatal("base reference loss did not require a keyframe")
	}
	r.damage(frame{tid: 1})
	if v.Epoch.Load() != 2 {
		t.Fatal("unknown reference structure bypassed keyframe recovery")
	}
}

func TestFlexibleEnhancementDamageKeepsIndependentDecoderReferences(t *testing.T) {
	v := &Peer{}
	r := &Room{viewers: map[*Peer]bool{v: true}}
	r.damage(frame{tid: 1, flexible: true})
	if v.Epoch.Load() != 0 || v.temporalEpoch.Load() != 0 || r.PLIsSent.Load() != 0 {
		t.Fatal("explicit enhancement loss invalidated independent base references")
	}
	r.damage(frame{tid: 0, flexible: true})
	if v.Epoch.Load() != 1 {
		t.Fatal("flexible base loss bypassed independent-key recovery")
	}
	// The input queue can drop the same enhancement before a viewer pacer
	// sees it. Missing picture IDs still fail its explicit-reference check.
	v.frames = make(chan frame, 1)
	v.frames <- frame{}
	p := &Peer{Room: r}
	r.publisher = p
	v.Epoch.Store(0)
	p.distribute(frame{tid: 1, flexible: true})
	if v.Epoch.Load() != 0 || v.temporalEpoch.Load() != 0 {
		t.Fatal("input queue loss unnecessarily replaced flexible decoder references")
	}
}
