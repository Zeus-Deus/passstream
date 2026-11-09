package server

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/passstream/passstream/internal/hostview"
	"github.com/passstream/passstream/internal/store"
)

type savedState struct {
	State *hostview.SenderState
	At    *int64
}

type recentIP struct {
	ip    string
	until int64
}
type liveStream struct {
	mu          sync.Mutex
	refs        int       // guarded by Server.mu; pins callers against eviction
	used        time.Time // guarded by Server.mu
	recentIPs   map[string]recentIP
	persisted   string
	ended       bool
	state       *hostview.SenderState
	stateAt     *int64
	mem         hostview.JoinMemory
	conns       map[string]hostview.Connection
	sockets     map[string]*signalPeer
	publisher   *signalPeer
	pending     map[string]controlMessage // unacknowledged restrictions; guarded by mu
	subscribers map[chan event]bool
	list        hostview.ViewerList
}
type event struct {
	Kind string
	Data any
}
type bucket struct {
	tokens float64
	at     time.Time
}

func (s *Server) allow(key string, rate float64, burst float64) bool {
	// Called with mu held. Idle buckets are discarded; the total map is capped.
	now := time.Now()
	b := s.limits[key]
	if b == nil {
		if len(s.limits) >= 16384 {
			for k, b := range s.limits {
				if now.Sub(b.at) > time.Minute {
					delete(s.limits, k)
				}
			}
			if len(s.limits) >= 16384 {
				return false
			}
		}
		b = &bucket{tokens: burst, at: now}
		s.limits[key] = b
	}
	b.tokens += now.Sub(b.at).Seconds() * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// runtime pins the entry. Callers release it after dropping its per-stream lock.
// SQLite reads happen before publishing the entry, never under Server.mu.
func (s *Server) runtime(id string) *liveStream {
	s.mu.Lock()
	l := s.live[id]
	if l != nil {
		l.refs++
		l.used = time.Now()
		s.mu.Unlock()
		return l
	}
	s.mu.Unlock()
	fresh := s.readRuntime(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	l = s.live[id]
	if l == nil {
		l = fresh
		s.live[id] = l
	}
	l.refs++
	l.used = time.Now()
	return l
}
func (s *Server) readRuntime(id string) *liveStream {
	l := &liveStream{conns: map[string]hostview.Connection{}, sockets: map[string]*signalPeer{}, subscribers: map[chan event]bool{}, recentIPs: map[string]recentIP{}, pending: map[string]controlMessage{}}
	_ = s.Store.Read("join:"+id, &l.mem)
	b, _ := json.Marshal(l.mem)
	l.persisted = string(b)
	var saved savedState
	if s.Store.Read("sender:"+id, &saved) == nil {
		l.state = saved.State
		l.stateAt = saved.At
	}
	return l
}
func (s *Server) release(l *liveStream) { s.mu.Lock(); l.refs--; l.used = time.Now(); s.mu.Unlock() }
func (s *Server) unpin(l *liveStream)   { s.mu.Lock(); l.refs--; s.mu.Unlock() }
func (s *Server) permit(key string, rate, burst float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allow(key, rate, burst)
}
func (s *Server) drop(reason string) { s.mu.Lock(); s.dropped[reason]++; s.mu.Unlock() }
func (s *Server) build(v store.Stream, l *liveStream) (store.Stream, error) {
	now := time.Now().UnixMilli()
	l.ended = v.EndedAt != nil
	cfg, e := s.Store.Settings()
	if e != nil {
		return v, e
	}
	cs := []hostview.Connection{}
	for id, c := range l.conns {
		if c.LeftAt != nil {
			if !cfg.Retention.KeepIP && !s.cfg.KeepIP {
				c.IP = nil
				l.conns[id] = c
			}
			if now-*c.LeftAt >= hostview.WindowMS {
				delete(l.conns, id)
				continue
			}
		}
		cs = append(cs, c)
	}
	for id, recent := range l.recentIPs {
		if now >= recent.until {
			delete(l.recentIPs, id)
		}
	}
	list, mem, history := hostview.Join(hostview.JoinInput{Source: "server", Now: now, State: l.state, StateAt: l.stateAt, PublisherOnline: l.publisher != nil, StreamEnded: v.EndedAt != nil, Conns: cs}, l.mem)
	// Raw IP addresses never enter persisted join memory unless explicitly enabled.
	persisted := mem
	if !cfg.Retention.KeepIP && !s.cfg.KeepIP {
		// Decode into a fresh value: unmarshalling over a copy of mem would reuse
		// its backing arrays, and the scrub below would also null the live rows.
		b, _ := json.Marshal(mem)
		persisted = hostview.JoinMemory{}
		_ = json.Unmarshal(b, &persisted)
		for i := range persisted.Rows {
			for j := range persisted.Rows[i].Snapshot.Connections {
				persisted.Rows[i].Snapshot.Connections[j].IP = nil
			}
		}
	}
	for _, h := range history {
		if !cfg.Retention.KeepIP && !s.cfg.KeepIP {
			for i := range h.Connections {
				h.Connections[i].IP = nil
			}
		}
		if e = s.Store.Record("history", v.ID, h.LastAt, h); e != nil {
			return v, e
		}
	}
	b, _ := json.Marshal(persisted)
	if string(b) != l.persisted {
		if e = s.Store.Put("join:"+v.ID, persisted); e != nil {
			return v, e
		}
		l.persisted = string(b)
	}
	l.mem = mem
	list.Meta = map[string]string{"geoip_attribution": geoAttribution}
	l.list = list
	v.Watching = list.Watching
	v.Knocking = list.Knocking
	v.Sender.Online = l.publisher != nil
	if l.state != nil {
		v.Sender.LastSeenAt = l.stateAt
		v.Effective = &store.Effective{Phase: l.state.Phase, Mode: l.state.Mode, Access: l.state.Access, E2EE: l.state.E2EE, Health: l.state.Health}
		if v.EndedAt == nil {
			v.Phase = l.state.Phase
		}
	}
	return v, nil
}
func (s *Server) broadcast(l *liveStream, e event) {
	for ch := range l.subscribers {
		select {
		case ch <- e:
		default:
			delete(l.subscribers, ch)
			close(ch)
		}
	}
}
func (s *Server) endStreamLocked(id, reason string, l *liveStream) error {
	v, e := s.Store.Get(id)
	if e != nil {
		return e
	}
	if v.EndedAt != nil {
		l.ended = true
		return nil
	}
	now := time.Now().UnixMilli()
	v.EndedAt = &now
	v.EndReason = &reason
	v.Phase = "ended"
	l.ended = true
	if e = s.Store.SaveStream(v); e != nil {
		return e
	}
	clear(l.pending)
	_, e = s.Store.DB.Exec("DELETE FROM transport_sessions WHERE stream=?", id)
	if e != nil {
		return e
	}
	if e = s.Store.ClearBans(id); e != nil {
		return e
	}
	if l.publisher != nil {
		wireReason := reason
		if wireReason != "expired" {
			wireReason = "deleted"
		}
		l.publisher.enqueue(map[string]any{"type": "stream.ended", "reason": wireReason})
	}
	for _, p := range l.sockets {
		if p.role == "view" {
			p.closeEnded()
		}
	}
	s.Media.Remove(id)
	v, e = s.build(v, l)
	s.broadcast(l, event{"stream", v})
	s.broadcast(l, event{"ended", map[string]string{"reason": reason}})
	return e
}
func (s *Server) maintain() {
	now := time.Now()
	s.mu.Lock()
	live := make(map[string]*liveStream, len(s.live))
	for id, l := range s.live {
		live[id] = l
		l.refs++
	}
	for k, c := range s.commands {
		if !c.running && now.Sub(c.at) > 10*time.Minute {
			delete(s.commands, k)
		}
	}
	s.mu.Unlock()
	for id, l := range live {
		l.mu.Lock()
		l.expireControls(now)
		active := len(l.sockets) > 0 || len(l.subscribers) > 0
		s.mu.Lock()
		evict := !active && len(l.pending) == 0 && l.refs == 1 && (l.ended || now.Sub(l.used) >= 5*time.Minute)
		s.mu.Unlock()
		if active || len(l.pending) > 0 || evict {
			v, err := s.Store.Get(id)
			if err == nil {
				if v.EndedAt == nil && v.ExpiresAt != nil && *v.ExpiresAt <= now.UnixMilli() {
					_ = s.endStreamLocked(id, "expired", l)
					v, err = s.Store.Get(id)
				}
				if err == nil {
					fresh, err := s.build(v, l)
					if err == nil && active {
						s.broadcast(l, event{"stream", fresh})
						s.broadcast(l, event{"viewers", l.list})
					}
					evict = !active && (evict || v.EndedAt != nil)
				}
			} else {
				evict = !active
			}
		}
		if !active {
			// Ended streams may be evicted immediately; only active entries get periodic I/O.
			s.mu.Lock()
			if l.refs == 1 && evict {
				delete(s.live, id)
			}
			s.mu.Unlock()
		}
		l.mu.Unlock()
		s.unpin(l)
	}
}
