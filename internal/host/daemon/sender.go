package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/passstream/passstream/internal/host/localauth"
	"github.com/passstream/passstream/internal/host/proto"
	"github.com/passstream/passstream/internal/host/serverapi"
	"github.com/passstream/passstream/internal/host/state"
)

const (
	maxPageMessage = 8 << 20
	maxStateBytes  = 256 << 10
	pageQueue      = 128
)

func (d *Daemon) origin() string { return fmt.Sprintf("http://127.0.0.1:%d", d.cfg.Port) }
func (d *Daemon) host() string   { return fmt.Sprintf("127.0.0.1:%d", d.cfg.Port) }

// senderHandler serves the local SPA, authenticated sender socket and owner API.
func (d *Daemon) senderHandler() http.Handler {
	var files http.Handler
	if d.cfg.Assets != nil {
		files = http.FileServer(http.FS(d.cfg.Assets))
	}
	// 'wasm-unsafe-eval' (WASM compilation only, not JS eval) and cross-origin
	// isolation are for safe mode's on-page text recognition (threaded WASM).
	csp := "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; media-src 'self' blob:; " +
		"connect-src 'self' ws://" + d.host()
	if d.cfg.ServerURL != "" {
		csp += " " + d.cfg.ServerURL + " " + strings.TrimSuffix(serverapi.SignalURL(d.cfg.ServerURL), "/signal")
	}
	csp += "; worker-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Embedder-Policy", "require-corp")
		h.Set("Content-Security-Policy", csp)
		if r.Method != http.MethodGet && r.URL.Path != "/local/v1/features" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Check 2: exact Host (DNS rebinding).
		if r.Host != d.host() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Check 1: the peer is our uid (skipped only in the launch-file mode).
		if !d.peerOK(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		switch {
		case r.URL.Path == "/local/v1/frame" || r.URL.Path == "/local/v1/features":
			d.serveFeatures(w, r)
		case r.URL.Path == "/local/v1/sender":
			// Check 3: exact Origin; a missing or null Origin is refused.
			if r.Header.Get("Origin") != d.origin() || len(r.Header.Values("Origin")) != 1 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			d.serveSenderWS(w, r)
		case r.URL.Path == "/sender" || r.URL.Path == "/owner":
			d.serveIndex(w)
		case strings.HasPrefix(r.URL.Path, "/assets/") && files != nil:
			files.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (d *Daemon) serveIndex(w http.ResponseWriter) {
	var b []byte
	var err error = fs.ErrNotExist
	if d.cfg.Assets != nil {
		b, err = fs.ReadFile(d.cfg.Assets, "index.html")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("<h1>PassStream web is not built</h1>"))
		return
	}
	// Only the locally served SPA exposes the token-entry owner page. The
	// remote server's SPA fallback must never ask for local credentials.
	b = bytes.Replace(b, []byte("<head>"), []byte(`<head><meta name="passstream-local" content="1">`), 1)
	w.Write(b)
}

// peerOK is check 1: the client end of this connection, found by the
// reversed tuple in /proc/net/tcp*, belongs to our uid. Fails closed.
func (d *Daemon) peerOK(r *http.Request) bool {
	if d.cfg.PeerCheck == PeerLaunchFile {
		return true
	}
	client, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	la, ok := localAddr(r).(*net.TCPAddr)
	if !ok {
		return false
	}
	server := la.AddrPort()
	root := d.cfg.PeerProc
	if root == "" {
		root = d.cfg.Proc
	}
	uid, err := root.PeerUID(client, server)
	if err != nil || uid != d.uid {
		d.logf("host: refused a sender connection (peer uid check)")
		return false
	}
	return true
}

// pageConn is one sender WebSocket.
type pageConn struct {
	ws       *websocket.Conn
	out      chan []byte
	done     chan struct{}
	once     sync.Once
	instance string
}

func (p *pageConn) send(b []byte) {
	select {
	case <-p.done:
	case p.out <- b:
	default:
		p.close()
	}
}

func (p *pageConn) close() {
	p.once.Do(func() {
		close(p.done)
		p.ws.Close(websocket.StatusNormalClosure, "")
	})
}

func (p *pageConn) writer() {
	for {
		select {
		case <-p.done:
			return
		case b := <-p.out:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := p.ws.Write(ctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				p.close()
				return
			}
		}
	}
}

func (d *Daemon) serveSenderWS(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{d.host()}})
	if err != nil {
		return
	}
	ws.SetReadLimit(maxPageMessage)
	p := &pageConn{ws: ws, out: make(chan []byte, pageQueue), done: make(chan struct{})}
	go p.writer()
	defer p.close()
	st, kind, ok := d.authenticate(p)
	if !ok {
		return
	}
	d.pageLoop(st, p, kind)
}

// fail sends an error and closes, after the queued messages.
func (d *Daemon) failPage(p *pageConn, code string) {
	b, _ := json.Marshal(map[string]any{"type": "error", "code": code})
	p.send(b)
	time.Sleep(20 * time.Millisecond)
	deadline := time.Now().Add(time.Second)
	for len(p.out) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	p.close()
}

func (d *Daemon) read(p *pageConn, timeout time.Duration) (map[string]any, []byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	go func() {
		select {
		case <-p.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	typ, b, err := p.ws.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		return nil, nil, false
	}
	v, err := proto.DecodeJSON(b)
	if err != nil {
		return nil, nil, false
	}
	m, ok := v.(map[string]any)
	return m, b, ok
}

// authenticate runs the mutual HMAC exchange (CONTRACT §11.2), daemon
// first. It returns the stream and the key kind ("launch"|"attachment").
func (d *Daemon) authenticate(p *pageConn) (*stream, string, bool) {
	m, _, ok := d.read(p, d.cfg.AuthTimeout)
	if !ok || m["type"] != "hello" {
		return nil, "", false
	}
	if pr, _ := m["protocol"].(float64); pr != proto.Protocol {
		d.failPage(p, "protocol_mismatch")
		return nil, "", false
	}
	kid, _ := m["key_id"].(string)
	np, _ := m["nonce"].(string)
	keyID, ok1 := proto.Decode(kid, 16)
	nonceP, ok2 := proto.Decode(np, 32)
	if !ok1 || !ok2 {
		d.failPage(p, "auth_failed")
		return nil, "", false
	}
	d.mu.Lock()
	st := d.st
	var key []byte
	kind := ""
	code := "auth_failed"
	if st != nil && !st.ending {
		switch {
		case st.launch != nil && st.launch.id == kid:
			if time.Since(st.launch.minted) > d.cfg.LaunchTTL {
				d.retireLaunchLocked(st, "launch_expired")
				code = "launch_expired"
			} else {
				key, kind = st.launch.key, "launch"
			}
		case st.rec.Attachment != nil && st.rec.Attachment.KeyID == kid:
			key, _ = proto.Decode(st.rec.Attachment.Key, 32)
			kind = "attachment"
		case st.retired[kid] != "":
			code = st.retired[kid]
		}
	}
	d.mu.Unlock()
	if key == nil {
		d.failPage(p, code)
		return nil, "", false
	}
	nonceD := proto.RandBytes(32)
	proofD := localauth.Proof(key, localauth.RoleDaemon, keyID, nonceP, nonceD)
	b, _ := json.Marshal(map[string]any{"type": "challenge", "nonce": proto.Encode(nonceD), "proof": proto.Encode(proofD)})
	p.send(b)
	// One attempt, within the auth timeout of the challenge, on this
	// connection only: nonce_d never leaves this function.
	m, _, ok = d.read(p, d.cfg.AuthTimeout)
	if !ok {
		return nil, "", false
	}
	pp, _ := m["proof"].(string)
	proofP, okp := proto.Decode(pp, 32)
	if m["type"] != "auth" || !okp || !localauth.Verify(key, localauth.RolePage, keyID, nonceP, nonceD, proofP) {
		d.logf("host: sender authentication failed")
		d.failPage(p, "auth_failed")
		return nil, "", false
	}
	// Serialise completion per stream: the last to complete is the attachment.
	select {
	case st.exchange <- struct{}{}:
	case <-p.done:
		return nil, "", false
	}
	defer func() { <-st.exchange }()
	d.mu.Lock()
	if d.st != st || st.ending {
		d.mu.Unlock()
		d.failPage(p, "auth_failed")
		return nil, "", false
	}
	if kind == "launch" && (st.launch == nil || st.launch.id != kid) {
		// Retired while this exchange ran.
		c := st.retired[kid]
		d.mu.Unlock()
		if c == "" {
			c = "launch_consumed"
		}
		d.failPage(p, c)
		return nil, "", false
	}
	// Init carries the latches: only once they're on disk, like a command
	// or a state_ack. A failed write drops the socket without an error
	// code, which the page treats as a lost connection and retries; the
	// current page, if any, stays attached.
	if st.dirty && d.saveLocked(st) != nil {
		d.mu.Unlock()
		d.logf("host: sender page refused until the recovery record is written")
		p.close()
		return nil, "", false
	}
	var attach *state.Attachment
	if kind == "launch" {
		if st.rec.Attachment == nil {
			idb := proto.RandBytes(16)
			k := proto.RandBytes(32)
			st.rec.Attachment = &state.Attachment{KeyID: proto.Encode(idb), Key: proto.Encode(k)}
			if err := d.saveLocked(st); err != nil {
				st.rec.Attachment = nil
				d.mu.Unlock()
				d.failPage(p, "auth_failed")
				return nil, "", false
			}
		}
		attach = st.rec.Attachment
		st.att = attHandover
	} else {
		d.retireLaunchLocked(st, "launch_consumed")
		st.att = attAttached
		st.rec.Attachment.AttachedAt = nowMs()
	}
	old := st.page
	st.captures.Close()
	d.refusePublishLocked(st, "refused")
	st.page = p
	if old != nil {
		// Commands sent to the superseded page can't be answered by it any
		// more: their callers get sender_timeout (they may have applied).
		for id, ch := range st.pending {
			close(ch)
			delete(st.pending, id)
		}
	}
	if st.detachTimer != nil {
		st.detachTimer.Stop()
		st.detachTimer = nil
	}
	init := d.initLocked(st, attach)
	d.rejoinLocked(st)
	d.mu.Unlock()
	if old != nil {
		eb, _ := json.Marshal(map[string]any{"type": "error", "code": "superseded"})
		old.send(eb)
		go func() { time.Sleep(50 * time.Millisecond); old.close() }()
	}
	ib, _ := json.Marshal(init)
	p.send(ib)
	d.logf("host: sender page authenticated (%s key)", kind)
	return st, kind, true
}

func (d *Daemon) initLocked(st *stream, attach *state.Attachment) map[string]any {
	init := map[string]any{
		"type":       "init",
		"server_url": st.rec.ServerURL,
		"signal_url": serverapi.SignalURL(st.rec.ServerURL),
		"stream":     map[string]any{"id": st.rec.StreamID, "publish_token": st.rec.PublishToken, "view_link_base": st.rec.ViewLinkBase},
		"policy": map[string]any{"pin_policy": st.rec.Policy, "pin": st.explicitPin, "knock": st.rec.Knock,
			"mode": st.rec.Mode, "safe": st.rec.Safe},
		"latches":           st.rec.Latches,
		"active_stream_ids": []string{st.rec.StreamID},
	}
	if attach != nil {
		init["attachment"] = map[string]any{"key_id": attach.KeyID, "key": attach.Key}
	}
	return init
}

// pageResult is a command result from the page.
type pageResult struct {
	data map[string]any
	err  *proto.Error
}

var resultErrV = proto.Obj(
	proto.Req("code", proto.Str(func(s string) bool {
		return len(s) > 0 && len(s) <= 64 && strings.Trim(s, "abcdefghijklmnopqrstuvwxyz_") == ""
	}, "a code")),
	proto.Opt("message", proto.Label(200, 400)),
	proto.Opt("details", func(x any) (any, error) {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, nil
		}
		out := map[string]any{}
		for k, v := range m {
			switch t := v.(type) {
			case bool, float64:
				out[k] = t
			case string:
				out[k] = proto.Clean(t, 64, 256)
			}
		}
		return out, nil
	}),
)

func (d *Daemon) pageLoop(st *stream, p *pageConn, kind string) {
	defer d.pageGone(st, p)
	for {
		m, raw, ok := d.read(p, 365*24*time.Hour)
		if !ok {
			return
		}
		// Only the current page of the current stream speaks for it: a
		// superseded socket (in its 50 ms grace) or one of an ended stream
		// is ignored, whatever it sends. Each handler checks again under
		// the lock where it acts.
		if !d.isCurrent(st, p) {
			continue
		}
		switch m["type"] {
		case "publish_decision":
			d.publishDecision(st, p, m)
		case "capture", "recording":
			d.onCapture(st, p, m)
		case "owner_action":
			id, _ := m["id"].(string)
			op, _ := m["op"].(string)
			args, _ := m["args"].(map[string]any)
			if len(id) > 100 || !isFeatureOp(op) || op == "capture.auth" {
				continue
			}
			go d.ownerFeature(st, p, id, op, args)
		case "init_ack":
			d.mu.Lock()
			if d.st == st && st.page == p && st.att == attHandover {
				d.retireLaunchLocked(st, "launch_consumed")
				st.att = attAttached
				st.rec.Attachment.AttachedAt = nowMs()
				d.saveLocked(st)
				d.rejoinLocked(st)
			}
			d.mu.Unlock()
		case "state":
			if len(raw) > maxStateBytes+64 {
				continue // over the limit: dropped (CONTRACT §1.3)
			}
			d.onState(st, p, m["state"])
		case "secrets":
			d.onSecrets(st, p, m)
		case "alert":
			var a map[string]any
			if proto.IntoValue(m["alert"], proto.AlertV, &a) == nil {
				d.mu.Lock()
				if d.st == st && st.page == p {
					d.broadcastLocked("alert", a, false)
				}
				d.mu.Unlock()
			}
		case "latch_cleared":
			d.mu.Lock()
			if d.st == st && st.page == p {
				switch m["latch"] {
				case "locked":
					st.rec.Latches.Locked = false
				case "paused":
					st.rec.Latches.Paused = false
				case "frozen":
					st.rec.Latches.Frozen = false
				}
				d.saveLocked(st)
				d.scheduleLocked()
			}
			d.mu.Unlock()
		case "result":
			id, _ := m["id"].(string)
			var res pageResult
			if m["ok"] == true {
				res.data, _ = m["data"].(map[string]any)
				if res.data == nil {
					res.data = map[string]any{}
				}
			} else {
				var e proto.Error
				if proto.IntoValue(m["error"], resultErrV, &e) != nil {
					e = proto.Error{Code: "internal", Message: "malformed sender error"}
				}
				if e.Message == "" {
					e.Message = e.Code
				}
				res.err = &e
			}
			d.mu.Lock()
			var ch chan pageResult
			if d.st == st && st.page == p {
				ch = st.pending[id]
				delete(st.pending, id)
			}
			d.mu.Unlock()
			if ch != nil {
				ch <- res
			}
		case "ended":
			reason, _ := m["reason"].(string)
			switch reason {
			case "stopped", "window_closed", "deleted", "state_lost", "expired", "browser_failed":
			default:
				reason = "unknown"
			}
			// Claim termination before pageGone can detach this page.
			d.endFrom(st, p, reason)
			return
		}
	}
}

func (d *Daemon) isCurrent(st *stream, p *pageConn) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.st == st && st.page == p && !st.ending
}

// pageGone handles a dropped page socket (CONTRACT §5.2).
func (d *Daemon) pageGone(st *stream, p *pageConn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st != st || st.page != p || st.ending || d.ctx.Err() != nil {
		return // a daemon that's shutting down leaves the stream to its successor
	}
	st.page = nil
	st.captures.Close()
	d.refusePublishLocked(st, "refused")
	switch st.att {
	case attHandover:
		st.att = attLaunching
	case attAttached:
		st.att = attDetached
		st.detachTimer = time.AfterFunc(d.cfg.DetachGrace, func() { d.detachExpired(st) })
	}
	for id, ch := range st.pending {
		close(ch)
		delete(st.pending, id)
	}
	d.rejoinLocked(st)
}

// onState takes a SenderState: newest copy wins, latches are mirrored
// set-only and fsynced before the state_ack (HOST-CONTROL §5.6).
func (d *Daemon) onState(st *stream, p *pageConn, raw any) {
	var s proto.SenderState
	if err := proto.IntoValue(raw, proto.SenderStateV, &s); err != nil {
		return // dropped and counted
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st != st || st.page != p || s.StreamID != st.rec.StreamID {
		return
	}
	prev := st.state
	if prev != nil && prev.Instance == s.Instance && s.Seq <= prev.Seq {
		return
	}
	if s.Admissions == nil {
		s.Admissions = []proto.Admission{}
	}
	if s.Knocks == nil {
		s.Knocks = []proto.Knock{}
	}
	if s.Ends == nil {
		s.Ends = []proto.End{}
	}
	st.state, st.stateAt = &s, nowMs()
	if s.Access.Latches.Paused || s.Phase != "live" || s.E2EE.Suspended {
		st.captures.Close()
		d.refusePublishLocked(st, "refused")
	} else {
		st.captures.Allow()
		if st.demandPage != p {
			d.updateCaptureLocked(st)
		}
	}
	p.instance = s.Instance
	merged := st.rec.Latches.Or(s.Access.Latches)
	dirty := merged != st.rec.Latches
	st.rec.Latches = merged
	if st.rec.Attachment != nil && st.rec.Attachment.PageInstance != s.Instance {
		st.rec.Attachment.PageInstance = s.Instance
		dirty = true
	}
	if s.Phase == "live" && !st.liveSent {
		st.liveSent = true
		if st.rec.LiveAt == nil {
			t := nowMs()
			st.rec.LiveAt = &t
			dirty = true
		}
		d.broadcastLocked("live", map[string]any{"stream_id": st.rec.StreamID}, false)
	}
	if s.Mode == "text" || s.Mode == "motion" {
		if st.rec.Mode != s.Mode {
			st.rec.Mode = s.Mode
			dirty = true
		}
	}
	if st.rec.Safe != s.Safe {
		st.rec.Safe = s.Safe
		dirty = true
	}
	// state_ack promises the mirror is durable: no ack while the record
	// write fails, including a failure left over from an earlier change
	// (the page stops waiting after 1 s, HOST-CONTROL §5.6).
	if !(dirty || st.dirty) || d.saveLocked(st) == nil {
		ack, _ := json.Marshal(map[string]any{"type": "state_ack", "seq": s.Seq})
		p.send(ack)
	}
	d.diffAlertsLocked(prev, &s)
	d.rejoinLocked(st)
}

// diffAlertsLocked derives joined, left and knock from state diffs (§4.7).
func (d *Daemon) diffAlertsLocked(prev, cur *proto.SenderState) {
	connected := func(s *proto.SenderState) map[string]bool {
		out := map[string]bool{}
		if s != nil {
			for _, a := range s.Admissions {
				if a.Connected {
					out[a.ViewerID] = true
				}
			}
		}
		return out
	}
	before, after := connected(prev), connected(cur)
	var joined, left []string
	for v := range after {
		if !before[v] {
			joined = append(joined, v)
		}
	}
	for v := range before {
		if !after[v] {
			left = append(left, v)
		}
	}
	if len(joined) > 0 {
		d.broadcastLocked("alert", map[string]any{"kind": "joined", "viewer_ids": joined}, false)
	}
	if len(left) > 0 {
		d.broadcastLocked("alert", map[string]any{"kind": "left", "viewer_ids": left}, false)
	}
	known := map[string]bool{}
	if prev != nil {
		for _, k := range prev.Knocks {
			known[k.KnockID] = true
		}
	}
	for _, k := range cur.Knocks {
		if !known[k.KnockID] {
			d.broadcastLocked("alert", map[string]any{"kind": "knock", "knock_id": k.KnockID, "row_code": k.RowCode}, false)
		}
	}
}

// onSecrets stores the PIN and link in memory only.
func (d *Daemon) onSecrets(st *stream, p *pageConn, m map[string]any) {
	pin, ok1 := optString(m["pin"])
	pending, ok2 := optString(m["pending_pin"])
	link, ok3 := m["link"].(string)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st != st || st.page != p || !ok1 || !ok2 || !ok3 || !validLink(link, st.rec.ViewLinkBase) {
		return
	}
	first := !st.secrets.have
	st.secrets = secrets{have: true, pin: pin, pending: pending, link: link}
	if first {
		d.broadcastLocked("link_ready", map[string]any{"stream_id": st.rec.StreamID}, false)
	}
	d.scheduleLocked()
}

func optString(v any) (*string, bool) {
	if v == nil {
		return nil, true
	}
	s, ok := v.(string)
	if !ok || s == "" || len(s) > 1024 {
		return nil, false
	}
	return &s, true
}

// validLink requires the canonical form <base>/v#c=<cap>&k=<b64url>&s=<b64url>
// built on the server's view_link_base (CONTRACT §3).
func validLink(link, base string) bool {
	rest, ok := strings.CutPrefix(link, base+"&k=")
	if !ok {
		return false
	}
	k, s, ok := strings.Cut(rest, "&s=")
	return ok && isB64Token(k) && isB64Token(s)
}

func isB64Token(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	_, ok := proto.Decode(s, len(s)*6/8)
	return ok
}
