package sfu

import (
	"fmt"
	"strings"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// ValidateVideoOffer refuses an active video m-line without VP9 profile 0.
// Every forwarding/repair path interprets non-flexible VP9 L1T3 descriptors;
// silently accepting another codec would bypass those dependency guarantees.
func ValidateVideoOffer(offer webrtc.SessionDescription) error {
	var session sdp.SessionDescription
	if err := session.Unmarshal([]byte(offer.SDP)); err != nil {
		return fmt.Errorf("invalid SDP: %w", err)
	}
	for _, media := range session.MediaDescriptions {
		if media.MediaName.Media != "video" || media.MediaName.Port.Value == 0 {
			continue
		}
		if _, inactive := media.Attribute("inactive"); inactive {
			continue
		}
		supported := false
		for _, pt := range media.MediaName.Formats {
			vp9, profile0 := false, true
			for _, a := range media.Attributes {
				fields := strings.Fields(a.Value)
				if len(fields) < 2 || fields[0] != pt {
					continue
				}
				if a.Key == "rtpmap" {
					vp9 = strings.EqualFold(fields[1], "VP9/90000")
				}
				if a.Key == "fmtp" {
					for _, param := range strings.Split(strings.Join(fields[1:], ""), ";") {
						if strings.HasPrefix(param, "profile-id=") && param != "profile-id=0" {
							profile0 = false
						}
					}
				}
			}
			supported = supported || vp9 && profile0
		}
		if !supported {
			return fmt.Errorf("unsupported_codec: PassStream requires VP9 profile 0 (non-flexible L1T3)")
		}
	}
	return nil
}
