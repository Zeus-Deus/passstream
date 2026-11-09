package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/store"
)

type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func errorStatus(code string) int {
	switch code {
	case "invalid_argument", "passphrase_required":
		return 400
	case "unauthorized":
		return 401
	case "forbidden_scope", "origin_rejected", "local_owner_required":
		return 403
	case "not_found", "unknown_admission", "unknown_knock":
		return 404
	case "conflict", "already_live", "idempotency_conflict", "sender_offline", "not_live", "rotation_pending", "code_mismatch":
		return 409
	case "stream_ended":
		return 410
	case "payload_too_large":
		return 413
	case "rate_limited":
		return 429
	case "storage":
		return 503
	case "sender_timeout":
		return 504
	default:
		return 500
	}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code string, details map[string]any) {
	if status == 429 {
		w.Header().Set("Retry-After", "1")
	}
	writeJSON(w, status, map[string]any{"error": apiError{code, strings.ReplaceAll(code, "_", " "), details}})
}
func fail(w http.ResponseWriter, code string) { writeError(w, errorStatus(code), code, nil) }
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	b, e := io.ReadAll(io.LimitReader(r.Body, 65537))
	if e != nil {
		fail(w, "invalid_argument")
		return false
	}
	if len(b) > 65536 {
		fail(w, "payload_too_large")
		return false
	}
	if len(bytes.TrimSpace(b)) == 0 {
		b = []byte("{}")
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) || len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '{' || json.Unmarshal(b, v) != nil {
		fail(w, "invalid_argument")
		return false
	}
	if validateRequestBody(b, r.URL.Path) == false {
		fail(w, "invalid_argument")
		return false
	}

	return true
}

type principal struct {
	Key     store.Key
	Cookie  bool
	Expires int64
}
type principalKey struct{}

func who(r *http.Request) principal { return r.Context().Value(principalKey{}).(principal) }
func rank(scope string) int         { return map[string]int{"read": 1, "streams": 2, "owner": 3}[scope] }
func (s *Server) auth(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(s.cfg.BaseURL, "/")
		if o := r.Header.Get("Origin"); o != "" && o != origin {
			fail(w, "origin_rejected")
			return
		}
		var p principal
		var e error
		a := r.Header.Get("Authorization")
		if strings.HasPrefix(a, "Bearer ") {
			p.Key, e = s.Store.AuthKey(strings.TrimPrefix(a, "Bearer "))
		} else if c, err := r.Cookie("ps_session"); err == nil {
			p.Cookie = true
			p.Key, p.Expires, e = s.Store.AuthSession(c.Value)
		} else {
			e = sql.ErrNoRows
		}
		if e != nil {
			fail(w, "unauthorized")
			return
		}
		if p.Cookie && r.Method != "GET" && r.Header.Get("Origin") != origin {
			fail(w, "origin_rejected")
			return
		}
		if rank(p.Key.Scope) < rank(scope) || scope == "owner" && p.Cookie {
			fail(w, "forbidden_scope")
			return
		}
		if !s.permit("http:"+p.Key.ID, 20, 60) {
			fail(w, "rate_limited")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}
func (s *Server) routes(m *http.ServeMux) {
	for path, h := range map[string]http.HandlerFunc{"POST /api/v1/streams": s.createStream, "DELETE /api/v1/streams/{id}": s.deleteStream, "POST /api/v1/streams/{id}/commands": s.command, "PUT /api/v1/settings": s.putSettings} {
		m.HandleFunc(path, s.auth("streams", h))
	}
	for path, h := range map[string]http.HandlerFunc{"GET /api/v1/streams": s.listStreams, "GET /api/v1/streams/{id}": s.getStream, "GET /api/v1/streams/{id}/viewers": s.viewers, "GET /api/v1/streams/{id}/connections": s.connections, "GET /api/v1/streams/{id}/events": s.events, "GET /api/v1/streams/{id}/history": s.history, "GET /api/v1/audit": s.auditList, "GET /api/v1/settings": s.getSettings, "GET /api/v1/session": s.getSession, "DELETE /api/v1/session": s.deleteSession} {
		m.HandleFunc(path, s.auth("read", h))
	}
	m.HandleFunc("POST /api/v1/session", s.login)
	m.HandleFunc("POST /api/v1/keys", s.auth("owner", s.createKey))
	m.HandleFunc("GET /api/v1/keys", s.auth("owner", s.listKeys))
	m.HandleFunc("DELETE /api/v1/keys/{id}", s.auth("owner", s.deleteKey))
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, "not_found") })
}
func (s *Server) record(r *http.Request, stream, op string, target map[string]string, code string, id *string) {
	p := who(r)
	kind := "key"
	if p.Cookie {
		kind = "session"
	}
	result := "ok"
	var errCode *string
	if code != "" {
		result = "error"
		errCode = &code
	}
	var sid *string
	if stream != "" {
		sid = &stream
	}
	if target == nil {
		target = map[string]string{}
	}
	_ = s.Store.Record("audit", stream, time.Now().UnixMilli(), store.AuditEntry{ID: store.ID(), At: time.Now().UnixMilli(), StreamID: sid, Actor: store.Actor{Kind: kind, KeyID: &p.Key.ID}, Op: op, Target: target, Result: result, ErrorCode: errCode, IdempotencyKey: id})
}
func validPolicy(p hostview.PinPolicy) bool {
	switch p.Kind {
	case "none", "passphrase":
		return p.Length == nil
	case "digits":
		return p.Length != nil && *p.Length >= 4 && *p.Length <= 12
	}
	return false
}
func policyRank(p hostview.PinPolicy) uint64 {
	if p.Kind == "passphrase" {
		return 100
	}
	if p.Length != nil {
		return *p.Length
	}
	return 0
}
func (s *Server) createStream(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Label      *string             `json:"label"`
		Pin        *hostview.PinPolicy `json:"pin"`
		Explicit   bool                `json:"pin_explicit"`
		Knock      bool                `json:"knock"`
		Expires    json.RawMessage     `json:"expires_in_s"`
		SenderKind string              `json:"sender_kind"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Explicit && in.Pin == nil || in.Pin != nil && !validPolicy(*in.Pin) || in.SenderKind != "" && in.SenderKind != "local" && in.SenderKind != "browser" {
		fail(w, "invalid_argument")
		return
	}
	cfg, e := s.Store.Settings()
	if e != nil {
		fail(w, "internal")
		return
	}
	pin := cfg.Defaults.Pin
	if in.Pin != nil && (in.Explicit || policyRank(*in.Pin) > policyRank(pin)) {
		pin = *in.Pin
	}
	if in.SenderKind == "" {
		in.SenderKind = "browser"
	}
	if in.Label != nil {
		v := clean(*in.Label, 64, 256)
		in.Label = &v
	}
	var expiry *int64
	duration := cfg.Defaults.ExpiresInS
	if len(in.Expires) > 0 {
		duration = nil
		if string(in.Expires) != "null" {
			var n uint64
			if json.Unmarshal(in.Expires, &n) != nil {
				fail(w, "invalid_argument")
				return
			}
			duration = &n
		}
	}
	if duration != nil {
		if *duration > 315360000 {
			fail(w, "invalid_argument")
			return
		}
		n := time.Now().UnixMilli() + int64(*duration)*1000
		expiry = &n
	}
	v, view, pub, e := s.Store.CreateStream(store.Stream{Label: in.Label, Phase: "pending", E2EE: true, ExpiresAt: expiry, Sender: store.Sender{Kind: in.SenderKind}, Declared: store.Declared{Pin: pin, Knock: in.Knock || cfg.Defaults.Knock}, MaxViewers: cfg.Defaults.MaxViewers})
	if e != nil {
		fail(w, "internal")
		return
	}
	s.record(r, v.ID, "stream.create", nil, "", nil)
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	writeJSON(w, 201, struct {
		store.Stream
		View    string `json:"view_link_base"`
		Publish string `json:"publish_token"`
		Host    string `json:"host_url"`
	}{v, base + "/v#c=" + view, pub, base + "/host#id=" + v.ID + "&t=" + pub + "&c=" + view + "&pin=" + hostPin(pin) + "&knock=" + map[bool]string{true: "1", false: "0"}[v.Declared.Knock]})
}

// load returns a pinned, locked stream. The handler must unlock and release it.
func (s *Server) load(w http.ResponseWriter, r *http.Request) (store.Stream, *liveStream, bool) {
	v, e := s.Store.Get(r.PathValue("id"))
	if e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			fail(w, "not_found")
		} else {
			fail(w, "internal")
		}
		return v, nil, false
	}
	l := s.runtime(v.ID)
	l.mu.Lock()
	// Re-read under the stream lock so concurrent termination cannot be overwritten.
	v, e = s.Store.Get(v.ID)
	if e == nil && v.EndedAt == nil && v.ExpiresAt != nil && *v.ExpiresAt <= time.Now().UnixMilli() {
		e = s.endStreamLocked(v.ID, "expired", l)
		if e == nil {
			v, e = s.Store.Get(v.ID)
		}
	}
	if e == nil {
		v, e = s.build(v, l)
	}
	if e != nil {
		l.mu.Unlock()
		s.release(l)
		fail(w, "internal")
		return v, nil, false
	}
	return v, l, true
}
func (s *Server) getStream(w http.ResponseWriter, r *http.Request) {
	v, l, ok := s.load(w, r)
	if ok {
		l.mu.Unlock()
		s.release(l)
		writeJSON(w, 200, v)
	}
}
func (s *Server) deleteStream(w http.ResponseWriter, r *http.Request) {
	v, l, ok := s.load(w, r)
	if !ok {
		return
	}
	e := s.endStreamLocked(v.ID, "deleted", l)
	l.mu.Unlock()
	s.release(l)
	if e != nil {
		fail(w, "internal")
		return
	}
	s.record(r, v.ID, "stream.delete", nil, "", nil)
	w.WriteHeader(204)
}
func page(r *http.Request, def int) (int, int64, int64, bool) {
	q := r.URL.Query()
	limit := def
	var e error
	if q.Get("limit") != "" {
		limit, e = strconv.Atoi(q.Get("limit"))
		if e != nil || limit < 1 || limit > 500 {
			return 0, 0, 0, false
		}
	}
	var cursor, since int64
	if c := q.Get("cursor"); c != "" {
		b, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil {
			return 0, 0, 0, false
		}
		cursor, e = strconv.ParseInt(string(b), 10, 64)
		if e != nil || cursor < 0 {
			return 0, 0, 0, false
		}
	}
	if q.Get("since") != "" {
		since, e = strconv.ParseInt(q.Get("since"), 10, 64)
		if e != nil || since < 0 {
			return 0, 0, 0, false
		}
	}
	return limit, cursor, since, true
}
func cursor(n int64) *string {
	v := base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(n, 10)))
	return &v
}
func (s *Server) listStreams(w http.ResponseWriter, r *http.Request) {
	limit, c, _, ok := page(r, 50)
	state := r.URL.Query().Get("state")
	if !ok || state != "" && state != "live" && state != "ended" && state != "all" {
		fail(w, "invalid_argument")
		return
	}

	vv, e := s.Store.StreamPage(state, c, limit+1)
	if e != nil {
		fail(w, "internal")
		return
	}
	var next *string
	if len(vv) > limit {
		vv = vv[:limit]
		next = cursor(c + int64(limit))
	}
	out := []store.Stream{}
	for _, v := range vv {
		// Historical listing must not populate the live cache.
		s.mu.Lock()
		l := s.live[v.ID]
		if l != nil {
			l.refs++
		}
		s.mu.Unlock()
		cached := l != nil
		if !cached {
			l = s.readRuntime(v.ID)
		}
		l.mu.Lock()
		v, e = s.Store.Get(v.ID)
		if e == nil && v.EndedAt == nil && v.ExpiresAt != nil && *v.ExpiresAt <= time.Now().UnixMilli() {
			e = s.endStreamLocked(v.ID, "expired", l)
			if e == nil {
				v, e = s.Store.Get(v.ID)
			}
		}
		if e == nil {
			v, e = s.build(v, l)
		}
		l.mu.Unlock()
		if cached {
			s.release(l)
		}
		if e != nil {
			fail(w, "internal")
			return
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"streams": out, "next_cursor": next})
}
func (s *Server) viewers(w http.ResponseWriter, r *http.Request) {
	_, l, ok := s.load(w, r)
	if ok {
		list := l.list
		l.mu.Unlock()
		s.release(l)
		writeJSON(w, 200, list)
	}
}
func (s *Server) connections(w http.ResponseWriter, r *http.Request) {
	_, l, ok := s.load(w, r)
	if !ok {
		return
	}
	cs := []hostview.Connection{}
	for _, c := range l.conns {
		cs = append(cs, c)
	}
	l.mu.Unlock()
	s.release(l)
	sort.Slice(cs, func(i, j int) bool { return cs[i].ConnectionID < cs[j].ConnectionID })
	writeJSON(w, 200, map[string]any{"connections": cs, "meta": map[string]string{"geoip_attribution": geoAttribution}})
}
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	_, l, ok := s.load(w, r)
	if ok {
		l.mu.Unlock()
		s.release(l)
		s.records(w, r, "history", r.PathValue("id"), false)
	}
}
func (s *Server) auditList(w http.ResponseWriter, r *http.Request) {
	s.records(w, r, "audit", r.URL.Query().Get("stream_id"), true)
}
func (s *Server) records(w http.ResponseWriter, r *http.Request, kind, id string, desc bool) {
	limit, c, since, ok := page(r, 100)
	if !ok {
		fail(w, "invalid_argument")
		return
	}
	rows, e := s.Store.Records(kind, id, since, c, limit+1, desc)
	if e != nil {
		fail(w, "internal")
		return
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		next = cursor(rows[len(rows)-1].Seq)
	}
	out := []json.RawMessage{}
	for _, row := range rows {
		body := row.Body
		if kind == "history" {
			cfg, e := s.Store.Settings()
			if e != nil {
				fail(w, "internal")
				return
			}
			if !cfg.Retention.KeepIP && !s.cfg.KeepIP {
				var h hostview.HistoryEntry
				if json.Unmarshal(body, &h) == nil {
					for i := range h.Connections {
						h.Connections[i].IP = nil
					}
					body, _ = json.Marshal(h)
				}
			}
		}
		out = append(out, body)
	}
	writeJSON(w, 200, map[string]any{"entries": out, "next_cursor": next})
}
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.Settings()
	if e != nil {
		fail(w, "internal")
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var v store.Settings
	if !decode(w, r, &v) {
		return
	}
	if !validPolicy(v.Defaults.Pin) || v.Defaults.MaxViewers < 1 || v.Defaults.MaxViewers > 500 || v.Defaults.ExpiresInS != nil && *v.Defaults.ExpiresInS > 315360000 {
		fail(w, "invalid_argument")
		return
	}
	old, e := s.Store.Settings()
	if e != nil {
		fail(w, "internal")
		return
	}
	p := who(r)
	if v.Retention.KeepIP && !old.Retention.KeepIP && (p.Cookie || p.Key.Scope != "owner") {
		fail(w, "forbidden_scope")
		return
	}
	if e := s.Store.Put("settings", v); e != nil {
		fail(w, "internal")
		return
	}
	s.record(r, "", "settings.update", nil, "", nil)
	writeJSON(w, 200, v)
}
func (s *Server) cookie(w http.ResponseWriter, value string, expires int64) {
	u, _ := url.Parse(s.cfg.BaseURL)
	maxAge := 43200
	if value == "" {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: "ps_session", Value: value, Path: "/", HttpOnly: true, Secure: !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")), SameSite: http.SameSiteStrictMode, Expires: time.UnixMilli(expires), MaxAge: maxAge})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if o := r.Header.Get("Origin"); o != "" && o != strings.TrimRight(s.cfg.BaseURL, "/") {
		fail(w, "origin_rejected")
		return
	}
	a := r.Header.Get("Authorization")
	if !strings.HasPrefix(a, "Bearer ") {
		fail(w, "unauthorized")
		return
	}
	k, e := s.Store.AuthKey(strings.TrimPrefix(a, "Bearer "))
	if e != nil {
		fail(w, "unauthorized")
		return
	}
	if !s.permit("http:"+k.ID, 20, 60) {
		fail(w, "rate_limited")
		return
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 65537))
	if len(b) > 65536 {
		fail(w, "payload_too_large")
		return
	}
	if len(bytes.TrimSpace(b)) != 0 {
		fail(w, "invalid_argument")
		return
	}
	token, expires, e := s.Store.Login(k)
	if e != nil {
		fail(w, "internal")
		return
	}
	s.cookie(w, token, expires)
	s.record(r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{Key: k})), "", "session.login", nil, "", nil)
	w.WriteHeader(204)
}
func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	if !p.Cookie {
		fail(w, "unauthorized")
		return
	}
	writeJSON(w, 200, map[string]any{"scope": p.Key.Scope, "expires_at": p.Expires})
}
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	c, e := r.Cookie("ps_session")
	if e != nil {
		fail(w, "unauthorized")
		return
	}
	if e = s.Store.Logout(c.Value); e != nil {
		fail(w, "internal")
		return
	}
	s.cookie(w, "", 0)
	w.WriteHeader(204)
}
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Label   string  `json:"label"`
		Scope   string  `json:"scope"`
		Expires *uint64 `json:"expires_in_s"`
	}
	if !decode(w, r, &v) {
		return
	}
	if rank(v.Scope) == 0 || v.Label == "" || v.Expires != nil && *v.Expires > 315360000 {
		fail(w, "invalid_argument")
		return
	}
	var expiry *int64
	if v.Expires != nil {
		n := time.Now().UnixMilli() + int64(*v.Expires)*1000
		expiry = &n
	}
	k, token, e := s.Store.NewKey(clean(v.Label, 64, 256), v.Scope, expiry)
	if e != nil {
		fail(w, "internal")
		return
	}
	s.record(r, "", "key.create", nil, "", nil)
	writeJSON(w, 201, struct {
		store.Key
		Token string `json:"key"`
	}{k, token})
}
func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	vv, e := s.Store.Keys()
	if e != nil {
		fail(w, "internal")
		return
	}
	writeJSON(w, 200, map[string]any{"keys": vv})
}
func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	e := s.Store.DeleteKey(r.PathValue("id"))
	if e != nil {
		if errors.Is(e, store.ErrLastOwner) {
			fail(w, "conflict")
		} else if errors.Is(e, sql.ErrNoRows) {
			fail(w, "not_found")
		} else {
			fail(w, "internal")
		}
		return
	}
	s.record(r, "", "key.delete", nil, "", nil)
	w.WriteHeader(204)
}

// hostPin encodes the creation policy for the server-served sender's
// fragment: "none", "passphrase" or "digits:<n>".
func hostPin(p hostview.PinPolicy) string {
	if p.Kind == "digits" && p.Length != nil {
		return fmt.Sprintf("digits:%d", *p.Length)
	}
	return p.Kind
}
