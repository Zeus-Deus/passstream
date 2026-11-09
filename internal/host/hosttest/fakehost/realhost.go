package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/passstream/passstream/internal/host/listen"
)

// The fixture proxy records IPC and simulates only shell-environment failures.
// Every successful command/status goes through the shipped host executable.
type realHost struct {
	cmd   *exec.Cmd
	log   *os.File
	ln    *net.UnixListener
	sock  string
	mu    sync.Mutex
	conns map[net.Conn]bool
}

func (h *harness) startRealHost(port int) error {
	config := filepath.Join(h.realDir, "config", "passstream")
	runtime := filepath.Join(h.realDir, "run")
	state := filepath.Join(h.realDir, "host-state")
	for _, dir := range []string{config, runtime, state, filepath.Join(h.realDir, "home")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	key, err := os.ReadFile(h.cfg.KeyPath)
	if err == nil {
		target := filepath.Join(config, "key")
		if err = os.WriteFile(target, key, 0o600); err != nil {
			return err
		}
		fi, _ := os.Stat(h.cfg.KeyPath)
		if fi != nil {
			os.Chmod(target, fi.Mode().Perm())
		}
	}
	text := "server = " + h.cfg.ServerURL + "\nport = " + strconv.Itoa(port) + "\nbrowser = " + strings.Join(h.cfg.Browser, " ") + "\n"
	if err := os.WriteFile(filepath.Join(config, "config"), []byte(text), 0o600); err != nil {
		return err
	}
	r := &realHost{sock: filepath.Join(runtime, "passstream", "host.sock"), conns: map[net.Conn]bool{}}
	r.log, err = os.OpenFile(filepath.Join(h.realDir, "host.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	r.cmd = exec.Command(h.realBin, "host", "--idle-exit", "-1s")
	r.cmd.Env = append(os.Environ(), "HOME="+filepath.Join(h.realDir, "home"), "XDG_RUNTIME_DIR="+runtime, "XDG_CONFIG_HOME="+filepath.Dir(config), "XDG_STATE_HOME="+state, "XDG_CACHE_HOME="+filepath.Join(h.realDir, "cache"), "XDG_DATA_HOME="+filepath.Join(h.realDir, "data"))
	r.cmd.Stdout, r.cmd.Stderr = r.log, r.log
	if err := r.cmd.Start(); err != nil {
		r.log.Close()
		return err
	}
	ready := false
	for i := 0; i < 200; i++ {
		if c, err := net.Dial("unix", r.sock); err == nil {
			c.Close()
			ready = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		r.stop()
		return fmt.Errorf("real host did not become ready")
	}
	if err := listen.PrivateDir(h.cfg.RuntimeDir); err != nil {
		r.stop()
		return err
	}
	path := filepath.Join(h.cfg.RuntimeDir, "host.sock")
	r.ln, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		r.stop()
		return err
	}
	os.Chmod(path, 0o600)
	h.realHost = r
	h.cfg.Port = port
	go func() {
		for {
			c, err := r.ln.Accept()
			if err != nil {
				return
			}
			go h.proxyClient(r, c)
		}
	}()
	return nil
}

func (h *harness) proxyClient(r *realHost, c net.Conn) {
	up, err := net.Dial("unix", r.sock)
	if err != nil {
		c.Close()
		return
	}
	r.mu.Lock()
	r.conns[c] = true
	r.conns[up] = true
	r.mu.Unlock()
	defer func() {
		c.Close()
		up.Close()
		r.mu.Lock()
		delete(r.conns, c)
		delete(r.conns, up)
		r.mu.Unlock()
	}()
	var write sync.Mutex
	send := func(v any) { b, _ := json.Marshal(v); write.Lock(); defer write.Unlock(); c.Write(append(b, '\n')) }
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer c.Close()
		s := bufio.NewScanner(up)
		s.Buffer(make([]byte, 4096), 8<<20)
		hold := time.Now().Add(h.cfg.StatusDelay)
		for s.Scan() {
			var m map[string]any
			if json.Unmarshal(s.Bytes(), &m) != nil {
				return
			}
			data, _ := m["data"].(map[string]any)
			if daemon, ok := data["daemon"].(map[string]any); ok {
				h.fixtureStatus(daemon)
				if delay := time.Until(hold); delay > 0 {
					time.Sleep(delay)
				}
			}
			send(m)
		}
	}()
	s := bufio.NewScanner(c)
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		h.request(s.Bytes())
		var m map[string]any
		if json.Unmarshal(s.Bytes(), &m) != nil {
			return
		}
		if m["op"] == "start" && h.cfg.Proc != "" {
			send(map[string]any{"id": m["id"], "ok": false, "error": map[string]any{"code": "port_not_owned", "message": "fixture port owner", "details": map[string]any{"uid": 1001}}})
			continue
		}
		if _, err := up.Write(append(append([]byte{}, s.Bytes()...), '\n')); err != nil {
			break
		}
	}
	up.Close()
	<-done
}

func (r *realHost) stop() {
	if r.ln != nil {
		r.ln.Close()
	}
	r.mu.Lock()
	for c := range r.conns {
		c.Close()
	}
	r.mu.Unlock()
	if r.cmd != nil && r.cmd.Process != nil {
		r.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { r.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(7 * time.Second):
			r.cmd.Process.Kill()
			<-done
		}
	}
	if r.log != nil {
		r.log.Close()
	}
}

// fixtureStatus changes only metadata needed by explicit environment failures.
func (h *harness) fixtureStatus(daemon map[string]any) {
	if h.cfg.Proc != "" || h.cfg.StatusDelay > 0 && h.mode == "socket_activated" {
		daemon["port_mode"] = h.mode
	}
	if h.cfg.Proc != "" {
		daemon["port_owner"] = "not_owned"
	}
}
