package ctrl

import "math"

// DeadlineRepair implements A2/A12/A13's burst/deadline inputs without
// looking inside either encrypted media kind.
type DeadlineRepair struct{}

// Resend answers one repair trigger, a receiver NACK or a TWCC loss report
// of a critical packet (C2 §5 A12/A13: early critical retransmission), with
// the same per-packet bound. A copy sent less than one RTT ago is still in
// flight: a second trigger for it (the NACK after an early retransmission,
// or the reverse) is a duplicate, not new loss evidence.
func (DeadlineRepair) Resend(q RepairQuery) bool {
	return q.Current && q.Sends < NACKMaxSends && q.PredictedArrival.Before(q.Deadline) && (q.Sends == 0 || q.Since >= q.RTT)
}

// FEC protects TL0 and key packets. A group of n media packets and one
// parity fails when two or more of its packets are lost; at the measured
// loss L a one-per-1/FECRatio group fails more often than an unprotected
// packet is lost once L ≈ 14% (26% against 20% at L = 20%), so the repair
// that follows each failure (≥ one RTT per round, against the frame's
// deadline) is no longer the exception. The group then halves (one parity
// per two media packets fails 10% of the time at 20%; C2 §5 A12/A13).
// Bursts and NACK infeasibility double the ratio and interleave as before.
func (DeadlineRepair) FEC(e Estimate, feasible bool) (float64, int) {
	if e.Loss < FECLossOn {
		return 0, 1
	}
	if feasible && e.LossRun < FECBurstRun && groupFailure(int(math.Round(1/FECRatio)), e.Loss) <= e.Loss {
		return FECRatio, 1
	}
	return 2 * FECRatio, max(1, int(math.Ceil(e.LossRun)))
}

// groupFailure is the probability that a parity group of n media packets
// and one parity loses two or more packets to independent loss l.
func groupFailure(n int, l float64) float64 {
	k := float64(n + 1)
	return 1 - math.Pow(1-l, k) - k*l*math.Pow(1-l, k-1)
}

// FECGroup is the parity group size for ratio and the interleave depth an
// encoder can actually use for it. A level-0 ULPFEC mask covers 16
// consecutive sequence numbers (RFC 5109 §7.4), so a complete group of size
// packets spread depth apart needs depth × (size − 1) < 16. A format limit,
// not a tunable: a mean loss run, which the receiver reports, cannot allocate
// more interleave buckets than the mask can protect (C2-1).
func FECGroup(ratio float64, interleave int) (size, depth int) {
	if ratio <= 0 {
		return 0, 1
	}
	size = max(1, int(math.Round(1/ratio)))
	if size == 1 {
		return 1, 1
	}
	return size, max(1, min(interleave, 15/(size-1)))
}
