package sfusim

import (
	"math"
	"math/rand/v2"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Profile is one encoding's statistical description.
type Profile struct {
	FPS   float64
	Layer [3]float64 // per-TID bps (not cumulative)
	// EncoderBitrate is the configured encoding cap, independent of sparse
	// actual output. Zero preserves the legacy custom-profile fallback.
	EncoderBitrate int
	KeyBytes       func(*rand.Rand) int
	Noise          float64       // lognormal sigma of delta-frame sizes
	Endpoint       time.Duration // capture+encode+decode+display, added to G2G (calibrated)
	Flexible       bool          // VP9 flexible mode with explicit references (measured per source)
	// LowKeyBytes, when set, is the source's measured low-rendition key size
	// (the low encoding of a sparse source has smaller keys than
	// LowProfile's).
	LowKeyBytes func(*rand.Rand) int
}

// Archived round-5 medians (docs/GATES.md "Source model"): 147 distinct
// detailed keys had p50 251 KB, p95 326 KB, max 524 KB.
func archivedKey(r *rand.Rand) int {
	if r.Float64() < 0.9 {
		return 250_600 + r.IntN(1_400)
	}
	return 252_000 + r.IntN(308_000)
}

var (
	// Detailed is the 2560×1440 dense editor plus animated terminal at 2 Mbit/s.
	Detailed = Profile{FPS: 15, Layer: [3]float64{1_134_161, 354_680, 660_698}, EncoderBitrate: 2_000_000, KeyBytes: archivedKey, Noise: 0.25, Endpoint: 100 * time.Millisecond, Flexible: true}
	// Production is contentHint=text capture at the 4 Mbit/s default.
	Production = Profile{FPS: 14, Layer: [3]float64{2_238_182, 702_582, 1_253_055}, EncoderBitrate: 4_000_000, KeyBytes: archivedKey, Noise: 0.25, Endpoint: 100 * time.Millisecond}
	// LowProfile is the ~320 px fallback encoding capped at 100 kbit/s.
	LowProfile = Profile{FPS: 15, Layer: [3]float64{50_000, 20_000, 30_000}, EncoderBitrate: 100_000, KeyBytes: func(r *rand.Rand) int { return 8_500 + r.IntN(1_000) }, Noise: 0.25, Endpoint: 30 * time.Millisecond}
)

// Frame is one encoded picture as the SFU sees it: sizes, the VP9 payload
// descriptor fields and capture time. Ref is the picture it predicts from.
type Frame struct {
	Rend      ctrl.Rendition
	Num       uint64
	TID       uint8
	Key       bool
	Switch    bool
	Capture   time.Duration
	Size      int
	Packets   int
	Ref       *Frame
	Flexible  bool
	TL0       uint8         // non-flexible TL0PICIDX: advances on each TL0 delta
	Assembled time.Duration // F-3: origin of post-assembly key delivery

	ingressLost bool // packets after the first never reach the SFU
}

const (
	rtpPayload  = 1200
	rtpOverhead = 60 // IP + UDP + SRTP + RTP + extensions
	pubDelay    = 5 * time.Millisecond
	// pubRate is the publisher pacer's send rate per SSRC. libwebrtc paces
	// at 2.5 × the network target, not the encoder rate, and the publisher's
	// loopback leg under the SFU's 50 Mbit/s REMB ceiling is not a
	// bottleneck. Measured (a1-smoke-main, Chromium outbound-rtp
	// totalPacketSendDelay): a 251 KB detailed key accumulated 690–951 ms of
	// pacer delay over its ≈221 packets (mean 3.1–4.3 ms per packet, a
	// 6–9 ms train); all detailed packets averaged 0.48–0.51 ms, clock
	// packets 0.10 ms. 270 Mbit/s gives a 7.4 ms key train and 0.4 ms mean
	// delay on detailed deltas.
	pubRate = 270_000_000
)

// PacketSize is the wire size of packet i of f.
func (f *Frame) PacketSize(i int) int {
	base := f.Size / f.Packets
	if i < f.Size%f.Packets {
		base++
	}
	return base + rtpOverhead
}

// Wire is f's bytes on the wire: payload plus per-packet headers.
func (f *Frame) Wire() int { return f.Size + f.Packets*rtpOverhead }

// Replay overrides the generator from an archived browser trace.
type Replay struct {
	At    []time.Duration
	FPS   []float64
	Layer [][3]float64
	Keys  []KeyEvent
}

// KeyEvent is a publisher keyframe observed in a trace.
type KeyEvent struct {
	At    time.Duration
	Bytes int
}

func (r *Replay) at(t time.Duration) (float64, [3]float64) {
	i := 0
	for i+1 < len(r.At) && r.At[i+1] <= t {
		i++
	}
	return r.FPS[i], r.Layer[i]
}

// Source is one rendition's encoder.
type Source struct {
	sim    *Sim
	rend   ctrl.Rendition
	prof   Profile
	rng    *rand.Rand
	replay *Replay
	keyAt  int

	num          uint64
	pos          int
	prev, tl0    *Frame
	tl0idx       uint8 // TL0PICIDX of the latest base picture
	keyPending   bool
	pausedUntil  time.Duration
	paceAt       time.Duration // the rendition's pacer queue is busy until then
	onPacket     func(f *Frame, i int)
	boost        *keyBoost // the production sender's key repayment boost (native only)
	Keys, Frames int
	KeyRequests  int
	Captures     []time.Duration
}

func newSource(s *Sim, stream uint64, r ctrl.Rendition, p Profile, onPacket func(*Frame, int)) *Source {
	return &Source{sim: s, rend: r, prof: p, rng: s.Rand(stream), onPacket: onPacket, keyPending: true}
}

func (s *Source) Start(at time.Duration) { s.sim.At(at, s.tick) }

// RequestKey is a PLI arriving at the publisher. A replayed source honours
// it unless the trace has its own key in the next 2 s, so the trace's keys
// are not doubled.
func (s *Source) RequestKey() {
	s.KeyRequests++
	if s.replay != nil {
		now := s.sim.Now()
		for _, k := range s.replay.Keys {
			if k.At >= now && k.At < now+2*time.Second {
				return
			}
		}
	}
	s.keyPending = true
}

func (s *Source) rates() (float64, [3]float64) {
	if s.replay != nil {
		return s.replay.at(s.sim.Now())
	}
	return s.prof.FPS, s.prof.Layer
}

func (s *Source) tick() {
	now := s.sim.Now()
	fps, layer := s.rates()
	if fps <= 0 {
		fps = 1
	}
	s.sim.After(time.Duration(float64(time.Second)/fps), s.tick)
	if s.replay != nil && s.keyAt < len(s.replay.Keys) && s.replay.Keys[s.keyAt].At <= now {
		s.keyAt++
		s.keyPending = true
	}
	if now < s.pausedUntil && !s.keyPending {
		return
	}
	total := layer[0] + layer[1] + layer[2]
	repay := time.Duration(-1)
	f := &Frame{Rend: s.rend, Num: s.num, Capture: now, Flexible: s.prof.Flexible}
	s.num++
	if s.keyPending {
		s.keyPending = false
		f.Key, f.Switch = true, true
		f.Size = s.prof.KeyBytes(s.rng)
		if s.replay != nil && s.keyAt > 0 && s.replay.Keys[s.keyAt-1].Bytes > 0 {
			f.Size = s.replay.Keys[s.keyAt-1].Bytes
		}
		s.pos = 0
		s.Keys++
		// Encoder rate control repays the key. The round-4 screen-baseline
		// cold start (2 Mbit/s cap, 251 KB key) measured 840 and 738 ms
		// ingress gaps after the key; scale 650–840 ms by key bits per
		// second of encoder rate, counted from the key's ingress completion
		// (set below, once the key's packets are scheduled).
		// With the production sender's boost (web/src/keyboost.ts) the
		// encoder repays the key at the boosted cap instead of its base.
		// The configured cap repays key debt, not the measured output of
		// a sparse scene (clock: ~202 kbit/s output under a 4 Mbit/s cap).
		// Custom profiles with no cap and archived replays, which carry no
		// configured-cap evidence, retain their previous output fallback.
		rc := total
		if s.replay == nil && s.prof.EncoderBitrate > 0 {
			rc = float64(s.prof.EncoderBitrate)
		}
		if s.boost != nil {
			rc = s.boost.key(now, float64(f.Size*8), rc)
		}
		repay = time.Duration(float64(f.Size*8) / rc * (0.70 + 0.20*s.rng.Float64()) * float64(time.Second))
	} else {
		switch s.pos % 4 {
		case 0:
			f.TID, f.Ref, f.Switch = 0, s.tl0, true
		case 2:
			f.TID, f.Ref = 1, s.tl0
		default:
			f.TID, f.Ref = 2, s.prev
		}
		share := [3]float64{4, 4, 2}[f.TID]
		mean := layer[f.TID] * share / fps / 8
		f.Size = max(200, int(mean*math.Exp(s.prof.Noise*s.rng.NormFloat64()-s.prof.Noise*s.prof.Noise/2)))
	}
	s.pos++
	if f.TID == 0 {
		if !f.Key {
			s.tl0idx++
		}
		s.tl0 = f
	}
	f.TL0 = s.tl0idx
	s.prev = f
	s.Frames++
	s.Captures = append(s.Captures, now)
	f.Packets = (f.Size + rtpPayload - 1) / rtpPayload
	// libwebrtc's pacer keeps one FIFO per SSRC, so a picture's packets
	// queue behind the previous picture of the same rendition instead of
	// overlapping it; the two renditions (separate SSRCs) still interleave.
	at := max(now+pubDelay, s.paceAt)
	last := at
	for i := 0; i < f.Packets; i++ {
		i := i
		last = at
		s.sim.At(at, func() { s.onPacket(f, i) })
		at += time.Duration(float64(f.PacketSize(i)*8) / pubRate * float64(time.Second))
	}
	s.paceAt = at
	if repay >= 0 {
		s.pausedUntil = last + repay
	}
}

// Production sender key repayment (web/src/keyboost.ts KEY_BOOST, A16
// ruling): on every encoded native key the sender raises the encoding's
// maxBitrate to repayment × debt bits / pauseMs, at least the base and at
// most min(ceiling, headroom − reserve), for clamp(2 × pause, 1 s, 2.5 s);
// a key during a boost adds to the debt still unrepaid at the raised cap.
//
// The model keeps its measured fixed-cap repayment coefficient (650–840 ms
// × key bits per second of encoder rate) and applies it to the boosted cap.
// Measured (docs/REPORT.md "C2 item 6a", test/torture/results/a1w-keyboost):
// on the 2 Mbit/s screen source, ~235 KB keys gave per-key post-key ingress
// max gaps of 132–155 ms with the boost (5.26 Mbit/s for 1 s) against
// 373–833 ms without. The model's gap at that cap is ≈0.25–0.32 s plus one
// frame interval, so it stays conservative against the measurement.
const (
	boostPause     = 300 * time.Millisecond
	boostRepayment = 0.84
	boostCeiling   = 16_000_000
	boostReserve   = 200_000
	boostMinHold   = time.Second
	boostMaxHold   = 2500 * time.Millisecond
	// boostHeadroom is the publisher's availableOutgoingBitrate: the SFU's
	// plain REMB (internal/sfu publisherCeiling, 50 Mbit/s); measured 49.99
	// Mbit/s in the keygap runs. The publisher leg is never the bottleneck.
	boostHeadroom = 50_000_000
)

type keyBoost struct {
	cap, debt float64
	at, until time.Duration
}

// key is KeyframeBoost.key for one encoded key of bits; it returns the cap
// the encoder repays it at. base is the encoding's own rate.
func (b *keyBoost) key(now time.Duration, bits, base float64) float64 {
	if now >= b.until {
		// restore(): the base cap is back and the debt forgotten.
		b.cap, b.debt, b.until = base, 0, now
	}
	drained := b.cap / boostRepayment * (now - b.at).Seconds()
	b.debt = max(0, b.debt-drained) + bits
	b.at = now
	ceiling := max(base, min(boostCeiling, boostHeadroom-boostReserve))
	b.cap = min(ceiling, max(base, boostRepayment*b.debt/boostPause.Seconds()))
	pause := time.Duration(boostRepayment * b.debt / b.cap * float64(time.Second))
	hold := min(boostMaxHold, max(boostMinHold, 2*pause))
	b.until = max(b.until, now+hold)
	return b.cap
}
