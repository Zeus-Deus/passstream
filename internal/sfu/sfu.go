package sfu

import (
	"encoding/json"
	"errors"
	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type Engine struct {
	settings webrtc.SettingEngine
	udp      *ice.UDPMuxDefault
	tcp      *iceTCPMux
	mu       sync.Mutex
	rooms    map[string]*Room
}
type Room struct {
	Frames            atomic.Uint64
	frameRate         atomic.Int64
	frameTimes        [256]int64
	frameHead, frameN int
	Packets           atomic.Uint64
	// Ingress diagnostics: packets before reorder, damaged frames, PLIs.
	RawPackets        atomic.Uint64
	Damaged           atomic.Uint64
	PLIsSent          atomic.Uint64
	PLIsHeld          atomic.Uint64
	Keyframes         atomic.Uint64
	keyBytes          atomic.Int64
	mu                sync.Mutex
	publisher         *Peer
	viewers           map[*Peer]bool
	ssrc              uint32
	lowSSRC           uint32
	hasLow            atomic.Bool
	lowBytes          [3]atomic.Uint64
	lowRate           [3]atomic.Int64
	lowPLIAt          time.Time
	lowPLIBusy        bool
	lowJoinTimer      *time.Timer
	lowJoinGeneration uint64
	pliAt             time.Time
	pliBusy           bool
	joinPending       bool
	joinGeneration    uint64
	joinTimer         *time.Timer
	// UnixNano of the latest ingress keyframe start, packet, and end (complete
	// or damaged). A keyframe is in flight between start and end.
	keyStart, keyLast, keyDone atomic.Int64
	source                     uint64
	tidBytes                   [3]atomic.Uint64
	tidRate                    [3]atomic.Int64
	audio                      roomAudio // opaque audio (audio.go)
	spatialBytes               [2][3]atomic.Uint64
	spatialRate                [2][3]atomic.Int64
	spatialMax                 atomic.Int32
}
type Peer struct {
	born                             time.Time
	observerMu                       sync.RWMutex
	heartbeatObserver                func(*bool)
	pliAllowed                       func() bool
	audio                            *audioLeg // viewers: opaque audio (audio.go)
	pathMu                           sync.Mutex
	receiverMinimum, feedbackMinimum pathMinimum
	pathEpoch                        atomic.Uint64
	nativeKeyAt                      atomic.Int64
	nativeKeyOwner                   *atomic.Int64 // Shared by replacements of this admitted viewer.
	heartbeatAcceptedAt              atomic.Int64
	Sent                             atomic.Uint64
	PC                               *webrtc.PeerConnection
	Track                            *repairTrack
	Pacer                            *Pacer
	BWE                              cc.BandwidthEstimator
	Room                             *Room
	Publish                          bool
	frames                           chan frame
	done                             chan struct{}
	once                             sync.Once
	sourceFeedbackOnce               sync.Once
	sourceMu                         sync.Mutex
	sourceControl                    *publisherController
	Layer                            atomic.Int32
	Spatial                          atomic.Int32
	Low                              atomic.Bool
	refreshEncoding                  atomic.Int32 // 0 follows selected encoding, 1 native, 2 low
	refreshRequestedAt               atomic.Int64
	connectedOnce                    atomic.Bool
	pathLimited                      atomic.Bool
	inheritedPath                    atomic.Bool
	Epoch                            atomic.Uint32
	temporalEpoch                    atomic.Uint32
	rtcpAt                           atomic.Int64
	heartbeatAt                      atomic.Int64
	fastRTTAt                        atomic.Int64
	receiverRTTAt                    atomic.Int64
	fastReceiverReports              atomic.Uint32
	pliGapUntil                      atomic.Int64
	SignalMu                         sync.Mutex // serializes offers across resumed WebSockets
	SignalGeneration                 atomic.Uint64
	rttNanos                         atomic.Int64
	initialRTT                       atomic.Int64
	baseDemand                       atomic.Int64
	iceAt                            atomic.Int64
	residualLoss                     atomic.Uint32
	reports                          atomic.Uint64
	decoderReported                  atomic.Bool
	decoderSupported                 atomic.Bool
	twccRTT, twccMinimum             atomic.Int64
	twccReports                      atomic.Uint64
	decodedMu                        sync.Mutex
	freshFrames                      atomic.Uint64
	decodedFPS                       atomic.Int64
	recentDecodedFPS                 atomic.Int64
	decodedSampleAt                  atomic.Int64
	decodedFeedbackAt                atomic.Int64
	freshAt                          atomic.Int64
	totalLost                        atomic.Int32
	highestSeq                       atomic.Uint32
	burst                            atomic.Uint32
	admitted                         atomic.Bool
	estimate                         atomic.Value
	adaptReason                      atomic.Value // string: last congestion reason
	adaptDowns                       atomic.Value // map[string]uint64 snapshot
}
type frame struct {
	packets          []*rtp.Packet
	at               time.Time
	tid              uint8
	key, up          bool
	low              bool
	epoch            uint32
	temporalEpoch    uint32
	temporalRecovery bool
	flexible         bool
	source           uint64
	keyBytes         int64
}

func newEngine(udpAddr, tcpAddr, advertiseIP string) (*Engine, error) {
	u, e := net.ListenPacket("udp4", udpAddr)
	if e != nil {
		return nil, e
	}
	t, e := net.Listen("tcp4", tcpAddr)
	if e != nil {
		u.Close()
		return nil, e
	}
	s := webrtc.SettingEngine{}
	s.SetIncludeLoopbackCandidate(true)
	if advertiseIP != "" {
		t = publicTCPListener(&s, t, advertiseIP)
	}
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4})
	_ = s.SetICERenomination(webrtc.WithRenominationInterval(500 * time.Millisecond))
	s.SetICEUseCandidateCheckPriority(false)
	// Browser recovery is driven by media/feedback liveness. Allow initial
	// nomination and DTLS retransmissions to survive several lossy round trips.
	s.SetICETimeouts(2*time.Second, 15*time.Second, 500*time.Millisecond)
	// Pion is the DTLS client; its default 1 s first flight retransmit dominates
	// connection setup under heavy loss. Backoff still doubles from here.
	s.SetDTLSRetransmissionInterval(100 * time.Millisecond)
	um := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: u})
	tm := newICETCPMux(t)
	s.SetICEUDPMux(um)
	s.SetICETCPMux(tm)
	return &Engine{settings: s, udp: um, tcp: tm, rooms: map[string]*Room{}}, nil
}
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.rooms {
		r.close()
	}
	e.udp.Close()
	e.tcp.Close()
}
func (e *Engine) Remove(id string) {
	e.mu.Lock()
	r := e.rooms[id]
	delete(e.rooms, id)
	e.mu.Unlock()
	if r != nil {
		r.close()
	}
}
func (r *Room) close() {
	r.mu.Lock()
	ps := []*Peer{}
	if r.publisher != nil {
		ps = append(ps, r.publisher)
	}
	for p := range r.viewers {
		ps = append(ps, p)
	}
	r.mu.Unlock()
	for _, p := range ps {
		p.Close()
	}
}
func (e *Engine) Peer(id string, publish bool) (*Peer, error) {
	e.mu.Lock()
	r := e.rooms[id]
	if r == nil {
		r = &Room{viewers: map[*Peer]bool{}}
		e.rooms[id] = r
	}
	e.mu.Unlock()
	p := &Peer{Room: r, Publish: publish, born: time.Now(), nativeKeyOwner: new(atomic.Int64), frames: make(chan frame, 16), done: make(chan struct{})}
	p.Layer.Store(2)
	keySize := r.keyBytes.Load()
	p.Low.Store(r.hasLow.Load() && (keySize == 0 || nativeRefreshTooSlow(keySize, initialWireRate)))
	ok := false
	defer func() {
		if !ok && p.Pacer != nil {
			_ = p.Pacer.Close()
		}
	}()
	m := &webrtc.MediaEngine{}
	// One encoding is forwarded intact; VP9 profile 0 is the measured phase-1 codec.
	fmtp := "profile-id=0"
	if publish {
		// Keep repaired random loss from reducing Chromium's publisher to
		// 30 kbit/s, where even a small independent keyframe takes seconds.
		fmtp += ";x-google-min-bitrate=128"
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: fmtp}, PayloadType: 98}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	if err := registerAudio(m); err != nil {
		return nil, err
	}
	if !publish {
		for i, mime := range []string{"video/red", "video/ulpfec"} {
			if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000}, PayloadType: webrtc.PayloadType(116 + i)}, webrtc.RTPCodecTypeVideo); err != nil {
				return nil, err
			}
		}
	}
	registry := &interceptor.Registry{}
	if !publish {
		p.Pacer = NewPacer()
		p.Pacer.effective.Store(int64(r.startingWireRate()))
		p.Pacer.OnDrop = func() {
			epoch := p.Epoch.Add(1)
			if os.Getenv("PASSSTREAM_TRACE_BUDGET") == "1" {
				log.Printf("media pacer reference lost epoch=%d drops=%d", epoch, p.Pacer.Drops.Load())
			}
		}
		factory, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
			return gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(2_000_000), gcc.SendSideBWEMinBitrate(64_000), gcc.SendSideBWEMaxBitrate(50_000_000), gcc.SendSideBWEPacer(p.Pacer))
		})
		if err != nil {
			return nil, err
		}
		factory.OnNewPeerConnection(func(_ string, b cc.BandwidthEstimator) { p.BWE = b })
		registry.Add(factory)
	}
	// Repair is owned here: the reorder deadline and cache age must bound retries.
	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeVideo)
	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"}, webrtc.RTPCodecTypeVideo)
	if err := webrtc.ConfigureRTCPReports(registry); err != nil {
		return nil, err
	}
	if publish {
		p.sourceControl = &publisherController{rate: 4_000_000}
		m.RegisterFeedback(webrtc.RTCPFeedback{Type: "goog-remb"}, webrtc.RTPCodecTypeVideo)
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sourceSendTimeURI}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	} else if err := webrtc.ConfigureTWCCSender(m, registry); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureStatsInterceptor(registry); err != nil {
		return nil, err
	}
	if !publish {
		if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, registry); err != nil {
			return nil, err
		}
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(e.settings)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	p.PC = pc
	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			if msg.IsString {
				if len(msg.Data) > 4096 || !p.acceptHeartbeat(time.Now()) {
					return
				}
				if d.Label() != "heartbeat" {
					_ = d.SendText(string(msg.Data))
					return
				}
				var request struct {
					Type        string  `json:"type"`
					FreshFrames *uint64 `json:"freshFrames"`
					Hidden      *bool   `json:"hidden"`
					At          *int64  `json:"at"`
					Diagnostics bool    `json:"diagnostics"`
				}
				if json.Unmarshal(msg.Data, &request) == nil {
					p.observe(request.Hidden)
				}
				if request.Type == "keyframe" && !p.Publish {
					p.requestKeyframe()
				}
				if !p.Publish && request.FreshFrames != nil {
					if request.At != nil {
						p.reportDecodedSample(time.Now(), *request.FreshFrames, *request.At)
					} else {
						p.reportDecoded(time.Now(), *request.FreshFrames)
					}
					p.noteHeartbeat(time.Now())
				}
				extra := map[string]any{}
				if p.Publish {
					if request.Diagnostics {
						extra["sourceFrameTimes"] = p.Room.sourceFrameTimes()
					}
					extra["sourceKeyframeBytes"] = p.Room.keyBytes.Load()
					p.sourceMu.Lock()
					extra["publisherBudget"] = p.sourceControl.rate
					extra["publisherQueueMs"] = p.sourceControl.queue(time.Now()).Milliseconds()
					extra["publisherReceivedWireRate"] = p.sourceControl.received
					p.sourceMu.Unlock()
				}
				if p.Track != nil {
					extra["fecPackets"] = p.Track.FECSent.Load()
					extra["pacerDrops"] = p.Pacer.Drops.Load()
					if n := p.Pacer.FeedbackRepairs.Load(); n > 0 {
						extra["feedbackRepairs"] = n
					}
					extra["targetBitrate"] = p.BWE.GetTargetBitrate()
					extra["estimate"] = p.estimate.Load()
					extra["baseWireDemand"] = p.baseDemand.Load()
					if frames := p.Pacer.FullLayerFrames(); len(frames) > 0 {
						extra["fullLayerTimestamps"] = frames
					}
					extra["mediaRates"] = p.Room.layerRates(uint8(p.Spatial.Load()))
					if p.Low.Load() {
						extra["mediaRates"] = p.Room.thumbnailRates()
					}
					extra["twccRTTMs"] = p.twccRTT.Load() / int64(time.Millisecond)
					extra["initialRTTMs"] = float64(p.initialRTT.Load()) / float64(time.Millisecond)
					extra["decoderReported"] = p.decoderReported.Load()
					extra["freshFrames"] = p.freshFrames.Load()
					extra["decodedFPS"] = float64(p.decodedFPS.Load()) / 1000
					extra["freshAgeMs"] = time.Since(time.Unix(0, p.freshAt.Load())).Milliseconds()
					extra["wireBudget"] = p.Pacer.wireRate(time.Now())
					extra["paddingBytes"] = p.Pacer.paddingBytes.Load()
					extra["receivedWireRate"] = p.Pacer.receiptRate.Load()
					extra["provenWireRate"] = p.Pacer.capacityRate(time.Now())
					extra["keyframeBytes"] = p.Pacer.keyframeBytes.Load()
					extra["sourceKeyframeBytes"] = p.Room.keyBytes.Load()
					extra["suppressedRepair"] = p.Pacer.Suppressed.Load()
					extra["catchupMs"] = p.Pacer.CatchupDelay(time.Now()).Milliseconds()
					extra["queueDelayMs"] = p.Pacer.QueueDelay(time.Now()).Milliseconds()
					extra["layerDowns"] = p.adaptDowns.Load()
					extra["repairBytes"] = p.Pacer.repairBytes.Load()
					extra["replay"] = p.Pacer.cache.outcomes()
					extra["wireBytes"] = p.Pacer.wireBytes.Load()
					extra["congestion"], _ = p.adaptReason.Load().(string)
					if p.audio != nil {
						extra["audio"] = map[string]uint64{"forwarded": p.audio.Forwarded.Load(), "dropped": p.audio.Dropped.Load(), "replayed": p.audio.Replayed.Load(), "paced": p.Pacer.audio.Sent.Load(), "late": p.Pacer.audio.Late.Load(), "spare": p.Pacer.audio.Spare.Load(), "refused": p.audio.Refused.Load(), "ingress": p.Room.audio.Packets.Load()}
					}
				}
				message := map[string]any{"repair": extra, "type": "heartbeat", "layer": p.Layer.Load(), "spatial": p.Spatial.Load(), "thumbnail": p.Low.Load(), "ingressFrames": p.Room.Frames.Load(), "ingressPackets": p.Room.Packets.Load(), "keyframes": p.Room.Keyframes.Load(), "forwardedPackets": p.Sent.Load(),
					"ingress": map[string]uint64{"raw": p.Room.RawPackets.Load(), "damaged": p.Room.Damaged.Load(), "pliSent": p.Room.PLIsSent.Load(), "pliHeld": p.Room.PLIsHeld.Load()}}
				if request.Diagnostics {
					message["diagnostics"] = true
				}
				b := heartbeatPayload(message, request.Diagnostics)
				_ = d.SendText(string(b))
			}
		})
	})
	role := "view"
	if publish {
		role = "publish"
	}
	start := time.Now()
	// Recovery diagnostics: state names and candidate types only, never addresses.
	logf := func(format string, args ...any) {
		log.Printf("media %s %p +%dms "+format, append([]any{role, p, time.Since(start).Milliseconds()}, args...)...)
	}
	if t := pc.SCTP().Transport().ICETransport(); t != nil {
		t.OnSelectedCandidatePairChange(func(pair *webrtc.ICECandidatePair) {
			p.resetPath(time.Now())
			logf("pair %s/%s -> %s/%s", pair.Local.Typ, pair.Local.Protocol, pair.Remote.Typ, pair.Remote.Protocol)
		})
	}
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		logf("pc %s", s)
		if s == webrtc.PeerConnectionStateConnected {
			if p.connectedOnce.Swap(true) {
				return
			}
			// Frames "sent" before DTLS completes never reached the viewer, so
			// liveness counts from here (see viewerLiveness).
			p.rtcpAt.CompareAndSwap(0, time.Now().UnixNano())
			// ICE statistics can still be empty when DTLS connects. Its handshake
			// interval supplies a pre-media upper RTT bound in that case.
			var iceRTT time.Duration
			if stats, ok := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePairStats(); ok {
				iceRTT = time.Duration(stats.CurrentRoundTripTime * float64(time.Second))
			}
			// Callback delivery can race; full pre-media setup is a safe upper bound.
			handshake := time.Since(start)
			if at := p.iceAt.Load(); at > 0 {
				handshake = time.Since(time.Unix(0, at))
			}
			p.initialRTT.Store(int64(initialRoundTrip(iceRTT, handshake)))
			logf("initial RTT %s (ICE %s, handshake %s)", time.Duration(p.initialRTT.Load()), iceRTT, handshake)
		}
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			p.Close()
		}
	})
	if publish {
		pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
			logf("ice %s", state)
			if state == webrtc.ICEConnectionStateConnected {
				p.iceAt.Store(time.Now().UnixNano())
				r.PLI()

			}
		})
		r.mu.Lock()
		old := r.publisher
		r.publisher = p
		r.source++
		r.ssrc = 0
		r.lowSSRC = 0
		r.frameHead, r.frameN = 0, 0
		r.hasLow.Store(false)
		r.keyBytes.Store(0)
		r.pliAt = time.Time{}
		r.mu.Unlock()
		if old != nil {
			old.Close()
		}
		pc.OnTrack(func(t *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			if t.Kind() == webrtc.RTPCodecTypeAudio {
				p.readAudio(t) // opaque: never parsed as VP9
				return
			}
			low := false
			video := 0
			for _, transceiver := range pc.GetTransceivers() {
				if transceiver.Kind() != webrtc.RTPCodecTypeVideo {
					continue
				}
				if transceiver.Receiver() == receiver {
					low = video == 1
					break
				}
				video++
			}
			if video > 1 {
				return
			}
			r.mu.Lock()
			if r.publisher != p {
				r.mu.Unlock()
				return
			}
			if low {
				r.lowSSRC = uint32(t.SSRC())
				r.hasLow.Store(true)
			} else {
				r.ssrc = uint32(t.SSRC())
				if os.Getenv("PASSSTREAM_TRACE_BUDGET") == "1" {
					log.Printf("media publisher native track ssrc=%d", r.ssrc)
				}
				for v := range r.viewers {
					v.Epoch.Add(1)
				}
			}
			r.mu.Unlock()
			p.sourceFeedbackOnce.Do(func() { go p.publisherFeedback() })
			var sendTimeID uint8
			for _, ext := range receiver.GetParameters().HeaderExtensions {
				if ext.URI == sourceSendTimeURI && ext.ID > 0 && ext.ID <= 255 {
					sendTimeID = uint8(ext.ID)
				}
			}
			p.read(t, low, sendTimeID)
		})
	} else {
		p.Track, err = newRepairTrack()
		if p.Track != nil {
			p.Track.pacer = p.Pacer
		}
		if err != nil {
			p.Close()
			return nil, err
		}
		sender, err := pc.AddTrack(p.Track)
		if err != nil {
			p.Close()
			return nil, err
		}
		if err := p.addAudio(pc); err != nil {
			p.Close()
			return nil, err
		}
		r.mu.Lock()
		r.viewers[p] = true
		r.mu.Unlock()
		go func() {
			ssrc := uint32(sender.GetParameters().Encodings[0].SSRC)
			for {
				pkts, _, err := sender.ReadRTCP()
				if err != nil {
					return
				}
				p.handleVideoRTCP(ssrc, pkts)
			}
		}()
		go p.forward()
		pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
			logf("ice %s", s)
			if s == webrtc.ICEConnectionStateConnected {
				p.viewerICEConnected(time.Now())
			}
		})
	}
	ok = true
	return p, nil
}
func (p *Peer) Close() {
	p.once.Do(func() {
		close(p.done)
		r := p.Room
		r.mu.Lock()
		if r.publisher == p {
			r.publisher = nil
			r.ssrc = 0
		}
		delete(r.viewers, p)
		r.mu.Unlock()
		if p.Pacer != nil {
			_ = p.Pacer.Close()
		}
		if p.PC != nil {
			go p.PC.Close()
		}
	})
}

// handleVideoRTCP applies a viewer's feedback for the video SSRC. SRTCP
// hands a compound packet to every SSRC it names, so audio report blocks,
// NACKs and PLIs arrive here too: only those for the video SSRC count, or
// audio's sequence numbers and loss would steer video adaptation and replay.
func (p *Peer) handleVideoRTCP(ssrc uint32, pkts []rtcp.Packet) {
	p.noteRTCP(time.Now())
	for _, pkt := range pkts {
		switch fb := pkt.(type) {
		case *rtcp.TransportLayerCC:
			p.observeFeedback(time.Now(), fb)
		case *rtcp.ReceiverReport:
			for _, report := range fb.Reports {
				if report.SSRC != ssrc {
					continue
				}
				p.residualLoss.Store(uint32(report.FractionLost))
				// TotalLost is a signed 24-bit field.
				p.totalLost.Store(int32(report.TotalLost<<8) >> 8)
				p.highestSeq.Store(report.LastSequenceNumber)
				p.reports.Add(1)
				if report.LastSenderReport != 0 {
					now := time.Now()
					ntp := uint64(now.Unix()+2208988800)<<32 | uint64(now.Nanosecond())*(1<<32)/1_000_000_000
					d := uint32(ntp>>16) - report.LastSenderReport - report.Delay
					if d < 1<<31 {
						p.observeReceiverRTT(now, time.Duration(d)*time.Second/65536)
					}
				}
			}
		case *rtcp.TransportLayerNack:
			if fb.MediaSSRC != ssrc {
				continue
			}
			var previous uint16
			var streak uint32
			for _, pair := range fb.Nacks {
				pair.Range(func(seq uint16) bool {
					if streak > 0 && seq == previous+1 {
						streak++
					} else {
						streak = 1
					}
					previous = seq
					for old := p.burst.Load(); streak > old; old = p.burst.Load() {
						if p.burst.CompareAndSwap(old, streak) {
							break
						}
					}
					return true
				})
			}
			for _, pair := range fb.Nacks {
				pair.Range(func(seq uint16) bool {
					p.Pacer.Replay(seq)
					return true
				})
			}
		case *rtcp.PictureLossIndication:
			if fb.MediaSSRC == ssrc {
				p.requestPLI()
			}
		case *rtcp.FullIntraRequest:
			for _, e := range fb.FIR {
				if e.SSRC == ssrc {
					p.requestPLI()
					break
				}
			}
		}
	}
}

// InheritPath keeps a replacement from flooding the same constrained path.
// Signaling calls it with the previous peer for this admitted session only.
func (p *Peer) InheritPath(previous *Peer) {
	if previous == nil {
		return
	}
	p.inheritedPath.Store(true)
	p.nativeKeyOwner = previous.nativeKeyOwner
	if p.nativeKeyOwner == nil {
		p.nativeKeyOwner = &previous.nativeKeyAt
	}
	if p.Pacer == nil || previous.Pacer == nil {
		return
	}
	// Preserve the applied wire budget. The numerical room ceiling may be
	// much larger than this viewer's receipt-bounded rate.
	p.Pacer.effective.Store(previous.Pacer.wireRate(time.Now()))
	p.pathLimited.Store(previous.pathLimited.Load())
	p.Pacer.smoothWire.Store(previous.Pacer.smoothWire.Load())
	if p.Room != nil && p.Room.hasLow.Load() && (previous.Low.Load() || p.Room.keyBytes.Load()*8 > p.Pacer.effective.Load()) {
		p.Low.Store(true)
	}
}

// ReportDecoded accepts the same untrusted viewer diagnostic over its
// authenticated signaling connection when SCTP setup or delivery is delayed.
// It never changes transport liveness or permits a publisher to report decoding.
func (p *Peer) ReportDecoded(count uint64, sampleAt int64) {
	if p.Publish {
		return
	}
	now := time.Now()
	if sampleAt == 0 {
		sampleAt = now.UnixMilli()
	}
	p.reportDecodedSample(now, count, sampleAt)
}

func (p *Peer) reportDecoded(now time.Time, count uint64) {
	p.reportDecodedSample(now, count, now.UnixMilli())
}

func (p *Peer) reportDecodedSample(now time.Time, count uint64, sampleAt int64) {
	if count == 0 {
		return
	}
	p.decodedMu.Lock()
	defer p.decodedMu.Unlock()
	p.decoderSupported.Store(true)
	p.decoderReported.Store(true)
	if previous := p.freshFrames.Load(); count > previous {
		elapsed := time.Duration(sampleAt-p.decodedSampleAt.Load()) * time.Millisecond
		if delta := count - previous; delta <= 240 && elapsed >= 100*time.Millisecond && elapsed <= 2*time.Second {
			// Half-second counters alternate between whole frames on sparse
			// temporal layers. Smooth productivity, never the liveness clock.
			p.decodedFPS.Store(smoothMediaRate(p.decodedFPS.Load(), int64(1000*float64(delta)/elapsed.Seconds())))
			p.recentDecodedFPS.Store(int64(1000 * float64(delta) / elapsed.Seconds()))
		} else {
			p.decodedFPS.Store(0)
			p.recentDecodedFPS.Store(0)
		}
		p.decodedSampleAt.Store(sampleAt)
		p.freshFrames.Store(count)
		p.freshAt.Store(now.UnixNano())
	}
	p.decodedFeedbackAt.Store(now.UnixNano())
}

// A temporal upgrade needs a full feedback sample, not several smoothing
// intervals still dominated by the previous base layer. Keep the smoothed
// floor for sparse counters, but let a current complete sample prove recovery.
func (p *Peer) decoderRate() int64 {
	return max(p.decodedFPS.Load(), p.recentDecodedFPS.Load())
}

// Allow an admitted keyframe to finish pacing and its bounded NACK repair
// before requesting another. Multiple viewers must not repeatedly supersede
// one another's still-useful keyframes on a constrained shared link.
func (p *Peer) requestKeyframe() {
	if !p.pliPermitted() {
		return
	}
	// Serialization temporarily pauses decoding. Explicit recovery signals
	// cannot abandon an admitted key or its bounded dependent-frame hold.
	if p.Pacer != nil && !p.livenessGap(time.Now()) && (p.Pacer.KeyframePending() || p.Pacer.CatchupDelay(time.Now()) > 0) {
		return
	}
	low := p.Low.Load() || p.Room.hasLow.Load()
	if target := p.refreshEncoding.Load(); target != 0 {
		low = target == 2
	}
	p.requestKeyframeFor(low)
}

func (p *Peer) requestKeyframeFor(low bool) {
	target := int32(1)
	if low {
		target = 2
	}
	now := time.Now()
	gap := p.livenessGap(now)
	previous := p.refreshEncoding.Load()
	if low != p.Low.Load() && previous == target && !gap && now.Sub(time.Unix(0, p.refreshRequestedAt.Load())) < 2*time.Second {
		return
	}
	if p.Pacer != nil && low == p.Low.Load() && !p.livenessGap(time.Now()) && (p.Pacer.KeyframePending() || p.Pacer.CatchupDelay(time.Now()) > 0 || time.Now().UnixNano() < p.Pacer.pliAfter.Load()) {
		return
	}
	if !low && !p.takeNativeKey(now) {
		return
	}
	if os.Getenv("PASSSTREAM_TRACE_RECOVERY") == "1" {
		log.Printf("recovery request viewer=%p low=%v currentLow=%v freshAge=%s", p, low, p.Low.Load(), now.Sub(time.Unix(0, p.freshAt.Load())))
	}
	p.refreshEncoding.Store(target)
	p.refreshRequestedAt.Store(now.UnixNano())
	if low {
		p.Room.lowPLI(low != p.Low.Load(), p.livenessGap(time.Now()))
	} else {
		p.Room.requestPLI(low != p.Low.Load(), p.livenessGap(now))
	}
}

// A request after a feedback outage must not inherit a keyframe holdoff from
// packets that were serialized into the outage.
func (p *Peer) livenessGap(now time.Time) bool {
	at := p.rtcpAt.Load()
	return now.UnixNano() < p.pliGapUntil.Load() || (at > 0 && now.Sub(time.Unix(0, at)) > max(time.Second, 4*time.Duration(p.rttNanos.Load())))
}

// The first heartbeat after a transport outage can arrive before periodic
// RTCP or the browser's next one-second keyframe timer. Request recovery now;
// feedback-gap evidence bypasses the old keyframe's pacing holdoff.
func (p *Peer) noteHeartbeat(now time.Time) {
	at := p.heartbeatAt.Swap(now.UnixNano())
	if at > 0 && now.Sub(time.Unix(0, at)) > time.Second && p.Sent.Load() > 0 && p.livenessGap(now) {
		p.requestKeyframe()
	}
}

// Reconnection is recovery on the same PC, not a fresh join. Preserve its
// outage evidence and request a prompt refresh instead of the 500ms join timer.
func (p *Peer) viewerICEConnected(now time.Time) {
	previous := p.iceAt.Swap(now.UnixNano())
	if previous == 0 {
		if p.inheritedPath.Load() {
			p.requestKeyframe()
			return
		}
		if p.Low.Load() {
			p.Room.lowPLI(true, false)
		} else {
			p.Room.pli(true)
		}
		// A low join cannot use a native key yet. Its proven upgrade requests
		// that key once; speculative native joins only burden other viewers.
		return
	}
	p.noteRTCP(now)
	p.requestKeyframe()
}

func (p *Peer) noteRTCP(now time.Time) {
	gap := false
	if at := p.rtcpAt.Load(); at > 0 && now.Sub(time.Unix(0, at)) > max(time.Second, 4*time.Duration(p.rttNanos.Load())) {
		p.pliGapUntil.Store(now.Add(500 * time.Millisecond).UnixNano())
		gap = true
	}
	p.rtcpAt.Store(now.UnixNano())
	if gap && p.Room != nil && p.Sent.Load() > 0 {
		// The first authenticated returning feedback proves the path is back.
		// Waiting for the next heartbeat adds up to another half second.
		p.requestKeyframe()
	}
}

// Consent checks can lag returning authenticated traffic by one check period.
// Resume an already established transport as soon as that traffic proves it.
func (p *Peer) mediaReady(now time.Time) bool {
	if p.PC == nil {
		return true
	}
	return mediaTransportReady(p.PC.ConnectionState(), now, max(p.rtcpAt.Load(), p.heartbeatAt.Load()))
}

// ICE's consent tick can lag the returning publisher by hundreds of ms. Sending
// a recovery key on the established encrypted transport lets the receiver
// produce feedback; the existing liveness gate still stops unacknowledged deltas.
func recoveryKeyframeReady(state webrtc.PeerConnectionState, established bool) bool {
	return established && state == webrtc.PeerConnectionStateDisconnected
}

func mediaTransportReady(state webrtc.PeerConnectionState, now time.Time, feedback int64) bool {
	if state == webrtc.PeerConnectionStateConnected {
		return true
	}
	return state == webrtc.PeerConnectionStateDisconnected && feedback > now.Add(-500*time.Millisecond).UnixNano()
}

func (r *Room) startingWireRate() int {
	rate := int64(0)
	for _, n := range r.layerRates(uint8(r.spatialMax.Load())) {
		rate += int64(n)
	}
	// Cover media, one parity copy and replay/startup headroom. Transport
	// evidence still bounds each viewer independently after admission.
	return max(initialWireRate, int(rate)*3, min(512_000, int(r.keyBytes.Load())*40))
}

func (r *Room) PLI() { r.pli(false) }

func (r *Room) pli(join bool) { r.requestPLI(join, false) }

func (r *Room) requestPLI(join, gap bool) { r.requestPLIForJoin(join, gap, 0) }

func (r *Room) requestPLIForJoin(join, gap bool, generation uint64) {
	r.mu.Lock()
	if generation != 0 {
		if !r.joinPending || r.joinGeneration != generation {
			r.mu.Unlock()
			return
		}
		r.joinPending = false
	}
	// Coalesce source keyframes across viewers so they do not repeatedly
	// supersede a paced keyframe or its deadline-bound repair.
	// A keyframe still arriving from a constrained publisher would be restarted
	// by another request; wait unless it stalls for 1 s or takes over 3 s.
	spacing := 300 * time.Millisecond
	if join {
		spacing = 500 * time.Millisecond
	}
	if !r.pliAt.IsZero() && r.keyStart.Load() < r.pliAt.UnixNano() {
		spacing = max(spacing, r.unansweredSpacing())
	}
	if gap {
		spacing = 100 * time.Millisecond
	}
	now := time.Now().UnixNano()
	start := r.keyStart.Load()
	inFlight := start > r.keyDone.Load() && now-r.keyLast.Load() < int64(time.Second) && now-start < int64(3*time.Second)
	if os.Getenv("PASSSTREAM_TRACE_RECOVERY") == "1" {
		log.Printf("recovery PLI join=%v gap=%v inflight=%v busy=%v age=%s spacing=%s", join, gap, inFlight, r.pliBusy, time.Since(r.pliAt), spacing)
	}
	if r.publisher == nil || r.ssrc == 0 || r.pliBusy || inFlight || time.Since(r.pliAt) < spacing {
		if join && r.publisher != nil && r.ssrc != 0 && !r.joinPending {
			r.joinPending = true
			r.joinGeneration++
			generation := r.joinGeneration
			wait := max(50*time.Millisecond, time.Until(r.pliAt.Add(spacing)))
			r.joinTimer = time.AfterFunc(wait, func() {
				r.requestPLIForJoin(true, false, generation)
			})
		}
		r.mu.Unlock()
		r.PLIsHeld.Add(1)
		return
	}
	publisher, ssrc := r.publisher, r.ssrc
	r.pliAt = time.Now()
	r.pliBusy = true
	r.mu.Unlock()
	// Only one RTCP write may be pending. Even a blocked publisher transport must
	// never hold the room lock or stop an ingress/healthy-viewer forwarding loop.
	r.PLIsSent.Add(1)
	go func() {
		_ = publisher.PC.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
		r.mu.Lock()
		r.pliBusy = false
		r.mu.Unlock()
	}()
}
func (p *Peer) read(t *webrtc.TrackRemote, low bool, sendTimeID uint8) {
	bytes, rates := &p.Room.tidBytes, &p.Room.tidRate
	if low {
		bytes, rates = &p.Room.lowBytes, &p.Room.lowRate
	}
	packets := make(chan *rtp.Packet, 256)
	go func() {
		defer close(packets)
		var high uint16
		seen := false
		for {
			pkt, _, err := t.ReadRTP()
			if err != nil {
				return
			}
			p.Room.RawPackets.Add(1)
			fresh := !seen || int16(pkt.SequenceNumber-high) > 0
			if fresh {
				high, seen = pkt.SequenceNumber, true
			}
			p.sourceMu.Lock()
			p.sourceControl.observe(time.Now(), pkt.GetExtension(sendTimeID), pkt.MarshalSize()+wireOverhead, fresh)
			p.sourceMu.Unlock()
			select {
			case packets <- pkt:
			case <-p.done:
				return
			}
		}
	}()
	reorder := newReorder()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var f frame
	var ts uint32
	var last uint16
	var bases baseContinuity
	valid := false
	rateAt := time.Now()
	lastIngress := rateAt
	previousFrames := p.Room.Frames.Load()
	var nackBusy atomic.Bool
	for {
		select {
		case pkt, ok := <-packets:
			if !ok {
				return
			}
			now := time.Now()
			if now.Sub(lastIngress) > time.Second {
				// The first returned packet can be an old retransmitted keyframe.
				// Request a new capture rather than waiting for periodic feedback.
				if low {
					p.Room.lowPLI(false, true)
				} else {
					p.Room.requestPLI(false, true)
				}
			}
			lastIngress = now
			var descriptor codecs.VP9Packet
			if _, err := descriptor.Unmarshal(pkt.Payload); err == nil && descriptor.B && descriptor.SID == 0 && (!descriptor.P || disposableRepair(f, &descriptor, pkt.Timestamp)) && reorder.keyframe(pkt.SequenceNumber) {
				p.Room.endFrame(f)
				f = frame{}
				valid = false
			}
			reorder.push(pkt, time.Now())
		case <-tick.C:
			if d := time.Since(rateAt); d >= 500*time.Millisecond {
				rateAt = time.Now()
				if !low {
					frames := p.Room.Frames.Load()
					p.Room.frameRate.Store(smoothMediaRate(p.Room.frameRate.Load(), int64(float64(frames-previousFrames)*1000/d.Seconds())))
					previousFrames = frames
				}
				for i := range rates {
					cur := int64(float64(bytes[i].Swap(0)) * 8 / d.Seconds())
					rates[i].Store(smoothMediaRate(rates[i].Load(), cur))
					for sid := 0; !low && sid < len(p.Room.spatialRate); sid++ {
						cur := int64(float64(p.Room.spatialBytes[sid][i].Swap(0)) * 8 / d.Seconds())
						p.Room.spatialRate[sid][i].Store(smoothMediaRate(p.Room.spatialRate[sid][i].Load(), cur))
					}
				}
			}
		case <-p.done:
			return
		}
		if nackBusy.CompareAndSwap(false, true) {
			missing := reorder.nacks(time.Now())
			if len(missing) == 0 {
				nackBusy.Store(false)
			} else {
				go func() {
					defer nackBusy.Store(false)
					_ = p.PC.WriteRTCP([]rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: uint32(t.SSRC()), Nacks: rtcp.NackPairsFromSequenceNumbers(missing)}})
				}()
			}
		}
		for _, pkt := range reorder.pop(time.Now()) {
			p.Room.Packets.Add(1)
			var vp codecs.VP9Packet
			if _, err := vp.Unmarshal(pkt.Payload); err != nil {
				continue
			}

			if bases.seen && pkt.SequenceNumber != last+1 {
				bases.gap = true
			}
			if bases.missing(&vp) {
				p.Room.damage(frame{low: low})
			}
			if !low && vp.B && !vp.P {
				p.Room.keyStart.Store(time.Now().UnixNano())
			}
			if len(f.packets) == 0 || pkt.Timestamp != ts {
				if len(f.packets) > 0 {
					p.Room.endFrame(f)
					// Chromium can omit the highest spatial layer, leaving marker
					// unset on a complete lower picture. E still bounds that picture.
					if valid && completePicture(f.packets) {
						p.distribute(f)
					} else {
						p.Room.damage(f)
					}
				}
				f = frame{at: time.Now(), tid: vp.TID, key: !vp.P, up: vp.U, low: low, temporalRecovery: vp.L && !vp.F, flexible: vp.F && vp.I && vp.SID == 0 && (!vp.P || len(vp.PDiff) > 0)}
				ts = pkt.Timestamp
				valid = vp.B
			} else if pkt.SequenceNumber != last+1 {
				valid = false
			}
			last = pkt.SequenceNumber
			f.packets = append(f.packets, pkt)
			if f.key && !f.low {
				p.Room.keyLast.Store(time.Now().UnixNano())
			}
			if len(f.packets) > 2048 {
				valid = false
				f.packets = nil
				continue
			}
			if !pkt.Marker {
				continue
			}
			p.Room.endFrame(f)
			if valid {
				p.distribute(f)
			} else {
				p.Room.damage(f)
			}
			f = frame{}
		}
	}
}

func (p *Peer) distribute(f frame) {
	// Fence metadata as well as forwarding: a late old key must not restore
	// the previous publisher's cost after replacement cleared it.
	r := p.Room
	r.mu.Lock()
	if r.publisher != p {
		r.mu.Unlock()
		return
	}

	if f.low && f.tid < 3 && !f.key {
		for _, q := range f.packets {
			p.Room.lowBytes[f.tid].Add(uint64(q.MarshalSize() + wireOverhead))
		}
	}
	if !f.low && f.tid < 3 {
		for _, q := range f.packets {
			var descriptor codecs.VP9Packet
			if _, err := descriptor.Unmarshal(q.Payload); err == nil && descriptor.SID < 3 {
				// Discover spatial layers on the first keyframe too, before
				// forwarding it. Only delta bytes enter steady-rate admission.
				p.Room.spatialMax.Store(max(p.Room.spatialMax.Load(), int32(descriptor.SID)))
				if f.key {
					continue
				}
				if descriptor.SID == 0 {
					p.Room.tidBytes[f.tid].Add(uint64(q.MarshalSize() + wireOverhead))
				} else {
					p.Room.spatialBytes[descriptor.SID-1][f.tid].Add(uint64(q.MarshalSize() + wireOverhead))
				}
			}
		}
	}
	if !f.low {
		p.Room.Frames.Add(1)
	}
	if f.key && f.low {
		if p.Room.lowJoinTimer != nil {
			p.Room.lowJoinTimer.Stop()
			p.Room.lowJoinTimer = nil
			p.Room.lowJoinGeneration++
		}
	}
	if f.key && !f.low {
		if os.Getenv("PASSSTREAM_TRACE_RECOVERY") == "1" {
			log.Printf("recovery source keyframe ts=%d", f.packets[0].Timestamp)
		}
		p.Room.Keyframes.Add(1)
		keyBytes := int64(0)
		for _, q := range f.packets {
			keyBytes += int64(q.MarshalSize() + wireOverhead)
		}
		p.Room.keyBytes.Store(keyBytes)
		f.keyBytes = keyBytes
		if p.Room.joinPending {
			p.Room.joinTimer.Stop()
			p.Room.joinPending = false
		}
	}
	f.at = time.Now() // Viewer queue age starts after ingress repair/assembly.

	f.source = r.source
	if !f.low {
		if r.frameN > 0 && os.Getenv("PASSSTREAM_TRACE_RECOVERY") == "1" {
			previous := r.frameTimes[(r.frameHead+len(r.frameTimes)-1)%len(r.frameTimes)]
			if gap := f.at.UnixMilli() - previous; gap > 400 {
				log.Printf("recovery source gap=%dms key=%v", gap, f.key)
			}
		}
		r.frameTimes[r.frameHead] = f.at.UnixMilli()
		r.frameHead = (r.frameHead + 1) % len(r.frameTimes)
		r.frameN = min(r.frameN+1, len(r.frameTimes))
	}
	for v := range r.viewers {
		if v.Low.Load() != f.low && !f.key {
			continue
		}
		f.epoch = v.Epoch.Load()
		f.temporalEpoch = v.temporalEpoch.Load()
		select {
		case v.frames <- f:
		default:
			if v.Low.Load() == f.low && (f.tid != 2 || f.key) && !(f.flexible && f.tid > 0 && !f.key) {
				if f.tid == 1 && !f.key && f.temporalRecovery {
					v.temporalEpoch.Add(1)
				} else {
					v.Epoch.Add(1)
				}
				if os.Getenv("PASSSTREAM_TRACE_BUDGET") == "1" {
					log.Printf("media input burst dropped reference low=%v tid=%d key=%v", f.low, f.tid, f.key)
				}
			}
		}
	}
	r.mu.Unlock()
}

// Admit opens the media barrier only after signaling re-admission succeeds.
// Phase 2 can defer this call until the replacement session grant is accepted.
func (p *Peer) Admit() { p.admitted.Store(true) }

func (p *Peer) forward() {
	traceBudget := os.Getenv("PASSSTREAM_TRACE_BUDGET") == "1"
	low, lowTarget := p.Low.Load(), p.Low.Load()
	if p.Pacer != nil {
		p.Pacer.SetThumbnail(low)
	}
	var seq uint16
	waiting := true
	var epoch uint32
	var temporal temporalContinuity
	layer := uint8(2)
	spatial, spatialTarget := uint8(0), uint8(0)
	target := uint8(1)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var live viewerLiveness
	var rewrite continuity
	control := wireController{}
	if p.Pacer != nil {
		control.rate = int(p.Pacer.effective.Load())
	}
	initialized := false
	var prevWire, prevRepair uint64
	var recentWire, recentRepair float64
	lastRateAt := time.Now()
	var lastSourceAt time.Time
	var repairRecovery decoderRepairRecovery
	var parity parityPolicy
	var nativeAdmission renditionAdmission
	var fallback renditionFallback
	pathEpoch := p.pathEpoch.Load()
	var trials capacityTrials

	for {
		select {
		case <-p.done:
			return
		case <-tick.C:
			if !initialized && p.inheritedPath.Load() {
				low, lowTarget = p.Low.Load(), p.Low.Load()
				p.Pacer.SetThumbnail(low)
			}
			if waiting && !live.dead(time.Now(), p.rtcpAt.Load(), time.Duration(p.rttNanos.Load())) && p.mediaReady(time.Now()) {
				if p.Sent.Load() == 0 && !p.inheritedPath.Load() {
					if low {
						p.Room.lowPLI(true, false)
					} else {
						p.Room.pli(true)
					}
				} else {
					p.requestKeyframe()
				}
			}
			if p.BWE != nil && p.admitted.Load() && p.mediaReady(time.Now()) {
				now := time.Now()
				if !initialized {
					control.rate = p.Room.startingWireRate()
					if p.inheritedPath.Load() {
						control.rate = int(p.Pacer.effective.Load())
					}
					initialized = true
				}
				if epoch := p.pathEpoch.Load(); epoch != pathEpoch {
					pathEpoch = epoch
					control.trend = delayTrend{}
					control.queueMinimum = pathMinimum{}
					control.queueExcess = 0
					control.rttReports = 0
					control.proof = 0
					control.proofAt = time.Time{}
					control.receivedRate = 0
					control.sentRate = 0
					control.queueSince = time.Time{}
					control.cutAt = time.Time{}
					control.limited = false
					control.probeAt = now
					trials = capacityTrials{}
				}
				stats := p.BWE.GetStats()
				stats["averageLoss"] = p.Pacer.mediaLoss(now)
				p.estimate.Store(stats)
				wire, repair := p.Pacer.videoWireBytes.Load(), p.Pacer.repairBytes.Load()
				sentRate := int(float64((wire-prevWire)*8) / now.Sub(lastRateAt).Seconds())
				lastRateAt = now
				recentWire = recentWire*.9 + float64(wire-prevWire)*.1
				recentRepair = recentRepair*.9 + float64(repair-prevRepair)*.1
				prevWire, prevRepair = wire, repair
				reserve := repairReserve(uint64(recentWire), uint64(recentRepair))
				demand := p.Room.nativeDemand()
				key := p.Room.keyBytes.Load()
				freshAge := now.Sub(time.Unix(0, p.freshAt.Load()))
				stalled := p.decoderReported.Load() && freshAge >= 1500*time.Millisecond
				queue := p.Pacer.QueueDelay(now)
				fps := p.decoderRate()
				sourceFPS := max(1000, p.Room.frameRate.Load())
				productive := freshAge < time.Second && fps >= selectedTemporalFPS(sourceFPS, layer)*3/4
				rtt := time.Duration(p.rttNanos.Load())
				excess := max(time.Duration(0), rtt-time.Duration(p.initialRTT.Load()))
				if at := p.receiverRTTAt.Load(); at == 0 || now.Sub(time.Unix(0, at)) > time.Second {
					excess = max(excess, time.Duration(p.twccRTT.Load()-p.twccMinimum.Load()))
				}
				delivered := p.Pacer.sustainedRate.Load()
				if now.Sub(time.Unix(0, p.Pacer.sustainedRateAt.Load())) > time.Second {
					delivered = 0
				}
				loss := p.Pacer.mediaLoss(now)
				allowParity := parity.allow(now, loss, p.residualLoss.Load(), p.fastRTTAt.Load())
				requiredWire := nativeWireRequired(demand, reserve)
				// Reserve the repair policy's actual copy budget before it
				// activates. Existing spaced-copy guards still run at send.
				if allowParity && loss > .01 && p.Pacer.spreadParity(now) {
					requiredWire = max(requiredWire, int64(demand)*33/10)
					if loss > .25 {
						requiredWire = max(requiredWire, int64(demand)*53/10)
					}
				}
				control.step(now, rateObservation{reports: p.Pacer.transitReports.Load(), delayAt: time.Unix(0, p.Pacer.transitAt.Load()), delay: time.Duration(p.Pacer.transitDelay.Load()), excess: excess, loss: loss, residual: float64(p.residualLoss.Load()) / 256, received: int(delivered), demand: int(max(requiredWire, nativeRefreshRate(key)) * 110 / 100), trial: p.Pacer.probeID.Load() > 0 && now.UnixNano() < p.Pacer.probeEnd.Load()+int64(max(100*time.Millisecond, rtt)), productive: productive, full: !low && productive && fullCadence(fps, sourceFPS), sentRate: sentRate, mediaDemand: demand, required: int(requiredWire), queue: queue, stalled: stalled, queueLoss: parity.queueLoss(now)})
				p.Pacer.effective.Store(int64(control.rate))
				// Discovery can wait for either direction to clear. Before fresh
				// forward feedback crosses a fallback, old native RTT already
				// exposes the queued work that a new padding trial must not fill.
				paddingDelay := max(control.queueExcess, max(0, max(rtt, time.Duration(p.twccRTT.Load()))-time.Duration(p.twccMinimum.Load()))/2)
				p.Pacer.networkExcess.Store(int64(paddingDelay))
				p.Pacer.probeRate.Store(int64(control.proof))
				p.Pacer.probeRateAt.Store(control.proofAt.UnixNano())
				p.Pacer.rtt.Store(int64(rtt))
				p.Pacer.pliRTT.Store(p.initialRTT.Load())
				p.Pacer.flightDemand.Store(int64(demand))
				p.Pacer.wireReservation.Store(requiredWire)
				// Flight guards serialized refresh work, rather than resolution.
				// A refresh fitting the ordinary 100ms queue needs no extra gate:
				// counting its redundant repair copies can otherwise stop fresh
				// video during a reverse-feedback loss burst.
				p.Pacer.boundFlight.Store(keySerialization(key, max(initialWireRate, int64(control.proof))) > 100*time.Millisecond)
				p.Pacer.smoothWire.Store(true)
				rates := p.Room.layerRates(spatial)
				if low {
					rates = p.Room.thumbnailRates()
				}
				selectedReserve := float64(0)
				if recentWire > 0 {
					selectedReserve = min(.85, recentRepair/recentWire)
				}
				audioRate := p.Room.audio.Rate()
				p.Pacer.SetAudioRate(audioRate)
				videoBudget := p.Pacer.mediaRate(now)
				target = admittedLayer(rates, int(videoBudget)-min(audioRate, int(videoBudget)/2), selectedReserve)
				// Preserve admitted dependencies, but admit only base pictures
				// while a refresh drains. Feeding every enhancement into a rate
				// that only fits steady demand leaves permanent catch-up debt.
				if (p.Pacer.KeyframePending() || p.Pacer.CatchupDelay(now) > 100*time.Millisecond) && requiredWire > p.Pacer.mediaRate(now) {
					target = 0
				}
				spatialTarget = p.Room.admittedSpatial(control.rate, reserve)
				// One rendition envelope: a one-second independent picture and
				// steady video must both fit the same receiver-proven reservation.
				proof := control.proof
				nativeBase := p.Room.layerRates(0)[0]
				fits := key > 0 && int64(control.rate) >= nativeWireRequired(nativeBase, reserve) && int64(proof) >= max(int64(nativeBase), nativeRefreshRate(key))
				wantedLow := !fits
				if !low {
					wantedLow = nativeWireRequired(rates[0], reserve) > p.Pacer.wireRate(now)
				}
				lowTarget = fallback.update(now, low, wantedLow)
				if low {
					// Keep the admitted reservation while its key is in transit.
					// An encoder repayment burst is new offered demand, not
					// evidence that the receiver lost its proven capacity.
					keyFits := key > 0 && min(int64(control.rate), int64(proof)) >= nativeRefreshRate(key)
					lowTarget = !nativeAdmission.allow(now, fits, keyFits)
				} else {
					nativeAdmission = renditionAdmission{}
					if p.pendingLowRefresh(now) {
						lowTarget = true
					}
				}
				if !p.Room.hasLow.Load() {
					lowTarget = false
				}
				if lowTarget != low {
					p.requestKeyframeFor(lowTarget)
				} else {
					p.refreshEncoding.Store(0)
				}
				trialRTT := max(rtt, time.Duration(p.twccRTT.Load()))
				trials.observe(now, p.Pacer, trialRTT, max(requiredWire, nativeRefreshRate(key)), productive && control.trend.slope() <= delaySlopeLimit && control.queueExcess < standingDelayLimit)
				if control.cutAt.Equal(now) {
					p.Pacer.probeUntil.Store(0)
				}
				// Exercise a missing reservation, not the unused numerical ceiling.
				trialNeeded := (low || layer < 2 || allowParity && loss > .01 && requiredWire > p.Pacer.wireRate(now)) && (!p.decoderReported.Load() || freshAge < 750*time.Millisecond) && queue < 20*time.Millisecond && !p.Pacer.KeyframePending() && p.Pacer.CatchupDelay(now) == 0
				// Offer the measured envelope itself. Flight, byte allowance and
				// duty cycle bound discovery without a second inflated target.
				ceiling := nativeTrialRate(requiredWire, key)
				p.Pacer.probeCeiling.Store(min(int64(control.rate), ceiling))
				if trialNeeded && trials.ready(now) {
					trials.start(now, p.Pacer, now.Add(capacityTrialDuration).UnixNano(), minBackoff)
					p.Pacer.notify()
				}
				p.pathLimited.Store(control.limited)
				p.baseDemand.Store(nativeWireRequired(rates[0], reserve))
				repairRecovery.request(now, p, lastSourceAt)
				group := int32(0)
				if allowParity && protectLoss(rtt, loss, stalled, excess, queue) && now.Sub(time.Unix(0, p.rtcpAt.Load())) < time.Second && (loss > .01 || p.burst.Load() > 1) {
					media := 0
					for i := 0; i <= int(target); i++ {
						media += rates[i]
					}
					if media*2 <= control.rate {
						group = 1
					}
				}
				p.Track.groupSize.Store(group)
				if traceBudget {
					log.Printf("media rate=%d proof=%d excess=%s forward=%s slope=%.1f sent=%d received=%d productive=%v cut=%v low=%v target=%d loss=%.3f demand=%d key=%d pending=%v fits=%v trialRate=%d offered=%d backoff=%s", control.rate, control.proof, excess, control.queueExcess, control.trend.slope(), control.sentRate, control.receivedRate, productive, control.cutAt.Equal(now), low, target, loss, demand, key, p.pendingLowRefresh(now), fits, p.Pacer.trialRate(now), trials.offered, trials.backoff)
				}
			}
		case f := <-p.frames:
			keyBytes := f.keyBytes
			if keyBytes == 0 {
				keyBytes = p.Room.keyBytes.Load()
			}
			keyTooSlow := initialized && nativeRefreshTooSlow(keyBytes, int64(control.rate))
			if f.key && !f.low && initialized && control.proof > 0 {
				keyTooSlow = !nativeUpgradeFits(keyBytes, int64(control.rate), int64(control.proof))
			}

			if f.key && !f.low && p.Room.hasLow.Load() && keyTooSlow {
				// A shared refresh must not serialize a large native key in
				// front of an already constrained viewer's independent decoder.
				lowTarget = true
				nativeAdmission = renditionAdmission{}
				if !low {
					p.requestKeyframeFor(true)
				} else {
					// The low decoder is already live. Asking it for another key
					// can also refresh the shared publisher source and repeat debt.
					p.refreshEncoding.Store(0)
				}
				continue
			}
			if !initialized && p.inheritedPath.Load() {
				low, lowTarget = p.Low.Load(), p.Low.Load()
				p.Pacer.SetThumbnail(low)
			}
			if f.low != low {
				if !f.key || f.low != lowTarget {
					continue
				}
				low = f.low
				if !low {
					target, layer = 2, 2
				} else {
					p.pathMu.Lock()
					p.Pacer.resetProof()
					p.pathMu.Unlock()
					control.proof, control.receivedRate, control.sentRate = 0, 0, 0
					control.proofAt = time.Time{}
					control.rate = min(control.rate, initialWireRate)
					p.Pacer.effective.Store(int64(control.rate))
					trials = capacityTrials{}
				}
				p.Low.Store(low)
				p.decodedFPS.Store(0)
				p.recentDecodedFPS.Store(0)
				p.decodedSampleAt.Store(0)
				if p.Pacer != nil {
					p.Pacer.SetThumbnail(low)
				}
				waiting = true
			}
			if f.key && low {
				target, layer = 0, 0
			}

			lastSourceAt = f.at
			ready := p.mediaReady(time.Now())
			if !ready && f.key && p.PC != nil {
				ready = recoveryKeyframeReady(p.PC.ConnectionState(), p.connectedOnce.Load())
			}
			if f.key && os.Getenv("PASSSTREAM_TRACE_RECOVERY") == "1" {
				log.Printf("recovery viewer keyframe ts=%d ready=%v low=%v", f.packets[0].Timestamp, ready, low)
			}
			if !p.admitted.Load() || !ready {
				waiting = true
				continue
			}
			// Diagnostics: log why a viewer starts waiting for a keyframe.
			wait := func(reason string) {
				if !waiting {
					log.Printf("media view %p waits for keyframe: %s", p, reason)
				}
				waiting = true
			}
			if f.epoch != epoch {
				if traceBudget {
					log.Printf("media dependency epoch %d -> %d current=%d low=%v key=%v drops=%d", epoch, f.epoch, p.Epoch.Load(), f.low, f.key, p.Pacer.Drops.Load())
				}
				epoch = f.epoch
				wait("dependency lost")
			}
			if time.Since(f.at) > 100*time.Millisecond {
				if f.tid == 2 && !f.key {
					continue
				}
				wait("frame expired before forwarding")
				p.requestKeyframe()
				continue
			}
			if live.dead(time.Now(), p.rtcpAt.Load(), time.Duration(p.rttNanos.Load())) && !f.key {
				wait("no RTCP from viewer")
				continue
			}
			if spatialTarget < spatial || f.key && p.Sent.Load() > 0 {
				spatial = spatialTarget
			}
			p.Spatial.Store(int32(spatial))
			layer = selectLayer(layer, target, f.tid, f.key, f.up, f.flexible)
			if p.Pacer != nil {
				p.Pacer.SetTemporal(layer)
			}
			p.Layer.Store(int32(layer))
			if f.tid > layer {
				continue
			}
			if waiting && !f.key {
				continue
			}
			if !temporal.allow(f) {
				continue
			}
			waiting = false
			live.sent(time.Now())
			rewrite.start(f)
			spatialEncoding := !low && p.Room.spatialMax.Load() > 0
			last := len(f.packets) - 1
			if spatialEncoding {
				last = spatialEnd(f.packets, spatial)
			}
			if f.key && !low && p.Pacer != nil {
				p.Pacer.probeUntil.Store(0)
			}
			for i, orig := range f.packets {
				if spatialEncoding {
					var descriptor codecs.VP9Packet
					if _, err := descriptor.Unmarshal(orig.Payload); err != nil || descriptor.SID > spatial {
						continue
					}
				}
				// Descriptor rewriting needs a private payload, but upstream header
				// extensions are discarded and need no per-viewer clone.
				pkt := &rtp.Packet{Header: orig.Header, Payload: append([]byte(nil), orig.Payload...)}
				// Marker ends the selected spatial picture, including when the
				// publisher continues with higher spatial layers of this timestamp.
				pkt.Marker = i == last
				seq++
				pkt.SequenceNumber = seq
				rewrite.packet(pkt)

				p.Sent.Add(1)
				// Upstream extension IDs are scoped to its SDP; downstream interceptors add TWCC.
				pkt.Extension = false
				pkt.Extensions = nil
				pkt.ExtensionProfile = 0
				if err := p.Track.writeOwnedRTP(pkt); err != nil {
					if !errors.Is(err, net.ErrClosed) {
						p.requestKeyframe()
					}
					break
				}
			}
		}
	}
}

func (r *Room) endFrame(f frame) {
	if f.key && !f.low {
		r.keyDone.Store(time.Now().UnixNano())
	}
}

// Flexible enhancements recover through explicit references at each viewer;
// non-flexible reference damage needs its retained switching point or a key.
func (r *Room) damage(f frame) {
	if !f.key && (f.tid == 2 || f.flexible && f.tid > 0) {
		return
	}
	r.Damaged.Add(1)
	baseRecovery := f.tid == 1 && !f.key && f.temporalRecovery
	r.mu.Lock()
	for v := range r.viewers {
		if v.Low.Load() == f.low {
			if baseRecovery {
				v.temporalEpoch.Add(1)
			} else {
				v.Epoch.Add(1)
			}
		}
	}
	r.mu.Unlock()
	if baseRecovery {
		return
	}
	if f.low {
		r.lowPLI(false, false)
	} else {
		r.PLI()
	}
}
