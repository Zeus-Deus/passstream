// Package serverapi is the daemon's client for the PassStream server's HTTP
// API (CONTRACT §9). It treats the server as hostile input: bounded bodies,
// schema-validated JSON, cleaned strings, and URLs derived only from the
// configured base, never from a response (HOST-CONTROL §5.1).
package serverapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/passstream/passstream/internal/host/proto"
)

// NormalizeBase validates a server base URL like the PassPage baseUrl: one
// http(s) origin, https unless loopback, no userinfo, path, query, fragment,
// controls or globs. It returns "scheme://host[:port]".
func NormalizeBase(s string) (string, error) {
	bad := proto.E("invalid_argument", "server URL must be one http(s) origin")
	if s == "" || len(s) > 512 || strings.ContainsAny(s, "*?#@ \\") {
		return "", bad
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", bad
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Host == "" {
		return "", bad
	}
	if u.Path != "" && u.Path != "/" {
		return "", bad
	}
	host := strings.ToLower(u.Hostname())
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(host) {
			return "", proto.E("invalid_argument", "plain http is allowed only on loopback")
		}
	default:
		return "", bad
	}
	out := u.Scheme + "://" + host
	if strings.Contains(host, ":") {
		out = u.Scheme + "://[" + host + "]"
	}
	if p := u.Port(); p != "" {
		out += ":" + p
	}
	return out, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SignalURL derives the signaling URL from the base.
func SignalURL(base string) string {
	if strings.HasPrefix(base, "https://") {
		return "wss://" + strings.TrimPrefix(base, "https://") + "/signal"
	}
	return "ws://" + strings.TrimPrefix(base, "http://") + "/signal"
}

// Client calls the server with the owner's key.
type Client struct {
	Base string
	Key  string
	HTTP *http.Client
}

// New returns a client with bounded timeouts.
func New(base, key string) *Client {
	return &Client{Base: base, Key: key, HTTP: &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // never follow the key elsewhere
		},
	}}
}

// Settings is CONTRACT §9.7 (the parts the daemon uses).
type Settings struct {
	Defaults struct {
		Pin   proto.PinPolicy `json:"pin"`
		Knock bool            `json:"knock"`
	} `json:"defaults"`
}

var settingsV = proto.Obj(proto.Req("defaults", proto.Obj(proto.Req("pin", proto.VPolicy), proto.Req("knock", proto.Bool))))

// CreateStream is the request of POST /api/v1/streams.
type CreateStream struct {
	Label       *string         `json:"label"`
	Pin         proto.PinPolicy `json:"pin"`
	PinExplicit bool            `json:"pin_explicit"`
	Knock       bool            `json:"knock"`
	SenderKind  string          `json:"sender_kind"`
}

// Created is what the daemon keeps from the 201.
type Created struct {
	ID           string `json:"id"`
	ViewLinkBase string `json:"view_link_base"`
	PublishToken string `json:"publish_token"`
}

var createdV = proto.Obj(proto.Req("id", proto.VId22), proto.Req("view_link_base", proto.Str(func(s string) bool { return len(s) < 1024 }, "a link base")),
	proto.Req("publish_token", proto.VToken43))

const (
	smallBody = 64 << 10
	bigBody   = 8 << 20
)

func (c *Client) do(ctx context.Context, method, path string, body any, limit int64) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return nil, 0, proto.E("internal", "bad request")
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, proto.E("server_unreachable", "the server can't be reached")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, proto.E("server_unreachable", "the server's reply was cut off")
	}
	if int64(len(b)) > limit {
		return nil, resp.StatusCode, proto.E("payload_too_large", "the server's reply is too large")
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, serverError(resp.StatusCode, b)
	}
	return b, resp.StatusCode, nil
}

var errV = proto.Obj(proto.Req("error", proto.Obj(
	proto.Req("code", proto.Str(func(s string) bool { return codeRE(s) }, "a code")),
	proto.Opt("message", proto.Label(200, 400)),
	proto.Opt("details", func(x any) (any, error) { return x, nil }),
)))

func codeRE(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c == '_') {
			return false
		}
	}
	return true
}

// serverError maps an HTTP error to the shared envelope. 401 means the key
// was rejected; other codes pass through when well-formed.
func serverError(status int, b []byte) *proto.Error {
	if status == http.StatusUnauthorized {
		return proto.E("key_rejected", "the server rejected the owner key")
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if proto.Into(b, errV, &env) == nil {
		e := proto.E(env.Error.Code, "%s", firstNonEmpty(env.Error.Message, env.Error.Code))
		if d := cleanDetails(env.Error.Details); d != nil {
			e.Details = d
		}
		return e
	}
	switch {
	case status == http.StatusNotFound:
		return proto.E("not_found", "not found on the server")
	case status == http.StatusForbidden:
		return proto.E("forbidden_scope", "the key lacks the scope")
	case status >= 500:
		return proto.E("server_unreachable", "server error %d", status)
	}
	return proto.E("internal", "unexpected server status %d", status)
}

// cleanDetails keeps only scalar details; strings are cleaned.
func cleanDetails(d map[string]any) map[string]any {
	if len(d) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range d {
		if !codeRE(k) || len(out) >= 8 {
			continue
		}
		switch t := v.(type) {
		case bool, float64:
			out[k] = t
		case string:
			out[k] = proto.Clean(t, 64, 256)
		}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Settings reads GET /api/v1/settings.
func (c *Client) Settings(ctx context.Context) (Settings, error) {
	var s Settings
	b, _, err := c.do(ctx, "GET", "/api/v1/settings", nil, smallBody)
	if err != nil {
		return s, err
	}
	if err := proto.Into(b, settingsV, &s); err != nil {
		return s, proto.E("server_unreachable", "the server sent malformed settings")
	}
	return s, nil
}

// Create calls POST /api/v1/streams. The view link base must be exactly
// <base>/v#c=<Token43>: the daemon never trusts a server-chosen origin.
func (c *Client) Create(ctx context.Context, req CreateStream) (Created, error) {
	var out Created
	b, _, err := c.do(ctx, "POST", "/api/v1/streams", req, smallBody)
	if err != nil {
		return out, err
	}
	if err := proto.Into(b, createdV, &out); err != nil {
		return out, proto.E("server_unreachable", "the server sent a malformed stream")
	}
	cap, ok := strings.CutPrefix(out.ViewLinkBase, c.Base+"/v#c=")
	if !ok || !proto.IsToken43(cap) {
		return out, proto.E("server_unreachable", "the server's view link doesn't match its base URL")
	}
	return out, nil
}

// Delete calls DELETE /api/v1/streams/{id} (idempotent).
func (c *Client) Delete(ctx context.Context, id string) error {
	_, status, err := c.do(ctx, "DELETE", "/api/v1/streams/"+url.PathEscape(id), nil, smallBody)
	if status == http.StatusNotFound {
		return nil
	}
	return err
}

// Connections reads GET /api/v1/streams/{id}/connections.
func (c *Client) Connections(ctx context.Context, id string) ([]proto.Connection, error) {
	b, _, err := c.do(ctx, "GET", "/api/v1/streams/"+url.PathEscape(id)+"/connections", nil, bigBody)
	if err != nil {
		return nil, err
	}
	var out struct {
		Connections []proto.Connection `json:"connections"`
	}
	v := proto.Obj(proto.Req("connections", proto.Arr(2000, proto.ConnectionV)))
	if err := proto.Into(b, v, &out); err != nil {
		return nil, proto.E("server_unreachable", "the server sent malformed connections")
	}
	return out.Connections, nil
}

// Command is POST /api/v1/streams/{id}/commands; it returns the result
// object unwrapped from {id, ok, result}.
func (c *Client) Command(ctx context.Context, id, cmdID, op string, args any) (map[string]any, error) {
	b, _, err := c.do(ctx, "POST", "/api/v1/streams/"+url.PathEscape(id)+"/commands",
		map[string]any{"id": cmdID, "op": op, "args": args}, smallBody)
	if err != nil {
		return nil, err
	}
	var out struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(b, &out); err != nil || !out.OK || out.Result == nil {
		return nil, proto.E("server_unreachable", "the server sent a malformed command result")
	}
	return out.Result, nil
}

// ValidActionID is the server's command idempotency key format (CONTRACT
// §9.6): 1–64 characters of [A-Za-z0-9_-].
func ValidActionID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Ban runs the server's ban and returns {ip_banned, admission_ids}
// (CONTRACT Appendix A #15). actionID is the caller's id for this user
// action, reused on its retries, and becomes the server command id so a
// retry is deduplicated; "" gets a fresh id (no deduplication).
func (c *Client) Ban(ctx context.Context, streamID, connID, actionID string) (map[string]any, error) {
	if actionID == "" {
		actionID = "ban-" + proto.NewId22()
	}
	res, err := c.Command(ctx, streamID, actionID, "ban", map[string]any{"connection_id": connID})
	if err != nil {
		return nil, err
	}
	var out struct {
		IPBanned     bool     `json:"ip_banned"`
		AdmissionIDs []string `json:"admission_ids"`
	}
	v := proto.Obj(proto.Req("ip_banned", proto.Bool), proto.Req("admission_ids", proto.Arr(500, proto.VId22)))
	if err := proto.IntoValue(any(res), v, &out); err != nil {
		return nil, proto.E("server_unreachable", "the server sent a malformed ban result")
	}
	if out.AdmissionIDs == nil {
		out.AdmissionIDs = []string{}
	}
	return map[string]any{"ip_banned": out.IPBanned, "admission_ids": out.AdmissionIDs}, nil
}

// endReason preserves known reasons and maps future display-only values to unknown.
func endReason(r string) string {
	switch r {
	case "stopped", "window_closed", "expired", "deleted", "state_lost", "browser_failed":
		return r
	}
	return "unknown"
}

// streamEnd reads a Stream snapshot: whether it's terminal, and why.
func streamEnd(b []byte) (string, bool) {
	var st struct {
		Phase     string  `json:"phase"`
		EndReason *string `json:"end_reason"`
	}
	if json.Unmarshal(b, &st) != nil || st.Phase != "ended" {
		return "", false
	}
	r := ""
	if st.EndReason != nil {
		r = *st.EndReason
	}
	return endReason(r), true
}

// StreamEnded reads GET /api/v1/streams/{id} and reports whether the
// stream has ended on the server, and why. A 404 means it's gone:
// "deleted". The daemon calls it whenever it (re)connects to the events,
// since the server doesn't replay an "ended" it sent while disconnected.
func (c *Client) StreamEnded(ctx context.Context, id string) (string, bool, error) {
	b, status, err := c.do(ctx, "GET", "/api/v1/streams/"+url.PathEscape(id), nil, smallBody)
	if status == http.StatusNotFound {
		return "deleted", true, nil
	}
	if err != nil {
		return "", false, err
	}
	r, ended := streamEnd(b)
	return r, ended, nil
}

// maxEvent bounds one server-sent event the daemon reads (its data lines
// together). Other events' data is discarded as it arrives, so a large
// viewers event costs no memory.
const maxEvent = 64 << 10

// Events follows GET /api/v1/streams/{id}/events and calls ended with the
// end reason when the server ends the stream: an "ended" event, or a
// "stream" snapshot whose phase is "ended" (on connect the server sends the
// current stream first and never replays "ended", CONTRACT §9.3). It
// returns when ctx ends, the connection drops, or an event it needs is
// over maxEvent.
func (c *Client) Events(ctx context.Context, id string, ended func(reason string)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+"/api/v1/streams/"+url.PathEscape(id)+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "text/event-stream")
	hc := *c.HTTP
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return proto.E("server_unreachable", "events")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, smallBody))
		return serverError(resp.StatusCode, b)
	}
	return readEvents(resp.Body, ended)
}

// readEvents parses the event stream. Only "stream" and "ended" events are
// kept; their data is bounded by maxEvent in total, and an oversized one
// fails the connection (the caller reconnects and reconciles). The
// "event:" field may come after data lines, so data is kept (bounded)
// until the event's name is known not to matter.
func readEvents(body io.Reader, ended func(reason string)) error {
	br := bufio.NewReaderSize(body, 4096)
	event := ""
	var data bytes.Buffer
	over := false
	relevant := func() bool { return event == "" || event == "stream" || event == "ended" }
	for {
		line, long, err := readLine(br, maxEvent)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("events: %w", err)
		}
		switch {
		case len(line) == 0 && !long:
			if over && (event == "stream" || event == "ended") {
				return proto.E("payload_too_large", "a server event is too large")
			}
			if !over {
				switch event {
				case "ended":
					var d struct {
						Reason string `json:"reason"`
					}
					if json.Unmarshal(data.Bytes(), &d) == nil {
						ended(endReason(d.Reason))
					}
				case "stream":
					if r, ok := streamEnd(data.Bytes()); ok {
						ended(r)
					}
				}
			}
			event, over = "", false
			data.Reset()
		case bytes.HasPrefix(line, []byte("event:")):
			event = strings.TrimSpace(string(line[6:]))
			if !relevant() {
				data.Reset()
			}
		case bytes.HasPrefix(line, []byte("data:")):
			if over || !relevant() {
				continue
			}
			v := bytes.TrimPrefix(line[5:], []byte(" "))
			if long || data.Len()+len(v)+1 > maxEvent {
				over = true
				data.Reset()
				continue
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(v)
		default:
			// Comments, id:, retry:, and the tail of an over-long line.
		}
	}
}

// readLine reads one line without its terminator. A line longer than max
// is consumed and discarded: it returns the first bytes with long set.
func readLine(br *bufio.Reader, max int) ([]byte, bool, error) {
	var buf []byte
	long := false
	for {
		frag, err := br.ReadSlice('\n')
		if !long {
			if len(buf)+len(frag) > max+2 {
				long = true
				if len(buf) < 16 {
					buf = append(buf, frag[:min(len(frag), 16)]...)
				}
			} else {
				buf = append(buf, frag...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return nil, false, err // a final line without a newline is incomplete
		}
		buf = bytes.TrimSuffix(buf, []byte("\n"))
		buf = bytes.TrimSuffix(buf, []byte("\r"))
		return buf, long, nil
	}
}
