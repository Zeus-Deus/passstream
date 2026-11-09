package ctrl

import "time"

// Every tunable of the controller lives here (docs/CONTROLLER.md). The budget
// is 40, each with one meaning; do not alias. A new or changed value must cite
// the internal/sfusim scenario it fixes.
const (
	// Estimator.
	RateMin            = 128_000                // bps; estimate floor ; A6 viewer_256k / A16 collapse
	RateInit           = 300_000                // bps; estimate before evidence; app-limited floor ; A1 good_join
	IncreasePerSecond  = 1.08                   // normal-state multiplicative growth ; A4 b1
	Backoff            = 0.85                   // congestion: R = max(RateMin, min(R, Backoff × acked)) ; A10 FIFO
	OveruseSlope       = 12.5                   // ms/s; fixed delay-slope threshold ; A7 rtt_step / A10 FIFO
	OveruseTime        = 100 * time.Millisecond // A10 FIFO
	TrendWindow        = time.Second            // A7 rtt_step
	AckedWindow        = 500 * time.Millisecond // A10 FIFO
	AppLimitedLookback = 2 * time.Second        // A4 b1
	AppLimitedFactor   = 1.1                    // growth past proven delivery bursts pictures into the bottleneck and trips overuse; probes discover above it ; A4 b1_3m
	CongestiveLoss     = 0.10                   // with delay evidence ; A10 FIFO
	LossWindow         = time.Second            // A2 bad_wifi
	PathWindow         = 10 * time.Second       // A7 rtt_step
	ProbeEvidenceTTL   = 30 * time.Second       // A5 b1_flap

	// Prober.
	ProbeLength      = 100 * time.Millisecond // A13 loss_prejoin / F-1
	ProbeMinPackets  = 5                      // A17 tl1_loss / F-1
	ProbeTimeout     = time.Second            // A8 outage
	ProbeBackoffMin  = 2 * time.Second        // A4 b1
	ProbeBackoffMax  = 15 * time.Second       // worst-case return detection: 15 s + probe + key ≤ 20 s ; A5 b1_flap / A18 native_1500k_return
	PaddingShare     = 0.10                   // A4 b1
	PaddingWindow    = 30 * time.Second       // A4 b1
	BootstrapPadding = 96_000                 // bytes per path generation, reported: three 100 ms rung probes (T0/T1/T2 ≈ 76 KB on 50 Mbit/s) ; A1 good_join / F-1
	ProbeHeadroom    = 1.1                    // offered ÷ target before loss compensation: dispersion margin ; A18 native_1500k / A1 good_join

	// Allocator.
	CostCycles     = 16                     // complete temporal cycles per ingress cost estimate ; A1 good_join / A18 native_1500k
	UpMargin       = 1.15                   // A4 b1 / A18 native_1500k
	DownTime       = 500 * time.Millisecond // A5 b1_flap
	TemporalUpTime = time.Second            // A4 b1
	RenditionDwell = 2 * time.Second        // A5 b1_flap
	KeyEnvelope    = 2 * time.Second        // A4k b1_3m_560k
	KeyFast        = 400 * time.Millisecond // switch rung: a low viewer's upgrade key delivered within this keeps the switch freeze short ; A1 good_join / A8 outage

	// Pacer.
	PacerBurst  = 10 * time.Millisecond  // A10 FIFO
	BurstFloor  = 1_260                  // bytes; one MTU-size RTP packet ; A6 viewer_256k
	FrameAge    = 100 * time.Millisecond // A3 isolation
	QueueBytes  = 1 << 20                // A16 collapse
	QueueFrames = 64                     // A16 collapse

	// Repair.
	NACKMaxSends  = 4           // 0.2^(n+1) per packet unrecovered at 20% loss; a 31-packet TL0 needs n ≥ 4 for ~1% ; A13 loss_prejoin_20 / A12 loss
	FrameDeadline = time.Second // A12 loss
	FECLossOn     = 0.01        // A2 bad_wifi
	FECRatio      = 0.25        // A13 loss_prejoin
	FECBurstRun   = 3           // packets; mean loss run that doubles and interleaves FEC ; A2 bad_wifi

	// Keys.
	KeyInterval   = 3 * time.Second // A14 join_storm
	KeyRetry      = time.Second     // A8 outage
	RecoveryQuiet = 2 * time.Second // A9 spam

	// Key-size admission (orchestrator decision on the C0 r3 admission
	// deadlock): a robust estimate over recent native keys, and a bounded
	// per-viewer back-off after an actual key is refused.
	KeySizeSamples  = 8                // most recent assembled keys per rendition ; A18 oversize regression
	KeySizeAge      = 60 * time.Second // older keys are forgotten ; A18 oversize regression
	KeySizeQuantile = 0.75             // nearest-rank quantile of the retained sizes ; A18 oversize regression
	KeyRefusedMin   = 3 * time.Second  // first back-off after an assembled key did not fit ; A4k b1_3m_560k
	KeyRefusedMax   = 30 * time.Second // back-off ceiling ; A18 oversize regression
)

// Configuration defaults are reviewed separately from the 40 fixed constants.
// ProbeMaxOverdrive: A13 loss_prejoin and F-1 equal-spacing loss regressions.
func defaultConfig() Config { return Config{MaxRate: 50_000_000, ProbeMaxOverdrive: 1.5} }
