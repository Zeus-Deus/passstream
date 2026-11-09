package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/passstream/passstream/internal/host/proto"
)

// conn is a connection to the daemon socket (CONTRACT §10).
type conn struct {
	c       net.Conn
	mu      sync.Mutex
	next    int
	waiting map[string]chan proto.Reply
	events  chan proto.Event
	closed  chan struct{}
}

type rawReply struct {
	ID    string          `json:"id"`
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *proto.Error    `json:"error"`
	Event string          `json:"event"`
}

// errUnreachable is exit code 3.
var errUnreachable = errors.New("the passstream daemon isn't reachable")

// socketPath is $XDG_RUNTIME_DIR/passstream/host.sock.
func socketPath() (string, error) {
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		return "", errors.New("XDG_RUNTIME_DIR is not set")
	}
	return filepath.Join(rt, "passstream", "host.sock"), nil
}

// checkSocket refuses a socket or directory that isn't ours and private.
func checkSocket(sock string) error {
	for _, p := range []string{filepath.Dir(sock), sock} {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != os.Geteuid() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("%s isn't private to this user", p)
		}
	}
	return nil
}

// dial connects, starting a self-bound daemon if nothing listens (§5.9),
// and sends hello.
func dial(spawn bool) (*conn, error) {
	sock, err := socketPath()
	if err != nil {
		return nil, err
	}
	c, err := tryDial(sock)
	if err != nil && spawn {
		if exe, e := os.Executable(); e == nil {
			cmd := exec.Command(exe, "host", "--detach")
			cmd.Stdout, cmd.Stderr = nil, os.Stderr
			cmd.Run()
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if c, err = tryDial(sock); err == nil {
				break
			}
		}
	}
	if err != nil {
		return nil, errUnreachable
	}
	cn := &conn{c: c, waiting: map[string]chan proto.Reply{}, events: make(chan proto.Event, 256), closed: make(chan struct{})}
	go cn.read()
	if _, err := cn.call("hello", map[string]any{"client": "cli", "protocol": proto.Protocol}); err != nil {
		c.Close()
		return nil, err
	}
	return cn, nil
}

func tryDial(sock string) (net.Conn, error) {
	if err := checkSocket(sock); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("unsafe socket: %w", err)
	}
	return net.DialTimeout("unix", sock, 2*time.Second)
}

func (cn *conn) read() {
	defer close(cn.closed)
	sc := bufio.NewScanner(cn.c)
	sc.Buffer(make([]byte, 64<<10), 9<<20)
	for sc.Scan() {
		var r rawReply
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if r.Event != "" {
			var data any
			json.Unmarshal(r.Data, &data)
			select {
			case cn.events <- proto.Event{Event: r.Event, Data: data}:
			default: // a slow reader drops events, never blocks replies
			}
			continue
		}
		cn.mu.Lock()
		ch := cn.waiting[r.ID]
		delete(cn.waiting, r.ID)
		cn.mu.Unlock()
		if ch != nil {
			var data any
			if len(r.Data) > 0 {
				json.Unmarshal(r.Data, &data)
			}
			ch <- proto.Reply{ID: r.ID, OK: r.OK, Data: data, Error: r.Error}
		}
	}
}

// call sends one request and waits for its reply.
func (cn *conn) call(op string, args any) (any, error) {
	return cn.callTimeout(op, args, 60*time.Second)
}

func (cn *conn) callTimeout(op string, args any, d time.Duration) (any, error) {
	cn.mu.Lock()
	cn.next++
	id := strconv.Itoa(cn.next)
	ch := make(chan proto.Reply, 1)
	cn.waiting[id] = ch
	cn.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"id": id, "op": op, "args": args})
	if _, err := cn.c.Write(append(b, '\n')); err != nil {
		return nil, errUnreachable
	}
	select {
	case r := <-ch:
		if !r.OK {
			if r.Error == nil {
				r.Error = proto.E("internal", "malformed reply")
			}
			return nil, r.Error
		}
		return r.Data, nil
	case <-cn.closed:
		return nil, errUnreachable
	case <-time.After(d):
		return nil, proto.E("sender_timeout", "no reply from the daemon")
	}
}

func (cn *conn) close() { cn.c.Close() }
