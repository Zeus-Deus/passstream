package sfu

import (
	"sync"
	"testing"
	"time"
)

func TestSignalingFreshnessSharesTheHeartbeatDecoderCounter(t *testing.T) {
	p := &Peer{}
	now := time.Now()
	p.ReportDecoded(0, now.UnixMilli())
	if p.decoderReported.Load() {
		t.Fatal("zero signaling sample established decoder reporting")
	}
	p.ReportDecoded(10, now.UnixMilli())
	if !p.decoderReported.Load() || p.freshFrames.Load() != 10 {
		t.Fatal("working video without SCTP was not reported")
	}
	fresh := p.freshAt.Load()
	p.reportDecoded(now.Add(100*time.Millisecond), 10)
	if p.freshAt.Load() != fresh {
		t.Fatal("the other channel's duplicate refreshed decoder liveness")
	}
	p.reportDecoded(now.Add(500*time.Millisecond), 17)
	p.ReportDecoded(16, now.Add(400*time.Millisecond).UnixMilli())
	if p.freshFrames.Load() != 17 {
		t.Fatal("delayed channel delivery rolled the shared counter back")
	}
	if p.rtcpAt.Load() != 0 || p.heartbeatAt.Load() != 0 {
		t.Fatal("signaling decoder feedback masked transport failure")
	}
}

func TestPublisherCannotReportViewerDecoderFreshness(t *testing.T) {
	p := &Peer{Publish: true}
	p.ReportDecoded(10, time.Now().UnixMilli())
	if p.decoderReported.Load() || p.freshFrames.Load() != 0 {
		t.Fatal("publisher report changed viewer decoding")
	}
}

func TestConcurrentDecoderChannelsCannotRollProgressBack(t *testing.T) {
	p := &Peer{}
	var wg sync.WaitGroup
	for n := uint64(1); n <= 128; n++ {
		wg.Add(1)
		go func(count uint64) { defer wg.Done(); p.ReportDecoded(count, time.Now().UnixMilli()) }(n)
	}
	wg.Wait()
	if p.freshFrames.Load() != 128 {
		t.Fatal("concurrent channels rolled decoder progress back", p.freshFrames.Load())
	}
}
