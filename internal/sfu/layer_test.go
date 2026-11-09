package sfu

import "testing"

func TestTemporalSwitchNeedsRetainedUpgradePoint(t *testing.T) {
	if got := selectLayer(2, 0, 2, false, false, false); got != 0 {
		t.Fatal("downswitch delayed")
	}
	if got := selectLayer(0, 2, 2, false, true, false); got != 0 {
		t.Fatal("upswitch on discarded dependency")
	}
	if got := selectLayer(0, 2, 0, false, false, false); got != 0 {
		t.Fatal("upswitch without U")
	}
	if got := selectLayer(0, 2, 0, false, true, false); got != 2 {
		t.Fatal("retained U did not unlock enhancement layers")
	}
	if got := selectLayer(0, 2, 0, true, false, false); got != 2 {
		t.Fatal("keyframe did not unlock layers")
	}
}

func TestFlexibleUpswitchChecksReferencesInsteadOfWaitingForU(t *testing.T) {
	p := &Pacer{}
	p.pictures.mark(100, true)
	if selectLayer(0, 2, 1, false, false, true) != 2 {
		t.Fatal("flexible mode waited indefinitely for a non-flexible U point")
	}
	if !p.allowTemporalPicture(pacedPacket{flexible: true, picture: 102, references: [3]uint8{2}, referenceCount: 1}) {
		t.Fatal("explicit retained base did not permit the enhancement")
	}
	if p.allowTemporalPicture(pacedPacket{flexible: true, picture: 102, references: [3]uint8{1}, referenceCount: 1}) {
		t.Fatal("upswitch bypassed an explicit missing reference")
	}
}
