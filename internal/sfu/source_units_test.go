package sfu

import (
	"github.com/passstream/passstream/internal/sfu/ctrl"
	"testing"
)

// The publisher diagnostic is assembled wire bytes, not encoded VP9 input
// bytes or SFrame output bytes. Keep that distinction when auditing archives.
func TestSourceKeyDiagnosticCountsAssembledWireIncludingDescriptors(t *testing.T) {
	h := newHarness(t)
	packets := h.packets(ctrl.Native, desc{key: true}, 200, 1175)
	wire, payload, protectedBody := 0, 0, 200*1175
	for _, p := range packets {
		wire += p.MarshalSize() + wireOverhead
		payload += len(p.Payload)
		h.asm[ctrl.Native].push(h.now, p)
	}
	t.Logf("protected body=%d RTP payload=%d assembled wire=%d", protectedBody, payload, wire)
	if got := int(h.r.keyBytes.Load()); got != wire || got <= payload || payload <= protectedBody {
		t.Fatalf("assembled wire=%d want%d > RTP payload%d > protected body%d", got, wire, payload, protectedBody)
	}
}
