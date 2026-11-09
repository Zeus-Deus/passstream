package sfu

import "github.com/pion/rtp/codecs"

// Non-flexible L1T3 base pictures do not reference TID1. After losing that
// enhancement, pass intact bases and suppress dependent enhancements until a
// retained TID0 switching point. RFC 9628's U bit only repairs layers ABOVE
// its own TID; U on the missing TID1 can still depend on the lost TID1.
type temporalContinuity struct {
	epoch   uint32
	waiting bool
}

func (t *temporalContinuity) allow(f frame) bool {
	if f.temporalEpoch != t.epoch {
		t.epoch = f.temporalEpoch
		t.waiting = true
	}
	if f.key || f.tid == 0 && f.up {
		t.waiting = false
	}
	return !t.waiting || f.tid == 0
}

// The measured non-flexible L1T3 stream never references its highest temporal
// picture. A later picture need not wait for that disposable picture's tail.
func disposableRepair(f frame, next *codecs.VP9Packet, timestamp uint32) bool {
	if len(f.packets) == 0 || f.tid != 2 || f.key || !next.B || !next.I || !next.L || next.F || next.SID != 0 || int32(timestamp-f.packets[0].Timestamp) <= 0 {
		return false
	}
	var previous codecs.VP9Packet
	if _, err := previous.Unmarshal(f.packets[0].Payload); err != nil || !previous.I || previous.F {
		return false
	}
	mask := uint16(0x7f)
	if len(f.packets[0].Payload) > 1 && f.packets[0].Payload[1]&0x80 != 0 {
		mask = 0x7fff
	}
	// A larger picture-ID jump may hide a whole base picture, whose repair
	// must not be abandoned along with the known disposable tail.
	return (next.PictureID-previous.PictureID)&mask == 1
}

// Losing an entire one-packet base picture leaves no partial frame for the
// assembler to reject. TL0PICIDX exposes that missing reference even when the
// reorder window has already abandoned its sequence numbers.
type baseContinuity struct {
	seen bool
	tl0  uint8
	gap  bool
}

func (b *baseContinuity) missing(v *codecs.VP9Packet) bool {
	if !v.B || v.SID != 0 || !v.L || v.F {
		return false
	}
	gap := b.gap
	b.gap = false
	if !v.P {
		b.seen = true
		b.tl0 = v.TL0PICIDX
		return false
	}
	if !b.seen {
		if v.TID == 0 {
			b.seen = true
			b.tl0 = v.TL0PICIDX
		}
		return false
	}
	delta := uint8(v.TL0PICIDX - b.tl0)
	// Encoder rate control can skip a base index without putting any packet
	// on the wire. Require actual unrepaired RTP loss before invalidating it.
	missing := gap && delta > 0 && delta < 128 && (v.TID > 0 || delta > 1)
	if v.TID == 0 || missing {
		b.tl0 = v.TL0PICIDX
	}
	return missing
}

// Flexible VP9 carries explicit picture references in its clear RTP descriptor.
// Keep extended picture numbers so a missing picture cannot borrow proof from
// the previous 7/15-bit wrap. The maximum reference distance is 127 pictures.
type pictureHistory struct {
	seen        bool
	value, mask uint16
	number      uint64
	delivered   [128]uint64
}

func (h *pictureHistory) sequence(value, mask uint16) uint64 {
	if !h.seen || h.mask != mask {
		h.number = uint64(value)
		h.seen = true
		h.mask = mask
	} else {
		h.number += uint64((value - h.value) & mask)
	}
	h.value = value
	return h.number
}
func (h *pictureHistory) contains(number uint64, refs [3]uint8, n uint8) bool {
	for _, distance := range refs[:n] {
		if distance == 0 || number < uint64(distance) {
			return false
		}
		reference := number - uint64(distance)
		if h.delivered[reference%128] != reference+1 {
			return false
		}
	}
	return true
}
func (h *pictureHistory) mark(number uint64, key bool) {
	if key {
		clear(h.delivered[:])
	}
	h.delivered[number%128] = number + 1
}
