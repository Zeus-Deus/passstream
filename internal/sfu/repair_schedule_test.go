package sfu

import (
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

type fixedRepairEstimator struct{ ctrl.FixedEstimator }

func (e *fixedRepairEstimator) Estimate(now time.Time) ctrl.Estimate {
	x := e.FixedEstimator.Estimate(now)
	x.RTT = 80 * time.Millisecond
	return x
}

// A warm source continues producing while a native key is paced. Its
// lower-priority deltas are not ahead of a key's RTX in Queue.best.
func TestKeyRepairOvertakesWarmDeltaBacklog(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	v.mu.Lock()
	v.ctl.Est = &fixedRepairEstimator{ctrl.FixedEstimator{Rate: 1_500_000}}
	v.ctl.Probe = ctrl.NoProber{}
	v.ctl.Alloc = &keySwitchAllocation{desired: ctrl.Option{Rendition: ctrl.Native, TID: 2}}
	v.step(h.now)
	v.ctl.Pacer.SetCluster(nil)
	v.mu.Unlock()
	h.send(ctrl.Native, desc{key: true}, 200, 1175)
	h.drain(v, 10*time.Millisecond)
	if len(w.written()) == 0 {
		t.Fatal("no early key packet")
	}
	seq := w.written()[0].SequenceNumber
	for i := 0; i < 10; i++ {
		tid := []uint8{2, 1, 2, 0}[i%4]
		h.send(ctrl.Native, desc{tid: tid}, 17, 1175)
		h.drain(v, 65*time.Millisecond)
	}
	v.mu.Lock()
	c := v.cache.get(seq)
	if c == nil || !c.frame.info.Key || !h.now.Before(c.deadline) {
		v.mu.Unlock()
		t.Fatal("key cache/deadline fixture invalid")
	}
	queued, _ := v.ctl.Pacer.Queued()
	predicted, scheduled := v.arrival(h.now, ctrl.KindVideo, c.pkt.MarshalSize()+wireOverhead)
	if !scheduled {
		v.mu.Unlock()
		t.Fatal("repair schedule unexpectedly unknown")
	}
	v.resendVideo(h.now, []uint16{seq}, false)
	rtx := 0
	for _, p := range v.pending {
		if p.kind == outRTX {
			rtx++
		}
	}
	t.Logf("queued=%d, repair prediction=%v, remaining deadline=%v, RTX admitted=%d", queued, predicted.Sub(h.now), c.deadline.Sub(h.now), rtx)
	if rtx != 1 {
		t.Errorf("timely key RTX was refused behind lower-priority queued originals")
	}
	// Scheduler control: the same candidate, admitted directly, goes next
	// under the unchanged rate and deadline. No expedited/bypass send.
	if rtx == 0 {
		v.push(h.now, &outPacket{kind: outRTX, pkt: c.pkt}, ctrl.Packet{Size: c.pkt.MarshalSize() + wireOverhead, Kind: ctrl.KindVideo, Class: ctrl.ClassRTX, Rendition: ctrl.Native, Frame: c.frame.info.Picture, TID: 0, Last: true, Packets: 1, Deadline: c.deadline})
	}
	v.mu.Unlock()
	before := len(w.written())
	h.drain(v, 20*time.Millisecond)
	if !h.now.Add(40 * time.Millisecond).Before(c.deadline) {
		t.Fatal("real paced RTX departure missed its deadline")
	}
	after := w.written()[before:]
	if len(after) == 0 || after[0].SequenceNumber != seq {
		t.Fatal("RTX did not actually overtake the ordinary backlog")
	}
}

func TestVideoNACKBatchAccountsForEveryPreviouslyAdmittedRTX(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	v.mu.Lock()
	v.ctl.Est = &fixedRepairEstimator{ctrl.FixedEstimator{Rate: 1_500_000}}
	v.ctl.Probe = ctrl.NoProber{}
	v.ctl.Alloc = &keySwitchAllocation{desired: ctrl.Option{Rendition: ctrl.Low, TID: 2}}
	v.step(h.now)
	v.ctl.Pacer.SetCluster(nil)
	v.mu.Unlock()
	h.send(ctrl.Low, desc{key: true}, 20, 1000)
	h.drain(v, 200*time.Millisecond)
	var seqs []uint16
	for _, p := range w.written() {
		seqs = append(seqs, p.SequenceNumber)
	}
	v.mu.Lock()
	v.ctl.Est = &fixedRepairEstimator{ctrl.FixedEstimator{Rate: 128_000}}
	v.ctl.Pacer.SetRate(h.now, 128_000)
	deadline := h.now.Add(120 * time.Millisecond)
	for _, seq := range seqs {
		v.cache.get(seq).deadline = deadline
	}
	v.resendVideo(h.now, seqs, false)
	admitted := 0
	for _, seq := range seqs {
		admitted += v.cache.get(seq).sends
	}
	v.mu.Unlock()
	if admitted != 2 {
		t.Fatalf("batch admitted %d, want exactly2 that can arrive before120ms", admitted)
	}
	before := len(w.written())
	h.drain(v, 80*time.Millisecond)
	if got := len(w.written()) - before; got != admitted {
		t.Fatalf("timely departure count%d, admitted%d", got, admitted)
	}
}

func TestBlockedTransportWriterCannotPromiseRepairDeadline(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	w.block = make(chan struct{})
	defer close(w.block)
	defer v.peer.Close()
	h.now = time.Now()
	h.send(ctrl.Low, desc{key: true}, 3, 500)
	go v.run()
	until := time.Now().Add(time.Second)
	for v.writerPending.Load() == 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if v.writerPending.Load() == 0 {
		t.Fatal("writer never acquired a batch")
	}
	v.mu.Lock()
	var seq uint16
	found := false
	for _, c := range v.cache.entries {
		if c.pkt != nil {
			seq = c.pkt.SequenceNumber
			found = true
			break
		}
	}
	if !found {
		v.mu.Unlock()
		t.Fatal("no cached in-flight packet")
	}
	v.resendVideo(time.Now(), []uint16{seq}, false)
	if v.cache.get(seq).sends != 0 {
		v.mu.Unlock()
		t.Fatal("blocked write was assumed complete for RTX admission")
	}
	v.mu.Unlock()
}

func TestAudioNACKBatchAccountsForQueuedAudioAndPreviouslyAdmittedRTX(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	v.mu.Lock()
	v.ctl.Est = &fixedRepairEstimator{ctrl.FixedEstimator{Rate: 128_000}}
	v.ctl.Pacer.SetRate(h.now, 128_000)
	v.ctl.Pacer.SetCluster(nil)
	var seqs []uint16
	for i := 0; i < 20; i++ {
		seq := uint16(100 + i)
		seqs = append(seqs, seq)
		p := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, SSRC: 20}, Payload: make([]byte, 120)}
		v.audioCache.put(cachedPacket{pkt: p, at: h.now, last: h.now, deadline: h.now.Add(80 * time.Millisecond), generation: v.ctl.Generation()})
	}
	// Existing video RTX is lower than audio RTX at equal class. Ordinary
	// queued audio still precedes both; prediction must use that exact order.
	v.push(h.now, &outPacket{kind: outRTX, pkt: &rtp.Packet{Header: rtp.Header{Version: 2}, Payload: make([]byte, 1000)}}, ctrl.Packet{Size: 1062, Kind: ctrl.KindVideo, Class: ctrl.ClassRTX, Packets: 1, Last: true})
	v.mu.Unlock()
	v.pushAudio(h.now, &rtp.Packet{Header: rtp.Header{Version: 2}, Payload: make([]byte, 1000)}, 1)
	v.nackAudio(h.now, seqs)
	v.mu.Lock()
	admitted := 0
	for _, seq := range seqs {
		admitted += v.audioCache.get(seq).sends
	}
	v.mu.Unlock()
	if admitted <= 0 || admitted >= len(seqs) {
		t.Fatalf("audio batch admitted%d of%d; expected only timely prefix", admitted, len(seqs))
	}
	h.drain(v, 40*time.Millisecond)
	sent := 0
	for _, p := range w.written() {
		if p.SSRC == 20 && p.SequenceNumber >= 100 {
			sent++
		}
	}
	if sent != admitted {
		t.Fatalf("audio RTX departed within40ms=%d, admitted%d", sent, admitted)
	}
}
