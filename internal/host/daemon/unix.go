package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/passstream/passstream/internal/host/proto"
)

const (
	maxRequestLine = 1 << 20
	clientQueue    = 256
	maxInflight    = 16
)

// client is one connection on the daemon socket.
type client struct {
	conn      *net.UnixConn
	out       chan []byte
	once      sync.Once
	done      chan struct{}
	mu        sync.Mutex
	sub       bool
	kind      string
	holdUntil time.Time
}

func (c *client) subscribed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sub
}

// send queues a line; a client that can't keep up is dropped rather than
// allowed to block the daemon.
func (c *client) send(b []byte) {
	select {
	case <-c.done:
	case c.out <- b:
	default:
		c.close()
	}
}

func (c *client) close() {
	c.once.Do(func() {
		close(c.done)
		c.conn.Close()
	})
}

func (c *client) writer() {
	for {
		select {
		case <-c.done:
			return
		case b := <-c.out:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := c.conn.Write(b); err != nil {
				c.close()
				return
			}
		}
	}
}

func (d *Daemon) serveUnix() {
	for {
		conn, err := d.ln.Unix.AcceptUnix()
		if err != nil {
			if d.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !d.peerIsUs(conn) {
			conn.Close()
			continue
		}
		c := &client{conn: conn, out: make(chan []byte, clientQueue), done: make(chan struct{})}
		d.mu.Lock()
		d.clients[c] = true
		d.mu.Unlock()
		go c.writer()
		go d.readClient(c)
	}
}

// peerIsUs checks SO_PEERCRED against our uid (HOST-CONTROL §5.1).
func (d *Daemon) peerIsUs(conn *net.UnixConn) bool {
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		ok = err == nil && int(cred.Uid) == d.uid
	})
	return ok
}

func (d *Daemon) reply(c *client, id string, data any, err error) {
	r := proto.Reply{ID: id, OK: err == nil}
	if err != nil {
		r.Error = proto.AsError(err)
	} else {
		if data == nil {
			data = map[string]any{}
		}
		r.Data = data
	}
	b, e := json.Marshal(r)
	if e != nil {
		b, _ = json.Marshal(proto.Reply{ID: id, Error: proto.E("internal", "")})
	}
	c.send(append(b, '\n'))
}

func (d *Daemon) readClient(c *client) {
	defer func() {
		c.close()
		d.mu.Lock()
		delete(d.clients, c)
		d.idleSince = time.Now()
		d.mu.Unlock()
	}()
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 4096), maxRequestLine)
	greeted := false
	sem := make(chan struct{}, maxInflight)
	for sc.Scan() {
		line := sc.Bytes()
		if d.cfg.OnRequest != nil {
			d.cfg.OnRequest(append([]byte(nil), line...))
		}
		var req struct {
			ID   *string         `json:"id"`
			Op   *string         `json:"op"`
			Args json.RawMessage `json:"args"`
		}
		if json.Unmarshal(line, &req) != nil || req.ID == nil || req.Op == nil {
			d.reply(c, "", nil, proto.E("invalid_argument", "bad request line"))
			continue
		}
		id, op := *req.ID, *req.Op
		args, err := decodeArgs(req.Args)
		if err != nil {
			d.reply(c, id, nil, err)
			continue
		}
		if !greeted {
			if op != "hello" {
				d.reply(c, id, nil, proto.E("protocol_mismatch", "send hello first"))
				d.drainAndClose(c)
				return
			}
			data, err := d.hello(c, args)
			d.reply(c, id, data, err)
			if err != nil {
				if proto.AsError(err).Code == "protocol_mismatch" {
					d.drainAndClose(c)
					return
				}
				continue
			}
			greeted = true
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-c.done:
			return
		}
		go func() {
			defer func() { <-sem }()
			data, err := d.dispatch(c, op, args)
			if op == "status" && d.cfg.StatusDelay > 0 {
				time.Sleep(d.cfg.StatusDelay)
			}
			d.reply(c, id, data, err)
			if op == "shutdown" && err == nil {
				go func() { time.Sleep(100 * time.Millisecond); d.cancel() }()
			}
		}()
	}
}

// drainAndClose lets queued replies go out before the socket closes.
func (d *Daemon) drainAndClose(c *client) {
	deadline := time.Now().Add(time.Second)
	for len(c.out) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	c.close()
}

func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	v, err := proto.DecodeJSON(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, proto.E("invalid_argument", "args must be an object")
	}
	return m, nil
}

func (d *Daemon) hello(c *client, args map[string]any) (any, error) {
	if err := closed(args, "client", "protocol"); err != nil {
		return nil, err
	}
	kind, _ := args["client"].(string)
	if kind != "plugin" && kind != "cli" && kind != "shell" {
		return nil, proto.E("invalid_argument", "client must be plugin, cli or shell")
	}
	if p, ok := args["protocol"].(float64); !ok || p != proto.Protocol {
		return nil, proto.E("protocol_mismatch", "this daemon speaks protocol %d", proto.Protocol)
	}
	c.mu.Lock()
	c.kind = kind
	c.mu.Unlock()
	return map[string]any{"protocol": proto.Protocol, "version": d.cfg.Version, "server_url": d.cfg.ServerURL}, nil
}

// closed refuses any argument not named (CONTRACT §1.2: command arguments
// are closed).
func closed(args map[string]any, names ...string) error {
	for k := range args {
		ok := false
		for _, n := range names {
			if k == n {
				ok = true
				break
			}
		}
		if !ok {
			return proto.E("invalid_argument", "unknown argument %q", k)
		}
	}
	return nil
}

func runQuiet(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}
