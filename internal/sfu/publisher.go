package sfu

import (
	"slices"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

const sourceSendTimeURI = "http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time"

// publisherCeiling is the plain REMB the publisher gets: the controller's
// default Config.MaxRate. The SFU runs no publisher budget logic.
var publisherCeiling = ctrl.NewViewer(0, nil).Cfg.MaxRate

// picture is one assembled picture of one rendition, described only from RTP
// headers and the clear VP9 payload descriptor.
type picture struct {
	rend        ctrl.Rendition
	packets     []*rtp.Packet
	info        ctrl.FrameInfo
	switchPoint bool      // keyframe or TL0: a temporal upswitch may start here
	wire        int       // wire bytes as received
	capture     time.Time // from the RTP timestamp (frameClock)
	deadline    time.Time // useful deadline: capture + FrameDeadline; keys: assembly + KeyEnvelope
	source      uint64    // publisher epoch, set at distribution
}

// publisherFeedback sends the PLIs the room's key policy asks for and the
// plain REMB ceiling, for as long as p is the room's publisher.
func (p *Peer) publisherFeedback() {
	r := p.Room
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var rembAt time.Time
	for {
		select {
		case <-p.done:
			return
		case now := <-tick.C:
			r.mu.Lock()
			active, ssrcs := r.publisher == p, r.ssrc
			r.mu.Unlock()
			if !active {
				return
			}
			var out []rtcp.Packet
			for _, rend := range r.keys.Due(now) {
				if ssrc := ssrcs[rend]; ssrc != 0 {
					out = append(out, &rtcp.PictureLossIndication{MediaSSRC: ssrc})
					r.PLIsSent.Add(1)
				}
			}
			if now.Sub(rembAt) >= 500*time.Millisecond {
				rembAt = now
				var list []uint32
				for _, s := range ssrcs {
					if s != 0 {
						list = append(list, s)
					}
				}
				if len(list) > 0 {
					out = append(out, &rtcp.ReceiverEstimatedMaximumBitrate{Bitrate: float32(publisherCeiling), SSRCs: list})
				}
			}
			if len(out) > 0 {
				_ = p.PC.WriteRTCP(out)
			}
		}
	}
}

// read receives one rendition: upstream NACK repair through the reorder
// window, the ingress meter, and picture assembly. Every picture, key or
// delta, is assembled completely before any viewer sees its first packet.
func (p *Peer) read(t *webrtc.TrackRemote, rend ctrl.Rendition) {
	r := p.Room
	packets := make(chan *rtp.Packet, 256)
	go func() {
		defer close(packets)
		for {
			pkt, _, err := t.ReadRTP()
			if err != nil {
				return
			}
			r.RawPackets.Add(1)
			select {
			case packets <- pkt:
			case <-p.done:
				return
			}
		}
	}()
	reorder := newReorder()
	a := &assembler{rend: rend, emit: func(now time.Time, pic *picture) { r.distribute(p, now, pic) }, lose: func(now time.Time, info ctrl.FrameInfo) { r.ingressLost(p, now, info) }}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var nackBusy atomic.Bool
	for {
		select {
		case pkt, ok := <-packets:
			if !ok {
				return
			}
			// A new keyframe supersedes missing older pictures.
			var d codecs.VP9Packet
			if _, err := d.Unmarshal(pkt.Payload); err == nil && d.B && d.SID == 0 && !d.P {
				reorder.keyframe(pkt.SequenceNumber)
			}
			reorder.push(pkt, time.Now())
		case <-tick.C:
		case <-p.done:
			return
		}
		if nackBusy.CompareAndSwap(false, true) {
			missing := reorder.nacks(time.Now())
			if len(missing) == 0 {
				nackBusy.Store(false)
			} else {
				go func() {
					defer nackBusy.Store(false)
					_ = p.PC.WriteRTCP([]rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: uint32(t.SSRC()), Nacks: rtcp.NackPairsFromSequenceNumbers(missing)}})
				}()
			}
		}
		now := time.Now()
		for _, pkt := range reorder.pop(now) {
			r.Packets.Add(1)
			if len(pkt.Payload) == 0 {
				a.padding(pkt) // the publisher's own probing: no picture, no media cost
				continue
			}
			var d codecs.VP9Packet
			tid, key := uint8(255), false
			if _, err := d.Unmarshal(pkt.Payload); err == nil {
				key = !d.P
				if d.L {
					tid = d.TID
				}
			}
			r.meterVideo(now, rend, tid, key, uint64(pkt.Timestamp), pkt.MarshalSize()+wireOverhead)
			a.push(now, pkt)
		}
	}
}

// maxPicturePackets bounds one picture's assembly memory.
const maxPicturePackets = 2048

// assembler turns one rendition's in-order packets into pictures and reports
// publisher-leg loss the reorder window could not repair: damaged pictures
// and, from picture-ID and TL0PICIDX gaps, wholly missing ones.
type assembler struct {
	rend ctrl.Rendition
	emit func(time.Time, *picture)
	lose func(time.Time, ctrl.FrameInfo)

	packets     []*rtp.Packet
	ts          uint32
	last        uint16
	seen, valid bool
	gap         bool // unrepaired loss since the previous picture
	capture     time.Time
	clock       frameClock
	ids         pictureIDs
	picture     uint64 // last picture number seen
	hasPicture  bool
	tl0         uint8
	hasTL0      bool
	missing     [16]uint64 // flexible: recently missing picture numbers, +1
	missingN    int
}

// padding keeps sequence continuity across padding-only packets.
func (a *assembler) padding(pkt *rtp.Packet) {
	if a.seen && pkt.SequenceNumber != a.last+1 {
		a.gap, a.valid = true, false
	}
	a.seen, a.last = true, pkt.SequenceNumber
}

func (a *assembler) push(now time.Time, pkt *rtp.Packet) {
	if a.seen && pkt.SequenceNumber != a.last+1 {
		a.gap = true
		if len(a.packets) > 0 && pkt.Timestamp == a.ts {
			a.valid = false
		}
	}
	a.seen, a.last = true, pkt.SequenceNumber
	if len(a.packets) > 0 && pkt.Timestamp != a.ts {
		a.finish(now, false) // ended without its marker
	}
	if len(a.packets) == 0 {
		var d codecs.VP9Packet
		_, err := d.Unmarshal(pkt.Payload)
		a.ts, a.valid = pkt.Timestamp, err == nil && d.B
		a.capture = a.clock.observe(pkt.Timestamp, now)
	}
	a.packets = append(a.packets, pkt)
	if len(a.packets) >= maxPicturePackets {
		a.finish(now, false)
	} else if pkt.Marker {
		a.finish(now, a.valid)
	}
}

func (a *assembler) finish(now time.Time, complete bool) {
	pkts := a.packets
	a.packets = nil
	info, d, ok := a.describe(pkts, complete)
	if ok && complete && a.hasPicture && info.Picture == a.picture {
		ok = false // a second picture under the same picture ID: no boundary
	}
	if !ok {
		a.gap = true // unidentifiable: the next picture's gap checks decide
		if complete {
			// An unreadable descriptor is an unsupported structure: viewers
			// forwarding this rendition refuse it rather than pass it on.
			a.emit(now, &picture{rend: a.rend, packets: pkts, info: ctrl.FrameInfo{Rendition: a.rend, TID: 255}})
		}
		return
	}
	if !complete {
		a.lost(now, info)
		a.gap = true
		a.note(info, d)
		return
	}
	if info.Key {
		a.missingN = 0
	} else if a.gap && a.hasPicture && info.Picture > a.picture+1 {
		a.missed(now, info)
	}
	if info.Flexible && !info.Key && info.TID == 0 {
		for _, ref := range info.Refs {
			if a.isMissing(ref) {
				a.lose(now, ctrl.FrameInfo{Rendition: a.rend, Picture: ref, Flexible: true})
			}
		}
	}
	a.gap = false
	a.note(info, d)
	wire := 0
	for _, q := range pkts {
		wire += q.MarshalSize() + wireOverhead
	}
	pic := &picture{rend: a.rend, packets: pkts, info: info, switchPoint: info.Key || info.TID == 0, wire: wire, capture: a.capture, deadline: a.capture.Add(ctrl.FrameDeadline)}
	if info.Key {
		pic.deadline = now.Add(ctrl.KeyEnvelope) // assembly completion is the key's delivery origin
	}
	a.emit(now, pic)
}

// lost reports a damaged picture; flexible pictures are remembered so a
// later base referencing them is known to be broken.
func (a *assembler) lost(now time.Time, info ctrl.FrameInfo) {
	a.lose(now, info)
	if info.Flexible {
		a.remember(info.Picture)
	}
}

// missed handles wholly missing pictures before info. Non-flexible L1T3
// exposes a missing base through TL0PICIDX; anything else missing is treated
// as a TL1 loss (forward the base only until the next TL0). Flexible pictures
// carry their references, so missing ones are only remembered.
func (a *assembler) missed(now time.Time, info ctrl.FrameInfo) {
	if info.Flexible {
		for m := max(a.picture+1, info.Picture-uint64(len(a.missing))); m < info.Picture; m++ {
			a.remember(m)
		}
		return
	}
	lost := ctrl.FrameInfo{Rendition: a.rend, Picture: info.Picture - 1, TID: 1}
	if a.hasTL0 && (info.TID == 0 && info.TL0 != a.tl0+1 || info.TID > 0 && info.TL0 != a.tl0) {
		lost.TID = 0
	}
	a.lose(now, lost)
}

func (a *assembler) note(info ctrl.FrameInfo, d codecs.VP9Packet) {
	a.picture, a.hasPicture = info.Picture, true
	if d.L && !d.F && info.TID == 0 {
		a.tl0, a.hasTL0 = d.TL0PICIDX, true
	}
}

func (a *assembler) remember(n uint64) {
	a.missing[a.missingN%len(a.missing)] = n + 1
	a.missingN++
}

func (a *assembler) isMissing(n uint64) bool {
	for _, m := range a.missing[:min(a.missingN, len(a.missing))] {
		if m == n+1 {
			return true
		}
	}
	return false
}

// describe reads a picture's clear VP9 descriptor from every packet. Each
// packet's descriptor must parse and agree with the first on picture ID,
// mode, prediction and layer indices (and references or TL0PICIDX), and a
// complete picture must begin and end exactly at its first and last packet;
// otherwise a malformed or unsupported tail could ride on a valid head
// (C2-4). Only the clear descriptor is read, never the encrypted payload. A
// descriptor without a picture ID or layer indices cannot be checked against
// the supported structures; it is presented to continuity as TID 255, which
// refuses it.
func (a *assembler) describe(pkts []*rtp.Packet, complete bool) (ctrl.FrameInfo, codecs.VP9Packet, bool) {
	var d codecs.VP9Packet
	if len(pkts) == 0 {
		return ctrl.FrameInfo{}, d, false
	}
	if _, err := d.Unmarshal(pkts[0].Payload); err != nil || !d.I {
		return ctrl.FrameInfo{}, d, false
	}
	wide := len(pkts[0].Payload) > 1 && pkts[0].Payload[1]&0x80 != 0
	for i, q := range pkts {
		var e codecs.VP9Packet
		if _, err := e.Unmarshal(q.Payload); err != nil || !sameLayerFrame(d, e) || (len(q.Payload) > 1 && q.Payload[1]&0x80 != 0) != wide {
			return ctrl.FrameInfo{}, d, false
		}
		if complete && (e.B != (i == 0) || e.E != (i == len(pkts)-1)) {
			return ctrl.FrameInfo{}, d, false
		}
	}
	mask := uint16(0x7f)
	if wide {
		mask = 0x7fff
	}
	info := ctrl.FrameInfo{Rendition: a.rend, Picture: a.ids.unwrap(d.PictureID, mask), TID: d.TID, SID: d.SID, Key: !d.P, Flexible: d.F, TL0: d.TL0PICIDX}
	if !d.L {
		info.TID = 255
	}
	for _, diff := range d.PDiff {
		if diff == 0 || uint64(diff) > info.Picture {
			info.TID = 255
			break
		}
		info.Refs = append(info.Refs, info.Picture-uint64(diff))
	}
	return info, d, true
}

// sameLayerFrame reports whether descriptor e (a later packet) describes the
// same picture and layer frame as d (its first packet).
func sameLayerFrame(d, e codecs.VP9Packet) bool {
	if !e.I || e.PictureID != d.PictureID || e.P != d.P || e.F != d.F || e.L != d.L {
		return false
	}
	if d.L && (e.TID != d.TID || e.U != d.U || e.SID != d.SID || e.D != d.D || !d.F && e.TL0PICIDX != d.TL0PICIDX) {
		return false
	}
	return slices.Equal(e.PDiff, d.PDiff)
}

// pictureIDs extends 7- or 15-bit VP9 picture IDs to picture numbers.
type pictureIDs struct {
	seen        bool
	value, mask uint16
	number      uint64
}

func (h *pictureIDs) unwrap(value, mask uint16) uint64 {
	if !h.seen || h.mask != mask {
		// Start well above zero so references before the first picture stay positive.
		h.number, h.seen, h.mask = 1<<16+uint64(value), true, mask
	} else {
		h.number += uint64((value - h.value) & mask)
	}
	h.value = value
	return h.number
}

// distribute offers an assembled picture to every viewer. Keys are reported
// to the room's key policy at assembly, before any viewer forwards them.
func (r *Room) distribute(p *Peer, now time.Time, pic *picture) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.publisher != p {
		return
	}
	pic.source = r.source
	if pic.info.Key {
		r.keys.OnKey(now, pic.rend, pic.wire, pic.capture)
	}
	if pic.rend == ctrl.Native {
		r.Frames.Add(1)
		if pic.info.Key {
			r.Keyframes.Add(1)
			r.keyBytes.Store(int64(pic.wire))
		}
		r.frameTimes[r.frameHead] = now.UnixMilli()
		r.frameHead = (r.frameHead + 1) % len(r.frameTimes)
		r.frameN = min(r.frameN+1, len(r.frameTimes))
	}
	for v := range r.viewers {
		if v.view != nil {
			v.view.offer(now, pic)
		}
	}
}

func (r *Room) ingressLost(p *Peer, now time.Time, info ctrl.FrameInfo) {
	r.Damaged.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.publisher != p {
		return
	}
	for v := range r.viewers {
		if v.view != nil {
			v.view.ingressLost(now, info)
		}
	}
}
