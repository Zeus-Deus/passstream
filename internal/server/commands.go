package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/passstream/passstream/internal/store"
)

var commandID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var id22 = regexp.MustCompile(`^[A-Za-z0-9_-]{21}[AQgw]$`)
var connID = regexp.MustCompile(`^c_[A-Za-z0-9_-]{21}[AQgw]$`)

type commandRequest struct {
	ID   string                     `json:"id"`
	Op   string                     `json:"op"`
	Args map[string]json.RawMessage `json:"args"`
}
type commandResponse struct {
	status int
	body   any
}

// Retryable attempts keep their forwarding identity, but never a cached answer.
// Accepted storage writes finish on the sender and must not be resubmitted.
func (r commandResponse) retryable() bool {
	if r.status < 400 {
		return false
	}
	b, _ := json.Marshal(r.body)
	var v struct {
		Error apiError `json:"error"`
	}
	_ = json.Unmarshal(b, &v)
	if v.Error.Code == "storage" && (v.Error.Details["pending"] == true || v.Error.Details["applied"] == true) {
		return false
	}
	switch v.Error.Code {
	case "sender_offline", "not_live", "rotation_pending", "browser_failed":
		return true
	}
	return r.status >= 500 || r.status == http.StatusTooManyRequests
}

type commandCall struct {
	caller      string
	stream      string
	fingerprint string
	forwardID   string
	at          time.Time
	running     bool
	attempt     *commandAttempt
	response    commandResponse
}

const maxPendingControls = 1024
const pendingControlTTL = 10 * time.Minute

type controlMessage struct {
	queuedAt time.Time                  // local retention only; never sent to the sender
	Type     string                     `json:"type"`
	ID       string                     `json:"id"`
	Op       string                     `json:"op"`
	Args     map[string]json.RawMessage `json:"args"`
	Source   map[string]any             `json:"source"`
}

func (l *liveStream) sendControl(c controlMessage) {
	if c.Op == "kick" || c.Op == "lock" {
		// Keep the same forwarding id until the sender acknowledges the restriction.
		c.queuedAt = time.Now()
		l.pending[c.ID] = c
	}
	if l.publisher != nil {
		l.publisher.enqueue(c)
	}
}

// expireControls releases abandoned restrictions even after a retryable answer.
// A caller retry refreshes retention while preserving the forwarding identity.
func (l *liveStream) expireControls(now time.Time) {
	for id, c := range l.pending {
		if now.Sub(c.queuedAt) >= pendingControlTTL {
			delete(l.pending, id)
		}
	}
}

// An attempt's response is immutable after done closes, even when a new retry starts.
type commandAttempt struct {
	done     chan struct{}
	result   chan commandResponse
	response commandResponse
}

func validateCommand(v commandRequest) string {
	for k := range v.Args {
		switch k {
		case "rotate_pin", "pin", "keep_pin", "row_code", "policy":
			return "local_owner_required"
		}
	}
	allowed := map[string]bool{}
	switch v.Op {
	case "stop", "pause":
	case "mode":
		allowed["mode"] = true
	case "safe":
		// Safe mode on is restrictive; off loosens privacy and is local only.
		allowed["on"] = true
		var b bool
		if json.Unmarshal(v.Args["on"], &b) != nil || string(v.Args["on"]) == "null" {
			return "invalid_argument"
		}
		if !b {
			return "local_owner_required"
		}
	case "lock":
		allowed["locked"] = true
		var b bool
		if json.Unmarshal(v.Args["locked"], &b) != nil || string(v.Args["locked"]) == "null" {
			return "invalid_argument"
		}
		if !b {
			return "local_owner_required"
		}
	case "kick":
		allowed["admission_id"] = true
		allowed["lock"] = true
	case "knock.deny":
		allowed["knock_id"] = true
	case "annotations":
		// Viewer drawing off is restrictive; on is permissive and local only.
		allowed["viewers"] = true
		var b bool
		if json.Unmarshal(v.Args["viewers"], &b) != nil || string(v.Args["viewers"]) == "null" {
			return "invalid_argument"
		}
		if b {
			return "local_owner_required"
		}
	case "annotations.clear":
	case "ban":
		allowed["connection_id"] = true
	default:
		return "local_owner_required"
	}
	for k := range v.Args {
		if !allowed[k] {
			return "invalid_argument"
		}
	}
	for _, key := range []string{"admission_id", "knock_id", "connection_id", "mode"} {
		required := v.Op == "kick" && key == "admission_id" || v.Op == "knock.deny" && key == "knock_id" || v.Op == "ban" && key == "connection_id" || v.Op == "mode" && key == "mode"
		if required {
			var value string
			if json.Unmarshal(v.Args[key], &value) != nil {
				return "invalid_argument"
			}
			if key == "mode" {
				if value != "text" && value != "motion" {
					return "invalid_argument"
				}
			} else if key == "connection_id" {
				if !connID.MatchString(value) {
					return "invalid_argument"
				}
			} else if !id22.MatchString(value) {
				return "invalid_argument"
			}
		}
	}
	if b, ok := v.Args["lock"]; ok {
		var v bool
		if string(b) == "null" || json.Unmarshal(b, &v) != nil {
			return "invalid_argument"
		}
	}
	return ""
}
func failure(code string) commandResponse {
	return commandResponse{errorStatus(code), map[string]any{"error": apiError{Code: code, Message: code}}}
}
func (s *Server) command(w http.ResponseWriter, r *http.Request) {
	var v commandRequest
	if !decode(w, r, &v) {
		return
	}
	if v.ID != "" && !commandID.MatchString(v.ID) || v.Op == "" {
		fail(w, "invalid_argument")
		return
	}
	if v.Args == nil {
		v.Args = map[string]json.RawMessage{}
	}
	var id *string
	if v.ID != "" {
		id = &v.ID
	}
	target := map[string]string{}
	for _, k := range []string{"admission_id", "knock_id", "connection_id"} {
		var x string
		if json.Unmarshal(v.Args[k], &x) == nil {
			target[k] = x
		}
	}
	respond := func(resp commandResponse, replay bool) {
		if resp.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		if replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
		code := ""
		if resp.status >= 400 {
			if b, e := json.Marshal(resp.body); e == nil {
				var x struct {
					Error apiError `json:"error"`
				}
				_ = json.Unmarshal(b, &x)
				code = x.Error.Code
			}
		}
		s.record(r, r.PathValue("id"), v.Op, target, code, id)
		writeJSON(w, resp.status, resp.body)
	}
	if code := validateCommand(v); code != "" {
		respond(failure(code), false)
		return
	}
	canonical, _ := json.Marshal(v.Args)
	var normalized any
	_ = json.Unmarshal(canonical, &normalized)
	canonical, _ = json.Marshal(normalized)
	fingerprint := store.Hash(v.Op + string(canonical))
	key := who(r).Key.ID + ":" + r.PathValue("id") + ":" + v.ID

	s.mu.Lock()
	now := time.Now()
	var call *commandCall
	if v.ID != "" {
		call = s.commands[key]
	}
	if call != nil && !call.running && now.Sub(call.at) > 10*time.Minute {
		delete(s.commands, key)
		call = nil
	}
	if call != nil {
		if call.fingerprint != fingerprint {
			s.mu.Unlock()
			respond(failure("idempotency_conflict"), false)
			return
		}
		if call.running {
			attempt := call.attempt
			s.mu.Unlock()
			select {
			case <-attempt.done:
				respond(attempt.response, true)
			case <-r.Context().Done():
			}
			return
		}
		if call.response.status != 0 {
			resp := call.response
			s.mu.Unlock()
			respond(resp, true)
			return
		}
	}
	cached, running := 0, 0
	for _, c := range s.commands {
		if c.caller == who(r).Key.ID {
			cached++
		}
	}
	for _, c := range s.inflight {
		if c.caller == who(r).Key.ID {
			running++
		}
	}
	if running >= 8 || len(s.inflight) >= 1024 || call == nil && v.ID != "" && (cached >= 128 || len(s.commands) >= 4096) {
		s.mu.Unlock()
		respond(failure("rate_limited"), false)
		return
	}
	if call == nil {
		call = &commandCall{caller: who(r).Key.ID, stream: r.PathValue("id"), fingerprint: fingerprint, forwardID: store.ID()}
		if v.ID != "" {
			s.commands[key] = call
		}
	}
	s.inflight[call.forwardID] = call
	call.running = true
	call.at = now
	attempt := &commandAttempt{done: make(chan struct{}), result: make(chan commandResponse, 1)}
	call.attempt = attempt
	// Finish in a goroutine independent of the HTTP request. Disconnects don't orphan in-flight repeats.
	go s.runCommand(r.PathValue("id"), v, call, attempt, who(r))
	s.mu.Unlock()
	select {
	case <-attempt.done:
		respond(attempt.response, false)
	case <-r.Context().Done():
	}
}
func (s *Server) runCommand(stream string, v commandRequest, c *commandCall, attempt *commandAttempt, p principal) {
	resp := commandResponse{}
	st, e := s.Store.Get(stream)
	if e != nil {
		resp = failure("not_found")
	}
	var l *liveStream
	if e == nil {
		l = s.runtime(stream)
		defer s.release(l)
		l.mu.Lock()
		l.expireControls(time.Now())
		st, e = s.Store.Get(stream)
		if e != nil {
			resp = failure("not_found")
		}
	}
	if resp.status == 0 && v.Op == "stop" {
		if e = s.endStreamLocked(stream, "stopped", l); e != nil {
			resp = failure("internal")
		} else {
			resp = commandResponse{200, map[string]any{"id": c.forwardID, "ok": true, "result": map[string]any{}}}
		}
	} else if resp.status == 0 && st.EndedAt != nil {
		resp = failure("stream_ended")
	}
	if resp.status == 0 && v.Op == "ban" {
		var id string
		_ = json.Unmarshal(v.Args["connection_id"], &id)
		conn, ok := l.conns[id]
		ip := ""
		if ok && conn.IP != nil {
			ip = *conn.IP
		}
		if recent, ok := l.recentIPs[id]; ok && recent.until > timeNowMS() {
			ip = recent.ip
		}
		if ip == "" {
			resp = failure("not_found")
		} else {
			aids := []string{}
			if l.state != nil {
				for _, a := range l.state.Admissions {
					if a.PeerID == id {
						aids = append(aids, a.AdmissionID)
					}
				}
			}
			if len(l.pending)+len(aids) > maxPendingControls {
				resp = failure("rate_limited")
			} else if e = s.Store.Ban(stream, ip); e != nil {
				resp = failure("internal")
			} else {
				for _, aid := range aids {
					arg, _ := json.Marshal(aid)
					l.sendControl(controlMessage{Type: "control", ID: store.ID(), Op: "kick", Args: map[string]json.RawMessage{"admission_id": arg}, Source: map[string]any{"kind": "api", "key_id": p.Key.ID}})
				}
				for _, peer := range l.sockets {
					if store.IPKey(peer.ip) == store.IPKey(ip) && peer.role == "view" {
						peer.close("banned")
					}
				}
				resp = commandResponse{200, map[string]any{"id": c.forwardID, "ok": true, "result": map[string]any{"ip_banned": true, "admission_ids": aids}}}
			}
		}
	}
	if resp.status == 0 {
		if l.publisher == nil {
			resp = failure("sender_offline")
		} else if (v.Op == "kick" || v.Op == "lock") && len(l.pending) >= maxPendingControls && l.pending[c.forwardID].ID == "" {
			resp = failure("rate_limited")
		} else {
			kind := "api"
			if p.Cookie {
				kind = "dashboard"
			}
			l.sendControl(controlMessage{Type: "control", ID: c.forwardID, Op: v.Op, Args: v.Args, Source: map[string]any{"kind": kind, "key_id": p.Key.ID}})
		}
	}
	if l != nil {
		l.mu.Unlock()
	}
	if resp.status == 0 {
		timer := time.NewTimer(5 * time.Second)
		select {
		case resp = <-attempt.result:
		case <-timer.C:
			resp = failure("sender_timeout")
		case <-s.done:
			resp = failure("sender_offline")
		}
		timer.Stop()
	}
	s.mu.Lock()
	delete(s.inflight, c.forwardID)
	c.response = commandResponse{}
	if !resp.retryable() {
		c.response = resp
	}
	c.running = false
	c.at = time.Now()
	attempt.response = resp
	close(attempt.done)
	s.mu.Unlock()
}
