package sfu

import (
	"testing"
	"time"

	"github.com/pion/rtcp"
)

func TestFreshMediaLossExcludesPaddingAndDoesNotReplayOldFeedback(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	for i := 0; i < 6; i++ {
		slot := &p.departures[i]
		slot.sequence.Store(uint32(i + 1))
		slot.media.Store(uint32(i + 1))
		slot.at.Store(now.Add(-100 * time.Millisecond).UnixNano())
		slot.bytes.Store(100)
	}
	p.departures[2].media.Store(0) // expendable wire traffic
	f := &rtcp.TransportLayerCC{BaseSequenceNumber: 0, PacketStatusCount: 3, ReferenceTime: 123,
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.StatusVectorChunk{SymbolList: []uint16{rtcp.TypeTCCPacketNotReceived, rtcp.TypeTCCPacketReceivedSmallDelta, rtcp.TypeTCCPacketNotReceived}}},
		RecvDeltas:   []*rtcp.RecvDelta{{Delta: 1000}},
	}
	p.FeedbackRTT(f, now)
	if p.parityLossAt.Load() != now.UnixNano() {
		t.Fatal("positive feedback loss was not retained for delayed repair")
	}
	if p.mediaLoss(now) != .5 {
		t.Fatal("padding loss contaminated original-media loss", p.mediaLoss(now))
	}
	p.FeedbackRTT(f, now.Add(300*time.Millisecond))
	if p.mediaLossAt.Load() != now.UnixNano() || p.mediaLoss(now.Add(500*time.Millisecond)) != 0 {
		t.Fatal("duplicate feedback kept a past loss episode alive")
	}
	f = &rtcp.TransportLayerCC{BaseSequenceNumber: 3, PacketStatusCount: 2, ReferenceTime: 124,
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{RunLength: 2, PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta}},
		RecvDeltas:   []*rtcp.RecvDelta{{Delta: 1000}, {Delta: 10000}},
	}
	for i := 3; i < 6; i++ {
		p.departures[i].at.Store(now.Add(500 * time.Millisecond).UnixNano())
	}
	p.FeedbackRTT(f, now.Add(600*time.Millisecond))
	if p.parityLossAt.Load() != now.UnixNano() {
		t.Fatal("clean feedback erased or extended the repair episode")
	}
	if p.mediaLoss(now.Add(600*time.Millisecond)) != 0 {
		t.Fatal("new clean delivery retained an expired loss sample")
	}
	previous := p.mediaLossAt.Load()
	f.BaseSequenceNumber, f.PacketStatusCount, f.RecvDeltas = 5, 1, nil
	p.FeedbackRTT(f, now.Add(700*time.Millisecond))
	if p.mediaLossAt.Load() != previous || p.feedbackHigh != 4 {
		t.Fatal("malformed feedback consumed original-media status")
	}
}

func TestReturnedFeedbackDoesNotTurnAnOldOutageIntoCurrentLoss(t *testing.T) {
	now := time.Now()
	f := &rtcp.TransportLayerCC{PacketStatusCount: 1,
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{RunLength: 1, PacketStatusSymbol: rtcp.TypeTCCPacketNotReceived}},
	}
	for _, rtt := range []time.Duration{0, 800 * time.Millisecond} {
		p := &Pacer{}
		p.rtt.Store(int64(rtt))
		p.departures[0].sequence.Store(1)
		p.departures[0].media.Store(1)
		p.departures[0].at.Store(now.Add(-time.Second).UnixNano())
		p.FeedbackRTT(f, now)
		if got := p.mediaLoss(now); (got == 1) != (rtt > 0) {
			t.Fatal("outage history was current loss, or valid high-RTT feedback was ignored", rtt, got)
		}
	}
}

func TestOverlappingTransportFeedbackCountsDeliveredBytesOnce(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	for i := 0; i < 8; i++ {
		d := &p.departures[i]
		d.sequence.Store(uint32(i + 1))
		d.at.Store(now.Add(-time.Second).UnixNano())
		d.kind.Store(uint32(videoKind))
		d.bytes.Store(1000)
		d.total.Store(int64(i+1) * 1000)
	}
	for i := 0; i < 7; i++ {
		arrival := 6400*time.Millisecond + time.Duration(i)*100*time.Millisecond
		ref := arrival / (64 * time.Millisecond)
		first := arrival - ref*64*time.Millisecond
		f := &rtcp.TransportLayerCC{BaseSequenceNumber: uint16(i), PacketStatusCount: 2, ReferenceTime: uint32(ref), PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{RunLength: 2, PacketStatusSymbol: rtcp.TypeTCCPacketReceivedLargeDelta}}, RecvDeltas: []*rtcp.RecvDelta{{Delta: first.Microseconds()}, {Delta: 100_000}}}
		p.FeedbackRTT(f, now.Add(time.Duration(i)*100*time.Millisecond))
	}
	if got := p.delivery.rate(500 * time.Millisecond); got != 80_000 {
		t.Fatal("overlap inflated received capacity", got)
	}
	if p.feedbackDuplicates.Load() != 6 {
		t.Fatal("overlap wasn't consumed once", p.feedbackDuplicates.Load())
	}
}

func TestMalformedFeedbackCannotConsumeDeliveryProof(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	d := &p.departures[0]
	d.sequence.Store(1)
	d.at.Store(now.Add(-time.Second).UnixNano())
	d.kind.Store(uint32(videoKind))
	d.bytes.Store(1000)
	d.total.Store(1000)
	f := &rtcp.TransportLayerCC{PacketStatusCount: 2, PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{RunLength: 2, PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta}}, RecvDeltas: []*rtcp.RecvDelta{{Delta: 1000}}}
	p.FeedbackRTT(f, now)
	if d.delivered.Load() != 0 {
		t.Fatal("invalid tail consumed delivery prefix")
	}
	f.PacketStatusCount = 1
	p.FeedbackRTT(f, now)
	if p.delivery.count != 1 || d.delivered.Load() == 0 {
		t.Fatal("valid receipt was hidden by malformed feedback")
	}
}
