package server

import (
	"encoding/json"
	"testing"
)

// Safe mode on is restrictive (any channel); turning it off loosens privacy
// and is local only (CONTRACT §8.5, §9.6).
func TestSafeModeCommandFollowsTheMonotonicRule(t *testing.T) {
	for _, c := range []struct{ args, want string }{
		{`{"on":true}`, ""},
		{`{"on":false}`, "local_owner_required"},
		{`{}`, "invalid_argument"},
		{`{"on":null}`, "invalid_argument"},
		{`{"on":"yes"}`, "invalid_argument"},
		{`{"on":true,"extra":1}`, "invalid_argument"},
	} {
		v := commandRequest{Op: "safe"}
		if err := json.Unmarshal([]byte(c.args), &v.Args); err != nil {
			t.Fatal(err)
		}
		if got := validateCommand(v); got != c.want {
			t.Errorf("safe %s: got %q, want %q", c.args, got, c.want)
		}
	}
}
