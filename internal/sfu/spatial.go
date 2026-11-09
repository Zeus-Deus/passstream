package sfu

import (
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"time"
)

// Keep a proven upswitch open while its requested keyframe crosses the
// publisher leg. New queue growth or a reduced budget revokes it immediately.
type renditionAdmission struct{ until time.Time }

// A short base-rate transient after a refresh must not discard a working
// native decoder and force it to download another key on its return.
type renditionFallback struct{ since time.Time }

func (f *renditionFallback) update(now time.Time, low, wanted bool) bool {
	if low || !wanted {
		f.since = time.Time{}
		return wanted
	}
	if f.since.IsZero() {
		f.since = now
	}
	return now.Sub(f.since) >= time.Second
}

func (a *renditionAdmission) allow(now time.Time, proven, affordable bool) bool {
	if !affordable {
		a.until = time.Time{}
		return false
	}
	if a.until.IsZero() && proven {
		a.until = now.Add(2 * time.Second)
	}
	if a.until.IsZero() || !now.Before(a.until) {
		a.until = time.Time{}
		return false
	}
	return true
}

// L3T3_KEY carries independently predicted spatial layers between keyframes.
// Each viewer receives the base plus only the spatial layers it can afford.
// Upswitches wait for a keyframe; downswitches keep the already-decoded base.
func (r *Room) layerRates(spatial uint8) [3]int {
	rates := [3]int{}
	for tid := range rates {
		rates[tid] = int(r.tidRate[tid].Load())
		for sid := uint8(0); sid < spatial && sid < 2; sid++ {
			rates[tid] += int(r.spatialRate[sid][tid].Load())
		}
	}
	return rates
}
func (r *Room) admittedSpatial(budget int, reserve float64) uint8 {
	var fit uint8
	for sid := uint8(1); sid <= uint8(r.spatialMax.Load()); sid++ {
		rates := r.layerRates(sid)
		if float64(rates[0]+rates[1]+rates[2])*1.1 > float64(budget)*(1-reserve) {
			break
		}
		fit = sid
	}
	return fit
}

func completePicture(packets []*rtp.Packet) bool {
	if len(packets) == 0 {
		return false
	}
	var v codecs.VP9Packet
	_, err := v.Unmarshal(packets[len(packets)-1].Payload)
	return err == nil && v.E
}
func spatialEnd(packets []*rtp.Packet, spatial uint8) int {
	for i := len(packets) - 1; i >= 0; i-- {
		var v codecs.VP9Packet
		if _, err := v.Unmarshal(packets[i].Payload); err == nil && v.SID <= spatial {
			return i
		}
	}
	return -1
}

func (r *Room) thumbnailRates() [3]int {
	return [3]int{int(r.lowRate[0].Load()), int(r.lowRate[1].Load()), int(r.lowRate[2].Load())}
}

// Independent low-rate keyframes serve constrained viewers without forcing a
// full-resolution refresh on the other encoding. Joins share one 500ms request.
func (r *Room) lowPLI(join, gap bool) { r.lowPLIForGeneration(join, gap, 0) }

func (r *Room) lowPLIForGeneration(join, gap bool, generation uint64) {
	r.mu.Lock()
	if generation != 0 {
		if r.lowJoinTimer == nil || r.lowJoinGeneration != generation {
			r.mu.Unlock()
			return
		}
		r.lowJoinTimer = nil
	}
	spacing := time.Second
	if join {
		spacing = 500 * time.Millisecond
	}
	if gap {
		spacing = 100 * time.Millisecond
	}
	if r.publisher == nil || r.lowSSRC == 0 {
		r.mu.Unlock()
		return
	}
	if r.lowPLIBusy || time.Since(r.lowPLIAt) < spacing {
		if join && r.lowJoinTimer == nil {
			r.lowJoinGeneration++
			generation := r.lowJoinGeneration
			wait := max(50*time.Millisecond, time.Until(r.lowPLIAt.Add(spacing)))
			r.lowJoinTimer = time.AfterFunc(wait, func() {
				r.lowPLIForGeneration(true, false, generation)
			})
		}
		r.mu.Unlock()
		r.PLIsHeld.Add(1)
		return
	}
	publisher, ssrc := r.publisher, r.lowSSRC
	r.lowPLIAt = time.Now()
	r.lowPLIBusy = true
	r.mu.Unlock()
	r.PLIsSent.Add(1)
	go func() {
		_ = publisher.PC.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
		r.mu.Lock()
		r.lowPLIBusy = false
		r.mu.Unlock()
	}()
}

// A refresh is a one-off cost, not steady stream demand. Use it only before
// delta measurements exist; refresh serialization has its own admission check.
func (r *Room) nativeDemand() int {
	rates := r.layerRates(0)
	if measured := rates[0] + rates[1] + rates[2]; measured > 0 {
		return max(64_000, measured)
	}
	return max(64_000, int(r.keyBytes.Load())*8)
}

// Leave transport and playout room inside the one-second continuity envelope.
func nativeRefreshTooSlow(bytes, budget int64) bool {
	return bytes > 0 && keySerialization(bytes, budget) > keyEnvelope
}

// A requested key may be much larger than the previous key used by the probe.
// Recheck its actual cost before replacing an already live low decoder.
func nativeUpgradeFits(bytes, budget, received int64) bool {
	return bytes > 0 && budget > 0 && received > 0 && keySerialization(bytes, min(budget, received)) <= keyEnvelope
}

// Saturate unknown capacity so no unknown path can admit a native refresh.
func keySerialization(bytes, rate int64) time.Duration {
	if rate <= 0 {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(bytes*8) * time.Second / time.Duration(rate)
}
