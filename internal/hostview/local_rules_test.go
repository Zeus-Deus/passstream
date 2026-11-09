package hostview

// Ported from the daemon's former internal/host/join copy, so both callers keep
// the rules they were reviewed against.

import "testing"

func lconn(id string, joined int64, left *int64) Connection {
	return Connection{ConnectionID: id, JoinedAt: joined, LeftAt: left}
}

func ladm(aid, vid, peer string, connected bool, at int64) Admission {
	return Admission{AdmissionID: aid, ViewerID: vid, RowCode: "K7P", PeerID: peer, Connected: connected, AdmittedAt: at}
}

func lrow(l ViewerList, id string) *Viewer {
	for i := range l.Viewers {
		if l.Viewers[i].RowID == id {
			return &l.Viewers[i]
		}
	}
	return nil
}

func lhas(fs []string, f string) bool {
	for _, x := range fs {
		if x == f {
			return true
		}
	}
	return false
}

func TestLocalJoinRules(t *testing.T) {
	now := int64(1_000_000)
	at := now - 1000
	st := &SenderState{
		Admissions: []Admission{
			ladm("a1", "v1", "c_1", true, 10), ladm("a1b", "v1", "c_1", true, 20), // a grant re-admission: one claim
			ladm("a2", "v2", "c_s", true, 10), ladm("a3", "v3", "c_s", true, 10), // two viewers on one connection
			ladm("a4", "v4", "c_gone", true, 10),                                   // connected, but no open connection
			ladm("a5", "v5", "c_h", false, 30), ladm("a6", "v6", "c_h", false, 30), // historical tie
			ladm("a7", "v7", "c_7", false, 10), // away, connection still open
		},
		Knocks: []Knock{{KnockID: "k1", PeerID: "c_k", RowCode: "ACD"}, {KnockID: "k2", PeerID: "c_1", RowCode: "ACE"}},
	}
	conns := []Connection{lconn("c_1", 1, nil), lconn("c_s", 2, nil), lconn("c_h", 3, nil), lconn("c_7", 4, nil),
		lconn("c_k", 5, nil), lconn("c_new", 6, nil)}
	l, _, _ := Join(JoinInput{Source: "local", Now: now, State: st, StateAt: &at, PublisherOnline: true, Conns: conns}, JoinMemory{})
	r1 := lrow(l, "v:v1")
	if r1 == nil || r1.Status != "watching" || len(r1.Connections) != 1 || r1.AdmissionIDs[0] != "a1b" {
		t.Fatalf("v1 %+v", r1)
	}
	for _, v := range []string{"v:v2", "v:v3"} {
		if r := lrow(l, v); !lhas(r.Flags, "shared_connection") || len(r.Connections) != 0 {
			t.Errorf("%s %+v", v, r)
		}
	}
	if r := lrow(l, "v:v4"); !lhas(r.Flags, "no_connection") {
		t.Errorf("v4 %+v", r)
	}
	if r5, r6 := lrow(l, "v:v5"), lrow(l, "v:v6"); len(r5.Connections)+len(r6.Connections) != 0 || r5.Status != "left" {
		t.Errorf("historical tie attaches to neither: %+v %+v", r5, r6)
	}
	if r := lrow(l, "v:v7"); r.Status != "reconnecting" {
		t.Errorf("v7 %+v", r)
	}
	if r := lrow(l, "k:k1"); r.Status != "knocking" || len(r.Connections) != 1 {
		t.Errorf("k1 %+v", r)
	}
	if r := lrow(l, "k:k2"); len(r.Connections) != 0 {
		t.Errorf("a knock never takes a connection with a live claim: %+v", r)
	}
	if r := lrow(l, "c:c_new"); r == nil || r.Status != "joining" {
		t.Errorf("joining %+v", r)
	}
	if l.Watching != 4 || l.Knocking != 2 || l.SenderStale {
		t.Errorf("counts %d %d stale %v", l.Watching, l.Knocking, l.SenderStale)
	}
	if l.Viewers[0].Status != "watching" || l.Viewers[len(l.Viewers)-1].Status != "left" {
		t.Error("order")
	}
}

func TestLocalJoinLeftRowsAndHistory(t *testing.T) {
	at := int64(0)
	st := &SenderState{Admissions: []Admission{ladm("a1", "v1", "c_1", true, 1)}}
	l, mem, _ := Join(JoinInput{Now: 1000, State: st, StateAt: &at, PublisherOnline: true, Conns: []Connection{lconn("c_1", 1, nil)}}, JoinMemory{})
	if l.Watching != 1 {
		t.Fatal("watching")
	}
	// Kicked: the admission leaves the state with an End.
	st2 := &SenderState{Ends: []End{{Kind: "admission", AdmissionID: "a1", ViewerID: "v1", Cause: "kicked"}}}
	l, mem, h := Join(JoinInput{Now: 2000, State: st2, StateAt: &at, PublisherOnline: false, Conns: nil}, mem)
	r := lrow(l, "v:v1")
	if r == nil || r.Status != "left" || *r.LeftAt != 2000 || len(h) != 0 || !l.SenderStale {
		t.Fatalf("left row %+v history %v", r, h)
	}
	// Five minutes later it goes to history with its cause.
	l, _, h = Join(JoinInput{Now: 2000 + 5*60_000, State: &SenderState{}, StateAt: &at, PublisherOnline: true}, mem)
	if lrow(l, "v:v1") != nil || len(h) != 1 || h[0].End != "kicked" {
		t.Fatalf("history %+v", h)
	}
}
