package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/store"
)

func TestContractErrorStatuses(t *testing.T) {
	for code, status := range map[string]int{"invalid_argument": 400, "passphrase_required": 400, "unauthorized": 401, "forbidden_scope": 403, "origin_rejected": 403, "local_owner_required": 403, "not_found": 404, "unknown_admission": 404, "unknown_knock": 404, "conflict": 409, "already_live": 409, "idempotency_conflict": 409, "sender_offline": 409, "not_live": 409, "rotation_pending": 409, "code_mismatch": 409, "stream_ended": 410, "payload_too_large": 413, "rate_limited": 429, "internal": 500, "storage": 503, "sender_timeout": 504} {
		t.Run(code, func(t *testing.T) {
			w := httptest.NewRecorder()
			fail(w, code)
			if w.Code != status {
				t.Fatal(w.Code)
			}
			var envelope struct {
				Error apiError `json:"error"`
			}
			if json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Error.Code != code {
				t.Fatal(w.Body.String())
			}
			if status == 429 && w.Header().Get("Retry-After") == "" {
				t.Fatal("missing retry")
			}
		})
	}
}
func TestSenderRequiredFieldsAndBounds(t *testing.T) {
	state := validState(store.ID(), store.ID())
	b, _ := json.Marshal(state)
	for _, change := range []func(map[string]json.RawMessage){func(m map[string]json.RawMessage) { delete(m, "access") }, func(m map[string]json.RawMessage) { m["admissions"] = json.RawMessage("null") }, func(m map[string]json.RawMessage) { m["seq"] = json.RawMessage("9007199254740992") }, func(m map[string]json.RawMessage) { m["access"] = json.RawMessage(`{"pin":null}`) }, func(m map[string]json.RawMessage) { m["v"] = json.RawMessage("2") }} {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(b, &m)
		change(m)
		v, _ := json.Marshal(m)
		if _, e := parseState(v); e == nil {
			t.Fatal(string(v))
		}
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	m["phase"] = json.RawMessage(`"future"`)
	v, _ := json.Marshal(m)
	s, e := parseState(v)
	if e != nil || s.Phase != "unknown" {
		t.Fatal(s, e)
	}
	m["seq"] = json.RawMessage("null")
	v, _ = json.Marshal(m)
	if requiredJSON(v, reflect.TypeFor[hostview.SenderState]()) == nil {
		t.Fatal("null uint")
	}
	state.Admissions = make([]hostview.Admission, 501)
	b, _ = json.Marshal(state)
	if _, e = parseState(b); e == nil {
		t.Fatal("admission bound")
	}
}
func TestProxyHintsAndMissingGeoDB(t *testing.T) {
	f, e := openFacts(Config{TrustProxy: "127.0.0.1/32,10.0.0.0/8", GeoIPCity: filepath.Join(t.TempDir(), "missing")})
	if e != nil {
		t.Fatal(e)
	}
	defer f.close()
	for _, tt := range []struct{ peer, xff, want string }{{"192.0.2.1:123", "203.0.113.1", "192.0.2.1"}, {"127.0.0.1:123", "203.0.113.1, 10.0.0.1", "203.0.113.1"}, {"127.0.0.1:123", "spoof, 192.0.2.2", "192.0.2.2"}, {"127.0.0.1:123", "broken", "127.0.0.1"}} {
		r := httptest.NewRequest("GET", "http://localhost/", nil)
		r.RemoteAddr = tt.peer
		r.Header.Set("X-Forwarded-For", tt.xff)
		if got := f.address(r); got != tt.want {
			t.Fatal(tt, got)
		}
		if f.geo(tt.want) != nil {
			t.Fatal("missing DB not nullable")
		}
	}
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	r.Header.Set("Sec-CH-UA-Platform", `"Android"`)
	r.Header.Set("Sec-CH-UA", `"Google Chrome";v="144"`)
	if d := device(r); d == nil || *d != "Android · Chrome" {
		t.Fatal(d)
	}
	if got := clean("a\n\u202eb\u200bc", 64, 256); got != "abc" {
		t.Fatal(got)
	}
}
func TestExpiryPaginationHistoryAndOfflineStop(t *testing.T) {
	f := setup(t)
	id, pub, _ := f.create(`{"expires_in_s":0}`)
	_, v := f.join(id, "publish", pub, "10.0.0.2")
	if string(v["code"]) != `"stream_ended"` {
		t.Fatal(v)
	}
	status, out, _ := f.request("GET", "/api/v1/streams/"+id, f.key, "")
	if status != 200 || string(out["phase"]) != `"ended"` {
		t.Fatal(status, out)
	}
	for i := 0; i < 3; i++ {
		f.create(`{}`)
	}
	status, out, _ = f.request("GET", "/api/v1/streams?limit=2", f.key, "")
	if status != 200 || string(out["next_cursor"]) == "null" {
		t.Fatal(status, out)
	}
	var cursor string
	_ = json.Unmarshal(out["next_cursor"], &cursor)
	status, out, _ = f.request("GET", "/api/v1/streams?limit=2&cursor="+cursor, f.key, "")
	if status != 200 {
		t.Fatal(status, out)
	}
	status, out, _ = f.request("GET", "/api/v1/audit?limit=1", f.key, "")
	if status != 200 || string(out["next_cursor"]) == "null" {
		t.Fatal(status, out)
	}
	_ = json.Unmarshal(out["next_cursor"], &cursor)
	status, out, _ = f.request("GET", "/api/v1/audit?cursor="+cursor, f.key, "")
	if status != 200 {
		t.Fatal(status, out)
	}
	id, _, _ = f.create(`{}`)
	status, out, _ = f.request("POST", "/api/v1/streams/"+id+"/commands", f.key, `{"op":"stop"}`)
	if status != 200 {
		t.Fatal(status, out)
	}
	status, out, _ = f.request("POST", "/api/v1/streams/"+id+"/commands", f.key, `{"op":"pause"}`)
	if status != 410 || errCode(out) != "stream_ended" {
		t.Fatal(status, out)
	}
	for _, path := range []string{"/api/v1/streams?limit=0", "/api/v1/audit?since=-1", "/api/v1/streams?cursor=bad!", "/api/v1/streams?state=bad"} {
		status, out, _ = f.request("GET", path, f.key, "")
		if status != 400 {
			t.Fatal(path, status, out)
		}
	}
}
func TestHandshakeLimitsAndProtocol(t *testing.T) {
	f := setup(t)
	id, _, cap := f.create(`{}`)
	f.s.mu.Lock()
	for i := 0; i < 20; i++ {
		_ = f.s.allow("handshake-ip:127.0.0.1", 2, 20)
	}
	f.s.mu.Unlock()
	status, out, _ := f.request("GET", "/signal", "", "")
	if status != 429 || errCode(out) != "rate_limited" {
		t.Fatal(status, out)
	}
	_ = id
	_ = cap
}
func TestExplicitExpiryNullAndRetentionOwner(t *testing.T) {
	f := setup(t)
	settings := store.DefaultSettings()
	n := uint64(60)
	settings.Defaults.ExpiresInS = &n
	if e := f.s.Store.Put("settings", settings); e != nil {
		t.Fatal(e)
	}
	id, _, _ := f.create(`{"expires_in_s":null}`)
	v, _ := f.s.Store.Get(id)
	if v.ExpiresAt != nil {
		t.Fatal("explicit null inherited expiry")
	}
	_, key, e := f.s.Store.NewKey("agent", "streams", nil)
	if e != nil {
		t.Fatal(e)
	}
	settings.Retention.KeepIP = true
	b, _ := json.Marshal(settings)
	status, out, _ := f.request("PUT", "/api/v1/settings", key, string(b))
	if status != 403 || errCode(out) != "forbidden_scope" {
		t.Fatal(status, out)
	}
}
func TestUnknownAlertsNeverCarrySecretFields(t *testing.T) {
	for _, b := range []string{`{"kind":"pin_changed","pin":"secret"}`, `{"kind":"future","link":"secret"}`} {
		if parseAlert([]byte(b)) != nil {
			t.Fatal(b)
		}
	}
	a := parseAlert([]byte(`{"kind":"state_mismatch","pin":"secret"}`))
	b, _ := json.Marshal(a)
	if strings.Contains(string(b), "secret") {
		t.Fatal(string(b))
	}
}
func TestSessionIdleAndKeyExpiry(t *testing.T) {
	f := setup(t)
	expired := time.Now().Add(-time.Second).UnixMilli()
	_, token, e := f.s.Store.NewKey("expired", "read", &expired)
	if e != nil {
		t.Fatal(e)
	}
	status, out, _ := f.request("GET", "/api/v1/streams", token, "")
	if status != 401 {
		t.Fatal(status, out)
	}
	k, _ := f.s.Store.AuthKey(f.key)
	session, _, e := f.s.Store.Login(k)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.Store.DB.Exec("UPDATE dashboard_sessions SET idle=?", expired); e != nil {
		t.Fatal(e)
	}
	if _, _, e = f.s.Store.AuthSession(session); e == nil {
		t.Fatal("idle session accepted")
	}
}

// A tiny synthetic MMDB maps both branches to one public test record. No third
// party geolocation dataset or IP addresses are bundled with the test suite.
func TestOfflineGeoLookup(t *testing.T) {
	text := func(s string) []byte {
		prefix := []byte{byte(2<<5 | len(s))}
		if len(s) >= 29 {
			prefix = []byte{byte(2<<5 | 29), byte(len(s) - 29)}
		}
		return append(prefix, []byte(s)...)
	}
	uintValue := func(n int) []byte {
		if n < 256 {
			return []byte{byte(6<<5 | 1), byte(n)}
		}
		return []byte{byte(6<<5 | 2), byte(n >> 8), byte(n)}
	}
	obj := func(fields ...[]byte) []byte {
		out := []byte{byte(7<<5 | len(fields)/2)}
		for _, f := range fields {
			out = append(out, f...)
		}
		return out
	}
	record := obj(text("country"), obj(text("iso_code"), text("BE")), text("city"), obj(text("names"), obj(text("en"), text("Brussels"))), text("autonomous_system_number"), uintValue(64500), text("autonomous_system_organization"), text("Test ASN"))
	metadata := obj(text("node_count"), uintValue(1), text("record_size"), uintValue(24), text("ip_version"), uintValue(4), text("database_type"), text("Synthetic"), text("binary_format_major_version"), uintValue(2), text("binary_format_minor_version"), uintValue(0), text("build_epoch"), uintValue(1))
	b := append([]byte{0, 0, 17, 0, 0, 17}, make([]byte, 16)...)
	b = append(b, record...)
	b = append(b, []byte("\xab\xcd\xefMaxMind.com")...)
	b = append(b, metadata...)
	path := filepath.Join(t.TempDir(), "test.mmdb")
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	f, e := openFacts(Config{GeoIPCity: path, GeoIPASN: path})
	if e != nil {
		t.Fatal(e)
	}
	defer f.close()
	g := f.geo("192.0.2.1")
	if g == nil || g.Country == nil || *g.Country != "BE" || g.City == nil || *g.City != "Brussels" || g.ASN == nil || *g.ASN != 64500 || g.ASOrg == nil || *g.ASOrg != "Test ASN" {
		t.Fatalf("%+v", g)
	}
}
