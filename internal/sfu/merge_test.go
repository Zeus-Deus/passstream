package sfu

import (
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestVideoOfferRequiresVP9ProfileZero(t *testing.T) {
	for _, tc := range []struct {
		name, codec, fmtp string
		pass              bool
	}{
		{"VP9", "VP9", "profile-id=0", true},
		{"VP9 default", "VP9", "", true},
		{"VP8", "VP8", "", false},
		{"VP9 profile 2", "VP9", "profile-id=2", false},
		{"H264", "H264", "profile-level-id=42001f", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 98\r\na=rtpmap:98 " + tc.codec + "/90000\r\na=fmtp:98 " + tc.fmtp + "\r\n"}
			err := ValidateVideoOffer(offer)
			if (err == nil) != tc.pass {
				t.Fatalf("ValidateVideoOffer: %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "requires VP9 profile 0") {
				t.Fatal(err)
			}
		})
	}
}
