package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/daemon"
	"github.com/passstream/passstream/internal/host/hosttest"
	"github.com/passstream/passstream/internal/host/listen"
	"github.com/passstream/passstream/internal/host/proto"
	"github.com/passstream/passstream/internal/host/state"
)

func TestMain(m *testing.M) {
	hosttest.MaybeFakeBrowser()
	os.Exit(m.Run())
}

const testKey = "psk_test_owner_key_0123456789"

// logBuf collects the daemon's log so tests can scan it for secrets.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", a...)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// env is one daemon under test with its fake server and fake browser.
type env struct {
	t        *testing.T
	dir      string
	runtime  string
	stateDir string
	keyPath  string
	urlFile  string
	srv      *hosttest.FakeServer
	cfg      daemon.Config
	d        *daemon.Daemon
	cancel   context.CancelFunc
	done     chan struct{}
	log      *logBuf
	port     int
	sock     string
}

type opt func(*env)

func withBrowser(mode string) opt {
	return func(e *env) { e.cfg.Browser = hosttest.BrowserCommand(os.Args[0], mode, e.urlFile) }
}

func newEnv(t *testing.T, opts ...opt) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir, runtime: filepath.Join(dir, "run", "passstream"), stateDir: filepath.Join(dir, "state"),
		keyPath: filepath.Join(dir, "config", "key"), urlFile: filepath.Join(dir, "app-url"), log: &logBuf{}}
	os.MkdirAll(filepath.Dir(e.keyPath), 0o700)
	os.WriteFile(e.keyPath, []byte(testKey+"\n"), 0o600)
	srv, err := hosttest.NewFakeServer(testKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	e.srv = srv
	e.cfg = daemon.Config{
		Version: "test", ServerURL: srv.URL, KeyPath: e.keyPath, RuntimeDir: e.runtime, StateDir: e.stateDir,
		Profile: filepath.Join(dir, "profile"), IdleExit: -1, Logf: e.log.Logf, StopWait: 300 * time.Millisecond,
		Browser: hosttest.BrowserCommand(os.Args[0], "page", e.urlFile),
	}
	for _, o := range opts {
		o(e)
	}
	e.sock = filepath.Join(e.runtime, "host.sock")
	e.start(0)
	t.Cleanup(func() {
		e.stopStream()
		e.kill()
	})
	return e
}

// start runs a daemon (port 0 picks a free one the first time).
func (e *env) start(port int) {
	e.t.Helper()
	ln, err := listen.SelfBind(e.runtime, port)
	if err != nil {
		e.t.Fatal(err)
	}
	e.port = ln.HTTP.Addr().(*net.TCPAddr).Port
	cfg := e.cfg
	cfg.Port = e.port
	d, err := daemon.New(cfg, ln)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.d, e.cancel, e.done = d, cancel, make(chan struct{})
	go func() { d.Run(ctx); close(e.done) }()
}

// kill stops the daemon without ending the stream (like a crash: the
// browser and the record survive).
func (e *env) kill() {
	if e.cancel == nil {
		return
	}
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(10 * time.Second):
		e.t.Error("daemon didn't stop")
	}
	e.cancel = nil
}

func (e *env) restart() {
	e.kill()
	e.start(e.port)
}

func (e *env) stopStream() {
	if e.cancel == nil {
		return
	}
	c, err := hosttest.DialClient(e.sock)
	if err != nil {
		return
	}
	defer c.Close()
	c.Hello("cli")
	c.CallTimeout("stop", map[string]any{}, 20*time.Second)
}

func (e *env) client(kind string) *hosttest.Client {
	e.t.Helper()
	var c *hosttest.Client
	var err error
	for i := 0; i < 50; i++ {
		if c, err = hosttest.DialClient(e.sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(c.Close)
	if err := c.Hello(kind); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) origin() string { return fmt.Sprintf("http://127.0.0.1:%d", e.port) }

// appURL waits for the idle fake browser to record its --app URL.
func (e *env) appURL() string {
	e.t.Helper()
	for i := 0; i < 200; i++ {
		if b, err := os.ReadFile(e.urlFile); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatal("the fake browser never started")
	return ""
}

func (e *env) record() *state.Record {
	r, err := state.Load(filepath.Join(e.stateDir, "host-state.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func status(t *testing.T, c *hosttest.Client) proto.HostStatus {
	t.Helper()
	hs, err := c.Status()
	if err != nil {
		t.Fatal(err)
	}
	return hs
}

func code(err error) string {
	if e, ok := err.(*proto.Error); ok {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return "?" + err.Error()
}

func call(t *testing.T, c *hosttest.Client, op string, args map[string]any) map[string]any {
	t.Helper()
	b, err := c.Call(op, args)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

// startLive starts a share through the socket and waits for live.
func (e *env) startLive(c *hosttest.Client, args map[string]any) string {
	e.t.Helper()
	call(e.t, c, "subscribe", map[string]any{})
	m := call(e.t, c, "start", args)
	id := m["stream_id"].(string)
	if _, ok := c.WaitEvent(func(ev proto.Event) bool { return ev.Event == "live" }, 10*time.Second); !ok {
		e.t.Fatalf("never live; log:\n%s", e.log)
	}
	return id
}

// cmdlinesContaining returns the command lines that mention s among our
// own processes: descendants of this test process and members of the given
// process groups (the browser's own session). Other users' and unrelated
// processes are ignored: a 4-digit PIN collides with random digits.
func cmdlinesContaining(s string, pgids ...int) []string {
	type proc struct{ ppid, pgrp int }
	all := map[int]proc{}
	ents, _ := os.ReadDir("/proc")
	for _, en := range ents {
		var pid int
		if _, err := fmt.Sscan(en.Name(), &pid); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", en.Name(), "stat"))
		if err != nil {
			continue
		}
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		var p proc
		fmt.Sscan(f[1], &p.ppid)
		fmt.Sscan(f[2], &p.pgrp)
		all[pid] = p
	}
	ours := func(pid int) bool {
		for _, g := range pgids {
			if all[pid].pgrp == g {
				return true
			}
		}
		for n := 0; pid > 1 && n < 64; n++ {
			if pid == os.Getpid() {
				return true
			}
			pid = all[pid].ppid
		}
		return false
	}
	var out []string
	for pid := range all {
		if !ours(pid) {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "cmdline"))
		if err == nil && strings.Contains(string(b), s) {
			out = append(out, strings.ReplaceAll(string(b), "\x00", " "))
		}
	}
	return out
}

func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

func contextFromSignals() context.Context {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	go func() { <-ctx.Done(); stop() }()
	return ctx
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func readRecord(t *testing.T, path string) *state.Record {
	t.Helper()
	r, err := state.Load(path)
	if err != nil || r == nil {
		t.Fatalf("record: %v", err)
	}
	return r
}
