package hostview

import "math"

// Health is an allowlist of owner-visible sender telemetry, never secrets.
type Health struct {
	UplinkKbps        *uint64  `json:"uplink_kbps"`
	EncodeMS          *float64 `json:"encode_ms"`
	FPS               *float64 `json:"fps"`
	QualityLimitation string   `json:"quality_limitation"`
	EncodeCPUPct      *float64 `json:"encode_cpu_pct,omitempty"`
	Width             *uint64  `json:"width,omitempty"`
	Height            *uint64  `json:"height,omitempty"`
}

// Valid bounds telemetry before it reaches any owner surface.
func (h Health) Valid() bool {
	within := func(v *float64, max float64) bool {
		return v == nil || !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v >= 0 && *v <= max
	}
	return within(h.EncodeMS, 60000) && within(h.FPS, 1000) && within(h.EncodeCPUPct, 100) &&
		(h.UplinkKbps == nil || *h.UplinkKbps <= 1<<53-1) &&
		(h.Width == nil || *h.Width > 0 && *h.Width <= 16384) && (h.Height == nil || *h.Height > 0 && *h.Height <= 16384)
}
