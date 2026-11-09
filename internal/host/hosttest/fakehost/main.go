// Command fakehost runs the host daemon with a simulated sender page, behind the command-line interface of
// the Omarchy plugin's Python mock (passstream-omarchy-plugin
// tests/mock-daemon.py). The plugin's own tests run against it unmodified
// through a one-line python shim (test/plugin/run.sh), so they exercise the
// real daemon socket, the real attachment handshake and the real recovery.
// --real-server-bin runs the shipped serve and host executables in temporary
// XDG directories; without it, daemon-package unit tests keep the fake API.
//
// Differences from the mock, by design: the daemon is real, so --fail-op
// fails only commands forwarded to the sender page, and --no-reply-for and
// --reply-delay are ignored.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/passstream/passstream/internal/host/daemon"
	"github.com/passstream/passstream/internal/host/hosttest"
	"github.com/passstream/passstream/internal/host/listen"
	"github.com/passstream/passstream/internal/host/procnet"
	"github.com/passstream/passstream/internal/host/proto"
	"github.com/passstream/passstream/internal/host/state"
	"github.com/passstream/passstream/internal/host/supervise"
)

const key = "fakehost-owner-key"

func main() {
	hosttest.MaybeFakeBrowser()
	os.Exit(run())
}

type harness struct {
	cfg      daemon.Config
	mode     string
	logMu    sync.Mutex
	logOn    bool
	logFile  *os.File
	d        *daemon.Daemon
	cancel   context.CancelFunc
	done     chan struct{}
	realBin  string
	realDir  string
	realHost *realHost
}

func run() int {
	rt := os.Getenv("XDG_RUNTIME_DIR")
	fl := flag.NewFlagSet("fakehost", flag.ContinueOnError)
	realBin := fl.String("real-server-bin", "", "run the shipped serve command instead of the fake API")
	scenario := fl.String("scenario", "idle", "")
	keyState := fl.String("key", "ok", "")
	server := fl.String("server", "ok", "")
	portOwner := fl.String("port-owner", "ok", "")
	portMode := fl.String("port-mode", "socket_activated", "")
	failOp := fl.String("fail-op", "", "")
	failCode := fl.String("fail-code", "internal", "")
	fl.String("no-reply-for", "", "ignored")
	fl.Float64("reply-delay", 0, "ignored")
	statusDelay := fl.Float64("status-delay", 0, "")
	phase := fl.String("phase", "", "")
	fl.Float64("attach-delay", 0.4, "ignored")
	picker := fl.Float64("picker-delay", 0.6, "")
	step := fl.Float64("step", 1, "")
	restartAfter := fl.Float64("restart-after", 0, "")
	downFor := fl.Float64("down-for", 1, "")
	endAfter := fl.Float64("end-after", 0, "")
	logPath := fl.String("log", "", "")
	fl.Int("seed", 7, "ignored")
	fl.String("socket", "", "ignored: the daemon uses $XDG_RUNTIME_DIR/passstream/host.sock")
	verbose := fl.Bool("verbose", false, "daemon log to stderr")
	if fl.Parse(os.Args[1:]) != nil || rt == "" {
		return 2
	}
	tmp, err := os.MkdirTemp("", "fakehost-")
	if err != nil {
		return 1
	}
	defer os.RemoveAll(tmp)
	keyPath := filepath.Join(tmp, "key")
	switch *keyState {
	case "ok":
		os.WriteFile(keyPath, []byte(key), 0o600)
	case "unsafe":
		os.WriteFile(keyPath, []byte(key), 0o644)
		os.Chmod(keyPath, 0o644)
	case "invalid":
		os.WriteFile(keyPath, []byte("two words"), 0o600)
	case "rejected":
		os.WriteFile(keyPath, []byte("not-the-key"), 0o600)
	}
	var srv *hosttest.FakeServer
	if *realBin != "" {
		srv, err = hosttest.NewRealServer(*realBin, filepath.Join(tmp, "server"))
	} else {
		srv, err = hosttest.NewFakeServer(key)
	}
	if err != nil {
		return 1
	}
	defer srv.Close()
	if *keyState == "ok" {
		os.WriteFile(keyPath, []byte(srv.Key), 0o600)
	}
	names := strings.Split(*scenario, ",")
	srv.Scenario = hosttest.Scenario{Names: names, Step: *step, Phase: *phase, FailOp: *failOp, FailCode: *failCode,
		PickerMs: int(*picker * 1000), NoLive: *picker < 0, SignalURL: srv.RealSignalURL()}
	// The plugin checks links against the mock's public base.
	base := "https://stream.example"
	if *realBin != "" {
		base = srv.RealURL()
	}
	srv.PublicURL = base
	switch *server {
	case "unauthorized":
		srv.Unauthorized = true
	}
	if *server == "unreachable" {
		srv.Close()
	}
	h := &harness{mode: *portMode, realBin: *realBin, realDir: tmp}
	if *logPath != "" {
		if h.logFile, err = os.OpenFile(*logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err != nil {
			return 1
		}
	}
	logf := func(string, ...any) {}
	if *verbose {
		logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, "fakehost: "+f+"\n", a...) }
	}
	h.cfg = daemon.Config{
		Version: "0.3.0-fakehost", ServerURL: base, KeyPath: keyPath,
		RuntimeDir: filepath.Join(rt, "passstream"), StateDir: filepath.Join(tmp, "state"), Profile: filepath.Join(tmp, "profile"),
		Browser:      hosttest.BrowserCommand(mustExe(), "page", "watch:"+strconv.Itoa(os.Getpid())+",api:"+srv.URL),
		APITransport: srv.Transport(),
		// The browser is a process group even when the port mode claims
		// socket activation: the harness never creates real systemd units.
		Supervision: supervise.ProcessGroup,
		IdleExit:    -1, StopWait: 500 * time.Millisecond, Logf: logf, UnitActive: func() bool { return true },
		StatusDelay: time.Duration(*statusDelay * float64(time.Second)),
		OnRequest:   h.request,
	}
	port := freePort()
	if *portOwner != "ok" {
		// Someone else (uid 1001) holds the sender port: a fabricated table
		// for the port-owner check; the peer check still reads /proc.
		h.cfg.Proc = fakeOwner(tmp, port, 1001)
		h.cfg.PeerProc = "/proc"
	}
	if err := h.start(port); err != nil {
		fmt.Fprintln(os.Stderr, "fakehost:", err)
		return 1
	}
	sock := filepath.Join(h.cfg.RuntimeDir, "host.sock")
	if has(names, "live", "viewers", "knock", "failed-pins") {
		// With --phase the sender reports that phase and never "live".
		want := "live"
		if *phase != "" {
			want = "link_ready"
		}
		if err := h.goLive(sock, has(names, "knock"), want); err != nil {
			fmt.Fprintln(os.Stderr, "fakehost:", err)
			h.shutdown(sock)
			return 1
		}
	}
	h.logMu.Lock()
	h.logOn = true
	h.logMu.Unlock()
	fmt.Println("MOCK_READY " + sock)
	os.Stdout.Sync()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if *restartAfter > 0 {
		go func() {
			time.Sleep(time.Duration(*restartAfter * float64(time.Second)))
			fmt.Println("MOCK_DOWN")
			h.stopDaemon()
			time.Sleep(time.Duration(*downFor * float64(time.Second)))
			if err := h.start(h.cfg.Port); err != nil {
				fmt.Fprintln(os.Stderr, "fakehost: restart:", err)
			}
		}()
	}
	if *endAfter > 0 {
		go func() {
			time.Sleep(time.Duration(*endAfter * float64(time.Second)))
			if r, _ := state.Load(filepath.Join(h.cfg.StateDir, "host-state.json")); r != nil && r.Browser != nil {
				syscall.Kill(-r.Browser.PGID, syscall.SIGKILL) // the window closes
			}
		}()
	}
	<-ctx.Done()
	h.shutdown(sock)
	return 0
}

func mustExe() string {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return exe
}

func has(names []string, want ...string) bool {
	for _, n := range names {
		for _, w := range want {
			if n == w {
				return true
			}
		}
	}
	return false
}

// request records what clients sent (the mock's --log), after setup.
func (h *harness) request(line []byte) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	if h.logOn && h.logFile != nil {
		h.logFile.Write(append(append([]byte(nil), line...), '\n'))
	}
}

// start runs one daemon instance on a free port (never 47823, which the
// plugin's tests hold themselves).
func (h *harness) start(port int) error {
	if h.realBin != "" {
		return h.startRealHost(port)
	}
	ln, err := listen.SelfBind(h.cfg.RuntimeDir, port)
	if err != nil {
		return err
	}
	ln.Mode = h.mode // socket_activated: the harness plays systemd's part
	h.cfg.Port = ln.HTTP.Addr().(*net.TCPAddr).Port
	d, err := daemon.New(h.cfg, ln)
	if err != nil {
		ln.Close()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.d, h.cancel, h.done = d, cancel, make(chan struct{})
	go func() { d.Run(ctx); close(h.done) }()
	return nil
}

func (h *harness) stopDaemon() {
	if h.realHost != nil {
		h.realHost.stop()
		h.realHost = nil
		return
	}
	if h.cancel != nil {
		h.cancel()
		<-h.done
		h.cancel = nil
	}
}

// goLive starts a share through the socket and waits for live.
func (h *harness) goLive(sock string, knock bool, want string) error {
	c, err := hosttest.DialClient(sock)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("shell"); err != nil {
		return err
	}
	c.Call("subscribe", map[string]any{})
	if _, err := c.Call("start", map[string]any{"knock": knock}); err != nil {
		return err
	}
	if _, ok := c.WaitEvent(func(e proto.Event) bool { return e.Event == want }, 15*time.Second); !ok {
		return fmt.Errorf("the simulated sender never went live")
	}
	return nil
}

// shutdown ends any share (so no fake browser outlives the harness) and
// stops the daemon.
func (h *harness) shutdown(sock string) {
	if c, err := hosttest.DialClient(sock); err == nil {
		if c.Hello("shell") == nil {
			c.CallTimeout("stop", map[string]any{}, 10*time.Second)
		}
		c.Close()
	}
	h.stopDaemon()
}

// fakeOwner writes a /proc/net table in which uid holds 127.0.0.1:port.
func fakeOwner(dir string, port, uid int) procnet.Root {
	root := filepath.Join(dir, "proc")
	os.MkdirAll(filepath.Join(root, "net"), 0o700)
	hdr := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	line := fmt.Sprintf("   0: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000  %d        0 1 1 0000000000000000 100 0 0 10 0\n", port, uid)
	os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(hdr+line), 0o600)
	os.WriteFile(filepath.Join(root, "net", "tcp6"), []byte(hdr), 0o600)
	return procnet.Root(root)
}

func freePort() int {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
