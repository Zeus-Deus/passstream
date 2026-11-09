package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/passstream/passstream/internal/sfu"
	"github.com/passstream/passstream/internal/store"
	"github.com/pion/webrtc/v4"
)

func signalMedia(t *testing.T, f *fixture, joined map[string]json.RawMessage) *sfu.Peer {
	t.Helper()
	var token string
	if err := json.Unmarshal(joined["session"], &token); err != nil {
		t.Fatal(err)
	}
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	return f.s.peers[store.Hash(token)]
}

func TestPingReportsDecodedWithClientSampleTime(t *testing.T) {
	var count uint64
	var at int64
	frames := uint64(17)
	pong := signalPong(message{FreshFrames: &frames, At: 1500}, func(n uint64, sample int64) { count = n; at = sample }, func() int64 { return 725 })
	if count != 17 || at != 1500 || pong["refreshMs"] != int64(725) {
		t.Fatalf("ping sample: %d at %d, pong %v", count, at, pong)
	}
	signalPong(message{}, func(uint64, int64) { t.Fatal("missing sample reported") }, func() int64 { return 0 })
}

func TestPongCarriesRefreshTransitMS(t *testing.T) {
	f := setup(t)
	_, _, view := f.create(`{}`)
	c, j := f.join("", "view", view, "10.0.0.2")
	media := signalMedia(t, f, j)
	sendWS(t, c, map[string]any{"type": "ping"})
	pong := readWS(t, c)
	var refresh int64
	if err := json.Unmarshal(pong["refreshMs"], &refresh); err != nil || refresh != media.RefreshTransitMS() {
		t.Fatalf("refresh advisory: %s (%v)", pong["refreshMs"], err)
	}
}

func TestReplacementInheritsAppliedViewerPath(t *testing.T) {
	f := setup(t)
	_, _, view := f.create(`{}`)
	c, j := f.join("", "view", view, "10.0.0.3")
	previous := signalMedia(t, f, j)
	// Obtain an actual small path budget via the same inheritance seam used in production.
	seed := &sfu.Peer{Pacer: sfu.NewPacer()}
	defer seed.Pacer.Close()
	seed.Pacer.SetTargetBitrate(128000)
	previous.InheritPath(seed)
	expected := previous.AppliedWireRate()
	var token string
	json.Unmarshal(j["session"], &token)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	next, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.h.URL, "http")+"/signal", &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"http://127.0.0.1:47823"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer next.CloseNow()
	c.CloseNow()
	if err = wsjson.Write(ctx, next, message{Type: "join", Protocol: 1, Role: "view", Session: token, Replace: true}); err != nil {
		t.Fatal(err)
	}
	joined := readWS(t, next)
	replacement := signalMedia(t, f, joined)
	if replacement == previous || replacement.AppliedWireRate() != expected {
		t.Fatalf("replacement rate=%d, previous=%d", replacement.AppliedWireRate(), expected)
	}
}

func TestUnsupportedVideoOfferReturnsClearError(t *testing.T) {
	f := setup(t)
	_, _, view := f.create(`{}`)
	c, j := f.join("", "view", view, "10.0.0.4")
	sendWS(t, c, map[string]any{"type": "offer", "sdp": webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=rtpmap:96 VP8/90000\r\n"}})
	reply := readWS(t, c)
	if string(reply["code"]) != `"unsupported_codec"` || signalMedia(t, f, j).PC.RemoteDescription() != nil {
		t.Fatalf("unsupported offer accepted: %v", reply)
	}
}
