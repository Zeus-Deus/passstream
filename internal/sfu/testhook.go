package sfu

import (
	"log"
	"os"
	"strconv"
)

// Test-only forwarding hooks for the Chromium TL2-drop decode check
// (web/vp9-tl2-drop.mjs, docs/REPORT.md). Each is off unless its environment
// variable is set when the server starts; nothing in production sets them.
//
// PASSSTREAM_TEST_MAX_TID=N caps every viewer's forwarded temporal layer at N.
// The cap goes through the forwarder's normal max-TID path (the TID handed to
// ctrl.Dependency.Allow), and ctrl is told the capped layer is applied. It
// does not change any controller decision.
//
// PASSSTREAM_TEST_BLACKHOLE_TID=N silently discards every delta picture of
// temporal layer N after continuity accepted it, without reporting a loss. It
// is a negative control: it removes references the decoder needs, bypassing
// the base-only rule.
var (
	testMaxTID       = testTID("PASSSTREAM_TEST_MAX_TID", 2)
	testBlackholeTID = testTID("PASSSTREAM_TEST_BLACKHOLE_TID", 255)
)

func testTID(name string, off uint8) uint8 {
	s := os.Getenv(name)
	if s == "" {
		return off
	}
	n, err := strconv.ParseUint(s, 10, 8)
	if err != nil || n > 2 {
		log.Fatalf("%s=%q: want 0, 1 or 2", name, s)
	}
	log.Printf("TEST HOOK ACTIVE: %s=%d (never set this in production)", name, n)
	return uint8(n)
}
