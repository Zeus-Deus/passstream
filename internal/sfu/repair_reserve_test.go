package sfu

import (
	"math"
	"testing"
)

func TestRepairReservationCoversParityAndReplayTogether(t *testing.T) {
	const wire, repair = 100_000, 60_000
	reserve := repairReserve(wire, repair)
	if math.Abs(reserve-.65) > 1e-9 {
		t.Fatal("combined parity and replay were clipped to a parity-only reserve", reserve)
	}
	if budget := nativeWireRequired(wire-repair, reserve); budget < wire {
		t.Fatal("measured repair still starved its already delivered media", budget)
	}
	if admittedLayer([3]int{10_000, 10_000, 20_000}, int(nativeWireRequired(40_000, reserve)), reserve) != 2 {
		t.Fatal("accounting for repair revoked the full media layer")
	}
}

func TestRepairReservationStaysBoundedAndPreservesQuietPaths(t *testing.T) {
	for _, tc := range []struct {
		wire, repair uint64
		want         float64
	}{
		{0, 0, .1}, {100_000, 0, .1}, {100_000, 20_000, .25}, {100_000, 100_000, .7},
	} {
		if got := repairReserve(tc.wire, tc.repair); math.Abs(got-tc.want) > 1e-9 {
			t.Fatal("wrong bounded repair reservation", tc, got)
		}
	}
}
