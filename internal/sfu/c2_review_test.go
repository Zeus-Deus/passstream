package sfu

import (
	"github.com/passstream/passstream/internal/sfu/ctrl"
	"testing"
	"time"
)

func TestC2ReviewPendingPayloadBound(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	for i := 0; i < 2000; i++ {
		h.now = h.now.Add(ctrl.KeyInterval)
		h.send(ctrl.Low, desc{key: true}, 8, 1000)
	}
	b, n := pacerQueue(v)
	var payload int
	for _, p := range v.pending {
		payload += len(p.pkt.Payload)
	}
	t.Logf("queue %d B/%d frames; pending map %d packets/%d payload B", b, n, len(v.pending), payload)
	if payload > ctrl.QueueBytes {
		t.Error("purged payloads retained when writer cannot Step")
	}
	_ = time.Second
}

func TestC2ReviewTailDescriptorMustNotBypassRejection(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	pkts := h.packets(ctrl.Low, desc{key: true}, 2, 500)
	// The first descriptor is supported; the last packet advertises TID=3.
	pkts[1].Payload[3] = (pkts[1].Payload[3] & 0x1f) | (3 << 5)
	for _, p := range pkts {
		h.asm[ctrl.Low].push(h.now, p)
	}
	h.drain(v, 100*time.Millisecond)
	t.Logf("forwarded packets from picture with unsupported tail TID: %d", len(w.written()))
	if len(w.written()) != 0 {
		t.Fatal("unsupported packet descriptor forwarded")
	}
}
