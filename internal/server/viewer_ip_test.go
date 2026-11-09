package server

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/store"
)

// Scrubbing IPs from the persisted join memory must not scrub the live
// ViewerList: during the session the owner sees the HTTPS-observed IP.
func TestViewerListKeepsSessionIPWhilePersistedMemoryDoesNot(t *testing.T) {
	f := setup(t)
	id, pub, cap := f.create(`{}`)
	p, _ := f.join(id, "publish", pub, "10.0.0.1")
	readType(t, p, "e2ee.peers")
	_, joined := f.join(id, "view", cap, "203.0.113.20")
	var conn string
	_ = json.Unmarshal(joined["connection_id"], &conn)
	state := validState(id, publisherInstance(f.s, id))
	state.Admissions = []hostview.Admission{{AdmissionID: store.ID(), ViewerID: store.ID(), RowCode: "K7P", PeerID: conn, Connected: true, AdmittedAt: 1}}
	sendWS(t, p, map[string]any{"type": "sender.state", "state": state})
	sendWS(t, p, map[string]string{"type": "ping"})
	readType(t, p, "pong")
	for i := 0; i < 3; i++ { // every rebuild, not just the first
		status, out, _ := f.request("GET", "/api/v1/streams/"+id+"/viewers", f.key, "")
		var list hostview.ViewerList
		_ = json.Unmarshal(mustJSON(t, out), &list)
		if status != 200 || len(list.Viewers) != 1 || len(list.Viewers[0].Connections) != 1 {
			t.Fatal(status, out)
		}
		if ip := list.Viewers[0].Connections[0].IP; ip == nil || *ip != "203.0.113.20" {
			t.Fatalf("build %d: live row lost its session IP: %v", i, ip)
		}
	}
	var mem hostview.JoinMemory
	if e := f.s.Store.Read("join:"+id, &mem); e != nil {
		t.Fatal(e)
	}
	if b, _ := json.Marshal(mem); bytes.Contains(b, []byte("203.0.113.20")) {
		t.Fatal("raw IP persisted in join memory")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
