package sfu

import (
	"encoding/binary"
	"strings"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Level-zero ULPFEC (RFC 5109 §§7–8), carried in RFC 2198 RED.
// TWCC values are zeroed for parity, as required by libwebrtc for mutable
// extensions; its negotiated extension layout is included in protection.
// The protected packets must lie within 16 sequence numbers of the first.
func ulpParity(packets []*rtp.Packet) []byte {
	size := 0
	for _, p := range packets {
		raw, _ := p.Marshal()
		size = max(size, len(raw)-12)
	}
	out := make([]byte, 14+size)
	base := packets[0].SequenceNumber
	binary.BigEndian.PutUint16(out[2:4], base)
	binary.BigEndian.PutUint16(out[10:12], uint16(size))
	var mask uint16
	for _, p := range packets {
		raw, _ := p.Marshal()
		out[0] ^= raw[0] & 0x3f
		out[1] ^= p.PayloadType
		if p.Marker {
			out[1] ^= 0x80
		}
		binary.BigEndian.PutUint32(out[4:8], binary.BigEndian.Uint32(out[4:8])^p.Timestamp)
		binary.BigEndian.PutUint16(out[8:10], binary.BigEndian.Uint16(out[8:10])^uint16(len(raw)-12))
		mask |= 0x8000 >> uint16(p.SequenceNumber-base)
		for i, b := range raw[12:] {
			out[14+i] ^= b
		}
	}
	binary.BigEndian.PutUint16(out[12:14], mask)
	return out
}

// boundTrack is a local track whose packets the viewer's writer goroutine
// sends itself: it stamps sequence numbers and the transport-wide sequence,
// and chooses RED/ULPFEC payload types, so it writes to the bound stream
// directly (through the PC's interceptors) rather than via WriteRTP.
type boundTrack struct {
	*webrtc.TrackLocalStaticRTP
	mu      sync.Mutex
	binding binding
}

// binding is what negotiation fixed for one track.
type binding struct {
	writer                webrtc.TrackLocalWriter
	ssrc                  uint32
	mediaPT, redPT, fecPT uint8
	red                   bool // RED and ULPFEC both negotiated (video)
	twccID                uint8
}

func newBoundTrack(c webrtc.RTPCodecCapability, id string) (*boundTrack, error) {
	t, err := webrtc.NewTrackLocalStaticRTP(c, id, "passstream")
	return &boundTrack{TrackLocalStaticRTP: t}, err
}

func (t *boundTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codec, err := t.TrackLocalStaticRTP.Bind(ctx)
	if err != nil {
		return codec, err
	}
	b := binding{writer: ctx.WriteStream(), ssrc: uint32(ctx.SSRC()), mediaPT: uint8(codec.PayloadType)}
	haveRed, haveFEC := false, false
	for _, c := range ctx.CodecParameters() {
		switch strings.ToLower(c.MimeType) {
		case "video/red":
			b.redPT, haveRed = uint8(c.PayloadType), true
		case "video/ulpfec":
			b.fecPT, haveFEC = uint8(c.PayloadType), true
		}
	}
	for _, ext := range ctx.HeaderExtensions() {
		if strings.Contains(ext.URI, "transport-wide-cc") {
			b.twccID = uint8(ext.ID)
		}
	}
	b.red = haveRed && haveFEC
	t.mu.Lock()
	t.binding = b
	t.mu.Unlock()
	return codec, nil
}

func (t *boundTrack) bound() binding {
	if t == nil {
		return binding{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.binding
}
