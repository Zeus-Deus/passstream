package sfu

import (
	"os"
	"testing"
)

// The test-only forwarding hooks change nothing unless their variables are set.
func TestForwardingTestHooksOffByDefault(t *testing.T) {
	if os.Getenv("PASSSTREAM_TEST_MAX_TID") == "" && testMaxTID != 2 {
		t.Fatalf("max TID cap %d without PASSSTREAM_TEST_MAX_TID", testMaxTID)
	}
	if os.Getenv("PASSSTREAM_TEST_BLACKHOLE_TID") == "" && testBlackholeTID <= 2 {
		t.Fatalf("blackhole TID %d without PASSSTREAM_TEST_BLACKHOLE_TID", testBlackholeTID)
	}
}
