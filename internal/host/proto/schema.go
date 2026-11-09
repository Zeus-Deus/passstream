package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// V validates (and may normalise) one decoded JSON value. It implements the
// schema notation of CONTRACT §1.1 for input the daemon doesn't trust: the
// sender page's reports and the server's responses.
type V func(v any) (any, error)

func bad(f string, a ...any) error { return E("invalid_argument", f, a...) }

// Field describes one object member.
type Field struct {
	Name     string
	V        V
	Optional bool
}

// Req and Opt build fields.
func Req(name string, v V) Field { return Field{Name: name, V: v} }
func Opt(name string, v V) Field { return Field{Name: name, V: v, Optional: true} }

// Obj validates an object; unknown members are dropped (CONTRACT §1.2), so
// nothing the schema doesn't name is ever stored or forwarded.
func Obj(fields ...Field) V {
	return func(v any) (any, error) {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, bad("expected an object")
		}
		out := make(map[string]any, len(fields))
		for _, f := range fields {
			x, present := m[f.Name]
			if !present {
				if f.Optional {
					continue
				}
				return nil, bad("missing %s", f.Name)
			}
			y, err := f.V(x)
			if err != nil {
				return nil, bad("%s: %s", f.Name, msgOf(err))
			}
			out[f.Name] = y
		}
		return out, nil
	}
}

func msgOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Message
	}
	return err.Error()
}

// Null allows null in addition to v.
func Null(v V) V {
	return func(x any) (any, error) {
		if x == nil {
			return nil, nil
		}
		return v(x)
	}
}

// Bool requires a JSON boolean.
func Bool(x any) (any, error) {
	if b, ok := x.(bool); ok {
		return b, nil
	}
	return nil, bad("expected a boolean")
}

// Uint requires an integer in [0, 2^53−1].
func Uint(x any) (any, error) {
	f, ok := x.(float64)
	if !ok || f < 0 || f > 1<<53-1 || f != math.Trunc(f) {
		return nil, bad("expected a uint")
	}
	return f, nil
}

// Num requires a finite number.
func Num(x any) (any, error) {
	f, ok := x.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, bad("expected a number")
	}
	return f, nil
}

// Const requires one exact uint value.
func Const(n float64) V {
	return func(x any) (any, error) {
		if f, ok := x.(float64); ok && f == n {
			return f, nil
		}
		return nil, bad("expected %v", n)
	}
}

// Str requires a string matching pred.
func Str(pred func(string) bool, what string) V {
	return func(x any) (any, error) {
		s, ok := x.(string)
		if !ok || !pred(s) {
			return nil, bad("expected %s", what)
		}
		return s, nil
	}
}

// Enum requires one of the values; security inputs use this.
func Enum(vals ...string) V {
	return func(x any) (any, error) {
		s, ok := x.(string)
		if ok {
			for _, v := range vals {
				if s == v {
					return s, nil
				}
			}
		}
		return nil, bad("unknown value")
	}
}

// EnumOrUnknown maps an unknown string to "unknown" (display-only and
// UI-gating enums, CONTRACT §1.2).
func EnumOrUnknown(vals ...string) V {
	return func(x any) (any, error) {
		s, ok := x.(string)
		if !ok {
			return nil, bad("expected a string")
		}
		for _, v := range vals {
			if s == v {
				return s, nil
			}
		}
		return "unknown", nil
	}
}

// Label clamps and strips a display string (CONTRACT §1.1).
func Label(maxRunes, maxBytes int) V {
	return func(x any) (any, error) {
		s, ok := x.(string)
		if !ok {
			return nil, bad("expected a string")
		}
		return Clean(s, maxRunes, maxBytes), nil
	}
}

// Clean strips control, bidi and zero-width characters and clamps a string.
func Clean(s string, maxRunes, maxBytes int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || isBidiOrZW(r) {
			continue
		}
		w := utf8.RuneLen(r)
		if n >= maxRunes || b.Len()+w > maxBytes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

func isBidiOrZW(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2060 && r <= 0x2069,
		r == 0xFEFF, r == 0x061C, r == 0x00AD, r == 0x180E:
		return true
	}
	return unicode.Is(unicode.Cf, r)
}

// Arr validates an array of at most max elements.
func Arr(max int, elem V) V {
	return func(x any) (any, error) {
		a, ok := x.([]any)
		if !ok {
			return nil, bad("expected an array")
		}
		if len(a) > max {
			return nil, E("payload_too_large", "array over %d", max)
		}
		out := make([]any, len(a))
		for i, e := range a {
			y, err := elem(e)
			if err != nil {
				return nil, bad("[%d]: %s", i, msgOf(err))
			}
			out[i] = y
		}
		return out, nil
	}
}

// Any tries the alternatives in order.
func Any(vs ...V) V {
	return func(x any) (any, error) {
		var last error
		for _, v := range vs {
			y, err := v(x)
			if err == nil {
				return y, nil
			}
			last = err
		}
		return nil, last
	}
}

// DecodeJSON parses one JSON value with numbers as float64, refusing
// trailing data.
func DecodeJSON(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, bad("malformed JSON")
	}
	if d.More() {
		return nil, bad("trailing data")
	}
	return v, nil
}

// Into validates b against v and decodes the normalised result into out.
func Into(b []byte, v V, out any) error {
	x, err := DecodeJSON(b)
	if err != nil {
		return err
	}
	return IntoValue(x, v, out)
}

// IntoValue validates an already decoded value and decodes it into out.
func IntoValue(x any, v V, out any) error {
	y, err := v(x)
	if err != nil {
		return err
	}
	nb, err := json.Marshal(y)
	if err != nil {
		return fmt.Errorf("remarshal: %w", err)
	}
	return json.Unmarshal(nb, out)
}

// Common field validators.
var (
	VId22    = Str(IsId22, "an Id22")
	VToken43 = Str(IsToken43, "a Token43")
	VConnID  = Str(IsConnID, "a connection id")
	VRowCode = Str(IsRowCode, "a row code")
	VMs      = Uint
	VPolicy  = V(func(x any) (any, error) {
		p, err := ParsePinPolicy(x)
		if err != nil {
			return nil, err
		}
		m := map[string]any{"kind": p.Kind}
		if p.Kind == "digits" {
			m["length"] = float64(p.Length)
		}
		return m, nil
	})
)

// LatchesV requires all three latches as booleans: a malformed latch
// invalidates the whole message (CONTRACT Appendix A #16).
var LatchesV = Obj(Req("locked", Bool), Req("paused", Bool), Req("frozen", Bool))

// AccessV is Access.
var AccessV = Obj(
	Req("pin", Obj(Req("required", Bool), Req("kind", Enum("digits", "passphrase", "none")), Req("length", Null(Uint)))),
	Req("pin_version", Uint),
	Req("rotation_pending", Bool),
	Req("reconciling", Bool),
	Req("latches", LatchesV),
	Req("knock", Bool),
	Req("failures", Obj(Req("stream", Uint), Req("limit", Uint))),
)

// E2EEV is SenderState.e2ee.
var E2EEV = Obj(Req("status", EnumOrUnknown("beta", "on")), Req("epoch", Uint), Req("suspended", Bool), Req("revocation_not_durable", Bool))

// HealthV normalises invalid advisory telemetry to null, preserving sender facts.
var HealthV V = func(v any) (any, error) {
	cleaned, err := healthV(v)
	if err != nil {
		return nil, nil
	}
	return cleaned, nil
}

func healthV(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil || len(b) > 1024 {
		return nil, bad("health exceeds 1 KiB")
	}
	cleaned, err := Obj(Req("uplink_kbps", Null(Uint)), Req("encode_ms", Null(Num)), Req("fps", Null(Num)),
		Req("quality_limitation", EnumOrUnknown("none", "cpu", "bandwidth", "other")),
		Opt("encode_cpu_pct", Null(Num)), Opt("width", Null(Uint)), Opt("height", Null(Uint)))(v)
	if err != nil {
		return nil, err
	}
	b, _ = json.Marshal(cleaned)
	var h Health
	if json.Unmarshal(b, &h) != nil || !h.Valid() {
		return nil, bad("health out of range")
	}
	return cleaned, nil
}

// KnockEnds are the KnockEnd reasons (CONTRACT §5.3).
var KnockEnds = []string{"denied", "timeout", "peer_closed", "replaced", "pin_rotated", "stopped", "commit_failed", "queue_full"}

// SenderStateV is SenderState with its limits (CONTRACT §1.3).
var SenderStateV = Obj(
	Req("v", Const(1)),
	Req("stream_id", VId22),
	Req("instance", VId22),
	Req("seq", Uint),
	Req("phase", EnumOrUnknown("starting", "picking", "live", "paused", "ended")),
	Opt("end_reason", EnumOrUnknown("stopped", "window_closed", "expired", "deleted", "state_lost", "browser_failed")),
	Req("mode", EnumOrUnknown("text", "motion")),
	Req("e2ee", E2EEV),
	Req("access", AccessV),
	Req("admissions", Arr(500, Obj(Req("admission_id", VId22), Req("viewer_id", VId22), Req("row_code", VRowCode),
		Req("peer_id", VConnID), Req("connected", Bool), Req("via_grant", Bool), Req("admitted_at", VMs), Req("kick_incomplete", Bool)))),
	Req("knocks", Arr(50, Obj(Req("knock_id", VId22), Req("peer_id", VConnID), Req("row_code", VRowCode), Req("at", VMs), Req("expires_at", VMs)))),
	Req("ends", Arr(100, Any(
		Obj(Req("kind", Enum("admission")), Req("admission_id", VId22), Req("viewer_id", VId22), Req("cause", EnumOrUnknown("kicked", "revoked", "stream_ended"))),
		Obj(Req("kind", Enum("knock")), Req("knock_id", VId22), Req("reason", EnumOrUnknown(KnockEnds...))),
	))),
	Req("health", Null(HealthV)),
)

// ConnectionV is a server-observed Connection, cleaned of hostile strings.
var ConnectionV = Obj(
	Req("connection_id", VConnID),
	Req("joined_at", VMs),
	Req("left_at", Null(VMs)),
	Req("ip", Null(Label(64, 64))),
	Req("geo", Null(Obj(Req("country", Null(Label(8, 16))), Req("city", Null(Label(64, 256))), Req("asn", Null(Uint)), Req("as_org", Null(Label(64, 256)))))),
	Req("user_agent", Null(Label(256, 256))),
	Req("device", Null(Label(64, 256))),
	Req("path", Null(EnumOrUnknown("udp", "tcp", "turn_udp", "turn_tcp", "turn_tls"))),
	Req("stats", Null(Obj(Req("rtt_ms", Null(Num)), Req("loss_pct", Null(Num)), Req("bitrate_kbps", Null(Uint)), Req("freezes", Uint), Req("temporal_layer", Null(Uint))))),
	Req("stats_at", Null(VMs)),
	Req("reported", Obj(Req("label", Null(Label(64, 256))), Req("tab_hidden", Null(Bool)))),
)

// AlertV validates the alerts a sender may emit (CONTRACT §4.7, §11.1).
// pin_changed is accepted only with by "window": the page's own UI.
var AlertV = Any(
	Obj(Req("kind", Enum("failed_pin")), Req("count", Uint), Req("sources", Uint), Req("from_at", VMs), Req("to_at", VMs)),
	Obj(Req("kind", Enum("frozen")), Req("failures", Uint)),
	Obj(Req("kind", Enum("unfrozen")), Req("pin_rotated", Bool)),
	Obj(Req("kind", Enum("revocation_durability")), Req("not_durable", Bool)),
	Obj(Req("kind", Enum("state_mismatch"))),
	Obj(Req("kind", Enum("knock_ended")), Req("knock_id", VId22), Req("reason", EnumOrUnknown(KnockEnds...))),
	Obj(Req("kind", Enum("pin_changed")), Req("pin", VPolicy), Req("by", Enum("window"))),
)
