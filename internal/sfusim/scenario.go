package sfusim

import (
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Ideal is a Link index meaning an unimpaired path with no delay.
const Ideal = -1

// ViewerConfig places one viewer on the scenario's links.
type ViewerConfig struct {
	Join       time.Duration
	Fwd, Rev   int // indexes into Scenario.Links, or Ideal
	NominalRTT time.Duration
	Spam       []Interval  // decoder-stall reports every 10 ms
	Switch     *PathSwitch // a selected-pair change onto other links
	// Consent models the production media barrier (internal/sfu
	// mediaReady): the SFU stops forwarding ConsentTimeout after the last
	// reverse-path packet, reports the forwarded rendition's pictures as
	// lost (OnBroken for bases), and resumes at the next reverse packet.
	Consent bool
	Replace *PCReplace // the viewer's browser replaces its PeerConnection
}

// ConsentTimeout is Pion's ICE disconnected timeout in internal/sfu (New).
const ConsentTimeout = 2 * time.Second

// PCReplace replaces a viewer's PeerConnection, as web/src/session.ts
// replace() does: the old PC closes (its ctrl.Viewer leaves the room's
// Keys, its receiver stops), and Setup later the new PC joins with a loop
// from the old one's Handoff (rate and session only) in a new generation and
// a new receiver without decoder, NACK or feedback state. The old PC's late
// feedback never reaches the new one.
type PCReplace struct {
	At time.Duration
	// Spread phases the replacement across seeds: seed n replaces at
	// At + ((n−1) mod 100)/100 × Spread.
	Spread time.Duration
	// Setup is the signaling, ICE and DTLS time from the old PC's close to
	// the new PC's join (measured 27–31 ms in flap-diag-{1..4}).
	Setup time.Duration
	// Again repeats the replacement request at these offsets after the
	// first (a client that replaces repeatedly).
	Again []time.Duration
}

// start is the replacement time for this run's seed.
func (p *PCReplace) start(seed uint64) time.Duration {
	return p.At + time.Duration(int64(p.Spread)*int64((seed-1)%100)/100)
}

// PathSwitch moves a viewer onto other links; packets already on the old
// links still arrive, including late feedback.
type PathSwitch struct {
	At       time.Duration
	Fwd, Rev int
}

// TCPFlow is a competing bulk Cubic transfer.
type TCPFlow struct {
	Data, Ack int // link indexes
	Start     time.Duration
	Rwnd      int // bytes; 0 = DefaultRwnd
}

// DefaultRwnd is the receive-window cap calibrated against the browser
// fifo_tcp_only_512kbit queue probe (docs/REPORT.md).
var DefaultRwnd = 18_000

// QueueProbe sends a timestamped 64-byte datagram every 500 ms through Link
// once, like web/torture.mjs queueProbe.
type QueueProbe struct{ Link int }

// Scenario is one Tier A or calibration setup.
type Scenario struct {
	Name     string
	Duration time.Duration
	Native   Profile
	Low      Profile
	Audio    bool
	NoVideo  bool
	Links    []LinkConfig
	Viewers  []ViewerConfig
	TCP      []TCPFlow
	Probe    *QueueProbe
	Replay   [2]*Replay
	// IngressLoss marks pictures whose packets after the first never reach
	// the SFU (publisher-leg loss the SFU could not repair).
	IngressLoss func(*Frame) bool
	SampleQueue bool // record per-viewer pacer queue maxima every 10 ms
	// FixedCap turns off the production sender's keyframe boost
	// (web/src/keyboost.ts; the browser's ?keyboost=off control).
	FixedCap bool
}

// Controller builds the room key policy and per-viewer loops under test.
type Controller struct {
	Name   string
	Keys   func() ctrl.Keys
	Viewer func(id int, keys ctrl.Keys) *ctrl.Viewer
	// Replace builds a replacement PC's loop; nil: ctrl.NewViewerFrom.
	Replace    func(id int, keys ctrl.Keys, s ctrl.RateState) *ctrl.Viewer
	ResendNACK func(age time.Duration, sends int) bool // nil: ctrl.ResendNACK
	// Adjust, when set, edits each decision (calibration replays FEC).
	Adjust func(id int, now time.Duration, d *ctrl.Decision)
	// RTXCopy, when positive, sends a second copy of each retransmission
	// after this delay (the round-4 SFU's repair redundancy; replay only).
	RTXCopy time.Duration
	// FECAll applies Decision.FEC to every media packet, not only TL0 and
	// keys (the round-4 SFU's parity; replay only).
	FECAll bool
	// NoAppRecovery disables the viewer app's PC-replacement policy (a
	// decision replay reproduces the browser's own history instead).
	NoAppRecovery bool
}

// Default is the controller in internal/sfu/ctrl (stubs until A1).
var Default = Controller{Name: "ctrl", Keys: ctrl.NewRoomKeys, Viewer: ctrl.NewViewer, Replace: ctrl.NewViewerFrom}

// ProbeSample is one queue-probe residence measurement.
type ProbeSample struct{ At, Delay time.Duration }

// TCPSample is a cumulative delivered-bytes reading.
type TCPSample struct {
	At    time.Duration
	Bytes int64
}

// Result is everything a property or calibration needs from one run.
type Result struct {
	Scenario *Scenario
	Seed     uint64
	Viewers  []*ViewerResult
	Keys     []KeyRecord
	Frames   [2][]time.Duration // source capture times per rendition
	Requests [2]int             // key requests reaching the publisher
	PLIs     [2][]time.Duration // when the room's key policy emitted each PLI
	// Exceptions are the replacement low-key exceptions the room granted
	// (ctrl.RoomKeys; nil for other key policies).
	Exceptions []ctrl.Exception
	Probe      []ProbeSample
	TCP        [][]TCPSample
	TCPLoss    [][2]int // per flow: RTO timeouts, fast retransmits
	Links      []*Link
}

// ViewerResult is one viewer's observations.
type ViewerResult struct {
	Join      time.Duration
	Timeline  []Rendered
	Samples   []Sample
	TTFF      time.Duration // -1 if never rendered
	PLIs      int
	Stalls    int
	Drops     int
	Recovered int
	Lost      int
	Refused   int // pictures refused by the pacer's hard bounds
	// DeadComplete counts complete pictures the receiver could never decode
	// because a reference was missing.
	DeadComplete    int
	AudioGaps       []time.Duration // gaps >200 ms between playable audio frames
	AudioMissing    int             // audio frames never playable (lost or repaired too late)
	MaxQueuedBytes  int
	MaxQueuedFrames int
	// Calibration counters (docs/REPORT.md "Simulator reconciliation").
	Sent, Received [6]int64 // forward packets by kind: media, RTX, FEC, padding, audio, SR
	AudioRTX       int      // audio retransmissions sent
	VideoNACKs     int      // video NACK packets sent (inbound-rtp nackCount)
	AudioNACKs     int
	LossRuns       int64 // forward loss runs and lost packets (transport-wide sequence gaps)
	LossPkts       int64
	PlayoutMean    time.Duration   // mean viewer-app jitterBufferTarget over stats ticks
	AverageLoss    float64         // mean of the SFU diagnostics' averageLoss over stats ticks
	ChromeFreezes  int             // Chromium freezeCount
	Replacements   []time.Duration // PCs replaced by the viewer app's recovery policy
}

// Run executes one scenario with one seed.
func Run(sc *Scenario, seed uint64, c Controller) *Result {
	s := newSim(seed)
	res := &Result{Scenario: sc, Seed: seed}
	links := make([]*Link, len(sc.Links))
	for i, l := range sc.Links {
		links[i] = newLink(s, uint64(100+i), l)
	}
	res.Links = links
	if traceLinks != nil {
		traceLinks(links)
	}
	r := &room{sim: s, sc: sc, keys: c.Keys(), resend: c.ResendNACK, adjust: c.Adjust, rtxCopy: c.RTXCopy, fecAll: c.FECAll, links: links, replace: c.Replace, noRecovery: c.NoAppRecovery}
	if r.replace == nil {
		r.replace = ctrl.NewViewerFrom
	}
	get := r.link
	if !sc.NoVideo {
		low := sc.Low
		if low.FPS == 0 {
			low = LowProfile
			if sc.Native.LowKeyBytes != nil {
				low.KeyBytes = sc.Native.LowKeyBytes
			}
		}
		low.Flexible = sc.Native.Flexible // measured: both encodings of a source share the mode
		r.src[ctrl.Native] = newSource(s, 1, ctrl.Native, sc.Native, r.ingress)
		r.src[ctrl.Low] = newSource(s, 2, ctrl.Low, low, r.ingress)
		for rend, src := range r.src {
			src.replay = sc.Replay[rend]
			// The production sender boosts the native cap around its keys;
			// archived replays predate it and FixedCap is its ?keyboost=off.
			if rend == int(ctrl.Native) && src.replay == nil && !sc.FixedCap {
				src.boost = &keyBoost{}
			}
			src.Start(time.Duration(rend) * 3 * time.Millisecond)
		}
		s.Every(0, 20*time.Millisecond, r.pollKeys)
		if sc.Audio {
			s.Every(0, 20*time.Millisecond, r.audio)
		}
	}
	for i, vc := range sc.Viewers {
		v := &viewer{room: r, id: i, cfg: vc, fwd: get(vc.Fwd, uint64(200+i)), rev: get(vc.Rev, uint64(300+i)), pending: map[uint64]*outPkt{}}
		v.cache[0], v.cache[1], v.audioCache = map[int64]*cacheEntry{}, map[int64]*cacheEntry{}, map[int64]*cacheEntry{}
		v.ctl = c.Viewer(i, r.keys)
		v.ctl.Pacer.SetRelease(v.release)
		r.viewers = append(r.viewers, v)
		s.At(vc.Join, v.join)
	}
	res.TCP = make([][]TCPSample, len(sc.TCP))
	var flows []*TCP
	for i, f := range sc.TCP {
		rw := f.Rwnd
		if rw == 0 {
			rw = DefaultRwnd
		}
		t := newTCP(s, get(f.Data, uint64(400+i)), get(f.Ack, uint64(500+i)), 2000+i, rw)
		t.Start(f.Start)
		flows = append(flows, t)
		s.Every(f.Start, 500*time.Millisecond, func() { res.TCP[i] = append(res.TCP[i], TCPSample{s.Now(), t.Delivered}) })
	}
	if sc.Probe != nil {
		l := links[sc.Probe.Link]
		s.Every(0, 500*time.Millisecond, func() {
			sent := s.Now()
			l.Send(&Packet{Size: 64 + 28, Flow: 3000, deliver: func(*Packet) {
				res.Probe = append(res.Probe, ProbeSample{sent, s.Now() - sent})
			}})
		})
	}
	s.Run(sc.Duration)
	for _, v := range r.viewers {
		vr := &ViewerResult{Join: v.cfg.Join, Samples: v.Samples, Drops: v.Drops, TTFF: -1, Refused: v.Refused, MaxQueuedBytes: v.MaxQueuedBytes, MaxQueuedFrames: v.MaxQueuedFrames,
			Sent: v.SentPkts, AudioRTX: v.AudioRTX}
		if v.rx != nil {
			vr.Timeline, vr.TTFF, vr.PLIs, vr.Stalls = v.rx.Timeline, v.rx.TTFF, v.rx.PLIs, v.rx.Stalls
			vr.Recovered, vr.Lost = v.rx.Recovered, v.rx.Lost
			vr.DeadComplete = v.rx.deadComplete()
			vr.AudioGaps, vr.AudioMissing = v.rx.audioGaps()
			vr.Received, vr.VideoNACKs, vr.AudioNACKs, vr.ChromeFreezes = v.rx.Pkts, v.rx.videoNACKs, v.rx.audioNACKs, v.rx.freezes
			vr.PlayoutMean = v.rx.playSum / time.Duration(max(1, v.rx.playN))
			vr.AverageLoss = v.rx.lossSum / float64(max(1, v.rx.playN))
			vr.Replacements = v.rx.Replacements
			vr.LossRuns, vr.LossPkts = v.rx.LossRuns, v.rx.LossPkts
		}
		res.Viewers = append(res.Viewers, vr)
	}
	for _, t := range flows {
		res.TCPLoss = append(res.TCPLoss, [2]int{t.Timeouts, t.FastRtx})
	}
	res.Keys = r.keys0
	res.PLIs = r.plis
	if k, ok := r.keys.(interface{ Exceptions() []ctrl.Exception }); ok {
		res.Exceptions = k.Exceptions()
	}
	for rend, src := range r.src {
		if src != nil {
			res.Requests[rend] = src.KeyRequests
			res.Frames[rend] = src.Captures
		}
	}
	return res
}
