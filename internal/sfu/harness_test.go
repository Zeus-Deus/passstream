package sfu

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// harness drives a room's ingress assembler and its viewers' forwarders on
// an explicit clock, without PCs or the writer goroutine.

type recordWriter struct {
	mu    sync.Mutex
	pkts  []*rtp.Packet
	block chan struct{}
}

func (w *recordWriter) WriteRTP(h *rtp.Header, b []byte) (int, error) {
	if w.block != nil {
		<-w.block
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pkts = append(w.pkts, &rtp.Packet{Header: h.Clone(), Payload: bytes.Clone(b)})
	return len(b), nil
}
func (w *recordWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *recordWriter) written() []*rtp.Packet {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*rtp.Packet(nil), w.pkts...)
}

// desc is one picture's VP9 payload descriptor (RFC 9628).
type desc struct {
	tid, sid      uint8
	key, flexible bool
	noLayer       bool
	pdiff         []uint8
}

type harness struct {
	t   *testing.T
	r   *Room
	pub *Peer
	asm [2]*assembler
	seq [2]uint16
	ts  [2]uint32
	pid [2]uint16
	tl0 [2]uint8
	now time.Time
}

func newHarness(t *testing.T) *harness {
	r := newRoom()
	pub := &Peer{Room: r, Publish: true, done: make(chan struct{})}
	r.publisher = pub
	h := &harness{t: t, r: r, pub: pub, now: time.Now()}
	for rend := range h.asm {
		h.asm[rend] = &assembler{rend: ctrl.Rendition(rend),
			emit: func(now time.Time, p *picture) { r.distribute(pub, now, p) },
			lose: func(now time.Time, info ctrl.FrameInfo) { r.ingressLost(pub, now, info) }}
	}
	return h
}

// packets builds one picture of n packets with body payload bytes each.
// Non-flexible pictures must follow the measured 0,2,1,2 pattern after a key.
func (h *harness) packets(rend ctrl.Rendition, d desc, n, body int) []*rtp.Packet {
	h.pid[rend]++
	h.ts[rend] += 6000
	if d.tid == 0 && !d.key {
		h.tl0[rend]++
	}
	var out []*rtp.Packet
	for i := range n {
		b0 := byte(0x80)
		if !d.noLayer {
			b0 |= 0x20
		}
		if !d.key {
			b0 |= 0x40
		}
		if d.flexible {
			b0 |= 0x10
		}
		if i == 0 {
			b0 |= 0x08
		}
		if i == n-1 {
			b0 |= 0x04
		}
		pl := []byte{b0, 0x80 | byte(h.pid[rend]>>8), byte(h.pid[rend])}
		if !d.noLayer {
			pl = append(pl, d.tid<<5|0x10|d.sid<<1)
			if !d.flexible {
				pl = append(pl, h.tl0[rend])
			}
		}
		if d.flexible && !d.key {
			for j, p := range d.pdiff {
				next := byte(0)
				if j < len(d.pdiff)-1 {
					next = 1
				}
				pl = append(pl, p<<1|next)
			}
		}
		pl = append(pl, bytes.Repeat([]byte{byte(i + 1)}, body)...)
		h.seq[rend]++
		out = append(out, &rtp.Packet{Header: rtp.Header{Version: 2, SSRC: 1000 + uint32(rend), SequenceNumber: h.seq[rend], Timestamp: h.ts[rend], Marker: i == n-1}, Payload: pl})
	}
	return out
}

func (h *harness) send(rend ctrl.Rendition, d desc, n, body int) {
	for _, p := range h.packets(rend, d, n, body) {
		h.asm[rend].push(h.now, p)
	}
}

// viewer adds an admitted viewer that has joined: its first Step asked for low.
func (h *harness) viewer() (*viewer, *recordWriter) {
	p := &Peer{Room: h.r, done: make(chan struct{})}
	p.admitted.Store(true)
	w := &recordWriter{}
	video := &boundTrack{binding: binding{writer: w, ssrc: 10, mediaPT: 98, twccID: 3}}
	audio := &boundTrack{binding: binding{writer: w, ssrc: 20, mediaPT: audioPayloadType, twccID: 3}}
	p.view = newViewer(p, h.r, video, audio)
	h.r.mu.Lock()
	h.r.viewers[p] = true
	h.r.mu.Unlock()
	v := p.view
	v.mu.Lock()
	v.joined = true
	v.step(h.now)
	v.mu.Unlock()
	return v, w
}

// drain runs the writer loop on the harness clock without stepping.
func (h *harness) drain(v *viewer, d time.Duration) {
	for end := h.now.Add(d); h.now.Before(end); h.now = h.now.Add(5 * time.Millisecond) {
		v.mu.Lock()
		batch := v.drain(h.now, nil)
		v.mu.Unlock()
		for _, w := range batch {
			_, _ = w.writer.WriteRTP(&w.h, w.payload)
		}
	}
}

func pacerQueue(v *viewer) (int, int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.ctl.Pacer.Queued()
}

func pendingRenditions(v *viewer) map[ctrl.Rendition]int {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[ctrl.Rendition]int{}
	for _, o := range v.pending {
		if o.kind == outMedia {
			out[o.frame.info.Rendition]++
		}
	}
	return out
}
