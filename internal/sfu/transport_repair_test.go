package sfu

import (
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

func TestTransportLossRepairsPacketsWithinTheReplayLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rtt   time.Duration
		key   int64
		media uint32
		count uint16
		age   time.Duration
		want  uint64
	}{
		{"lost NACK", 80 * time.Millisecond, 8_000, 8, 2, 0, 1},
		{"replay or parity", 80 * time.Millisecond, 8_000, 0, 2, 0, 0},
		{"local path", time.Millisecond, 8_000, 8, 2, 0, 0},
		{"FIFO queue", 500 * time.Millisecond, 8_000, 8, 2, 0, 0},
		{"large refresh", 80 * time.Millisecond, 250_000, 8, 2, 0, 1},
		{"malformed feedback", 80 * time.Millisecond, 8_000, 8, 3, 0, 0},
		{"expired media", 80 * time.Millisecond, 8_000, 8, 2, 2 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPacer()
			defer p.Close()
			now := time.Now()
			p.rtt.Store(int64(tc.rtt))
			p.keyframeBytes.Store(tc.key)
			p.cache.put(&rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 1, SequenceNumber: 7, PayloadType: 98}, Payload: []byte{0x68, 0, 0, 1}}, now.Add(-tc.age))
			p.departures[0].at.Store(now.Add(-100 * time.Millisecond).UnixNano())
			p.departures[0].media.Store(tc.media)
			p.departures[0].sequence.Store(1)
			p.departures[1].at.Store(now.Add(-80 * time.Millisecond).UnixNano())
			p.departures[1].sequence.Store(2)
			feedback := &rtcp.TransportLayerCC{PacketStatusCount: tc.count, PacketChunks: []rtcp.PacketStatusChunk{&rtcp.StatusVectorChunk{SymbolList: []uint16{rtcp.TypeTCCPacketNotReceived, rtcp.TypeTCCPacketReceivedSmallDelta}}}, RecvDeltas: []*rtcp.RecvDelta{{Delta: 1000}}}
			p.FeedbackRTT(feedback, now)
			if got := p.FeedbackRepairs.Load(); got != tc.want {
				t.Fatal("unexpected transport-triggered repair", got, tc.want)
			}
			p.FeedbackRTT(feedback, now.Add(time.Millisecond))
			if got := p.FeedbackRepairs.Load(); got != tc.want {
				t.Fatal("overlapping feedback bypassed replay spacing", got)
			}
		})
	}
}
