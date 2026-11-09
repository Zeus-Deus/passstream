package sfu

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

// Wire-layer integration tests: what internal/sfu still owns around ctrl
// (assembly, key admission plumbing, rendition switching, refusal of
// unsupported structures, writer isolation and the generation fence).

func TestKeyIsAssembledBeforeItsFirstPacketIsForwarded(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	pkts := h.packets(ctrl.Low, desc{key: true}, 3, 1000)
	for _, p := range pkts[:2] {
		h.asm[ctrl.Low].push(h.now, p)
	}
	if b, f := pacerQueue(v); b != 0 || f != 0 {
		t.Fatalf("partial key reached the viewer: %d bytes, %d frames", b, f)
	}
	if bytes, _ := h.r.keys.Size(h.now, ctrl.Low); bytes != 0 {
		t.Fatal("partial key reported to the key policy")
	}
	h.asm[ctrl.Low].push(h.now, pkts[2])
	if _, f := pacerQueue(v); f != 1 || pendingRenditions(v)[ctrl.Low] != 3 {
		t.Fatalf("assembled key not forwarded whole: %d frames, pending %v", f, pendingRenditions(v))
	}
	if bytes, samples := h.r.keys.Size(h.now, ctrl.Low); samples != 1 || bytes < 3000 {
		t.Fatalf("key size at assembly: %d bytes from %d keys", bytes, samples)
	}
}

func TestRefusedKeyStopsTheRenditionAndRecoversOnTheNextKey(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	h.r.keys.Due(h.now) // the join PLI
	h.send(ctrl.Low, desc{key: true}, 2, 1000)
	h.drain(v, 200*time.Millisecond)
	if !v.forwarding(ctrl.Low) || len(w.written()) != 2 {
		t.Fatalf("join key not delivered: %d packets", len(w.written()))
	}
	// 120 KB at R = RateInit needs 480 kbit/s over the key envelope.
	h.send(ctrl.Low, desc{key: true}, 100, 1200)
	if v.forwarding(ctrl.Low) || !v.stopped {
		t.Fatal("refused key left the rendition forwarding")
	}
	if pendingRenditions(v)[ctrl.Low] != 0 {
		t.Fatal("refused key was queued")
	}
	h.send(ctrl.Low, desc{tid: 2}, 1, 500)
	if _, f := pacerQueue(v); f != 0 {
		t.Fatal("a delta referencing the refused key was forwarded")
	}
	due := h.r.keys.Due(h.now.Add(ctrl.KeyInterval))
	if len(due) != 1 || due[0] != ctrl.Low {
		t.Fatalf("no recovery key requested after refusal: %v", due)
	}
	h.send(ctrl.Low, desc{key: true}, 2, 1000)
	if !v.forwarding(ctrl.Low) || pendingRenditions(v)[ctrl.Low] != 2 {
		t.Fatal("the next deliverable key did not resume the viewer")
	}
}

func TestRenditionSwitchesOnlyAtATargetKey(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	h.send(ctrl.Low, desc{key: true}, 2, 1000)
	h.drain(v, 200*time.Millisecond)
	h.send(ctrl.Low, desc{tid: 2}, 3, 1100)
	h.send(ctrl.Low, desc{tid: 1}, 3, 1100)
	v.mu.Lock()
	v.ctl.Alloc = &keySwitchAllocation{desired: ctrl.Option{Rendition: ctrl.Native, TID: 2}}
	v.ctl.Probe = ctrl.NoProber{} // this owning test controls policy and exercises the wire boundary
	v.want, v.wantTID = ctrl.Native, 2
	v.mu.Unlock()
	h.send(ctrl.Native, desc{tid: 0}, 2, 1000)
	if pendingRenditions(v)[ctrl.Native] != 0 || pendingRenditions(v)[ctrl.Low] == 0 {
		t.Fatalf("switched before the target key: %v", pendingRenditions(v))
	}
	before := len(w.written())
	h.send(ctrl.Native, desc{key: true}, 3, 1000)
	if _, frames := pacerQueue(v); frames != 3 || !v.forwarding(ctrl.Low) {
		t.Fatalf("old rendition lost before transactional departure: %d frames queued", frames)
	}
	h.drain(v, 300*time.Millisecond)
	sent := w.written()[before:]
	if len(sent) != 3 {
		t.Fatalf("native key packets written: %d", len(sent))
	}
	for i := 1; i < len(sent); i++ {
		if sent[i].SequenceNumber != sent[i-1].SequenceNumber+1 || sent[i].Timestamp != sent[0].Timestamp {
			t.Fatal("switch key is not one contiguous picture")
		}
	}
	if a := v.ctl.Applied(); a.Rendition != ctrl.Native {
		t.Fatalf("applied %v after the key's last packet", a)
	}
}

// Pictures of one rendition leave in order (ctrl pacer), each with one
// contiguous sequence block; a later disposable picture may be shed by
// FrameAge while an earlier one is still departing.
func TestPicturesLeaveInOrderWithContiguousSequenceNumbers(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	h.send(ctrl.Low, desc{key: true}, 1, 500)
	h.drain(v, 200*time.Millisecond)
	h.send(ctrl.Low, desc{tid: 2}, 4, 1100)
	h.drain(v, 5*time.Millisecond) // part of the TL2 picture departs
	h.send(ctrl.Low, desc{tid: 1}, 2, 1100)
	h.drain(v, time.Second)
	byPicture := map[uint32][]uint16{}
	var last uint32
	for i, p := range w.written() {
		if i > 0 && p.Timestamp != last && int32(p.Timestamp-last) < 0 {
			t.Fatalf("picture %d sent after newer %d", p.Timestamp, last)
		}
		last = p.Timestamp
		byPicture[p.Timestamp] = append(byPicture[p.Timestamp], p.SequenceNumber)
	}
	if len(byPicture) < 2 {
		t.Fatalf("pictures written: %d", len(byPicture))
	}
	for ts, seqs := range byPicture {
		lo, hi := seqs[0], seqs[0]
		for _, s := range seqs {
			if int16(s-lo) < 0 {
				lo = s
			}
			if int16(s-hi) > 0 {
				hi = s
			}
		}
		if int(hi-lo)+1 != len(seqs) {
			t.Fatalf("picture %d has non-contiguous sequence numbers %v", ts, seqs)
		}
	}
}

func TestUnsupportedStructuresAreRefusedNotForwarded(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  desc
	}{
		{"spatial layer", desc{tid: 0, sid: 1}},
		{"TID above 2", desc{tid: 3}},
		{"no layer indices", desc{noLayer: true}},
		{"flexible zero reference distance", desc{tid: 1, flexible: true, pdiff: []uint8{0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			v, w := h.viewer()
			h.send(ctrl.Low, desc{key: true}, 1, 500)
			h.drain(v, 100*time.Millisecond)
			h.send(ctrl.Low, tc.bad, 2, 500)
			h.send(ctrl.Low, desc{tid: 2}, 1, 500)
			h.drain(v, 200*time.Millisecond)
			if len(w.written()) != 1 || v.forwarding(ctrl.Low) {
				t.Fatalf("unsupported picture forwarded or tolerated: %d packets", len(w.written()))
			}
		})
	}
	t.Run("spatial key", func(t *testing.T) {
		h := newHarness(t)
		v, w := h.viewer()
		h.send(ctrl.Low, desc{key: true, sid: 1}, 2, 500)
		h.send(ctrl.Low, desc{key: true}, 1, 500) // the rendition stays refused
		h.send(ctrl.Low, desc{tid: 2}, 1, 500)
		h.drain(v, 200*time.Millisecond)
		if len(w.written()) != 0 {
			t.Fatalf("unsupported rendition forwarded: %d packets", len(w.written()))
		}
	})
}

func TestIngressBaseLossBreaksTheChainAndRequestsLow(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	h.r.keys.Due(h.now)
	h.send(ctrl.Low, desc{key: true}, 1, 500)
	for _, tid := range []uint8{2, 1, 2} {
		h.send(ctrl.Low, desc{tid: tid}, 1, 500)
	}
	h.drain(v, 200*time.Millisecond)
	h.packets(ctrl.Low, desc{tid: 0}, 2, 500) // a TL0 picture lost upstream
	h.send(ctrl.Low, desc{tid: 2}, 1, 500)
	h.send(ctrl.Low, desc{tid: 1}, 1, 500)
	h.drain(v, 200*time.Millisecond)
	if len(w.written()) != 4 {
		t.Fatalf("pictures after a lost base were forwarded: %d packets", len(w.written()))
	}
	if due := h.r.keys.Due(h.now.Add(ctrl.KeyInterval)); len(due) != 1 || due[0] != ctrl.Low {
		t.Fatalf("broken base requested no low key: %v", due)
	}
}

func TestSlowWriterBlocksOnlyItsOwnViewer(t *testing.T) {
	h := newHarness(t)
	var views []*viewer
	var writers []*recordWriter
	for i := range 2 {
		p := &Peer{Room: h.r, done: make(chan struct{})}
		p.admitted.Store(true)
		w := &recordWriter{}
		if i == 0 {
			w.block = make(chan struct{})
		}
		p.view = newViewer(p, h.r, &boundTrack{binding: binding{writer: w, ssrc: 10, mediaPT: 98}}, &boundTrack{})
		h.r.mu.Lock()
		h.r.viewers[p] = true
		h.r.mu.Unlock()
		views, writers = append(views, p.view), append(writers, w)
		go p.view.run()
		defer p.Close()
	}
	defer close(writers[0].block)
	for _, v := range views {
		for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
			v.mu.Lock()
			joined := v.joined
			v.mu.Unlock()
			if joined {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("viewer did not join")
			}
		}
	}
	start := time.Now()
	h.now = time.Now()
	h.send(ctrl.Low, desc{key: true}, 2, 500)
	for i := range 30 {
		h.now = time.Now()
		h.send(ctrl.Low, desc{tid: [4]uint8{0, 2, 1, 2}[(i+1)%4]}, 1, 300)
		time.Sleep(15 * time.Millisecond)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("ingress blocked behind a slow viewer for %s", d)
	}
	for deadline := time.Now().Add(2 * time.Second); len(writers[1].written()) < 28; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("healthy viewer starved: %d packets", len(writers[1].written()))
		}
	}
}

func TestPathChangeFencesRepairOfOlderGenerations(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	h.send(ctrl.Low, desc{key: true}, 2, 500)
	h.drain(v, 100*time.Millisecond)
	old := w.written()[0].SequenceNumber
	v.pathChange(h.now)
	v.nackVideo(h.now, []uint16{old})
	if b, _ := pacerQueue(v); b != 0 {
		t.Fatal("a packet from the previous path generation was retransmitted")
	}
	h.send(ctrl.Low, desc{tid: 2}, 1, 500)
	h.drain(v, 100*time.Millisecond)
	fresh := w.written()[2].SequenceNumber
	v.nackVideo(h.now, []uint16{fresh})
	h.drain(v, 100*time.Millisecond)
	sent := w.written()
	if last := sent[len(sent)-1]; len(sent) != 4 || last.SequenceNumber != fresh {
		t.Fatal("a current-generation NACK was not answered")
	}
}

type recordingEstimator struct {
	ctrl.Estimator
	arrivals []ctrl.Arrival
}

func (e *recordingEstimator) OnFeedback(now time.Time, f ctrl.Feedback) {
	e.arrivals = append(e.arrivals, f.Arrivals...)
	e.Estimator.OnFeedback(now, f)
}

func TestTWCCUnwrapsTransportSequenceAndIgnoresDuplicates(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	est := &recordingEstimator{Estimator: v.ctl.Est}
	v.ctl.Est = est
	v.tseq = 70_000
	small := rtcp.TypeTCCPacketReceivedSmallDelta
	fb := &rtcp.TransportLayerCC{BaseSequenceNumber: 69_990 - 65_536, PacketStatusCount: 3, ReferenceTime: 10, FbPktCount: 7,
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.StatusVectorChunk{SymbolSize: rtcp.TypeTCCSymbolSizeTwoBit, SymbolList: []uint16{small, rtcp.TypeTCCPacketNotReceived, small}}},
		RecvDeltas:   []*rtcp.RecvDelta{{Type: small, Delta: 1000}, {Type: small, Delta: 2000}}}
	v.feedback(h.now, fb)
	v.feedback(h.now, fb)
	a := est.arrivals
	if len(a) != 3 || a[0].Seq != 69_990 || a[1].Seq != 69_991 || a[2].Seq != 69_992 {
		t.Fatalf("arrivals %+v", a)
	}
	if a[0].At.IsZero() || !a[1].At.IsZero() || a[2].At.Sub(a[0].At) != 2*time.Millisecond {
		t.Fatalf("arrival times %+v", a)
	}
}

func TestReplacementInheritsTheHandedOffRate(t *testing.T) {
	h := newHarness(t)
	a, _ := h.viewer()
	b, _ := h.viewer()
	a.inherit(ctrl.RateState{Rate: 200_000})
	b.peer.InheritPath(a.peer)
	if got := b.peer.AppliedWireRate(); got != 200_000 {
		t.Fatalf("replacement rate %d, want the handed-off 200000", got)
	}
	if b.ctl.Generation() == 0 {
		t.Fatal("replacement kept the previous path generation")
	}
}

func TestRTCPCountsOnlyThisReadersSSRC(t *testing.T) {
	h := newHarness(t)
	v, _ := h.viewer()
	const video, audio = 10, 20
	v.handleRTCP(h.now, video, true, []rtcp.Packet{&rtcp.ReceiverReport{Reports: []rtcp.ReceptionReport{{SSRC: video, FractionLost: 1}, {SSRC: audio, FractionLost: 255}}}})
	if v.peer.residualLoss.Load() != 1 || v.peer.reports.Load() != 1 {
		t.Fatalf("audio report block reached video: %d/%d", v.peer.residualLoss.Load(), v.peer.reports.Load())
	}
	h.send(ctrl.Low, desc{key: true}, 1, 500)
	h.drain(v, 100*time.Millisecond)
	v.pushAudio(h.now, &rtp.Packet{Header: rtp.Header{Timestamp: 960}, Payload: []byte{1, 2, 3}}, 1)
	h.drain(v, 20*time.Millisecond)
	// Video NACKs arriving with the audio sender's compound are not audio's, and vice versa.
	v.handleRTCP(h.now, audio, false, []rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: video, Nacks: rtcp.NackPairsFromSequenceNumbers([]uint16{1})}})
	v.handleRTCP(h.now, video, true, []rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: audio, Nacks: rtcp.NackPairsFromSequenceNumbers([]uint16{1})}})
	if b, _ := pacerQueue(v); b != 0 {
		t.Fatal("a NACK for the other SSRC was answered")
	}
	v.handleRTCP(h.now, audio, false, []rtcp.Packet{&rtcp.TransportLayerNack{MediaSSRC: audio, Nacks: rtcp.NackPairsFromSequenceNumbers([]uint16{1})}})
	if b, _ := pacerQueue(v); b == 0 {
		t.Fatal("audio NACK not answered")
	}
}

func TestParityProtectsDepartedKeyPackets(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	v.mu.Lock()
	v.video.binding.red, v.video.binding.redPT, v.video.binding.fecPT = true, 116, 117
	v.fec, v.interleave = 0.25, 1
	v.mu.Unlock()
	h.send(ctrl.Low, desc{key: true}, 8, 500)
	h.drain(v, 500*time.Millisecond)
	var media []uint16
	var parity []*rtp.Packet
	for _, p := range w.written() {
		if p.PayloadType != 116 {
			t.Fatal("media not RED-encapsulated")
		}
		if p.Payload[0] == 117 {
			parity = append(parity, p)
		} else {
			media = append(media, p.SequenceNumber)
		}
	}
	if len(media) != 8 || len(parity) != 2 {
		t.Fatalf("media %d parity %d", len(media), len(parity))
	}
	if base := binary.BigEndian.Uint16(parity[0].Payload[3:5]); base != media[0] || binary.BigEndian.Uint16(parity[0].Payload[13:15]) != 0xf000 {
		t.Fatalf("parity covers base %d mask %x, media starts %d", base, binary.BigEndian.Uint16(parity[0].Payload[13:15]), media[0])
	}
}

// A negotiated viewer PC binds both tracks and routes RTCP per sender.
func TestViewerPCBindsTracksAndReadsVideoReports(t *testing.T) {
	e, err := New("127.0.0.1:0", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	p, err := e.Peer("review", false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	c.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	g := webrtc.GatheringCompletePromise(c)
	o, err := c.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SetLocalDescription(o); err != nil {
		t.Fatal(err)
	}
	<-g
	if err = p.PC.SetRemoteDescription(*c.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	g = webrtc.GatheringCompletePromise(p.PC)
	a, err := p.PC.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.PC.SetLocalDescription(a); err != nil {
		t.Fatal(err)
	}
	<-g
	if err = c.SetRemoteDescription(*p.PC.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.ConnectionState() != webrtc.PeerConnectionStateConnected && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.ConnectionState() != webrtc.PeerConnectionStateConnected {
		t.Fatal("not connected")
	}
	if b := p.view.video.bound(); b.writer == nil || b.ssrc == 0 || p.view.audio.bound().writer == nil {
		t.Fatal("tracks not bound")
	}
	vs, as := p.view.video.bound().ssrc, p.view.audio.bound().ssrc
	if err = c.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverReport{SSRC: 1234, Reports: []rtcp.ReceptionReport{{SSRC: vs, FractionLost: 1}, {SSRC: as, FractionLost: 255}}}}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for p.reports.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a late audio block would land now
	if p.residualLoss.Load() != 1 || p.reports.Load() != 1 {
		t.Fatalf("video report: loss=%d reports=%d, want 1/1", p.residualLoss.Load(), p.reports.Load())
	}
}
