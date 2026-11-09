package server

import (
	"encoding/json"
	"testing"
)

// Viewer drawing off and clearing are restrictive (any channel); turning
// viewer drawing on is permissive and local only (CONTRACT §8.5, §9.6).
func TestAnnotationCommandsFollowTheMonotonicRule(t *testing.T) {
	req := func(op, args string) commandRequest {
		v := commandRequest{Op: op}
		if args != "" {
			if err := json.Unmarshal([]byte(args), &v.Args); err != nil {
				t.Fatal(err)
			}
		}
		return v
	}
	for _, c := range []struct{ op, args, want string }{
		{"annotations", `{"viewers":false}`, ""},
		{"annotations", `{"viewers":true}`, "local_owner_required"},
		{"annotations", `{}`, "invalid_argument"},
		{"annotations", `{"viewers":null}`, "invalid_argument"},
		{"annotations", `{"viewers":"no"}`, "invalid_argument"},
		{"annotations", `{"viewers":false,"extra":1}`, "invalid_argument"},
		{"annotations.clear", `{}`, ""},
		{"annotations.clear", `{"scope":"all"}`, "invalid_argument"},
		{"pause", `{}`, ""},
		{"resume", `{}`, "local_owner_required"},
	} {
		if got := validateCommand(req(c.op, c.args)); got != c.want {
			t.Errorf("%s %s: got %q, want %q", c.op, c.args, got, c.want)
		}
	}
}
