package hostview

import (
	"fmt"
	"reflect"
	"testing"
)

func TestJoinTable(t *testing.T) {
	now := int64(1000)
	a := Admission{AdmissionID: "a1", ViewerID: "v1", RowCode: "K7P", PeerID: "c_1", Connected: true, AdmittedAt: 100}
	b := Admission{AdmissionID: "a2", ViewerID: "v2", RowCode: "ACD", PeerID: "c_2", Connected: true, AdmittedAt: 200}
	cs := []Connection{{ConnectionID: "c_1", JoinedAt: 100}, {ConnectionID: "c_2", JoinedAt: 200}}
	tests := []struct {
		name       string
		admissions []Admission
		conns      []Connection
		online     bool
		watching   uint64
		flags      string
		attached   int
	}{
		{"distinct", []Admission{a, b}, cs, true, 2, "", 1},
		{"same grant", []Admission{a, {AdmissionID: "a3", ViewerID: "v1", PeerID: "c_1", Connected: true, AdmittedAt: 200}}, cs, true, 1, "", 1},
		{"shared", []Admission{a, {AdmissionID: "a2", ViewerID: "v2", PeerID: "c_1", Connected: true}}, cs, true, 2, "shared_connection", 0},
		{"missing", []Admission{a}, nil, true, 1, "no_connection", 0},
		{"stale", []Admission{a}, cs, false, 1, "sender_stale", 1},
		{"historical newest", []Admission{{AdmissionID: "a1", ViewerID: "v1", PeerID: "c_1", AdmittedAt: 300}, {AdmissionID: "a2", ViewerID: "v2", PeerID: "c_1", AdmittedAt: 200}}, cs, true, 0, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := JoinInput{Source: "server", Now: now, StateAt: &now, PublisherOnline: tt.online, State: &SenderState{Admissions: tt.admissions}, Conns: tt.conns}
			v, m, h := Join(in, JoinMemory{})
			v2, m2, h2 := Join(in, JoinMemory{})
			if !reflect.DeepEqual(v, v2) || !reflect.DeepEqual(m, m2) || !reflect.DeepEqual(h, h2) {
				t.Fatal("not deterministic")
			}
			if v.Watching != tt.watching {
				t.Fatal(v)
			}
			var row *Viewer
			for i := range v.Viewers {
				if v.Viewers[i].RowID == "v:v1" {
					row = &v.Viewers[i]
				}
			}
			if row == nil || len(row.Connections) != tt.attached {
				t.Fatal(v)
			}
			if tt.flags != "" {
				found := false
				for _, f := range row.Flags {
					found = found || f == tt.flags
				}
				if !found {
					t.Fatal(row)
				}
			}
		})
	}
}
func TestSwappedRelayCannotRetargetAdmission(t *testing.T) {
	n := int64(100)
	s := SenderState{Admissions: []Admission{{AdmissionID: "selected", ViewerID: "alice", PeerID: "c_bob", Connected: true}, {AdmissionID: "other", ViewerID: "bob", PeerID: "c_alice", Connected: true}}}
	v, _, _ := Join(JoinInput{Now: n, StateAt: &n, PublisherOnline: true, State: &s, Conns: []Connection{{ConnectionID: "c_alice"}, {ConnectionID: "c_bob"}}}, JoinMemory{})
	for _, r := range v.Viewers {
		if r.RowID == "v:alice" && (r.AdmissionIDs[0] != "selected" || r.Connections[0].ConnectionID != "c_bob") {
			t.Fatal(r)
		}
	}
}
func TestJoinHistory(t *testing.T) {
	for _, cause := range []string{"kicked", "revoked", "stream_ended", ""} {
		t.Run(cause, func(t *testing.T) {
			n := int64(100)
			s := SenderState{Admissions: []Admission{{AdmissionID: "a", ViewerID: "v", PeerID: "c", Connected: true, AdmittedAt: 1}}}
			in := JoinInput{Now: n, StateAt: &n, PublisherOnline: true, State: &s, Conns: []Connection{{ConnectionID: "c", JoinedAt: 1}}}
			_, mem, _ := Join(in, JoinMemory{})
			s.Admissions = nil
			if cause != "" {
				s.Ends = []End{{Kind: "admission", AdmissionID: "a", Cause: cause}}
			}
			in.Now = 200
			in.Conns = nil
			v, mem, h := Join(in, mem)
			if len(h) != 0 || v.Viewers[0].Status != "left" {
				t.Fatal(v, h)
			}
			in.Now = 200 + WindowMS
			v, _, h = Join(in, mem)
			want := cause
			if want == "" {
				want = "left"
			}
			if len(v.Viewers) != 0 || len(h) != 1 || h[0].End != want {
				t.Fatal(v, h)
			}
		})
	}
}
func TestKnockEndsAndStreamEnd(t *testing.T) {
	for reason, want := range map[string]string{"denied": "denied", "timeout": "knock_timeout", "pin_rotated": "denied", "commit_failed": "denied", "stopped": "stream_ended", "peer_closed": "left", "replaced": "left", "queue_full": "left"} {
		n := int64(1)
		s := SenderState{Knocks: []Knock{{KnockID: "k", PeerID: "c"}}}
		in := JoinInput{Now: n, StateAt: &n, State: &s}
		_, m, _ := Join(in, JoinMemory{})
		s.Knocks = nil
		s.Ends = []End{{Kind: "knock", KnockID: "k", Reason: reason}}
		_, _, h := Join(in, m)
		if len(h) != 1 || h[0].End != want {
			t.Fatal(reason, h)
		}
	}
}

func TestConnectionCapsTiesKnocksAndTerminalHistory(t *testing.T) {
	n := int64(1000)
	s := SenderState{Admissions: []Admission{}, Knocks: []Knock{}}
	cs := []Connection{}
	for i := 0; i < 110; i++ {
		cs = append(cs, Connection{ConnectionID: fmt.Sprint(i), JoinedAt: int64(i)})
	}
	in := JoinInput{Now: n, StateAt: &n, State: &s, PublisherOnline: true, Conns: cs}
	v, _, _ := Join(in, JoinMemory{})
	if len(v.Viewers) != 100 {
		t.Fatal(len(v.Viewers))
	}
	s.Admissions = []Admission{{AdmissionID: "a", ViewerID: "one", PeerID: "p", AdmittedAt: 1}, {AdmissionID: "b", ViewerID: "two", PeerID: "p", AdmittedAt: 1}}
	in.Conns = []Connection{{ConnectionID: "p"}}
	v, _, _ = Join(in, JoinMemory{})
	for _, r := range v.Viewers {
		if len(r.Connections) != 0 {
			t.Fatal("historical tie attached", r)
		}
	}
	s.Admissions = []Admission{{AdmissionID: "a", ViewerID: "one", PeerID: "p", Connected: true}}
	s.Knocks = []Knock{{KnockID: "k", PeerID: "p"}}
	v, _, _ = Join(in, JoinMemory{})
	for _, r := range v.Viewers {
		if r.Status == "knocking" && len(r.Connections) != 0 {
			t.Fatal("knock stole live claim")
		}
	}
	s.Admissions = nil
	v, mem, _ := Join(in, JoinMemory{})
	if v.Knocking != 1 {
		t.Fatal(v)
	}
	in.StreamEnded = true
	v, _, h := Join(in, mem)
	if len(v.Viewers) != 0 || len(h) != 1 || h[0].End != "stream_ended" {
		t.Fatal(v, h)
	}
	s.Admissions = []Admission{}
	s.Knocks = nil
	in.StreamEnded = false
	in.Conns = nil
	for i := 0; i < 5; i++ {
		s.Admissions = append(s.Admissions, Admission{AdmissionID: fmt.Sprint(i), ViewerID: "v", PeerID: fmt.Sprint(i), Connected: true, AdmittedAt: int64(i)})
		in.Conns = append(in.Conns, Connection{ConnectionID: fmt.Sprint(i), JoinedAt: int64(i)})
	}
	v, _, _ = Join(in, JoinMemory{})
	if len(v.Viewers) != 1 || len(v.Viewers[0].Connections) != 3 || v.Viewers[0].AdmissionIDs[0] != "4" {
		t.Fatal(v)
	}
}

func TestHistoryEmitsOnceUntilReconnection(t *testing.T) {
	n := int64(1)
	s := SenderState{Admissions: []Admission{{AdmissionID: "a", ViewerID: "v", PeerID: "c", AdmittedAt: 1}}}
	in := JoinInput{Now: n, StateAt: &n, State: &s}
	_, m, _ := Join(in, JoinMemory{})
	in.Now = WindowMS + 2
	_, m, h := Join(in, m)
	if len(h) != 1 {
		t.Fatal(h)
	}
	for i := 0; i < 3; i++ {
		v, next, h := Join(in, m)
		m = next
		if len(h) != 0 || len(v.Viewers) != 0 {
			t.Fatal("duplicate history", v, h)
		}
	}
	s.Admissions[0].Connected = true
	v, m, h := Join(in, m)
	if v.Watching != 1 || len(h) != 0 {
		t.Fatal(v, h)
	}
	in.StreamEnded = true
	_, m, h = Join(in, m)
	if len(h) != 1 {
		t.Fatal(h)
	}
	_, _, h = Join(in, m)
	if len(h) != 0 {
		t.Fatal("terminal history repeated", h)
	}
}
