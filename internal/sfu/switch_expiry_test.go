package sfu

import (
	"github.com/passstream/passstream/internal/sfu/ctrl"
	"testing"
	"time"
)

type expiryKeys struct {
	ctrl.Keys
	broken int
}

func (k *expiryKeys) Request(now time.Time, id int, r ctrl.Rendition, c ctrl.Cause) ctrl.KeyStatus {
	s := k.Keys.Request(now, id, r, c)
	if c == ctrl.CauseBroken && s != ctrl.KeyIgnored {
		k.broken++
	}
	return s
}
func TestOptionalExpiryPreservesUnstartedDecoder(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unstarted", true: "started-abort-control"}[committed], func(t *testing.T) {
			h := newHarness(t)
			v, w := h.viewer()
			a := &keySwitchAllocation{desired: ctrl.Option{Rendition: ctrl.Low, TID: 2}}
			k := &expiryKeys{Keys: v.ctl.Keys}
			v.ctl.Keys = k
			v.ctl.Alloc = a
			v.ctl.Probe = ctrl.NoProber{}
			v.ctl.Est = &ctrl.FixedEstimator{Rate: 1_500_000}
			v.step(h.now)
			lowAt := h.now
			h.send(ctrl.Low, desc{key: true, flexible: true}, 2, 500)
			h.drain(v, 100*time.Millisecond)
			a.desired = ctrl.Option{Rendition: ctrl.Native, TID: 0}
			v.step(h.now)
			admitted := h.now
			h.send(ctrl.Native, desc{key: true, flexible: true}, 200, 1176)
			if committed {
				h.drain(v, 10*time.Millisecond)
			}
			oldLowIntact := v.forwarding(ctrl.Low)
			x, transactional := any(v.ctl).(interface {
				BeforeSend(time.Time, int, ctrl.Option, ctrl.Costs)
			})
			if transactional {
				x.BeforeSend(h.now, 1, a.desired, h.r.costs(h.now))
			}
			h.now = admitted.Add(ctrl.KeyEnvelope)
			if transactional {
				x.BeforeSend(h.now, 1, a.desired, h.r.costs(h.now))
			} else {
				v.ctl.Pacer.Pop(h.now)
			}
			v.step(h.now)
			// Advance the RTP clock with elapsed source time after this idle interval.
			h.ts[ctrl.Low] += uint32(h.now.Sub(lowAt)*90000/time.Second) - 6000
			before := len(w.written())
			h.send(ctrl.Low, desc{flexible: true, pdiff: []uint8{1}}, 1, 300)
			h.drain(v, 10*time.Millisecond)
			t.Logf("transactional=%v started=%v lowIntact=%v acceptedCauseBroken=%d freshLowPackets=%d", transactional, committed, oldLowIntact, k.broken, len(w.written())-before)
			if committed && k.broken == 0 {
				t.Fatal("real interrupted key lost unchanged hard-abort recovery")
			}
			if transactional && !committed && oldLowIntact && k.broken != 0 {
				t.Fatal("optional key expired without starting, but requested broken-chain recovery for an intact low decoder")
			}
		})
	}
}
