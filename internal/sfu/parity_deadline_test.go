package sfu

import (
	"testing"
	"time"
)

func TestExtraParityFitsTheRTTScaledRepairBuffer(t *testing.T) {
	for _, rtt := range []time.Duration{50, 63, 80, 117, 149} {
		rtt *= time.Millisecond
		delay := extraParityDelay(rtt)
		// The client already budgets propagation in the received picture. Keep
		// another paced MTU (<=20ms at 3.3x 200k) inside its repair allowance.
		buffer := max(150*time.Millisecond, 65*time.Millisecond+rtt*3/2)
		if delay+20*time.Millisecond > buffer {
			t.Fatal("second parity can arrive after repair playout", rtt, delay, buffer)
		}
		if delay < 100*time.Millisecond || delay > 160*time.Millisecond || delay-80*time.Millisecond < 20*time.Millisecond {
			t.Fatal("second parity lost bounded time diversity", rtt, delay)
		}
	}
}
