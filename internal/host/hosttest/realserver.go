package hosttest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/passstream/passstream/internal/host/proto"
)

// realBackend runs the shipped serve command. Only scenario orchestration stays
// in the fixture; API auth, storage, commands and signaling use production code.
type realBackend struct {
	once  sync.Once
	cmd   *exec.Cmd
	log   *os.File
	proxy *httputil.ReverseProxy
	url   string
	mu    sync.Mutex
	caps  map[string]string
	peers map[string]*websocket.Conn
}

func NewRealServer(binary, data string) (*FakeServer, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := ln.Addr().String()
	ln.Close()
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(data, "server.log"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	b := &realBackend{url: "http://" + addr, log: log, caps: map[string]string{}, peers: map[string]*websocket.Conn{}}
	b.cmd = exec.Command(binary, "serve", "--data", data, "--listen", addr, "--base-url", b.url, "--ice-udp", "127.0.0.1:0", "--ice-tcp", "127.0.0.1:0", "--turn", "127.0.0.1:0", "--trust-proxy", "127.0.0.1/32")
	b.cmd.Env = append(os.Environ(), "HOME="+filepath.Join(data, "home"), "XDG_RUNTIME_DIR="+filepath.Join(data, "runtime"), "XDG_CONFIG_HOME="+filepath.Join(data, "config"), "XDG_STATE_HOME="+filepath.Join(data, "state"), "XDG_CACHE_HOME="+filepath.Join(data, "cache"), "XDG_DATA_HOME="+filepath.Join(data, "data"))
	b.cmd.Stdout, b.cmd.Stderr = log, log
	if err := b.cmd.Start(); err != nil {
		log.Close()
		return nil, err
	}
	ready := false
	client := &http.Client{Timeout: time.Second}
	for i := 0; i < 200; i++ {
		if resp, e := client.Get(b.url + "/readyz"); e == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		b.close()
		return nil, fmt.Errorf("real server did not become ready")
	}
	key, err := os.ReadFile(filepath.Join(data, "owner-key"))
	if err != nil {
		b.close()
		return nil, err
	}
	f, err := NewFakeServer(strings.TrimSpace(string(key)))
	if err != nil {
		b.close()
		return nil, err
	}
	f.real = b
	target, _ := url.Parse(b.url)
	b.proxy = httputil.NewSingleHostReverseProxy(target)
	b.proxy.FlushInterval = -1
	b.proxy.ModifyResponse = func(r *http.Response) error {
		if r.Request.Method == "POST" && r.Request.URL.Path == "/api/v1/streams" && r.StatusCode == 201 {
			body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
			r.Body.Close()
			if err != nil {
				return err
			}
			var s struct {
				ID   string `json:"id"`
				Base string `json:"view_link_base"`
			}
			if err := json.Unmarshal(body, &s); err != nil {
				return err
			}
			b.mu.Lock()
			b.caps[s.ID] = strings.TrimPrefix(s.Base, b.url+"/v#c=")
			b.mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		return nil
	}
	return f, nil
}

func (b *realBackend) close() {
	b.once.Do(func() {
		b.mu.Lock()
		for _, p := range b.peers {
			p.CloseNow()
		}
		b.mu.Unlock()
		if b.cmd != nil && b.cmd.Process != nil {
			b.cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan struct{})
			go func() { b.cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(7 * time.Second):
				b.cmd.Process.Kill()
				<-done
			}
		}
		if b.log != nil {
			b.log.Close()
		}
	})
}

func (f *FakeServer) RealSignalURL() string {
	if f.real == nil {
		return ""
	}
	return "ws" + strings.TrimPrefix(f.real.url, "http") + "/signal"
}

func (f *FakeServer) RealURL() string {
	if f.real == nil {
		return ""
	}
	return f.real.url
}

func (p *Page) openRealSignal(ctx context.Context, init map[string]any) error {
	p.mu.Lock()
	signalURL, id, instance := p.scenario.SignalURL, p.streamID, p.instance
	p.mu.Unlock()
	if signalURL == "" {
		return nil
	}
	s, _, err := websocket.Dial(ctx, signalURL, nil)
	if err != nil {
		return err
	}
	stream := init["stream"].(map[string]any)
	if err := WriteMsg(ctx, s, map[string]any{"type": "join", "protocol": 1, "role": "publish", "id": id, "credential": stream["publish_token"], "instance": instance, "sender_kind": "local"}); err != nil {
		s.CloseNow()
		return err
	}
	joined, err := ReadMsg(ctx, s)
	if err != nil || joined["type"] != "joined" {
		s.CloseNow()
		return fmt.Errorf("real publisher join failed")
	}
	p.mu.Lock()
	p.signal = s
	p.mu.Unlock()
	go func() { <-ctx.Done(); s.CloseNow() }()
	go func() {
		for {
			m, err := ReadMsg(ctx, s)
			if err != nil {
				return
			}
			if m["type"] == "control" {
				m["type"] = "command"
				p.command(m)
			}
		}
	}()
	return nil
}

func (b *realBackend) connection(stream string, c proto.Connection) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c.LeftAt != nil {
		if p := b.peers[c.ConnectionID]; p != nil {
			p.CloseNow()
			delete(b.peers, c.ConnectionID)
		}
		return c.ConnectionID, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	h := http.Header{}
	if c.IP != nil {
		h.Set("X-Forwarded-For", *c.IP)
	}
	p, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(b.url, "http")+"/signal", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return "", err
	}
	join := map[string]any{"type": "join", "role": "view", "protocol": 1, "credential": b.caps[stream]}
	if c.Reported.Label != nil {
		join["label"] = *c.Reported.Label
	}
	if err := WriteMsg(ctx, p, join); err != nil {
		p.CloseNow()
		return "", err
	}
	m, err := ReadMsg(ctx, p)
	if err != nil || m["type"] != "joined" {
		p.CloseNow()
		return "", fmt.Errorf("real viewer join failed")
	}
	id := fmt.Sprint(m["connection_id"])
	b.peers[id] = p
	go func() {
		for {
			if _, err := ReadMsg(context.Background(), p); err != nil {
				return
			}
		}
	}()
	return id, nil
}
