package sfu

import (
	"bytes"
	"encoding/binary"
	"github.com/pion/rtp"
	"testing"
	"time"
)

func TestSevereShortRTTLossRetainsParityWithinStableCapacity(t *testing.T) {
	if !protectLoss(40*time.Millisecond, .3, false, 0, 0) || !protectLoss(40*time.Millisecond, .3, true, 0, 0) {
		t.Fatal("severe independent loss disabled keyframe repair at short RTT")
	}
	if protectLoss(40*time.Millisecond, .05, false, 0, 0) || protectLoss(40*time.Millisecond, .3, true, 200*time.Millisecond, 0) || protectLoss(0, .3, false, 0, 0) {
		t.Fatal("parity ignored cheap replay, missing RTT, or congestion")
	}
	if !protectLoss(80*time.Millisecond, .05, false, 0, 0) {
		t.Fatal("ordinary Wi-Fi lost parity protection")
	}
}

func TestULPParityRepairsEachPacketWithMutableExtension(t *testing.T) {
	var packets []*rtp.Packet
	for i := 0; i < 4; i++ {
		p := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 98, SequenceNumber: uint16(65534 + i), Timestamp: uint32(9000 + i), Marker: i == 3}, Payload: bytes.Repeat([]byte{byte(i + 1)}, 10+i)}
		_ = p.SetExtension(3, []byte{0, 0})
		packets = append(packets, p)
	}
	fec := ulpParity(packets)
	if binary.BigEndian.Uint16(fec[12:14]) != 0xf000 {
		t.Fatal("mask")
	}
	for lost := range packets {
		body := append([]byte(nil), fec[14:]...)
		length := binary.BigEndian.Uint16(fec[8:10])
		flags := fec[0]
		pt := fec[1]
		for i, p := range packets {
			if i == lost {
				continue
			}
			raw, _ := p.Marshal()
			length ^= uint16(len(raw) - 12)
			flags ^= raw[0] & 0x3f
			pt ^= raw[1]
			for j, b := range raw[12:] {
				body[j] ^= b
			}
		}
		want, _ := packets[lost].Marshal()
		if !bytes.Equal(body[:length], want[12:]) || flags != want[0]&0x3f || pt != want[1] {
			t.Fatalf("failed packet %d", lost)
		}
	}
}

func TestFECMaskFollowsWireSequenceAfterDrops(t *testing.T) {
	p := NewPacer()
	defer p.Close()
	p.mapping[10] = sequenceMapping{10, 1, true}
	p.mapping[11] = sequenceMapping{11, 2, true}
	packets := []*rtp.Packet{{Header: rtp.Header{SequenceNumber: 10}, Payload: []byte{1}}, {Header: rtp.Header{SequenceNumber: 11}, Payload: []byte{2}}}
	payload := append([]byte{120}, ulpParity(packets)...)
	if !p.translateFEC(payload) || binary.BigEndian.Uint16(payload[3:5]) != 1 || binary.BigEndian.Uint16(payload[13:15]) != 0xc000 {
		t.Fatal("mask not translated")
	}
	p.mapping[10] = sequenceMapping{}
	if p.translateFEC(payload) {
		t.Fatal("parity for unsent media accepted")
	}
}

type parityRecordingWriter struct{ packets chan *rtp.Packet }

func (w parityRecordingWriter) WriteRTP(header *rtp.Header, payload []byte) (int, error) {
	w.packets <- &rtp.Packet{Header: header.Clone(), Payload: bytes.Clone(payload)}
	return header.MarshalSize() + len(payload), nil
}
func (w parityRecordingWriter) Write(b []byte) (int, error) { return len(b), nil }

func TestWiFiParitySpreadsWithoutHoldingMediaOrAddingPackets(t *testing.T) {
	p := &Pacer{}
	p.effective.Store(4_000_000)
	p.rtt.Store(int64(80 * time.Millisecond))
	p.flightDemand.Store(200_000)
	p.keyframeBytes.Store(9_000)
	w := parityRecordingWriter{make(chan *rtp.Packet, 4)}
	track := &repairTrack{writer: w, pacer: p, mediaPT: 98, redPT: 116, fecPT: 120, red: true}
	track.groupSize.Store(1)
	start := time.Now()
	for i := 0; i < 2; i++ {
		if err := track.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, Timestamp: uint32(9000 + i*6000), Marker: true}, Payload: []byte{1, 2, 3}}); err != nil {
			t.Fatal(err)
		}
		if packet := <-w.packets; packet.Payload[0] != 98 || packet.SequenceNumber != uint16(i+1) {
			t.Fatal("media waited behind deferred parity", packet)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case packet := <-w.packets:
			if time.Since(start) < 60*time.Millisecond || packet.Payload[0] != 120 || packet.SequenceNumber != uint16(i+3) {
				t.Fatal("parity lacked temporal diversity or broke wire sequence", packet)
			}
		case <-time.After(time.Second):
			t.Fatal("parity was retained indefinitely")
		}
	}
	track.mu.Lock()
	defer track.mu.Unlock()
	if track.FECSent.Load() != 2 || len(w.packets) != 0 {
		t.Fatal("temporal diversity added wire packets")
	}
}

func TestExtraWiFiParityRequiresLossAndSpareWireCapacity(t *testing.T) {
	now := time.Now()
	p := &Pacer{}
	p.effective.Store(700_000)
	p.flightDemand.Store(200_000)
	p.rtt.Store(int64(80 * time.Millisecond))
	p.keyframeBytes.Store(9_000)
	if p.extraParity(now) {
		t.Fatal("clean traffic gained an additional parity copy")
	}
	p.mediaLossAt.Store(now.UnixNano())
	p.mediaLossPPM.Store(50_000)
	if !p.extraParity(now) {
		t.Fatal("a loss burst with spare wire capacity lost time diversity")
	}
	p.effective.Store(600_000)
	if p.extraParity(now) {
		t.Fatal("additional repair exceeded its media-plus-repair reservation")
	}
	p.effective.Store(700_000)
	if p.extraParity(now.Add(2 * time.Second)) {
		t.Fatal("stale loss kept additional repair enabled")
	}
}
