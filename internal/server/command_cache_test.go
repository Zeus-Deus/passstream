package server

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/store"
)

func TestCommandRetriesTransientResultsWithStableIdentity(t *testing.T) {
	f := setup(t)
	id, pub, _ := f.create(`{}`)
	path := "/api/v1/streams/" + id + "/commands"
	body := `{"id":"offline-retry","op":"pause"}`
	status, out, _ := f.request("POST", path, f.key, body)
	if status != 409 || errCode(out) != "sender_offline" {
		t.Fatal(status, out)
	}
	p, _ := f.join(id, "publish", pub, "192.0.2.1")
	readType(t, p, "e2ee.peers")
	type answer struct {
		status int
		replay string
	}
	results := make(chan answer, 1)
	request := func(body string) {
		s, _, h := f.request("POST", path, f.key, body)
		results <- answer{s, h.Get("Idempotent-Replay")}
	}
	go request(body)
	c := readType(t, p, "control")
	sendWS(t, p, map[string]any{"type": "control.result", "id": json.RawMessage(c["id"]), "ok": true, "result": map[string]any{}})
	if got := <-results; got.status != 200 || got.replay != "" {
		t.Fatal(got)
	}
	for _, code := range []string{"storage", "internal", "sender_timeout", "browser_failed", "rate_limited", "rotation_pending", "sender_offline"} {
		t.Run(code, func(t *testing.T) {
			body := `{"id":"retry-` + code + `","op":"kick","args":{"admission_id":"` + store.ID() + `"}}`
			go request(body)
			first := readType(t, p, "control")
			sendWS(t, p, map[string]any{"type": "control.result", "id": json.RawMessage(first["id"]), "ok": false, "error": apiError{Code: code}})
			if got := <-results; got.status != errorStatus(code) {
				t.Fatal(got)
			}
			go request(body)
			second := readType(t, p, "control")
			if !bytes.Equal(first["id"], second["id"]) {
				t.Fatal("retry changed forwarding id")
			}
			sendWS(t, p, map[string]any{"type": "control.result", "id": json.RawMessage(second["id"]), "ok": true, "result": map[string]any{}})
			if got := <-results; got.status != 200 || got.replay != "" {
				t.Fatal(got)
			}
			s, _, h := f.request("POST", path, f.key, body)
			if s != 200 || h.Get("Idempotent-Replay") != "true" {
				t.Fatal("success was not cached", s, h)
			}
		})
	}
}

func TestCommandResultClassification(t *testing.T) {
	for _, code := range []string{"unknown_admission", "invalid_argument", "code_mismatch", "stream_ended", "local_owner_required"} {
		if failure(code).retryable() {
			t.Fatal("terminal error classified retryable", code)
		}
	}
	for _, key := range []string{"applied", "pending"} {
		r := commandResponse{503, map[string]any{"error": apiError{Code: "storage", Details: map[string]any{key: true}}}}
		if r.retryable() {
			t.Fatal("accepted storage write must not be resubmitted", key)
		}
	}
}

func TestCommandRealTimeoutThenRetry(t *testing.T) {
	f := setup(t)
	id, pub, _ := f.create(`{}`)
	path := "/api/v1/streams/" + id + "/commands"
	p, _ := f.join(id, "publish", pub, "192.0.2.1")
	readType(t, p, "e2ee.peers")
	type answer struct {
		status int
		replay string
		code   string
	}
	results := make(chan answer, 4)
	request := func(body string) {
		s, out, h := f.request("POST", path, f.key, body)
		results <- answer{s, h.Get("Idempotent-Replay"), errCode(out)}
	}
	body := `{"id":"probe-timeout","op":"kick","args":{"admission_id":"` + store.ID() + `"}}`
	start := time.Now()
	go request(body)
	first := readType(t, p, "control")
	got := <-results
	t.Logf("first attempt: %+v after %v", got, time.Since(start).Round(time.Millisecond))
	if got.status != 504 || got.code != "sender_timeout" || time.Since(start) < 5*time.Second {
		t.Fatal(got)
	}
	// late terminal answer after timeout
	sendWS(t, p, map[string]any{"type": "control.result", "id": json.RawMessage(first["id"]), "ok": true, "result": map[string]any{"admission_ids": []string{}, "locked": false}})
	sendWS(t, p, map[string]any{"type": "ping"})
	readType(t, p, "pong")
	l := f.s.runtime(id)
	l.mu.Lock()
	var fid string
	_ = json.Unmarshal(first["id"], &fid)
	_, still := l.pending[fid]
	l.mu.Unlock()
	f.s.release(l)
	if still {
		t.Fatal("late terminal ack retained restriction")
	}
	go request(body)
	second := readType(t, p, "control")
	if !bytes.Equal(first["id"], second["id"]) {
		t.Fatal("forward id changed")
	}
	sendWS(t, p, map[string]any{"type": "control.result", "id": json.RawMessage(second["id"]), "ok": true, "result": map[string]any{}})
	got = <-results
	if got.status != 200 || got.replay != "" {
		t.Fatal(got)
	}
	s, _, h := f.request("POST", path, f.key, body)
	if s != 200 || h.Get("Idempotent-Replay") != "true" {
		t.Fatal(s, h)
	}
}

func TestRetryableRestrictionExpiresAndReleasesRuntime(t *testing.T) {
	f := setup(t)
	id, pub, _ := f.create(`{}`)
	p, _ := f.join(id, "publish", pub, "192.0.2.1")
	readType(t, p, "e2ee.peers")
	result := make(chan int, 1)
	go func() {
		status, _, _ := f.request("POST", "/api/v1/streams/"+id+"/commands", f.key, `{"id":"abandoned","op":"lock","args":{"locked":true}}`)
		result <- status
	}()
	msg := readType(t, p, "control")
	sendWS(t, p, map[string]any{"type": "control.result", "id": json.RawMessage(msg["id"]), "ok": false, "error": apiError{Code: "internal"}})
	if got := <-result; got != 500 {
		t.Fatal(got)
	}
	l := f.s.runtime(id)
	l.mu.Lock()
	if len(l.pending) != 1 {
		t.Fatal("retryable restriction lost")
	}
	for key, c := range l.pending {
		c.queuedAt = time.Now().Add(-pendingControlTTL)
		l.pending[key] = c
	}
	l.expireControls(time.Now())
	if len(l.pending) != 0 {
		t.Fatal("abandoned restriction retained")
	}
	// Releasing an expired slot admits new work at the same cap.
	l.sendControl(controlMessage{ID: "new", Op: "lock"})
	if len(l.pending) != 1 {
		t.Fatal("expired slot not reusable")
	}
	for key, c := range l.pending {
		c.queuedAt = time.Now().Add(-pendingControlTTL)
		l.pending[key] = c
	}
	l.mu.Unlock()
	f.s.release(l)
	p.CloseNow()
	deadline := time.Now().Add(time.Second)
	for {
		l.mu.Lock()
		sockets := len(l.sockets)
		l.mu.Unlock()
		f.s.mu.Lock()
		refs := l.refs
		f.s.mu.Unlock()
		if sockets == 0 && refs == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("publisher did not release runtime")
		}
		time.Sleep(time.Millisecond)
	}
	f.s.mu.Lock()
	l.used = time.Now().Add(-10 * time.Minute)
	f.s.mu.Unlock()
	f.s.maintain()
	f.s.mu.Lock()
	retained := f.s.live[id] != nil
	f.s.mu.Unlock()
	if retained {
		t.Fatal("expired restriction blocked idle eviction")
	}
}
