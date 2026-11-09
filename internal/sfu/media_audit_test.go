package sfu

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Tier B correlation diagnostics (docs/GATES.md "Full native" and "Unique
// original media"); web/torture.mjs matches them against the receiver.

func TestFullNativeTimestampsStartAtTheFirstT2NativePicture(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	h.send(ctrl.Low, desc{key: true}, 2, 1000)
	h.drain(v, 200*time.Millisecond)
	if d := v.diagnostics(h.now); d["fullLayerFirst"] != nil || d["fullLayerTimestamps"] != nil {
		t.Fatalf("low pictures reported as full native: %v", d["fullLayerTimestamps"])
	}
	v.mu.Lock()
	v.ctl.Alloc = &keySwitchAllocation{desired: ctrl.Option{Rendition: ctrl.Native, TID: 2}}
	v.ctl.Probe = ctrl.NoProber{}
	v.want, v.wantTID = ctrl.Native, 2
	v.mu.Unlock()
	before := len(w.written())
	h.send(ctrl.Native, desc{key: true}, 3, 1000)
	h.drain(v, 300*time.Millisecond)
	h.send(ctrl.Native, desc{tid: 2}, 1, 800)
	h.drain(v, 100*time.Millisecond)
	sent := w.written()[before:]
	if len(sent) != 4 {
		t.Fatalf("native packets written: %d", len(sent))
	}
	d := v.diagnostics(h.now)
	if d["fullLayerFirst"] != sent[0].Timestamp {
		t.Fatalf("first full-native picture %v, want the key's rewritten timestamp %d", d["fullLayerFirst"], sent[0].Timestamp)
	}
	if got := d["fullLayerTimestamps"].([]uint32); len(got) != 2 || got[0] != sent[0].Timestamp || got[1] != sent[3].Timestamp {
		t.Fatalf("full-native timestamps %v", got)
	}
	// A temporal downswitch ends the episode: no picture is full native.
	v.mu.Lock()
	v.wantTID = 1
	v.tid = 1
	v.apply(h.now)
	v.mu.Unlock()
	if d := v.diagnostics(h.now); d["fullLayerFirst"] != nil || d["fullLayerTimestamps"] != nil {
		t.Fatal("full-native episode survived a downswitch to T1")
	}
}

// Each video sequence number counts once when TWCC reports it received:
// the original or its first retransmission, never a duplicate, FEC or
// padding. Bytes are RTP payload bytes, the unit of offeredMedia.
func TestUniqueMediaCountsEachSequenceNumberOnceAtTheReceiver(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	v.mu.Lock()
	v.video.binding.red, v.video.binding.redPT, v.video.binding.fecPT = true, 116, 117
	v.fec, v.interleave = 0.25, 1
	v.mu.Unlock()
	h.send(ctrl.Low, desc{key: true}, 4, 500)
	h.drain(v, 300*time.Millisecond)
	var media []uint16
	payload := 0
	isMedia := func(p *rtp.Packet) bool { return !p.Padding && len(p.Payload) > 0 && p.Payload[0] != 117 }
	for _, p := range w.written() {
		if isMedia(p) {
			media = append(media, p.SequenceNumber)
			payload += len(p.Payload) - 1 // RED header byte is not media
		}
	}
	if len(media) != 4 {
		t.Fatalf("media packets %d", len(media))
	}
	// One original is lost, then retransmitted twice.
	v.nackVideo(h.now, []uint16{media[1]})
	h.drain(v, 50*time.Millisecond)
	v.nackVideo(h.now, []uint16{media[1]})
	h.drain(v, 50*time.Millisecond)
	v.mu.Lock()
	sent := v.tseq
	offered := v.media.offered[ctrl.Low][0]
	v.mu.Unlock()
	small := rtcp.TypeTCCPacketReceivedSmallDelta
	symbols := make([]uint16, sent)
	deltas := []*rtcp.RecvDelta{}
	for i := range symbols {
		symbols[i] = small
		deltas = append(deltas, &rtcp.RecvDelta{Type: small, Delta: 250})
	}
	// Transport sequence numbers follow departure order: the second media
	// packet is the one the receiver lost.
	lost := -1
	for i, p := range w.written() {
		if isMedia(p) && p.SequenceNumber == media[1] {
			lost = i
			break
		}
	}
	symbols[lost] = rtcp.TypeTCCPacketNotReceived
	deltas = deltas[:len(deltas)-1]
	fb := &rtcp.TransportLayerCC{BaseSequenceNumber: 0, PacketStatusCount: uint16(sent), ReferenceTime: 1, FbPktCount: 1,
		PacketChunks: chunks(symbols), RecvDeltas: deltas}
	v.feedback(h.now, fb)
	fb.FbPktCount = 2 // the same arrivals reported again
	v.feedback(h.now, fb)
	d := v.diagnostics(h.now)["uniqueMedia"].(map[string]any)
	if d["packets"] != uint64(4) || d["bytes"] != uint64(payload) {
		t.Fatalf("unique media %v, want 4 packets / %d payload bytes", d, payload)
	}
	if offered != uint64(payload) {
		t.Fatalf("offered %d payload bytes, delivered unit %d", offered, payload)
	}
}

func chunks(symbols []uint16) []rtcp.PacketStatusChunk {
	var out []rtcp.PacketStatusChunk
	for i := 0; i < len(symbols); i += 7 {
		out = append(out, &rtcp.StatusVectorChunk{SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit, SymbolList: symbols[i:min(i+7, len(symbols))]})
	}
	return out
}

func TestHeartbeatKeepsHarnessCorrelationWithinOneDatagram(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	v.mu.Lock()
	for i := range 16 {
		v.media.fullPicture(4_000_000_000 + uint32(i)*6000)
	}
	v.media.unique, v.media.uniquePkts, v.media.offered = 1<<40, 1<<30, [2][3]uint64{{1 << 40, 1 << 40, 1 << 40}, {1 << 40, 1 << 40, 1 << 40}}
	v.mu.Unlock()
	extra := v.diagnostics(h.now)
	frames := make([]int64, 256)
	extra["sourceFrameTimes"] = frames
	extra["mediaRates"] = []float64{1e6, 1e6, 1e6}
	b := heartbeatPayload(map[string]any{"type": "heartbeat", "repair": extra, "layer": 2, "ingressFrames": uint64(1 << 40)}, false)
	if len(b) > 900 {
		t.Fatalf("heartbeat %d bytes", len(b))
	}
	var m struct {
		Repair struct {
			FullLayerTimestamps []uint32
			FullLayerFirst      *uint32
			UniqueMedia         map[string]any
			OfferedMedia        [][]uint64
		}
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Repair.FullLayerFirst == nil || len(m.Repair.FullLayerTimestamps) != 4 || m.Repair.UniqueMedia["bytes"] == nil || len(m.Repair.OfferedMedia) != 2 {
		t.Fatalf("compact heartbeat lost the harness correlation: %s", b)
	}
}
