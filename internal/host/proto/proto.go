// Package proto holds the wire types and validators shared by the local
// host daemon and the CLI (CONTRACT.md §1, §4, §10, §11, §14).
package proto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/passstream/passstream/internal/hostview"
	"strings"
)

// Protocol is the version on the daemon socket and the sender WebSocket.
const Protocol = 1

// RowAlphabet is the row-code alphabet (CONTRACT §4.3).
const RowAlphabet = "ACDEFHJKMNPRTUVWXY3479"

// Error is the shared error envelope (CONTRACT §14).
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// E builds an Error. The message is for people and never holds a secret.
func E(code, format string, a ...any) *Error {
	m := code
	if format != "" {
		m = fmt.Sprintf(format, a...)
	}
	return &Error{Code: code, Message: m}
}

// WithDetails returns e with details set.
func (e *Error) WithDetails(d map[string]any) *Error { e.Details = d; return e }

// AsError converts any error into an envelope, mapping unknown errors to internal.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	return E("internal", "internal error")
}

var b64 = base64.RawURLEncoding

func isB64(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	// Canonical: the trailing bits must be zero, so one value has one form.
	b, err := b64.DecodeString(s)
	return err == nil && b64.EncodeToString(b) == s
}

// IsId22 reports a canonical 16-byte b64url id.
func IsId22(s string) bool { return isB64(s, 22) }

// IsToken43 reports a canonical 32-byte b64url token.
func IsToken43(s string) bool { return isB64(s, 43) }

// IsConnID reports "c_" + Id22.
func IsConnID(s string) bool { return strings.HasPrefix(s, "c_") && IsId22(s[2:]) }

// IsRowCode reports a canonical row code.
func IsRowCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if strings.IndexByte(RowAlphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// RandBytes returns n CSPRNG bytes; it panics only if the kernel RNG fails.
func RandBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// NewId22 returns a fresh random Id22.
func NewId22() string { return b64.EncodeToString(RandBytes(16)) }

// NewToken43 returns a fresh random Token43 and its bytes.
func NewToken43() (string, []byte) {
	b := RandBytes(32)
	return b64.EncodeToString(b), b
}

// Decode decodes a canonical b64url value of the given byte length.
func Decode(s string, n int) ([]byte, bool) {
	b, err := b64.DecodeString(s)
	if err != nil || len(b) != n || b64.EncodeToString(b) != s {
		return nil, false
	}
	return b, true
}

// Encode encodes bytes as b64url without padding.
func Encode(b []byte) string { return b64.EncodeToString(b) }

// PinPolicy is CONTRACT §4.1.
type PinPolicy struct {
	Kind   string `json:"kind"`
	Length int    `json:"length,omitempty"`
}

// DefaultPolicy is four digits (PLAN "Link + PIN").
var DefaultPolicy = PinPolicy{Kind: "digits", Length: 4}

// Rank orders policies by strictness.
func (p PinPolicy) Rank() int {
	switch p.Kind {
	case "digits":
		return p.Length
	case "passphrase":
		return 100
	}
	return 0
}

// Stricter returns the stricter of two policies (CONTRACT §4.1).
func Stricter(a, b PinPolicy) PinPolicy {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

// ParsePinPolicy validates a closed PinPolicy object. Unknown kinds are
// refused, never defaulted (CONTRACT §1.2).
func ParsePinPolicy(v any) (PinPolicy, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return PinPolicy{}, E("invalid_argument", "pin policy must be an object")
	}
	switch m["kind"] {
	case "digits":
		if len(m) != 2 {
			return PinPolicy{}, E("invalid_argument", "digits policy takes kind and length")
		}
		n, ok := m["length"].(float64)
		if !ok || n != float64(int(n)) || n < 4 || n > 12 {
			return PinPolicy{}, E("invalid_argument", "digits length must be 4 to 12")
		}
		return PinPolicy{Kind: "digits", Length: int(n)}, nil
	case "passphrase", "none":
		if len(m) != 1 {
			return PinPolicy{}, E("invalid_argument", "policy takes only kind")
		}
		return PinPolicy{Kind: m["kind"].(string)}, nil
	}
	return PinPolicy{}, E("invalid_argument", "unknown pin policy kind")
}

// The CONTRACT §4.2–§4.6 wire types are shared with the server through
// internal/hostview, which also holds the one viewer join (CONTRACT §6).
type (
	Latches      = hostview.Latches
	PinInfo      = hostview.PinState
	Failures     = hostview.Failures
	Access       = hostview.Access
	Admission    = hostview.Admission
	Knock        = hostview.Knock
	End          = hostview.End
	Health       = hostview.Health
	E2EE         = hostview.E2EE
	SenderState  = hostview.SenderState
	Geo          = hostview.Geo
	Stats        = hostview.Stats
	Reported     = hostview.Reported
	Connection   = hostview.Connection
	ViewerSender = hostview.ViewerSender
	Viewer       = hostview.Viewer
	ViewerList   = hostview.ViewerList
)

// DaemonStatus is HostStatus.daemon (CONTRACT §10).
type DaemonStatus struct {
	Version     string `json:"version"`
	ServerURL   string `json:"server_url"`
	Key         string `json:"key"`
	Server      string `json:"server"`
	PortMode    string `json:"port_mode"`
	Supervision string `json:"supervision"`
	PortOwner   string `json:"port_owner"`
}

// StreamStatus is HostStatus.stream (CONTRACT §10).
type StreamStatus struct {
	ID         string   `json:"id"`
	Label      *string  `json:"label"`
	Phase      string   `json:"phase"`
	Attachment string   `json:"attachment"`
	Mode       string   `json:"mode"`
	Safe       bool     `json:"safe"`
	StartedAt  int64    `json:"started_at"`
	LiveAt     *int64   `json:"live_at"`
	Access     *Access  `json:"access"`
	E2EE       *E2EE    `json:"e2ee"`
	Health     *Health  `json:"health"`
	Watching   int      `json:"watching"`
	Knocking   int      `json:"knocking"`
	Viewers    []Viewer `json:"viewers"`
	HasLink    bool     `json:"has_link"`
}

// HostStatus never contains the PIN or the link.
type HostStatus struct {
	Daemon DaemonStatus  `json:"daemon"`
	Stream *StreamStatus `json:"stream"`
}

// Alert is kept as a validated JSON object: the daemon forwards the
// sender-emitted kinds and derives a few of its own (CONTRACT §4.7).
type Alert = map[string]any

// Request is one daemon socket request line (CONTRACT §10).
type Request struct {
	ID   string          `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args"`
}

// Reply is one daemon socket reply line.
type Reply struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// Event is one daemon socket event line.
type Event struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}
