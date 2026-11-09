package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/proto"
	"github.com/passstream/passstream/internal/hostview"
)

func TestOwnerHealthRelayAndViewerIsolation(t *testing.T) {
	f := setup(t)
	id, pub, cap := f.create(`{}`)
	p, _ := f.join(id, "publish", pub, "192.0.2.1")
	readType(t, p, "e2ee.peers")
	v, _ := f.join(id, "view", cap, "192.0.2.2")
	h := json.RawMessage(`{"uplink_kbps":2400,"encode_ms":4.5,"encode_cpu_pct":13.5,"fps":30,"width":1920,"height":1080,"quality_limitation":"cpu","pin":"SECRET_PIN","link":"SECRET_LINK","key":"SECRET_KEY"}`)
	st := validState(id, publisherInstance(f.s, id))
	b, _ := json.Marshal(st)
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(b, &wire)
	wire["health"] = h
	sendWS(t, p, map[string]any{"type": "sender.state", "state": wire})
	sendWS(t, p, map[string]any{"type": "ping"})
	readType(t, p, "pong")
	check := func(b []byte) {
		t.Helper()
		if bytes.Contains(b, []byte("SECRET")) {
			t.Fatal("unknown health field leaked")
		}
		var s struct {
			Effective struct {
				Health *hostview.Health `json:"health"`
			} `json:"effective"`
		}
		if json.Unmarshal(b, &s) != nil || s.Effective.Health == nil {
			t.Fatal("health missing", string(b))
		}
		h := s.Effective.Health
		if *h.UplinkKbps != 2400 || *h.EncodeCPUPct != 13.5 || *h.EncodeMS != 4.5 || *h.FPS != 30 || *h.Width != 1920 || *h.Height != 1080 || h.QualityLimitation != "cpu" {
			t.Fatal(h)
		}
	}
	status, out, _ := f.request("GET", "/api/v1/streams/"+id, f.key, "")
	if status != 200 {
		t.Fatal(status)
	}
	b, _ = json.Marshal(out)
	check(b)
	status, out, _ = f.request("GET", "/api/v1/streams", f.key, "")
	var list []json.RawMessage
	_ = json.Unmarshal(out["streams"], &list)
	if status != 200 || len(list) != 1 {
		t.Fatal(status, out)
	}
	check(list[0])
	r, _ := http.NewRequest("GET", f.h.URL+"/api/v1/streams/"+id+"/events", nil)
	r.Header.Set("Authorization", "Bearer "+f.key)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, _ := reader.ReadString('\n')
	if line != "event: stream\n" {
		t.Fatal(line)
	}
	line, _ = reader.ReadString('\n')
	check([]byte(strings.TrimPrefix(line, "data: ")))
	for _, key := range []string{"", cap, pub} {
		for _, path := range []string{"/api/v1/streams/" + id, "/api/v1/streams/" + id + "/events", "/api/v1/streams"} {
			s, out, _ := f.request("GET", path, key, "")
			if s != 401 || errCode(out) != "unauthorized" {
				t.Fatal("viewer read owner health", s, out)
			}
		}
	}
	// A viewer cannot publish fake sender metrics or receive other senders' metrics.
	st.Health = &hostview.Health{QualityLimitation: "bandwidth"}
	st.Seq = 100
	sendWS(t, v, map[string]any{"type": "sender.state", "state": st})
	sendWS(t, v, map[string]any{"type": "ping"})
	for {
		m := readWS(t, v)
		b, _ := json.Marshal(m)
		if bytes.Contains(b, []byte("health")) {
			t.Fatal("viewer received owner health")
		}
		if string(m["type"]) == `"pong"` {
			break
		}
	}
	_, out, _ = f.request("GET", "/api/v1/streams/"+id, f.key, "")
	b, _ = json.Marshal(out)
	check(b)
	var saved savedState
	if f.s.Store.Read("sender:"+id, &saved) != nil {
		t.Fatal("no persisted state")
	}
	b, _ = json.Marshal(saved)
	if bytes.Contains(b, []byte("SECRET")) {
		t.Fatal("secret persisted")
	}
	p.CloseNow()
	deadline := time.Now().Add(time.Second)
	for {
		_, out, _ = f.request("GET", "/api/v1/streams/"+id, f.key, "")
		var sender struct {
			Online bool `json:"online"`
		}
		_ = json.Unmarshal(out["sender"], &sender)
		if !sender.Online {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("publisher did not go offline")
		}
		time.Sleep(time.Millisecond)
	}
	b, _ = json.Marshal(out)
	check(b)

}

func TestHealthBounds(t *testing.T) {
	for _, health := range []string{
		`{"encode_cpu_pct":101}`, `{"encode_cpu_pct":-1}`, `{"width":0}`, `{"width":1920.5}`, `{"uplink_kbps":1.5}`, `{"fps":"bad"}`, `{"height":16385}`, `{"fps":1001}`, `{"encode_ms":60001}`,
		`{"extra":"` + strings.Repeat("x", 1024) + `"}`,
	} {
		st := validState("AAAAAAAAAAAAAAAAAAAAAA", "AAAAAAAAAAAAAAAAAAAAAA")
		st.Access.Latches = hostview.Latches{Locked: true, Paused: true, Frozen: true}
		st.Phase = "paused"
		st.Admissions = []hostview.Admission{{AdmissionID: "AAAAAAAAAAAAAAAAAAAAAA", ViewerID: "AAAAAAAAAAAAAAAAAAAAAA", PeerID: "c_AAAAAAAAAAAAAAAAAAAAAA", RowCode: "K7P"}}
		st.Knocks = []hostview.Knock{{KnockID: "BBBBBBBBBBBBBBBBBBBBBA", PeerID: "c_AAAAAAAAAAAAAAAAAAAAAA", RowCode: "ACD", At: 1, ExpiresAt: 2}}
		b, _ := json.Marshal(st)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		var h map[string]any
		_ = json.Unmarshal([]byte(health), &h)
		for k, v := range map[string]any{"uplink_kbps": nil, "encode_ms": nil, "fps": nil, "quality_limitation": "none"} {
			if _, ok := h[k]; !ok {
				h[k] = v
			}
		}
		m["health"] = h
		b, _ = json.Marshal(m)
		parsed, err := parseState(b)
		if err != nil || parsed.Health != nil {
			t.Fatal("invalid health must become null", err, health[:min(len(health), 100)])
		}

		var local proto.SenderState
		if err := proto.Into(b, proto.SenderStateV, &local); err != nil || !reflect.DeepEqual(local, parsed) {
			t.Fatal("daemon disagrees on malformed health", err, local)
		}
		st.Health = nil
		if !reflect.DeepEqual(parsed, st) {
			t.Fatal("invalid health changed sender facts", parsed)
		}
	}
}

func TestSenderEndReasonsIncludeBrowserFailureAndExpiry(t *testing.T) {
	for _, reason := range []string{"browser_failed", "expired", "unknown"} {
		st := validState("AAAAAAAAAAAAAAAAAAAAAA", "AAAAAAAAAAAAAAAAAAAAAA")
		st.Phase = "ended"
		st.EndReason = reason
		b, _ := json.Marshal(st)
		parsed, err := parseState(b)
		if err != nil || senderEndReason(parsed.EndReason) != reason {
			t.Fatal(reason, err)
		}
	}
}

func TestUnknownSenderEndReasonIsDisplayOnly(t *testing.T) {
	st := validState("AAAAAAAAAAAAAAAAAAAAAA", "AAAAAAAAAAAAAAAAAAAAAA")
	st.Phase = "ended"
	st.EndReason = "future-reason-with-SECRET"
	b, _ := json.Marshal(st)
	parsed, err := parseState(b)
	if err != nil || parsed.EndReason != "unknown" || senderEndReason(parsed.EndReason) != "unknown" {
		t.Fatal("future end reason invalidated a terminal report", err)
	}
}
