package daemon

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"testing"
)

func TestActionFingerprintUsesProcessHMAC(t *testing.T) {
	args := map[string]any{"pin": "4821"}
	b, _ := json.Marshal([]any{"pin.rotate", args})
	mac := hmac.New(sha256.New, actionFingerprintKey)
	mac.Write(b)
	fp := actionFingerprint("pin.rotate", args)
	plain := sha256.Sum256(b)
	if !hmac.Equal(fp[:], mac.Sum(nil)) || fp == plain || len(actionFingerprintKey) != 32 {
		t.Fatal("fingerprint is not a process-keyed HMAC")
	}
	if fp != actionFingerprint("pin.rotate", args) || fp == actionFingerprint("pin.rotate", map[string]any{"pin": "4822"}) {
		t.Fatal("unstable or nonbinding fingerprint")
	}
}
