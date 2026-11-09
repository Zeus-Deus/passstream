package sfu

import (
	"encoding/json"
	"log"
	"maps"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

type Engine struct {
	settings webrtc.SettingEngine
	udp      *ice.UDPMuxDefault
	tcp      *iceTCPMux
	mu       sync.Mutex
	rooms    map[string]*Room
}

// Room is one stream: its publisher, its viewers, and the room-wide ctrl
// state (key policy and ingress meter), each behind its own lock. Lock order:
// mu, then a viewer's mu, then meterMu, then the keys lock.
type Room struct {
	Frames     atomic.Uint64
	Packets    atomic.Uint64
	RawPackets atomic.Uint64 // ingress before reorder
	Damaged    atomic.Uint64 // pictures lost on the publisher leg
	PLIsSent   atomic.Uint64
	Keyframes  atomic.Uint64
	keyBytes   atomic.Int64 // latest native key, wire bytes (diagnostics)
	nextViewer atomic.Int64

	mu                sync.Mutex
	publisher         *Peer
	viewers           map[*Peer]bool
	ssrc              [2]uint32 // per rendition
	source            uint64    // publisher epoch
	frameTimes        [256]int64
	frameHead, frameN int
	audio             roomAudio

	meterMu sync.Mutex
	meter   ctrl.Ingress
	keys    *lockedKeys
}

func newRoom() *Room {
	return &Room{viewers: map[*Peer]bool{}, keys: &lockedKeys{k: ctrl.NewRoomKeys()}}
}

func (r *Room) meterVideo(now time.Time, rend ctrl.Rendition, tid uint8, key bool, picture uint64, bytes int) {
	r.meterMu.Lock()
	r.meter.Video(now, rend, tid, key, picture, bytes)
	r.meterMu.Unlock()
}

func (r *Room) meterAudio(now time.Time, bytes int) {
	r.meterMu.Lock()
	r.meter.Audio(now, bytes)
	r.meterMu.Unlock()
}

func (r *Room) costs(now time.Time) ctrl.Costs {
	r.meterMu.Lock()
	defer r.meterMu.Unlock()
	return r.meter.Costs(now, r.keys)
}

// lockedKeys serialises the room's shared ctrl.Keys across the ingress,
// RTCP and writer goroutines of every viewer.
type lockedKeys struct {
	mu sync.Mutex
	k  ctrl.Keys
}

func (l *lockedKeys) Request(now time.Time, v int, r ctrl.Rendition, c ctrl.Cause) ctrl.KeyStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.k.Request(now, v, r, c)
}
func (l *lockedKeys) Allowed(now time.Time, v int, r ctrl.Rendition, c ctrl.Cause) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.k.Allowed(now, v, r, c)
}
func (l *lockedKeys) Due(now time.Time) []ctrl.Rendition {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.k.Due(now)
}
func (l *lockedKeys) OnKey(now time.Time, r ctrl.Rendition, bytes int, captured time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.k.OnKey(now, r, bytes, captured)
}
func (l *lockedKeys) Size(now time.Time, r ctrl.Rendition) (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.k.Size(now, r)
}
func (l *lockedKeys) OnDelivery(v int, e ctrl.KeyEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.k.OnDelivery(v, e)
}
func (l *lockedKeys) Leave(v int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.k.Leave(v)
}
func (l *lockedKeys) Replace(now time.Time, v, session int) ctrl.KeyStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.k.Replace(now, v, session)
}
func (l *lockedKeys) Times(r ctrl.Rendition) (time.Time, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.k.Times(r)
}

type Peer struct {
	born                time.Time
	observerMu          sync.RWMutex
	heartbeatObserver   func(*bool)
	pliAllowed          func() bool
	PC                  *webrtc.PeerConnection
	Room                *Room
	Publish             bool
	view                *viewer // viewers only
	done                chan struct{}
	once                sync.Once
	feedbackOnce        sync.Once
	Layer               atomic.Int32 // applied temporal layer
	thumbnail           atomic.Bool  // applied rendition is low
	Sent                atomic.Uint64
	admitted            atomic.Bool
	connectedOnce       atomic.Bool
	rtcpAt, heartbeatAt atomic.Int64
	heartbeatAcceptedAt atomic.Int64
	SignalMu            sync.Mutex // serializes offers across resumed WebSockets
	SignalGeneration    atomic.Uint64
	rttNanos            atomic.Int64
	initialRTT          atomic.Int64
	iceAt               atomic.Int64
	residualLoss        atomic.Uint32
	reports             atomic.Uint64
	decodedMu           sync.Mutex
	decoderReported     atomic.Bool
	freshFrames         atomic.Uint64
	decodedFPS          atomic.Int64
	decodedSampleAt     atomic.Int64
	freshAt             atomic.Int64
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
		r = newRoom()
		e.rooms[id] = r
	}
	e.mu.Unlock()
	p := &Peer{Room: r, Publish: publish, born: time.Now(), done: make(chan struct{})}
	p.Layer.Store(2)
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
	// Repair is owned here: the reorder deadline and ctrl's repair policy bound retries.
	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeVideo)
	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"}, webrtc.RTPCodecTypeVideo)
	if err := webrtc.ConfigureRTCPReports(registry); err != nil {
		return nil, err
	}
	if publish {
		m.RegisterFeedback(webrtc.RTCPFeedback{Type: "goog-remb"}, webrtc.RTPCodecTypeVideo)
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sourceSendTimeURI}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	} else if err := webrtc.ConfigureTWCCSender(m, registry); err != nil {
		// Negotiates transport-cc; the viewer's writer stamps the sequence itself.
		return nil, err
	}
	if err := webrtc.ConfigureStatsInterceptor(registry); err != nil {
		return nil, err
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(e.settings)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	p.PC = pc
	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			if !msg.IsString || len(msg.Data) > 4096 || !p.acceptHeartbeat(time.Now()) {
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
				at := time.Now().UnixMilli()
				if request.At != nil {
					at = *request.At
				}
				p.reportDecodedSample(time.Now(), *request.FreshFrames, at)
				p.heartbeatAt.Store(time.Now().UnixNano())
			}
			_ = d.SendText(string(heartbeatPayload(p.heartbeat(request.Diagnostics), request.Diagnostics)))
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
			if p.view != nil {
				p.view.pathChange(time.Now()) // generation fence
			}
			logf("pair %s/%s -> %s/%s", pair.Local.Typ, pair.Local.Protocol, pair.Remote.Typ, pair.Remote.Protocol)
		})
	}
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		logf("ice %s", s)
		if s == webrtc.ICEConnectionStateConnected {
			p.iceAt.CompareAndSwap(0, time.Now().UnixNano())
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		logf("pc %s", s)
		if p.view != nil {
			p.view.notify()
		}
		if s == webrtc.PeerConnectionStateConnected && !p.connectedOnce.Swap(true) {
			p.rtcpAt.CompareAndSwap(0, time.Now().UnixNano())
			// ICE statistics can still be empty when DTLS connects; the
			// handshake interval is then a pre-media upper RTT bound.
			handshake := time.Since(start)
			if at := p.iceAt.Load(); at > 0 {
				handshake = time.Since(time.Unix(0, at))
			}
			rtt := handshake
			if stats, ok := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePairStats(); ok && stats.CurrentRoundTripTime > 0 {
				rtt = time.Duration(stats.CurrentRoundTripTime * float64(time.Second))
			}
			p.initialRTT.Store(int64(rtt))
			logf("initial RTT %s", rtt)
		}
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			p.Close()
		}
	})
	if publish {
		r.mu.Lock()
		old := r.publisher
		r.publisher = p
		r.source++
		r.ssrc = [2]uint32{}
		r.frameHead, r.frameN = 0, 0
		r.keyBytes.Store(0)
		r.mu.Unlock()
		if old != nil {
			old.Close()
		}
		pc.OnTrack(func(t *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			if t.Kind() == webrtc.RTPCodecTypeAudio {
				p.readAudio(t) // opaque: never parsed
				return
			}
			// The first video transceiver carries native, the second low.
			rend, video := ctrl.Native, 0
			for _, transceiver := range pc.GetTransceivers() {
				if transceiver.Kind() != webrtc.RTPCodecTypeVideo {
					continue
				}
				if transceiver.Receiver() == receiver {
					if video == 1 {
						rend = ctrl.Low
					}
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
			r.ssrc[rend] = uint32(t.SSRC())
			r.mu.Unlock()
			p.feedbackOnce.Do(func() { go p.publisherFeedback() })
			p.read(t, rend)
		})
		return p, nil
	}
	video, err := newBoundTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0"}, "screen")
	if err != nil {
		pc.Close()
		return nil, err
	}
	audio, err := newBoundTrack(opusCapability, "audio")
	if err != nil {
		pc.Close()
		return nil, err
	}
	videoSender, err := pc.AddTrack(video)
	if err != nil {
		pc.Close()
		return nil, err
	}
	audioSender, err := pc.AddTrack(audio)
	if err != nil {
		pc.Close()
		return nil, err
	}
	p.view = newViewer(p, r, video, audio)
	r.mu.Lock()
	r.viewers[p] = true
	r.mu.Unlock()
	for _, s := range []struct {
		sender *webrtc.RTPSender
		video  bool
	}{{videoSender, true}, {audioSender, false}} {
		go func() {
			ssrc := uint32(s.sender.GetParameters().Encodings[0].SSRC)
			for {
				pkts, _, err := s.sender.ReadRTCP()
				if err != nil {
					return
				}
				p.view.handleRTCP(time.Now(), ssrc, s.video, pkts)
			}
		}()
	}
	go p.view.run()
	return p, nil
}

func (p *Peer) Close() {
	p.once.Do(func() {
		close(p.done)
		r := p.Room
		r.mu.Lock()
		if r.publisher == p {
			r.publisher = nil
			r.ssrc = [2]uint32{}
		}
		delete(r.viewers, p)
		r.mu.Unlock()
		if p.view != nil {
			p.view.leave()
			r.keys.Leave(p.view.id)
		}
		if p.PC != nil {
			go p.PC.Close()
		}
	})
}

// heartbeat is the diagnostic reply on the heartbeat data channel.
func (p *Peer) heartbeat(diagnostics bool) map[string]any {
	r := p.Room
	now := time.Now()
	extra := map[string]any{"sourceKeyframeBytes": r.keyBytes.Load()}
	if p.Publish {
		if diagnostics {
			extra["sourceFrameTimes"] = r.sourceFrameTimes()
		}
		extra["publisherBudget"] = publisherCeiling
	}
	if p.view != nil {
		maps.Copy(extra, p.view.diagnostics(now))
		extra["initialRTTMs"] = float64(p.initialRTT.Load()) / float64(time.Millisecond)
		extra["decoderReported"] = p.decoderReported.Load()
		extra["freshFrames"] = p.freshFrames.Load()
		extra["decodedFPS"] = float64(p.decodedFPS.Load()) / 1000
		extra["freshAgeMs"] = now.Sub(time.Unix(0, p.freshAt.Load())).Milliseconds()
	}
	message := map[string]any{"repair": extra, "type": "heartbeat", "layer": p.Layer.Load(), "thumbnail": p.thumbnail.Load(), "ingressFrames": r.Frames.Load(), "ingressPackets": r.Packets.Load(), "keyframes": r.Keyframes.Load(), "forwardedPackets": p.Sent.Load(),
		"ingress": map[string]uint64{"raw": r.RawPackets.Load(), "damaged": r.Damaged.Load(), "pliSent": r.PLIsSent.Load()}}
	if diagnostics {
		message["diagnostics"] = true
	}
	return message
}

// InheritPath starts a replacement PC from the previous one's rate, in a new
// path generation without admission evidence (ctrl.Viewer.Handoff).
// Signaling calls it with the previous peer for this admitted session only.
func (p *Peer) InheritPath(previous *Peer) {
	if previous == nil || p.view == nil || previous.view == nil {
		return
	}
	p.view.inherit(previous.view.handoff(time.Now()))
}

// Admit opens the media barrier only after signaling re-admission succeeds.
func (p *Peer) Admit() {
	p.admitted.Store(true)
	if p.view != nil {
		p.view.notify()
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

// reportDecodedSample keeps the viewer's decoded-frame counter for
// diagnostics; it never drives rate or layers.
func (p *Peer) reportDecodedSample(now time.Time, count uint64, sampleAt int64) {
	if count == 0 {
		return
	}
	p.decodedMu.Lock()
	defer p.decodedMu.Unlock()
	p.decoderReported.Store(true)
	if previous := p.freshFrames.Load(); count > previous {
		elapsed := time.Duration(sampleAt-p.decodedSampleAt.Load()) * time.Millisecond
		if delta := count - previous; delta <= 240 && elapsed >= 100*time.Millisecond && elapsed <= 2*time.Second {
			p.decodedFPS.Store(int64(1000 * float64(delta) / elapsed.Seconds()))
		} else {
			p.decodedFPS.Store(0)
		}
		p.decodedSampleAt.Store(sampleAt)
		p.freshFrames.Store(count)
		p.freshAt.Store(now.UnixNano())
	}
}

func (p *Peer) noteRTCP(now time.Time) { p.rtcpAt.Store(now.UnixNano()) }

// Consent checks can lag returning authenticated traffic by one check period.
// Resume an already established transport as soon as that traffic proves it.
func (p *Peer) mediaReady(now time.Time) bool {
	if p.PC == nil {
		return true
	}
	return mediaTransportReady(p.PC.ConnectionState(), now, max(p.rtcpAt.Load(), p.heartbeatAt.Load()))
}

func mediaTransportReady(state webrtc.PeerConnectionState, now time.Time, feedback int64) bool {
	if state == webrtc.PeerConnectionStateConnected {
		return true
	}
	return state == webrtc.PeerConnectionStateDisconnected && feedback > now.Add(-500*time.Millisecond).UnixNano()
}
