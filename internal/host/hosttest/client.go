package hosttest

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/passstream/passstream/internal/host/proto"
)

// Client is a daemon-socket client for tests (CONTRACT §10).
type Client struct {
	c       net.Conn
	mu      sync.Mutex
	next    int
	waiting map[string]chan reply
	events  []proto.Event
	times   []time.Time
	cond    *sync.Cond
	Closed  chan struct{}
}

type reply struct {
	OK    bool
	Data  json.RawMessage
	Error *proto.Error
}

// DialClient connects; hello is up to the caller.
func DialClient(sock string) (*Client, error) {
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, err
	}
	cl := &Client{c: c, waiting: map[string]chan reply{}, Closed: make(chan struct{})}
	cl.cond = sync.NewCond(&cl.mu)
	go cl.read()
	return cl, nil
}

// Hello sends hello as the given client kind.
func (cl *Client) Hello(kind string) error {
	_, err := cl.Call("hello", map[string]any{"client": kind, "protocol": 1})
	return err
}

func (cl *Client) read() {
	defer func() {
		close(cl.Closed)
		cl.mu.Lock()
		cl.cond.Broadcast()
		cl.mu.Unlock()
	}()
	sc := bufio.NewScanner(cl.c)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var m struct {
			ID    *string         `json:"id"`
			OK    bool            `json:"ok"`
			Data  json.RawMessage `json:"data"`
			Error *proto.Error    `json:"error"`
			Event string          `json:"event"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		cl.mu.Lock()
		if m.Event != "" {
			var d any
			json.Unmarshal(m.Data, &d)
			cl.events = append(cl.events, proto.Event{Event: m.Event, Data: d})
			cl.times = append(cl.times, time.Now())
			cl.cond.Broadcast()
		} else if m.ID != nil {
			if ch := cl.waiting[*m.ID]; ch != nil {
				delete(cl.waiting, *m.ID)
				ch <- reply{OK: m.OK, Data: m.Data, Error: m.Error}
			}
		}
		cl.mu.Unlock()
	}
}

// ErrClosed means the daemon closed the socket.
var ErrClosed = errors.New("socket closed")

// Call sends a request and returns the raw data or the error envelope.
func (cl *Client) Call(op string, args any) (json.RawMessage, error) {
	return cl.CallTimeout(op, args, 30*time.Second)
}

// CallTimeout is Call with a deadline.
func (cl *Client) CallTimeout(op string, args any, d time.Duration) (json.RawMessage, error) {
	cl.mu.Lock()
	cl.next++
	id := strconv.Itoa(cl.next)
	ch := make(chan reply, 1)
	cl.waiting[id] = ch
	cl.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"id": id, "op": op, "args": args})
	if _, err := cl.c.Write(append(b, '\n')); err != nil {
		return nil, ErrClosed
	}
	select {
	case r := <-ch:
		if !r.OK {
			if r.Error == nil {
				return nil, errors.New("error reply without an error")
			}
			return nil, r.Error
		}
		return r.Data, nil
	case <-cl.Closed:
		return nil, ErrClosed
	case <-time.After(d):
		return nil, errors.New("timeout waiting for " + op)
	}
}

// Status returns the HostStatus.
func (cl *Client) Status() (proto.HostStatus, error) {
	var hs proto.HostStatus
	b, err := cl.Call("status", map[string]any{})
	if err != nil {
		return hs, err
	}
	return hs, json.Unmarshal(b, &hs)
}

// Events returns the events so far.
func (cl *Client) Events() []proto.Event {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return append([]proto.Event(nil), cl.events...)
}

// TimedEvent is an event with its arrival time.
type TimedEvent struct {
	E  proto.Event
	At time.Time
}

// EventsTimed returns the events so far with their arrival times.
func (cl *Client) EventsTimed() []TimedEvent {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	out := make([]TimedEvent, len(cl.events))
	for i, e := range cl.events {
		out[i] = TimedEvent{e, cl.times[i]}
	}
	return out
}

// WaitEvent waits for an event matching pred (already received ones count).
func (cl *Client) WaitEvent(pred func(proto.Event) bool, d time.Duration) (proto.Event, bool) {
	deadline := time.Now().Add(d)
	t := time.AfterFunc(d, func() { cl.mu.Lock(); cl.cond.Broadcast(); cl.mu.Unlock() })
	defer t.Stop()
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for {
		for _, e := range cl.events {
			if pred(e) {
				return e, true
			}
		}
		select {
		case <-cl.Closed:
			return proto.Event{}, false
		default:
		}
		if time.Now().After(deadline) {
			return proto.Event{}, false
		}
		cl.cond.Wait()
	}
}

// Close closes the connection.
func (cl *Client) Close() { cl.c.Close() }

// Field digs into decoded JSON.
func Field(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}
