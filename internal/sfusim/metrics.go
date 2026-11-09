package sfusim

import (
	"slices"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// G2G is a rendered frame's glass-to-glass latency: modelled network and
// playout time plus the source profile's calibrated endpoint delay.
func (f Rendered) G2G() time.Duration { return f.At - f.Capture + f.Endpoint }

// Window is a scoring interval.
type Window struct{ From, To time.Duration }

func (w Window) has(t time.Duration) bool { return t >= w.From && t <= w.To }

// Frames returns rendered frames inside w.
func Frames(tl []Rendered, w Window) []Rendered {
	var out []Rendered
	for _, f := range tl {
		if w.has(f.At) {
			out = append(out, f)
		}
	}
	return out
}

// Fresh keeps frames with G2G ≤ 1 s, as freshTimeline does.
func Fresh(tl []Rendered, w Window) []Rendered {
	var out []Rendered
	for _, f := range Frames(tl, w) {
		if f.G2G() <= time.Second {
			out = append(out, f)
		}
	}
	return out
}

// Gaps lists inter-frame gaps over 500 ms including both window edges, as
// summarize() does.
func Gaps(frames []Rendered, w Window) []time.Duration {
	var gaps []time.Duration
	prev := w.From
	for _, f := range frames {
		if f.At-prev > 500*time.Millisecond {
			gaps = append(gaps, f.At-prev)
		}
		prev = f.At
	}
	if w.To-prev > 500*time.Millisecond {
		gaps = append(gaps, w.To-prev)
	}
	return gaps
}

// MaxGap is the largest gap (0 if none exceeds 500 ms).
func MaxGap(frames []Rendered, w Window) time.Duration {
	m := time.Duration(0)
	for _, g := range Gaps(frames, w) {
		m = max(m, g)
	}
	return m
}

// Pct is the nearest-rank percentile used by the browser harness.
func Pct(v []time.Duration, p float64) time.Duration {
	if len(v) == 0 {
		return -1
	}
	s := slices.Clone(v)
	slices.Sort(s)
	i := int(float64(len(s))*p+0.999999) - 1
	return s[min(len(s)-1, max(0, i))]
}

// G2Gs returns frame latencies.
func G2Gs(frames []Rendered) []time.Duration {
	out := make([]time.Duration, len(frames))
	for i, f := range frames {
		out[i] = f.G2G()
	}
	return out
}

// Samples returns samples inside w.
func Samples(v *ViewerResult, w Window) []Sample {
	var out []Sample
	for _, s := range v.Samples {
		if w.has(s.At) {
			out = append(out, s)
		}
	}
	return out
}

// NativeShare is the fraction of samples in w showing native pixels at any
// TID (full = true: native and forwarding T2). docs/GATES.md "Definitions".
func NativeShare(v *ViewerResult, w Window, full bool) float64 {
	s := Samples(v, w)
	if len(s) == 0 {
		return 0
	}
	n := 0
	for _, x := range s {
		if x.Native && (!full || x.Full) {
			n++
		}
	}
	return float64(n) / float64(len(s))
}

// FirstNative is the delay from w.From to the first native (or full) sample, or -1.
func FirstNative(v *ViewerResult, w Window, full bool) time.Duration {
	for _, s := range Samples(v, w) {
		if s.Native && (!full || s.Full) {
			return s.At - w.From
		}
	}
	return -1
}

// LongestNonNative is the longest run of consecutive non-native samples.
func LongestNonNative(v *ViewerResult, w Window) time.Duration {
	longest, start := time.Duration(0), time.Duration(-1)
	s := Samples(v, w)
	for i, x := range s {
		if !x.Native && start < 0 {
			start = x.At
		}
		if start >= 0 && (x.Native || i == len(s)-1) {
			longest = max(longest, x.At-start)
			start = -1
		}
	}
	return longest
}

// counters returns the difference of cumulative counters across w.
func counters(v *ViewerResult, w Window) (a, b Sample, ok bool) {
	s := Samples(v, w)
	if len(s) < 2 {
		return
	}
	return s[0], s[len(s)-1], true
}

// PaddingShare is padding bytes over video wire bytes in w.
func PaddingShare(v *ViewerResult, w Window) float64 {
	a, b, ok := counters(v, w)
	if !ok || b.WireVideo == a.WireVideo {
		return 0
	}
	return float64(b.Padding-a.Padding) / float64(b.WireVideo-a.WireVideo)
}

// UniqueRate is unique original media bps at the receiver over w.
func UniqueRate(v *ViewerResult, w Window) float64 {
	a, b, ok := counters(v, w)
	if !ok {
		return 0
	}
	return float64(b.Unique-a.Unique) * 8 / (b.At - a.At).Seconds()
}

// ReceivedRate is media+RTX+FEC bps at the receiver over w.
func ReceivedRate(v *ViewerResult, w Window) float64 {
	a, b, ok := counters(v, w)
	if !ok {
		return 0
	}
	return float64(b.Received-a.Received) * 8 / (b.At - a.At).Seconds()
}

// SourceFrames counts captured pictures of a rendition in w.
func SourceFrames(r *Result, rend ctrl.Rendition, w Window) int {
	n := 0
	for _, t := range r.Frames[rend] {
		if w.has(t) {
			n++
		}
	}
	return n
}

// OfferedRate is the native source's nominal media bps.
func OfferedRate(sc *Scenario) float64 {
	return sc.Native.Layer[0] + sc.Native.Layer[1] + sc.Native.Layer[2]
}

// LowRate is the low rendition's mean video bps.
func LowRate(sc *Scenario) float64 {
	low := sc.Low
	if low.FPS == 0 {
		low = LowProfile
	}
	return low.Layer[0] + low.Layer[1] + low.Layer[2]
}

// TCPGoodput is a flow's delivered bps over w.
func TCPGoodput(r *Result, flow int, w Window) float64 {
	var a, b *TCPSample
	for i := range r.TCP[flow] {
		s := &r.TCP[flow][i]
		if w.has(s.At) {
			if a == nil {
				a = s
			}
			b = s
		}
	}
	if a == nil || a == b {
		return 0
	}
	return float64(b.Bytes-a.Bytes) * 8 / (b.At - a.At).Seconds()
}

// ProbeDelays returns queue-probe residence times in w.
func ProbeDelays(r *Result, w Window) []time.Duration {
	var out []time.Duration
	for _, p := range r.Probe {
		if w.has(p.At) {
			out = append(out, p.Delay)
		}
	}
	return out
}

// NativeKeys counts native keyframes completing ingress in w.
func NativeKeys(r *Result, w Window) int {
	n := 0
	for _, k := range r.Keys {
		if k.Rend == ctrl.Native && w.has(k.At) {
			n++
		}
	}
	return n
}
