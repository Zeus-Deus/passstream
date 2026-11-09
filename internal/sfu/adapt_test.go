package sfu

import (
	"testing"
	"time"
)

func TestRepairedRandomLossRetainsDeliveredCapacityAcrossStalls(t *testing.T) {
	for _, productive := range []bool{false, true} {
		for _, loss := range []float64{.05, .1, .2, .3} {
			now := time.Unix(100, 0)
			c := wireController{rate: 2_000_000, proof: 2_000_000}
			for i := 0; i < 100; i++ {
				at := now.Add(time.Duration(i) * 100 * time.Millisecond)
				c.step(at, rateObservation{reports: uint64(i + 1), delayAt: at, loss: loss, residual: .03, received: 1_000_000, mediaDemand: 400_000, required: 2_000_000, demand: 2_000_000, productive: productive, stalled: !productive})
			}
			if c.rate != 2_000_000 || !c.cutAt.IsZero() {
				t.Fatal("repaired random loss discarded funded repair", productive, loss, c.rate)
			}
		}
	}
}

func TestUnproductiveLossWithInsufficientDeliveryCutsTheOfferedRate(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 2_000_000}
	for i := 0; i < 20; i++ {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{reports: uint64(i + 1), loss: .2, residual: .05, received: 200_000, mediaDemand: 1_000_000, required: 1_200_000, demand: 2_000_000, stalled: true})
	}
	if c.cutAt.IsZero() || c.rate > 200_000 {
		t.Fatal("unproductive overload retained an optimistic ceiling", c.rate)
	}
}

func TestNumericalCutsCannotCompoundTheWireTrialBackoff(t *testing.T) {
	now := time.Unix(100, 0)
	c := wireController{rate: 2_000_000}
	for i := 0; i < 10; i++ {
		c.step(now.Add(time.Duration(i)*100*time.Millisecond), rateObservation{reports: uint64(i + 1), excess: standingDelayLimit + 100*time.Millisecond, received: 300_000, mediaDemand: 1_000_000, demand: 2_000_000})
	}
	if c.cutAt.IsZero() || c.probeAt.Sub(c.cutAt) != minBackoff {
		t.Fatal("numerical cuts inherited a second exponential backoff", c.probeAt, c.cutAt)
	}
	c.step(now.Add(time.Minute), rateObservation{reports: 100, received: 2_000_000, mediaDemand: 1_000_000, required: 1_200_000, demand: 2_000_000, productive: true})
	if c.rate < 1_200_000 {
		t.Fatal("clear delivery could not restore its reservation", c.rate)
	}
}
