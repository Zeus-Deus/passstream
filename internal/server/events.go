package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	v, l, ok := s.load(w, r)
	if !ok {
		return
	}
	caller := who(r).Key.ID
	s.mu.Lock()
	full := s.subscriberCounts[caller] >= 8 || len(l.subscribers) >= 32
	if !full {
		s.subscriberCounts[caller]++
	}
	s.mu.Unlock()
	if full {
		l.mu.Unlock()
		s.release(l)
		fail(w, "rate_limited")
		return
	}
	ch := make(chan event, 32)
	l.subscribers[ch] = true
	list := l.list
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.subscribers, ch)
		l.mu.Unlock()
		s.mu.Lock()
		s.subscriberCounts[caller]--
		if s.subscriberCounts[caller] == 0 {
			delete(s.subscriberCounts, caller)
		}
		s.mu.Unlock()
		s.release(l)
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	controller := http.NewResponseController(w)
	send := func(kind string, data any) bool {
		b, e := json.Marshal(data)
		if e != nil {
			return false
		}
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, e = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b); e != nil {
			return false
		}
		return controller.Flush() == nil
	}
	if !send("stream", v) || !send("viewers", list) {
		return
	}
	lastView := time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if e.Kind == "viewers" {
				if time.Since(lastView) < time.Second {
					continue
				}
				lastView = time.Now()
			}
			if !s.stillAuthorized(r) {
				return
			}
			if !send(e.Kind, e.Data) {
				return
			}
		case <-ticker.C:
			if !s.stillAuthorized(r) {
				return
			}
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, e := fmt.Fprint(w, ": heartbeat\n\n"); e != nil {
				return
			}
			if controller.Flush() != nil {
				return
			}
		}
	}
}

func (s *Server) stillAuthorized(r *http.Request) bool {
	p := who(r)
	if p.Cookie {
		c, err := r.Cookie("ps_session")
		if err != nil {
			return false
		}
		_, _, err = s.Store.AuthSession(c.Value)
		return err == nil
	}
	_, err := s.Store.AuthKey(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	return err == nil
}
