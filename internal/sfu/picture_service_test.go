package sfu

import (
	"testing"
	"time"

	"github.com/passstream/passstream/internal/sfu/ctrl"
)

func TestPictureServiceCannotCreditABlockedPhysicalWrite(t *testing.T) {
	h := newHarness(t)
	v, w := h.viewer()
	w.block = make(chan struct{})
	defer close(w.block)
	defer v.peer.Close()
	h.now = time.Now()
	h.send(ctrl.Low, desc{key: true}, 3, 500)
	go v.run()
	for until := time.Now().Add(time.Second); v.writerPending.Load() == 0 && time.Now().Before(until); {
		time.Sleep(time.Millisecond)
	}
	if v.writerPending.Load() == 0 {
		t.Fatal("writer did not acquire physical work")
	}
	v.mu.Lock()
	query := ctrl.PictureQuery{Rendition: ctrl.Native, Frame: 8, Generation: v.ctl.Generation(), WriterPending: int(v.writerPending.Load()), Offer: []ctrl.Packet{{Kind: ctrl.KindVideo, Class: ctrl.ClassKey, Key: true, Rendition: ctrl.Native, Frame: 8, Size: 1200, FrameSize: 1200, Packets: 1, Last: true, Deadline: time.Now().Add(ctrl.KeyEnvelope)}}}
	result := v.ctl.PictureService(time.Now(), query)
	v.mu.Unlock()
	if result.Known || !result.Key.IsZero() {
		t.Fatal("blocked transport time credited as service")
	}
}
