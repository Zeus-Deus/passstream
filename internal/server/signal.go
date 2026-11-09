package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/sfu"
	"github.com/passstream/passstream/internal/store"
	"github.com/pion/webrtc/v4"
)

type message struct {
	Replace        bool                       `json:"replace,omitempty"`
	Type           string                     `json:"type"`
	FreshFrames    *uint64                    `json:"freshFrames,omitempty"`
	At             int64                      `json:"at,omitempty"`
	Role           string                     `json:"role,omitempty"`
	ID             string                     `json:"id,omitempty"`
	Credential     string                     `json:"credential,omitempty"`
	Session        string                     `json:"session,omitempty"`
	Protocol       int                        `json:"protocol,omitempty"`
	SenderKind     string                     `json:"sender_kind,omitempty"`
	Instance       string                     `json:"instance,omitempty"`
	Label          *string                    `json:"label,omitempty"`
	SDP            *webrtc.SessionDescription `json:"sdp,omitempty"`
	Candidate      *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
	Resumed        bool                       `json:"resumed,omitempty"`
	ICE            []webrtc.ICEServer         `json:"iceServers,omitempty"`
	StreamID       string                     `json:"stream_id,omitempty"`
	ConnectionID   string                     `json:"connection_id,omitempty"`
	SenderOnline   bool                       `json:"sender_online,omitempty"`
	SenderInstance *string                    `json:"sender_instance,omitempty"`
	Code           string                     `json:"code,omitempty"`
	B              string                     `json:"b,omitempty"`
	To             string                     `json:"to,omitempty"`
	State          json.RawMessage            `json:"state,omitempty"`
	Alert          json.RawMessage            `json:"alert,omitempty"`
	OK             *bool                      `json:"ok,omitempty"`
	Result         json.RawMessage            `json:"result,omitempty"`
	Error          *apiError                  `json:"error,omitempty"`
	// PinHint tells a viewer whether to ask for a PIN before its first
	// handshake. It's advisory: the sender's signed CHALLENGE decides, and a
	// wrong hint costs at most an unneeded PIN screen or one failed try.
	PinHint *hostview.PinPolicy `json:"pin_hint,omitempty"`
}
type signalPeer struct {
	id, stream, role, instance, ip string
	media                          *sfu.Peer
	conn                           *websocket.Conn
	queue                          chan any
	control                        chan any
	peerChanges                    chan struct{}
	drop                           func(string)
	done                           chan struct{}
	once                           sync.Once
	reason                         string
	generation                     uint64
	wireAt                         int64
	wireBytes                      uint64
}

// closeEnded closes a viewer because its stream ended: like "left" for the
// server's bookkeeping, but the socket closes with 1008 "stream_ended", so the
// viewer shows "ended" instead of reconnecting into a refused join (which
// reads as a kick).
func (p *signalPeer) closeEnded() {
	p.once.Do(func() {
		p.reason = "left"
		close(p.done)
		if p.conn != nil {
			go p.conn.Close(websocket.StatusPolicyViolation, "stream_ended")
		}
	})
}

func (p *signalPeer) close(reason string) {
	p.once.Do(func() {
		p.reason = reason
		close(p.done)
		if reason == "resync" && p.drop != nil {
			p.drop("publisher_control_overflow")
			// Count queued messages abandoned by the reset as well as failed enqueues.
			for i, n := 0, len(p.control); i < n; i++ {
				p.drop("publisher_control_drop")
			}
		}
		if p.conn != nil {
			if reason == "resync" {
				// A graceful close carries the reconnect reason without blocking a stream lock.
				go p.conn.Close(websocket.StatusTryAgainLater, "resync")
			} else {
				_ = p.conn.CloseNow()
			}
		}
	})
}
func (p *signalPeer) enqueue(v any) bool {
	select {
	case <-p.done:
		if p.role == "publish" && p.drop != nil {
			p.drop("publisher_control_drop")
		}
		return false
	default:
	}
	q := p.queue
	if p.role == "publish" {
		q = p.control
	}
	select {
	case q <- v:
		return true
	default:
		if p.role == "publish" {
			if p.drop != nil {
				p.drop("publisher_control_drop")
			}
			p.close("resync")
		} else {
			p.close("overflow")
		}
		return false
	}
}

// Called with the stream lock held. Relay backpressure can close many flooding
// viewers; coalesce their lifecycle changes instead of filling the control queue.
func (s *Server) peerClosed(l *liveStream, p *signalPeer) {
	if l.publisher == nil {
		return
	}
	if p.reason == "overflow" {
		select {
		case l.publisher.peerChanges <- struct{}{}:
		default:
		}
		return
	}
	l.publisher.enqueue(map[string]any{"type": "e2ee.peer_closed", "peer": p.id, "reason": p.reason})
}

func timeNowMS() int64 { return time.Now().UnixMilli() }
func (s *Server) signal(w http.ResponseWriter, r *http.Request) {
	ip := s.facts.address(r)
	s.mu.Lock()
	allowed := s.allow("handshake-ip:"+store.IPKey(ip), 2, 20)
	s.mu.Unlock()
	if !allowed {
		fail(w, "rate_limited")
		return
	}
	c, e := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if e != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(256 << 10)
	auth, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var hello message
	e = wsjson.Read(auth, c, &hello)
	cancel()
	if e != nil || hello.Type != "join" {
		return
	}
	reject := func(code string) {
		ctx, cc := context.WithTimeout(r.Context(), time.Second)
		defer cc()
		_ = wsjson.Write(ctx, c, map[string]string{"type": "error", "code": code})
	}
	if hello.Protocol != 1 {
		reject("upgrade_required")
		return
	}
	if hello.Role == "publish" && (!id22.MatchString(hello.Instance) || (hello.SenderKind != "local" && hello.SenderKind != "browser")) {
		reject("unauthorized")
		return
	}
	credential := hello.Credential
	if hello.Session != "" {
		credential = hello.Session
	}
	id, e := s.Store.Admit(hello.ID, hello.Role, credential)
	if e != nil {
		if errors.Is(e, store.ErrEnded) {
			reject("stream_ended")
		} else {
			reject("unauthorized")
		}
		return
	}
	st, e := s.Store.Get(id)
	if e != nil {
		return
	}
	l := s.runtime(id)
	defer s.release(l)
	l.mu.Lock()
	st, e = s.Store.Get(id)
	if e != nil || st.EndedAt != nil || st.ExpiresAt != nil && *st.ExpiresAt <= timeNowMS() {
		l.mu.Unlock()
		reject("stream_ended")
		return
	}
	if !s.permit("handshake-stream:"+id, 10, 50) || hello.Role == "view" && s.Store.Banned(id, ip) {
		l.mu.Unlock()
		reject("unauthorized")
		return
	}
	count := 0
	for _, p := range l.sockets {
		if p.role == "view" {
			count++
		}
	}
	if hello.Role == "view" && uint64(count) >= st.MaxViewers {
		l.mu.Unlock()
		reject("unauthorized")
		return
	}
	token := hello.Session
	if token == "" {
		token, e = s.Store.Session(id, hello.Role)
		if e != nil {
			l.mu.Unlock()
			return
		}
	}
	s.mu.Lock()
	media := s.peers[store.Hash(token)]
	resumed := !hello.Replace && media != nil && media.PC.ConnectionState() != webrtc.PeerConnectionStateClosed && media.PC.ConnectionState() != webrtc.PeerConnectionStateFailed
	if !resumed {
		previous := media
		if media != nil {
			media.Close()
		}
		media, e = s.Media.Peer(id, hello.Role == "publish")
		if e != nil {
			s.mu.Unlock()
			l.mu.Unlock()
			return
		}
		media.InheritPath(previous)
		s.peers[store.Hash(token)] = media
	}
	s.mu.Unlock()
	p := &signalPeer{id: "c_" + store.ID(), stream: id, role: hello.Role, instance: hello.Instance, ip: ip, media: media, conn: c, drop: s.drop, queue: make(chan any, 64), control: make(chan any, 64), peerChanges: make(chan struct{}, 1), done: make(chan struct{}), generation: media.SignalGeneration.Add(1)}
	for _, old := range l.sockets {
		if old.media == media {
			old.close("left")
		}
	}
	// Replacement publishers are fenced by the media engine and the control channel together.
	if p.role == "publish" {
		if l.publisher != nil {
			l.publisher.close("left")
		}
		l.publisher = p
		st.Sender.Kind = hello.SenderKind
		_ = s.Store.SaveStream(st)
	} else {
		now := timeNowMS()
		ua := clean(r.UserAgent(), 256, 256)
		label := hello.Label
		if label != nil {
			x := clean(*label, 64, 256)
			label = &x
		}
		l.conns[p.id] = hostview.Connection{ConnectionID: p.id, JoinedAt: now, IP: &ip, Geo: s.facts.geo(ip), UserAgent: &ua, Device: device(r), Reported: hostview.Reported{Label: label}}
	}
	l.sockets[p.id] = p
	var instance *string
	if l.publisher != nil {
		x := l.publisher.instance
		instance = &x
	}
	joined := message{Type: "joined", StreamID: id, ConnectionID: p.id, Session: token, Resumed: resumed, SenderOnline: l.publisher != nil, SenderInstance: instance, ICE: s.iceServers()}
	if p.role == "view" {
		joined.PinHint = pinHint(st, l.state)
	}
	var bootstrap []any
	if p.role == "publish" {
		bootstrap = append(bootstrap, joined)
		peers := []string{}
		for _, v := range l.sockets {
			if v.role == "view" {
				select {
				case <-v.done:
					continue
				default:
				}
				peers = append(peers, v.id)
				v.enqueue(map[string]any{"type": "e2ee.sender_back", "instance": p.instance})
			}
		}
		sort.Strings(peers)
		bootstrap = append(bootstrap, map[string]any{"type": "e2ee.peers", "peers": peers})
		l.expireControls(time.Now())
		ids := make([]string, 0, len(l.pending))
		for id := range l.pending {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			bootstrap = append(bootstrap, l.pending[id])
		}
	} else {
		p.enqueue(joined)
	}
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		p.close("left")
		delete(l.sockets, p.id)
		if p.role == "publish" {
			if l.publisher == p {
				l.publisher = nil
				for _, v := range l.sockets {
					if v.role == "view" {
						v.enqueue(map[string]any{"type": "e2ee.sender_away"})
					}
				}
			}
		} else {
			conn := l.conns[p.id]
			if p.reason != "left" {
				p.media.Close()
			}
			now := timeNowMS()
			l.recentIPs[p.id] = recentIP{ip: p.ip, until: now + hostview.WindowMS}
			conn.LeftAt = &now
			cfg, _ := s.Store.Settings()
			if !cfg.Retention.KeepIP && !s.cfg.KeepIP {
				conn.IP = nil
			}
			l.conns[p.id] = conn
			s.peerClosed(l, p)
		}
	}()
	go func() {
		for {
			select {
			case <-p.done:
				return
			case <-s.done:
				p.close("timeout")
				return
			default:
			}
			var v any
			if len(bootstrap) > 0 {
				// Replay directly so a ban larger than the queue can recover in one reconnect.
				v, bootstrap = bootstrap[0], bootstrap[1:]
			} else {
				// Prefer lifecycle/control traffic even while relay traffic is queued.
				select {
				case v = <-p.control:
				default:
					select {
					case <-p.done:
						return
					case <-s.done:
						p.close("timeout")
						return
					case v = <-p.control:
					case <-p.peerChanges:
						l.mu.Lock()
						peers := []string{}
						for _, peer := range l.sockets {
							if peer.role != "view" {
								continue
							}
							select {
							case <-peer.done:
								continue
							default:
							}
							peers = append(peers, peer.id)
						}
						sort.Strings(peers)
						l.mu.Unlock()
						v = map[string]any{"type": "e2ee.peers", "peers": peers}
					case v = <-p.queue:
					}
				}
			}
			{
				ctx, cc := context.WithTimeout(r.Context(), 3*time.Second)
				e := wsjson.Write(ctx, c, v)
				cc()
				if e != nil {
					p.close("timeout")
					return
				}
				if m, ok := v.(message); ok && m.Type == "joined" {
					media.Admit() // Open forwarding only after the accepted re-join was sent.
				}
				if value, ok := v.(map[string]any); ok && value["type"] == "stream.ended" {
					p.close("left")
					return
				}
			}
		}
	}()
	media.SetObserver(func(hidden *bool) {
		if p.role != "view" {
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if current := l.sockets[p.id]; current != p {
			return
		}
		c := l.conns[p.id]
		c.Reported.TabHidden = hidden
		l.conns[p.id] = s.transportFacts(p, c)
	}, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return s.permit("pli-ip:"+store.IPKey(p.ip), 2, 4) && s.permit("pli-stream:"+p.stream, 5, 10)
	})
	media.PC.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			p.enqueue(message{Type: "candidate", Candidate: func() *webrtc.ICECandidateInit { x := candidate.ToJSON(); return &x }()})
		}
	})
	for {
		var msg message
		_, raw, readErr := c.Read(r.Context())
		e = readErr
		if e == nil {
			e = json.Unmarshal(raw, &msg)
		}
		if msg.Type == "e2ee" && len(raw) > 6<<10 {
			s.drop("relay_size")
			continue
		}
		if e != nil {
			return
		}
		media.SignalMu.Lock()
		if media.SignalGeneration.Load() != p.generation {
			media.SignalMu.Unlock()
			return
		}
		err := s.handleSignal(p, msg)
		media.SignalMu.Unlock()
		if err != nil {
			return
		}
	}
}
func (s *Server) handleSignal(p *signalPeer, msg message) error {
	switch msg.Type {
	case "ping":
		p.enqueue(signalPong(msg, p.media.ReportDecoded, p.media.RefreshTransitMS))
	case "offer":
		if msg.SDP == nil {
			return errors.New("missing SDP")
		}
		if e := sfu.ValidateVideoOffer(*msg.SDP); e != nil {
			p.enqueue(message{Type: "error", Code: "unsupported_codec"})
			return nil
		}
		if e := p.media.PC.SetRemoteDescription(*msg.SDP); e != nil {
			return e
		}
		answer, e := p.media.PC.CreateAnswer(nil)
		if e != nil {
			return e
		}
		if e = p.media.PC.SetLocalDescription(answer); e != nil {
			return e
		}
		p.enqueue(message{Type: "answer", SDP: &answer})
	case "candidate":
		if msg.Candidate != nil {
			_ = p.media.PC.AddICECandidate(*msg.Candidate)
		}
	case "leave":
		p.media.Close()
		return errors.New("left")
	case "e2ee":
		if len(msg.B) > 5462 {
			s.drop("relay_size")
			return nil
		}
		b, e := base64.RawURLEncoding.Strict().DecodeString(msg.B)
		if e != nil || len(b) > 4096 || len(b) == 0 {
			s.drop("relay_invalid")
			return nil
		}
		l := s.runtime(p.stream)
		defer s.release(l)
		l.mu.Lock()
		defer l.mu.Unlock()
		if relayAnnotation(b) {
			s.relayAnnotationLocked(l, p, msg)
			return nil
		}
		ipRate, ipBurst := float64(64), float64(128)
		if p.role == "publish" {
			ipRate = 2048
			ipBurst = 4096
		}
		if !s.permit("relay-ip:"+p.role+":"+store.IPKey(p.ip), ipRate, ipBurst) || !s.permit("relay-stream:"+p.stream, 512, 1024) || (p.role == "view" && !s.permit("relay-peer:"+p.id, 16, 32)) {
			s.drop("relay_rate")
			return nil
		}
		if p.role == "view" {
			if l.publisher != nil {

				if len(l.publisher.queue) >= cap(l.publisher.queue)/2 {
					s.drop("relay_queue")
					p.close("overflow") // backpressure affects only the flooding viewer
				} else {
					select {
					case l.publisher.queue <- map[string]string{"type": "e2ee", "peer": p.id, "b": msg.B}:
					default:
						s.drop("relay_queue")
						p.close("overflow")
					}
				}
			}
		} else if l.publisher == p {
			if to := l.sockets[msg.To]; to != nil && to.role == "view" {
				to.enqueue(map[string]string{"type": "e2ee", "b": msg.B})
			}
		}
	case "e2ee.close":
		l := s.runtime(p.stream)
		defer s.release(l)
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.publisher == p {
			if to := l.sockets[msg.To]; to != nil && to.role == "view" {
				to.media.Close()
				to.close("kicked")
			}
		}
	case "sender.state":
		if p.role != "publish" {
			return nil
		}
		state, e := parseState(msg.State)
		if e != nil || state.StreamID != p.stream || state.Instance != p.instance {
			return nil
		}
		l := s.runtime(p.stream)
		defer s.release(l)
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.publisher != p || !s.permit("state:"+p.stream, 4, 4) || l.state != nil && l.state.Instance == state.Instance && state.Seq <= l.state.Seq {
			return nil
		}
		prior := l.state
		now := timeNowMS()
		stCheck, e := s.Store.Get(p.stream)
		if e != nil || stCheck.EndedAt != nil {
			return nil
		}
		if e = s.Store.Put("sender:"+p.stream, savedState{State: &state, At: &now}); e != nil {
			return nil
		}
		l.state = &state
		l.stateAt = &now
		st, e := s.Store.Get(p.stream)
		if e != nil {
			return nil
		}
		if state.Phase == "ended" {
			_ = s.endStreamLocked(p.stream, senderEndReason(state.EndReason), l)
		}
		st, _ = s.Store.Get(p.stream)
		st, e = s.build(st, l)
		if e == nil {
			_ = s.Store.SaveStream(st)
		}
		s.stateAlerts(l, prior, &state)
	case "control.result":
		if p.role != "publish" || msg.OK == nil {
			return nil
		}
		l := s.runtime(p.stream)
		defer s.release(l)
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.publisher != p {
			return nil
		}

		if !*msg.OK && msg.Error == nil {
			return nil
		}
		resp := commandResponse{200, map[string]any{"id": msg.ID, "ok": true, "result": json.RawMessage(msg.Result)}}
		if !*msg.OK {
			resp = commandResponse{errorStatus(msg.Error.Code), map[string]any{"error": msg.Error}}
		}
		if !resp.retryable() {
			delete(l.pending, msg.ID)
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		if c := s.inflight[msg.ID]; c != nil && c.running && c.stream == p.stream {
			select {
			case c.attempt.result <- resp:
			default:
			}
		}

	case "alert":
		if p.role == "publish" {
			l := s.runtime(p.stream)
			defer s.release(l)
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.publisher == p && s.permit("alert:"+p.stream, 4, 8) {
				if a := parseAlert(msg.Alert); a != nil {
					s.broadcast(l, event{"alert", a})
				}
			}
		}
	}
	return nil
}
func (s *Server) stateAlerts(l *liveStream, old, next *hostview.SenderState) {
	oldV := map[string]bool{}
	oldK := map[string]bool{}
	if old != nil {
		for _, a := range old.Admissions {
			oldV[a.ViewerID] = true
		}
		for _, k := range old.Knocks {
			oldK[k.KnockID] = true
		}
	}
	newV := map[string]bool{}
	for _, a := range next.Admissions {
		newV[a.ViewerID] = true
	}
	joined, left := []string{}, []string{}
	for id := range newV {
		if !oldV[id] {
			joined = append(joined, id)
		}
	}
	for id := range oldV {
		if !newV[id] {
			left = append(left, id)
		}
	}
	sort.Strings(joined)
	sort.Strings(left)
	if len(joined) > 0 {
		s.broadcast(l, event{"alert", map[string]any{"kind": "joined", "viewer_ids": joined}})
	}
	if len(left) > 0 {
		s.broadcast(l, event{"alert", map[string]any{"kind": "left", "viewer_ids": left}})
	}
	for _, k := range next.Knocks {
		if !oldK[k.KnockID] {
			peer := l.sockets[k.PeerID]
			if peer != nil && (!s.permit("knock-ip:"+store.IPKey(peer.ip), 1, 5) || !s.permit("knock-stream:"+peer.stream, 5, 50)) {
				continue
			}
			s.broadcast(l, event{"alert", map[string]any{"kind": "knock", "knock_id": k.KnockID, "row_code": k.RowCode}})
		}
	}
}

func senderEndReason(reason string) string {
	switch reason {
	case "stopped", "window_closed", "deleted", "state_lost", "expired", "browser_failed", "unknown":
		return reason
	}
	return "unknown"
}

// pinHint is the PIN policy a viewer should expect: the sender's latest
// report, else the policy declared at creation.
func pinHint(st store.Stream, state *hostview.SenderState) *hostview.PinPolicy {
	if state != nil {
		pin := state.Access.Pin
		if !pin.Required {
			return &hostview.PinPolicy{Kind: "none"}
		}
		return &hostview.PinPolicy{Kind: pin.Kind, Length: pin.Length}
	}
	hint := st.Declared.Pin
	return &hint
}

// The signaling and SCTP channels feed the same decoder counter. Signaling
// samples never renew media liveness or change admission.
func signalPong(msg message, report func(uint64, int64), refresh func() int64) map[string]any {
	if msg.FreshFrames != nil {
		report(*msg.FreshFrames, msg.At)
	}
	return map[string]any{"type": "pong", "refreshMs": refresh()}
}

// relayAnnotation reports whether an E2EE payload is an annotation message
// (PROTOCOL.md §20: version 1, type 0x09 or 0x0A). The type byte travels in
// the clear like every library message's; nothing else is parsed.
func relayAnnotation(b []byte) bool {
	return len(b) >= 2 && b[0] == 1 && (b[1] == 0x09 || b[1] == 0x0a)
}

// Annotation relay budgets, separate from the admission/rekey/ACK budgets so
// drawing can never use their capacity (CONTRACT §8.2).
const (
	annotationQueueShare = 4 // an annotation never fills more than 1/4 of a queue
)

// relayAnnotationLocked relays one annotation envelope under its own rate
// limits. It drops (and counts) rather than queueing behind control traffic,
// and never closes a peer for it. Called with l.mu held.
func (s *Server) relayAnnotationLocked(l *liveStream, p *signalPeer, msg message) {
	ipRate, ipBurst, streamRate, streamBurst := float64(32), float64(64), float64(128), float64(256)
	if p.role == "publish" {
		ipRate, ipBurst, streamRate, streamBurst = 512, 1024, 256, 512
	}
	if !s.permit("relay-ann-ip:"+p.role+":"+store.IPKey(p.ip), ipRate, ipBurst) ||
		!s.permit("relay-ann-stream:"+p.role+":"+p.stream, streamRate, streamBurst) ||
		(p.role == "view" && !s.permit("relay-ann-peer:"+p.id, 10, 20)) {
		s.drop("relay_annotation_rate")
		return
	}
	var q chan any
	var out any
	if p.role == "view" {
		if l.publisher == nil {
			return
		}
		q, out = l.publisher.queue, map[string]string{"type": "e2ee", "peer": p.id, "b": msg.B}
	} else if l.publisher == p {
		to := l.sockets[msg.To]
		if to == nil || to.role != "view" {
			return
		}
		q, out = to.queue, map[string]string{"type": "e2ee", "b": msg.B}
	} else {
		return
	}
	if len(q) >= cap(q)/annotationQueueShare {
		s.drop("relay_annotation_queue")
		return
	}
	select {
	case q <- out:
	default:
		s.drop("relay_annotation_queue")
	}
}
