package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"

	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/store"
)

// Required wire fields are checked before decoding: Go's zero values must not
// turn a missing security latch into false. Pointers are the nullable fields.
func requiredJSON(b []byte, t reflect.Type) error {
	if t.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			return nil
		}
		return requiredJSON(b, t.Elem())
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return errors.New("null field")
	}
	switch t.Kind() {
	case reflect.Uint64, reflect.Int64:
		n, e := strconv.ParseUint(string(b), 10, 53)
		if e != nil || n > 1<<53-1 {
			return errors.New("invalid uint")
		}
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if json.Unmarshal(b, &obj) != nil || obj == nil {
			return errors.New("object required")
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			name := strings.Split(tag, ",")[0]
			if name == "-" {
				continue
			}
			value, ok := obj[name]
			if !ok {
				if strings.Contains(tag, "omitempty") {
					continue
				}
				return errors.New("required field missing")
			}
			if e := requiredJSON(value, f.Type); e != nil {
				return e
			}
		}
	case reflect.Slice:
		var aa []json.RawMessage
		if json.Unmarshal(b, &aa) != nil {
			return errors.New("array required")
		}
		for _, a := range aa {
			if e := requiredJSON(a, t.Elem()); e != nil {
				return e
			}
		}
	}
	return nil
}
func parseState(b []byte) (hostview.SenderState, error) {
	var v hostview.SenderState
	if len(b) > 256<<10 {
		return v, errors.New("oversized")
	}
	// Telemetry is advisory: malformed health must not discard admissions or latches.
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(b, &wire); err != nil {
		return v, err
	}
	health, hasHealth := wire["health"]
	if hasHealth {
		wire["health"] = json.RawMessage("null")
		b, _ = json.Marshal(wire)
	}
	if e := requiredJSON(b, reflect.TypeFor[hostview.SenderState]()); e != nil {
		return v, e
	}
	if e := json.Unmarshal(b, &v); e != nil {
		return v, e
	}
	bad := func() (hostview.SenderState, error) {
		return hostview.SenderState{}, errors.New("invalid sender state")
	}
	if v.V != 1 || !id22.MatchString(v.StreamID) || !id22.MatchString(v.Instance) || v.Seq > 1<<53-1 || len(v.Admissions) > 500 || len(v.Knocks) > 50 || len(v.Ends) > 100 || !validPolicy(hostview.PinPolicy{Kind: v.Access.Pin.Kind, Length: v.Access.Pin.Length}) || v.Access.Pin.Required != (v.Access.Pin.Kind != "none") {
		return bad()
	}
	switch v.Phase {
	case "starting", "picking", "live", "paused", "ended":
	default:
		v.Phase = "unknown"
	}
	if v.Mode != "text" && v.Mode != "motion" {
		v.Mode = "unknown"
	}
	if v.E2EE.Status != "beta" && v.E2EE.Status != "on" {
		v.E2EE.Status = "unknown"
	}
	ids := map[string]bool{}
	for _, a := range v.Admissions {
		if !id22.MatchString(a.AdmissionID) || !id22.MatchString(a.ViewerID) || !connID.MatchString(a.PeerID) || !rowCode(a.RowCode) || a.AdmittedAt < 0 || ids[a.AdmissionID] {
			return bad()
		}
		ids[a.AdmissionID] = true
	}
	for _, k := range v.Knocks {
		if !id22.MatchString(k.KnockID) || !connID.MatchString(k.PeerID) || !rowCode(k.RowCode) || k.At < 0 || k.ExpiresAt < k.At || ids[k.KnockID] {
			return bad()
		}
		ids[k.KnockID] = true
	}
	for _, e := range v.Ends {
		switch e.Kind {
		case "admission":
			if !id22.MatchString(e.AdmissionID) || !id22.MatchString(e.ViewerID) || e.Cause != "kicked" && e.Cause != "revoked" && e.Cause != "stream_ended" {
				return bad()
			}
		case "knock":
			if !id22.MatchString(e.KnockID) || !validKnockEnd(e.Reason) {
				return bad()
			}
		default:
			return bad()
		}
	}
	if v.EndReason != "" && v.EndReason != "stopped" && v.EndReason != "window_closed" && v.EndReason != "deleted" && v.EndReason != "state_lost" && v.EndReason != "expired" && v.EndReason != "browser_failed" {
		v.EndReason = "unknown"
	}
	var h hostview.Health
	if len(health) <= 1024 && requiredJSON(health, reflect.TypeFor[hostview.Health]()) == nil && json.Unmarshal(health, &h) == nil && h.Valid() {
		v.Health = &h
		switch v.Health.QualityLimitation {
		case "none", "cpu", "bandwidth", "other":
		default:
			v.Health.QualityLimitation = "unknown"
		}
	}
	return v, nil
}
func rowCode(v string) bool {
	if len(v) != 3 {
		return false
	}
	for _, c := range v {
		if !strings.ContainsRune("ACDEFHJKMNPRTUVWXY3479", c) {
			return false
		}
	}
	return true
}
func parseAlert(b []byte) any {
	var kind struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(b, &kind) != nil {
		return nil
	}
	var target any
	switch kind.Kind {
	case "failed_pin":
		target = &struct {
			Kind    string `json:"kind"`
			Count   uint64 `json:"count"`
			Sources uint64 `json:"sources"`
			FromAt  uint64 `json:"from_at"`
			ToAt    uint64 `json:"to_at"`
		}{}
	case "frozen":
		target = &struct {
			Kind     string `json:"kind"`
			Failures uint64 `json:"failures"`
		}{}
	case "unfrozen":
		target = &struct {
			Kind       string `json:"kind"`
			PinRotated bool   `json:"pin_rotated"`
		}{}
	case "revocation_durability":
		target = &struct {
			Kind       string `json:"kind"`
			NotDurable bool   `json:"not_durable"`
		}{}
	case "state_mismatch":
		target = &struct {
			Kind string `json:"kind"`
		}{}
	case "knock_ended":
		var k struct {
			KnockID string `json:"knock_id"`
			Reason  string `json:"reason"`
		}
		if json.Unmarshal(b, &k) != nil || !id22.MatchString(k.KnockID) || !validKnockEnd(k.Reason) {
			return nil
		}
		target = &struct {
			Kind    string `json:"kind"`
			KnockID string `json:"knock_id"`
			Reason  string `json:"reason"`
		}{}
	default:
		return nil
	}
	if requiredJSON(b, reflect.TypeOf(target).Elem()) != nil || json.Unmarshal(b, target) != nil {
		return nil
	}
	return target
}

// Nullable request fields are explicit in the contract. Unknown fields remain
// ignored; the check only visits fields understood by these endpoints.
func validateRequestBody(b []byte, path string) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil {
		return false
	}
	for _, key := range []string{"pin", "pin_explicit", "knock", "sender_kind", "id", "op", "args", "defaults", "retention", "scope", "length", "kind", "keep_ip", "max_viewers"} {
		if v, ok := obj[key]; ok && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	if v, ok := obj["id"]; ok {
		var id string
		if json.Unmarshal(v, &id) != nil || !commandID.MatchString(id) {
			return false
		}
	}
	if strings.HasSuffix(path, "/settings") {
		if requiredJSON(b, reflect.TypeFor[store.Settings]()) != nil {
			return false
		}
	}
	return true
}

func validKnockEnd(reason string) bool {
	switch reason {
	case "denied", "timeout", "peer_closed", "replaced", "pin_rotated", "stopped", "commit_failed", "queue_full":
		return true
	}
	return false
}
