package sfu

import (
	"encoding/binary"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Short RTT makes isolated losses cheap to replay, but repeated independent
// loss can still exhaust a keyframe's retries. Keep parity during that repair
// stall when transit delay is stable; the caller reserves and paces its bytes.
func protectLoss(rtt time.Duration, loss float64, stalled bool, excess, queue time.Duration) bool {
	stable := excess < rttRise && queue < 20*time.Millisecond
	return rtt > 0 && rtt < 300*time.Millisecond && (rtt >= 50*time.Millisecond || loss > .2 && stable) && (!stalled || loss <= .25 || stable)
}

// The second copy must fit the RTT-scaled repair buffer after pacing and
// propagation, while remaining distinct from the first copy at 80ms.
func extraParityDelay(rtt time.Duration) time.Duration {
	return max(100*time.Millisecond, min(160*time.Millisecond, rtt*3/2))
}

// Level-zero ULPFEC (RFC 5109 §§7–8), carried in RFC 2198 RED.
// TWCC values are zeroed for parity, as required by libwebrtc for mutable
// extensions; its negotiated extension layout is included in protection. Repair and media share the congestion pacer.
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

type repairTrack struct {
	*webrtc.TrackLocalStaticRTP
	mu                    sync.Mutex
	writer                webrtc.TrackLocalWriter
	pacer                 *Pacer
	ssrc                  uint32
	mediaPT, redPT, fecPT uint8
	red                   bool
	seq                   uint16
	group                 []*rtp.Packet
	parity                [][]byte
	keyframe              bool
	twccID                uint8
	groupSize             atomic.Int32 // 0 = NACK only; 1..16 media packets per parity
	FECSent               atomic.Uint64
}

func newRepairTrack() (*repairTrack, error) {
	t, e := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0"}, "screen", "passstream")
	return &repairTrack{TrackLocalStaticRTP: t}, e
}
func (t *repairTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codec, e := t.TrackLocalStaticRTP.Bind(ctx)
	if e != nil {
		return codec, e
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writer = ctx.WriteStream()
	t.ssrc = uint32(ctx.SSRC())
	t.mediaPT = uint8(codec.PayloadType)
	if t.pacer != nil {
		t.pacer.mediaPT.Store(int32(t.mediaPT))
	}
	haveRed, haveFEC := false, false
	for _, c := range ctx.CodecParameters() {
		switch strings.ToLower(c.MimeType) {
		case "video/red":
			t.redPT = uint8(c.PayloadType)
			haveRed = true
		case "video/ulpfec":
			t.fecPT = uint8(c.PayloadType)
			haveFEC = true
		}
	}
	for _, ext := range ctx.HeaderExtensions() {
		if strings.Contains(ext.URI, "transport-wide-cc") {
			t.twccID = uint8(ext.ID)
			if t.pacer != nil {
				t.pacer.twccID.Store(int32(t.twccID))
			}
		}
	}
	t.red = haveRed && haveFEC
	if t.red && t.pacer != nil {
		t.pacer.redPT.Store(int32(t.redPT))
		t.pacer.fecPT.Store(int32(t.fecPT))
	}
	return codec, nil
}
func (t *repairTrack) WriteRTP(p *rtp.Packet) error { return t.writeRTP(p, false) }

// Forwarding owns its rewritten immutable payload. The pacer copies it before
// returning; FEC may retain this same payload until the picture ends.
func (t *repairTrack) writeOwnedRTP(p *rtp.Packet) error { return t.writeRTP(p, true) }
func (t *repairTrack) writeRTP(p *rtp.Packet, owned bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.writer == nil {
		return nil
	}
	var references [3]uint8
	descriptor := codecs.VP9Packet{PDiff: references[:0]}
	if _, err := descriptor.Unmarshal(p.Payload); err == nil && descriptor.B {
		t.keyframe = !descriptor.P
	}
	q := p
	if !owned {
		q = p.Clone()
	}
	t.seq++
	q.SequenceNumber = t.seq
	q.SSRC = t.ssrc
	q.PayloadType = t.mediaPT
	if t.twccID != 0 {
		_ = q.SetExtension(t.twccID, []byte{0, 0})
	}

	if t.red && t.groupSize.Load() > 0 {
		t.group = append(t.group, q)
	}
	if err := t.write(q); err != nil {
		return err
	}
	if len(t.group) > 0 && (p.Marker || len(t.group) >= 16) {
		count := max(1, int(t.groupSize.Load()))
		stripes := (len(t.group) + count - 1) / count
		for stripe := 0; stripe < stripes; stripe++ {
			var packets []*rtp.Packet
			for i := stripe; i < len(t.group); i += stripes {
				packets = append(packets, t.group[i])
			}
			t.parity = append(t.parity, ulpParity(packets))
		}
		t.group = nil
	}
	if p.Marker {
		parity, key := t.parity, t.keyframe
		t.parity, t.keyframe = nil, false
		if len(parity) > 0 && t.pacer != nil && t.pacer.spreadParity(time.Now()) {
			// Adjacent media/parity packets share a packet-loss burst. One
			// screen-frame interval gives existing parity temporal diversity,
			// within the Wi-Fi repair buffer. It still enters the same pacer.
			secondDelay := extraParityDelay(time.Duration(t.pacer.rtt.Load()))
			if key {
				// An independent refresh must complete before its dependents.
				// Send its existing first copy now; preserve temporal diversity
				// in the later copies without increasing the packet count.
				if err := t.writeParity(q.Header, parity); err != nil {
					return err
				}
				secondDelay = 80 * time.Millisecond
			} else {
				t.deferParity(q.Header, parity, 80*time.Millisecond, 0)
			}
			if t.pacer.extraParity(time.Now()) {
				// A second spaced copy survives longer loss/feedback bursts.
				// It requires loss plus 3.3x media headroom and is charged to
				// the same media/repair wire bucket as the first copy.
				t.deferParity(q.Header, parity, secondDelay, 1)
			}
			if t.pacer.severeParity(time.Now()) {
				// Immediate and later diversity supplement the 80ms/RTT copies
				// on compounded random loss, inside the same wire bucket.
				if key {
					t.deferParity(q.Header, parity, extraParityDelay(time.Duration(t.pacer.rtt.Load())), 2)
				} else if err := t.writeParity(q.Header, parity); err != nil {
					return err
				}
				t.deferParity(q.Header, parity, 180*time.Millisecond, 2)
			}
		} else if err := t.writeParity(q.Header, parity); err != nil {
			return err
		}
	}
	return nil
}

func (t *repairTrack) deferParity(header rtp.Header, parity [][]byte, delay time.Duration, tier uint8) {
	h := header.Clone()
	time.AfterFunc(delay, func() {
		if tier == 1 && !t.pacer.extraParity(time.Now()) || tier == 2 && !t.pacer.severeParity(time.Now()) {
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		_ = t.writeParity(h, parity)
	})
}

func (t *repairTrack) writeParity(header rtp.Header, parity [][]byte) error {
	for _, fec := range parity {
		t.seq++
		h := header.Clone()
		h.SequenceNumber = t.seq
		h.Marker = false
		h.PayloadType = t.redPT
		if _, err := t.writer.WriteRTP(&h, append([]byte{t.fecPT}, fec...)); err != nil {
			return err
		}
		t.FECSent.Add(1)
	}
	return nil
}
func (t *repairTrack) write(q *rtp.Packet) error {
	h := q.Header.Clone()
	payload := q.Payload
	if t.red {
		h.PayloadType = t.redPT
		payload = append([]byte{t.mediaPT}, payload...)
	}
	_, e := t.writer.WriteRTP(&h, payload)
	return e
}
