package hostview

import (
	"encoding/json"
	"sort"
	"testing"
)

func TestReviewDeterministicFlagsAndKnockFirstAt(t *testing.T) {
	in := JoinInput{Source: "server", Now: 500, State: &SenderState{Admissions: []Admission{
		{AdmissionID: "a", ViewerID: "one", PeerID: "missing", Connected: true},
		{AdmissionID: "b", ViewerID: "two", PeerID: "missing", Connected: true},
		{AdmissionID: "c", ViewerID: "one", PeerID: "other-missing", Connected: true},
	}, Knocks: []Knock{{KnockID: "knock", PeerID: "p", At: 100, ExpiresAt: 1000}}}, Conns: []Connection{{ConnectionID: "p", JoinedAt: 50}}}
	first, mem, _ := Join(in, JoinMemory{})
	expected, _ := json.Marshal(first)
	for i := 0; i < 100; i++ {
		got, _, _ := Join(in, JoinMemory{})
		b, _ := json.Marshal(got)
		if string(b) != string(expected) {
			t.Fatalf("unstable join: %s vs %s", b, expected)
		}
		for _, v := range got.Viewers {
			if !sort.StringsAreSorted(v.Flags) {
				t.Fatal(v.Flags)
			}
		}
	}
	in.Now = 600
	in.State.Knocks = nil
	_, _, history := Join(in, mem)
	if len(history) != 1 || history[0].FirstAt != 100 {
		t.Fatal(history)
	}
}
