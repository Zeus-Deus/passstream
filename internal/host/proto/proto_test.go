package proto

import (
	"strings"
	"testing"
)

func TestIDsAreCanonical(t *testing.T) {
	id := NewId22()
	tok, _ := NewToken43()
	if !IsId22(id) || !IsToken43(tok) || !IsConnID("c_"+id) {
		t.Fatal("fresh ids must validate")
	}
	// The last character of a 22-char id carries 4 spare bits; a non-zero
	// spare would be a second spelling of the same bytes.
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	alt := id[:21] + string(alpha[strings.IndexByte(alpha, id[21])+1])
	if IsId22(alt) {
		t.Fatalf("non-canonical id %s accepted", alt)
	}
	for _, s := range []string{"", "K7", "K7PX", "k7p", "B7P", "K7P "} {
		if IsRowCode(s) {
			t.Errorf("row code %q accepted", s)
		}
	}
	if !IsRowCode("K7P") {
		t.Error("K7P refused")
	}
}

func TestPolicy(t *testing.T) {
	ok := []any{map[string]any{"kind": "digits", "length": 4.0}, map[string]any{"kind": "none"}, map[string]any{"kind": "passphrase"}}
	for _, v := range ok {
		if _, err := ParsePinPolicy(v); err != nil {
			t.Errorf("%v: %v", v, err)
		}
	}
	bad := []any{nil, "none", map[string]any{"kind": "digits", "length": 3.0}, map[string]any{"kind": "digits", "length": 13.0},
		map[string]any{"kind": "digits", "length": 4.5}, map[string]any{"kind": "digits"}, map[string]any{"kind": "none", "length": 4.0},
		map[string]any{"kind": "pin"}}
	for _, v := range bad {
		if _, err := ParsePinPolicy(v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
	d4, d6 := PinPolicy{Kind: "digits", Length: 4}, PinPolicy{Kind: "digits", Length: 6}
	if Stricter(d4, d6) != d6 || Stricter(PinPolicy{Kind: "passphrase"}, d6).Kind != "passphrase" || Stricter(PinPolicy{Kind: "none"}, d4) != d4 {
		t.Fatal("stricter order")
	}
}

func TestCleanAndSchemas(t *testing.T) {
	if got := Clean("a‮b​c\x07d", 64, 256); got != "abcd" {
		t.Fatalf("clean %q", got)
	}
	if got := Clean(strings.Repeat("é", 100), 64, 256); len([]rune(got)) != 64 {
		t.Fatalf("clamp %d", len([]rune(got)))
	}
	var l Latches
	if err := Into([]byte(`{"locked":true,"paused":"yes","frozen":false}`), LatchesV, &l); err == nil {
		t.Fatal("a malformed latch must invalidate the object")
	}
	var a map[string]any
	if err := IntoValue(map[string]any{"kind": "pin_changed", "pin": map[string]any{"kind": "none"}, "by": "plugin"}, AlertV, &a); err == nil {
		t.Fatal("the page may only report pin_changed by window")
	}
	var s SenderState
	err := Into([]byte(`{"v":1,"stream_id":"AAAAAAAAAAAAAAAAAAAAAA","instance":"AAAAAAAAAAAAAAAAAAAAAA","seq":1,"end_reason":"future","phase":"warp","mode":"text","safe":true,
		"e2ee":{"status":"on","epoch":0,"suspended":false,"revocation_not_durable":false},
		"access":{"pin":{"required":false,"kind":"none","length":null},"pin_version":0,"rotation_pending":false,"reconciling":false,
		"latches":{"locked":false,"paused":false,"frozen":false},"knock":false,"failures":{"stream":0,"limit":20}},
		"admissions":[],"knocks":[],"ends":[],"health":null,"pin":"1234","link":"x"}`), SenderStateV, &s)
	if err != nil || s.Phase != "unknown" || !s.Safe || s.EndReason != "unknown" {
		t.Fatalf("state: %v %q", err, s.Phase)
	}
}
