package sfu

import (
	"bytes"
	"encoding/binary"
	"github.com/pion/rtp"
	"testing"
)

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
