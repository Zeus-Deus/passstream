package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/store"
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

func TestSenderStateSafeModeCompatibility(t *testing.T) {
	state := validState(store.ID(), store.ID())
	state.Access.Latches.Locked = true
	state.Admissions = []hostview.Admission{{AdmissionID: store.ID(), ViewerID: store.ID(), PeerID: "c_" + store.ID(), RowCode: "ACD"}}
	for _, safe := range []*bool{nil, new(false), new(true)} {
		state.Safe = safe
		wire, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseState(wire)
		if err != nil || !reflect.DeepEqual(got, state) || got.SafeEnabled() != (safe != nil && *safe) {
			t.Fatalf("state %s decoded to %+v, %v", wire, got, err)
		}
	}
}
