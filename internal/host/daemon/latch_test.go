package daemon_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/passstream/passstream/internal/host/daemon"
	"github.com/passstream/passstream/internal/host/hosttest"
	"github.com/passstream/passstream/internal/host/listen"
	"github.com/passstream/passstream/internal/host/proto"
)

// attachedPage authenticates as the page and acks, and returns once the
// daemon reports the attachment (so commands reach the page).
func attachedPage(t *testing.T, c *hosttest.Client, origin string, lid, lkey []byte) (*hosttest.Session, []byte, []byte) {
	t.Helper()
	s, err := hosttest.Authenticate(ctx5(), origin, lid, lkey)
	if err != nil {
		t.Fatal(err)
	}
	aid, akey := attachmentOf(t, s)
	hosttest.WriteMsg(ctx5(), s.WS, map[string]any{"type": "init_ack"})
	waitFor(t, "attached", 3*time.Second, func() bool { return status(t, c).Stream.Attachment == "attached" })
	return s, aid, akey
}

func senderState(id, instance string, seq int, l proto.Latches, mutate func(*proto.SenderState)) map[string]any {
	n := uint64(4)
	st := proto.SenderState{V: 1, StreamID: id, Instance: instance, Seq: uint64(seq), Phase: "live", Mode: "text",
		E2EE:       proto.E2EE{Status: "beta", Epoch: 1},
		Access:     proto.Access{Pin: proto.PinInfo{Required: true, Kind: "digits", Length: &n}, PinVersion: 1, Latches: l, Failures: proto.Failures{Limit: 20}},
		Admissions: []proto.Admission{}, Knocks: []proto.Knock{}, Ends: []proto.End{}}
	if mutate != nil {
		mutate(&st)
	}
	return map[string]any{"type": "state", "state": st}
}

// readUntil reads page messages until one has the given type.
func readUntil(t *testing.T, ws *websocket.Conn, typ string) map[string]any {
	t.Helper()
	for {
		m, err := hosttest.ReadMsg(ctx5(), ws)
		if err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		if m["type"] == typ {
			return m
		}
	}
}

// TestLatchesWriteAheadAndNeverLoosen covers HOST-CONTROL §5.6: a latch
// set by the plugin or CLI is fsynced before it's forwarded; while the
// page is detached pause and lock still succeed through the mirror; a
// state that lacks a latch never clears the mirror; init carries the
// mirror, so a reconnect can't unlock or resume.
func TestLatchesWriteAheadAndNeverLoosen(t *testing.T) {
	e, c, origin, lid, lkey := launched(t)
	s, aid, akey := attachedPage(t, c, origin, lid, lkey)
	id := hosttest.Field(s.Init, "stream", "id").(string)
	inst := proto.NewId22()
	hosttest.WriteMsg(ctx5(), s.WS, senderState(id, inst, 1, proto.Latches{}, nil))
	if m := readUntil(t, s.WS, "state_ack"); m["seq"] != 1.0 {
		t.Fatalf("ack %v", m)
	}

	// pause: the record has it before the page sees the command.
	done := make(chan error, 1)
	go func() { _, err := c.Call("pause", map[string]any{}); done <- err }()
	cmd := readUntil(t, s.WS, "command")
	if cmd["op"] != "pause" || !e.record().Latches.Paused {
		t.Fatalf("pause not durable before forwarding: %v %+v", cmd, e.record().Latches)
	}
	hosttest.WriteMsg(ctx5(), s.WS, map[string]any{"type": "result", "id": cmd["id"], "ok": true, "data": map[string]any{}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A stale state without the latch doesn't clear the mirror.
	hosttest.WriteMsg(ctx5(), s.WS, senderState(id, inst, 2, proto.Latches{}, nil))
	readUntil(t, s.WS, "state_ack")
	if !e.record().Latches.Paused || !status(t, c).Stream.Access.Latches.Paused {
		t.Fatal("a state without the latch cleared it")
	}
	// A page-originated lock is mirrored and fsynced before state_ack.
	hosttest.WriteMsg(ctx5(), s.WS, senderState(id, inst, 3, proto.Latches{Locked: true}, nil))
	readUntil(t, s.WS, "state_ack")
	if !e.record().Latches.Locked {
		t.Fatal("state_ack before the mirror was durable")
	}

	// Detached: pause and lock {locked:true} succeed; the rest doesn't.
	s.WS.CloseNow()
	waitFor(t, "detached", 3*time.Second, func() bool { return status(t, c).Stream.Attachment == "detached" })
	if _, err := c.Call("lock", map[string]any{"locked": true}); err != nil {
		t.Fatalf("lock while detached: %v", err)
	}
	for _, op := range []string{"resume", "mode"} {
		args := map[string]any{}
		if op == "mode" {
			args["mode"] = "motion"
		}
		if _, err := c.Call(op, args); code(err) != "sender_offline" {
			t.Fatalf("%s while detached: %v", op, err)
		}
	}
	if _, err := c.Call("lock", map[string]any{"locked": false}); code(err) != "sender_offline" {
		t.Fatalf("unlock while detached: %v", err)
	}
	// Reconnect: init carries the mirror; nothing was loosened.
	s2, err := hosttest.Authenticate(ctx5(), origin, aid, akey)
	if err != nil {
		t.Fatal(err)
	}
	var l proto.Latches
	b, _ := json.Marshal(s2.Init["latches"])
	json.Unmarshal(b, &l)
	if !l.Paused || !l.Locked {
		t.Fatalf("init latches %+v", l)
	}
	// Clearing needs an ok result from the page.
	go func() { _, err := c.Call("resume", map[string]any{}); done <- err }()
	cmd = readUntil(t, s2.WS, "command")
	hosttest.WriteMsg(ctx5(), s2.WS, map[string]any{"type": "result", "id": cmd["id"], "ok": false, "error": map[string]any{"code": "storage", "message": "x", "details": map[string]any{"applied": true}}})
	if err := <-done; code(err) != "storage" {
		t.Fatalf("resume error: %v", err)
	}
	if !e.record().Latches.Paused {
		t.Fatal("a failed resume cleared the mirror")
	}
	go func() { _, err := c.Call("resume", map[string]any{}); done <- err }()
	cmd = readUntil(t, s2.WS, "command")
	hosttest.WriteMsg(ctx5(), s2.WS, map[string]any{"type": "result", "id": cmd["id"], "ok": true, "data": map[string]any{}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.record().Latches.Paused {
		t.Fatal("resume ok didn't clear the mirror")
	}
	// latch_cleared from the window's own UI clears the lock.
	hosttest.WriteMsg(ctx5(), s2.WS, map[string]any{"type": "latch_cleared", "latch": "locked"})
	waitFor(t, "unlocked mirror", 2*time.Second, func() bool { return !e.record().Latches.Locked })
}

// TestKickLockSetsLatchEvenOnFailure: kick with lock fsyncs the lock first
// and keeps it whatever the kick does (HOST-CONTROL §4.3).
func TestKickLockSetsLatchEvenOnFailure(t *testing.T) {
	e, c, origin, lid, lkey := launched(t)
	s, _, _ := attachedPage(t, c, origin, lid, lkey)
	done := make(chan error, 1)
	go func() {
		_, err := c.Call("kick", map[string]any{"admission_id": proto.NewId22(), "lock": true})
		done <- err
	}()
	cmd := readUntil(t, s.WS, "command")
	if !e.record().Latches.Locked {
		t.Fatal("lock not durable before the kick was forwarded")
	}
	hosttest.WriteMsg(ctx5(), s.WS, map[string]any{"type": "result", "id": cmd["id"], "ok": false,
		"error": map[string]any{"code": "unknown_admission", "message": "gone", "details": map[string]any{"locked": true}}})
	err := <-done
	if code(err) != "unknown_admission" || err.(*proto.Error).Details["locked"] != true {
		t.Fatalf("kick result %v", err)
	}
	if !e.record().Latches.Locked {
		t.Fatal("lock lost")
	}
}

// TestStatusRateLimit: at most 4 status events per second, and
// statistics-only changes at most once per second (Appendix A #2).
func TestStatusRateLimit(t *testing.T) {
	_, c, origin, lid, lkey := launched(t)
	s, _, _ := attachedPage(t, c, origin, lid, lkey)
	id := hosttest.Field(s.Init, "stream", "id").(string)
	inst := proto.NewId22()
	go func() {
		for {
			if _, err := hosttest.ReadMsg(ctx5(), s.WS); err != nil {
				return
			}
		}
	}()
	time.Sleep(600 * time.Millisecond)
	before := len(statusEvents(c))
	start := time.Now()
	// 2 s of material changes (mode flips) at 50 per second.
	for i := 0; i < 100; i++ {
		mode := []string{"text", "motion"}[i%2]
		hosttest.WriteMsg(ctx5(), s.WS, senderState(id, inst, 10+i, proto.Latches{}, func(st *proto.SenderState) { st.Mode = mode }))
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	evs := statusEvents(c)[before:]
	if len(evs) == 0 {
		t.Fatal("no status events")
	}
	checkRate(t, evs, 4, "material")
	t.Logf("material: %d status events in %v", len(evs), time.Since(start).Round(time.Millisecond))

	// 2 s of statistics-only changes (health).
	time.Sleep(time.Second)
	before = len(statusEvents(c))
	for i := 0; i < 100; i++ {
		fps := float64(i)
		hosttest.WriteMsg(ctx5(), s.WS, senderState(id, inst, 200+i, proto.Latches{}, func(st *proto.SenderState) {
			st.Mode = "motion"
			st.Health = &proto.Health{FPS: &fps, QualityLimitation: "none"}
		}))
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(1200 * time.Millisecond)
	evs = statusEvents(c)[before:]
	checkRate(t, evs, 1, "statistics")
	if len(evs) == 0 {
		t.Fatal("statistics never reported")
	}
	t.Logf("statistics: %d status events over ~3 s", len(evs))
}

func statusEvents(c *hosttest.Client) []time.Time {
	var out []time.Time
	for _, ev := range c.EventsTimed() {
		if ev.E.Event == "status" {
			out = append(out, ev.At)
		}
	}
	return out
}

// checkRate requires no sliding one-second window with more than max events.
func checkRate(t *testing.T, at []time.Time, max int, what string) {
	t.Helper()
	for i := range at {
		n := 0
		for j := i; j < len(at) && at[j].Sub(at[i]) < time.Second-30*time.Millisecond; j++ {
			n++
		}
		if n > max {
			t.Fatalf("%s: %d status events within one second (max %d)", what, n, max)
		}
	}
}

// TestIdleExit: a subscribed connection keeps the daemon up (Appendix A
// #1); without clients or a stream it exits after the idle time.
func TestIdleExit(t *testing.T) {
	e := newEnv(t, func(e *env) { e.cfg.IdleExit = 400 * time.Millisecond })
	c := e.client("plugin")
	call(t, c, "subscribe", map[string]any{})
	select {
	case <-e.done:
		t.Fatal("exited while a subscriber was connected")
	case <-time.After(1200 * time.Millisecond):
	}
	c.Close()
	select {
	case <-e.done:
		e.cancel = nil
	case <-time.After(3 * time.Second):
		t.Fatal("no idle exit")
	}
}

// --- kill -9 recovery, with the daemon in its own process ---

const daemonArg = "__passstream_test_daemon__"

func init() {
	if len(os.Args) == 3 && os.Args[1] == daemonArg {
		os.Exit(runDaemonProcess(os.Args[2]))
	}
}

// procConfig is what the daemon subprocess needs (daemon.Config has funcs).
type procConfig struct {
	Port        int
	ServerURL   string
	KeyPath     string
	RuntimeDir  string
	StateDir    string
	Profile     string
	Browser     []string
	DetachGrace time.Duration
	// SocketActivated takes the listeners from LISTEN_FDS (the test runs
	// the daemon under systemd-socket-activate) instead of binding.
	SocketActivated bool
}

func runDaemonProcess(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 1
	}
	var pc procConfig
	if json.Unmarshal(b, &pc) != nil {
		return 1
	}
	lock, err := listen.Lock(pc.RuntimeDir)
	if err != nil {
		return 1
	}
	defer lock.Close()
	var ln *listen.Listeners
	if pc.SocketActivated {
		ln, err = listen.FromSystemd()
		if err == nil && ln == nil {
			err = os.ErrNotExist
		}
	} else {
		ln, err = listen.SelfBind(pc.RuntimeDir, pc.Port)
	}
	if err != nil {
		os.WriteFile(path+".err", []byte(err.Error()), 0o600)
		return 1
	}
	var mu sync.Mutex
	logf, _ := os.OpenFile(path+".log", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	cfg := daemon.Config{Version: "test", Port: pc.Port, ServerURL: pc.ServerURL, KeyPath: pc.KeyPath, RuntimeDir: pc.RuntimeDir,
		StateDir: pc.StateDir, Profile: pc.Profile, Browser: pc.Browser, DetachGrace: pc.DetachGrace, IdleExit: -1,
		// Under the activator there's no passstream-host.socket unit, and
		// the browser stays a process group (no real transient units).
		UnitActive: func() bool { return true }, Supervision: "process_group",
		Logf: func(format string, a ...any) {
			mu.Lock()
			defer mu.Unlock()
			logf.WriteString(strings.TrimSpace(sprintf(format, a...)) + "\n")
		}}
	d, err := daemon.New(cfg, ln)
	if err != nil {
		return 1
	}
	d.Run(contextFromSignals())
	return 0
}

func TestRecoveryAfterKill9MidShare(t *testing.T) {
	dir := t.TempDir()
	srv, err := hosttest.NewFakeServer(testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	keyPath := filepath.Join(dir, "key")
	os.WriteFile(keyPath, []byte(testKey), 0o600)
	runtime := filepath.Join(dir, "run")
	port := freePort(t)
	pc := procConfig{Port: port, ServerURL: srv.URL, KeyPath: keyPath, RuntimeDir: runtime,
		StateDir: filepath.Join(dir, "state"), Profile: filepath.Join(dir, "profile"),
		DetachGrace: 3 * time.Second, Browser: hosttest.BrowserCommand(os.Args[0], "page", "-")}
	cfgPath := filepath.Join(dir, "daemon.json")
	b, err := json.Marshal(pc)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(cfgPath, b, 0o600)
	sock := filepath.Join(runtime, "host.sock")
	spawn := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], daemonArg, cfgPath)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	dialed := func() *hosttest.Client {
		var c *hosttest.Client
		waitFor(t, "the daemon socket", 5*time.Second, func() bool {
			cl, err := hosttest.DialClient(sock)
			if err != nil {
				return false
			}
			if cl.Hello("cli") != nil {
				cl.Close()
				return false
			}
			c = cl
			return true
		})
		return c
	}
	d1 := spawn()
	c := dialed()
	call(t, c, "subscribe", map[string]any{})
	id := call(t, c, "start", map[string]any{})["stream_id"].(string)
	if _, ok := c.WaitEvent(func(ev proto.Event) bool { return ev.Event == "live" }, 10*time.Second); !ok {
		t.Fatal("never live")
	}
	pin := call(t, c, "pin.reveal", map[string]any{})["pin"].(string)
	call(t, c, "pause", map[string]any{})
	rec := readRecord(t, filepath.Join(dir, "state", "host-state.json"))
	browser := rec.Browser.PID

	// kill -9 mid-share: no cleanup runs.
	d1.Process.Signal(syscall.SIGKILL)
	d1.Wait()
	c.Close()
	if syscall.Kill(browser, 0) != nil {
		t.Fatal("the browser died with the daemon")
	}
	d2 := spawn()
	defer func() { d2.Process.Signal(syscall.SIGTERM); d2.Wait() }()
	c = dialed()
	defer c.Close()
	waitFor(t, "re-attach after the restart", 8*time.Second, func() bool {
		hs := status(t, c)
		return hs.Stream != nil && hs.Stream.ID == id && hs.Stream.Attachment == "attached" && hs.Stream.HasLink
	})
	hs := status(t, c)
	if !hs.Stream.Access.Latches.Paused || hs.Stream.Phase != "paused" {
		t.Fatalf("the restart loosened a latch: %+v phase %s", hs.Stream.Access.Latches, hs.Stream.Phase)
	}
	if got := call(t, c, "pin.reveal", map[string]any{})["pin"]; got != pin {
		t.Fatal("secrets not re-sent after the restart")
	}
	if rec2 := readRecord(t, filepath.Join(dir, "state", "host-state.json")); rec2.Browser.PID != browser || rec2.Attachment.KeyID != rec.Attachment.KeyID {
		t.Fatal("the browser or the attachment changed")
	}

	// kill -9 again, and this time the browser goes too: the restarted
	// daemon can't re-adopt it and ends the stream.
	d2.Process.Signal(syscall.SIGKILL)
	d2.Wait()
	syscall.Kill(-rec.Browser.PGID, syscall.SIGKILL)
	time.Sleep(100 * time.Millisecond)
	d3 := spawn()
	defer func() { d3.Process.Signal(syscall.SIGTERM); d3.Wait() }()
	c3 := dialed()
	defer c3.Close()
	waitFor(t, "the stream to end", 5*time.Second, func() bool { return status(t, c3).Stream == nil })
	if !srv.Deleted(id) {
		t.Fatal("not deleted on the server")
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "host-state.json")); !os.IsNotExist(err) {
		t.Fatal("record left behind")
	}
	logs, _ := os.ReadFile(cfgPath + ".log")
	for _, s := range []string{pin, testKey, rec.PublishToken, rec.Attachment.Key} {
		if strings.Contains(string(logs), s) {
			t.Fatal("a secret is in the daemon log")
		}
	}
}

// TestSocketActivated runs the daemon under systemd-socket-activate: it
// takes both listeners from LISTEN_FDS (sd_listen_fds in pure Go), reports
// socket_activated, serves a share, and leaves the socket path to its owner.
func TestSocketActivated(t *testing.T) {
	act, err := exec.LookPath("systemd-socket-activate")
	if err != nil {
		t.Skip("needs systemd-socket-activate")
	}
	dir := t.TempDir()
	srv, err := hosttest.NewFakeServer(testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	keyPath := filepath.Join(dir, "key")
	os.WriteFile(keyPath, []byte(testKey), 0o600)
	runtime := filepath.Join(dir, "run")
	os.MkdirAll(runtime, 0o700)
	sock := filepath.Join(runtime, "host.sock")
	port := freePort(t)
	pc := procConfig{Port: port, ServerURL: srv.URL, KeyPath: keyPath, RuntimeDir: runtime, StateDir: filepath.Join(dir, "state"),
		Profile: filepath.Join(dir, "profile"), Browser: hosttest.BrowserCommand(os.Args[0], "page", "-"), SocketActivated: true}
	cfgPath := filepath.Join(dir, "daemon.json")
	b, _ := json.Marshal(pc)
	os.WriteFile(cfgPath, b, 0o600)
	cmd := exec.Command(act, "--now", "-l", "127.0.0.1:"+strconv.Itoa(port), "-l", sock, "--", os.Args[0], daemonArg, cfgPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Signal(syscall.SIGTERM); cmd.Wait() }()
	var c *hosttest.Client
	waitFor(t, "the activated daemon", 5*time.Second, func() bool {
		cl, err := hosttest.DialClient(sock)
		if err != nil || cl.Hello("cli") != nil {
			return false
		}
		c = cl
		return true
	})
	defer c.Close()
	hs := status(t, c)
	if hs.Daemon.PortMode != "socket_activated" || hs.Daemon.PortOwner != "ok" {
		e, _ := os.ReadFile(cfgPath + ".err")
		t.Fatalf("daemon %+v %s", hs.Daemon, e)
	}
	call(t, c, "subscribe", map[string]any{})
	call(t, c, "start", map[string]any{})
	if _, ok := c.WaitEvent(func(ev proto.Event) bool { return ev.Event == "live" }, 10*time.Second); !ok {
		t.Fatal("never live")
	}
	call(t, c, "stop", map[string]any{})
	call(t, c, "shutdown", map[string]any{})
	cmd.Wait()
	if _, err := os.Lstat(sock); err != nil {
		t.Fatal("the daemon unlinked a socket it didn't create")
	}
}

// TestStoppedDaemonLeavesTheStream: stopping a daemon (an in-process
// restart) must not end the stream later from a stale detach timer.
func TestStoppedDaemonLeavesTheStream(t *testing.T) {
	e := newEnv(t, func(e *env) { e.cfg.DetachGrace = 300 * time.Millisecond })
	c := e.client("cli")
	id := e.startLive(c, map[string]any{})
	e.restart()
	time.Sleep(800 * time.Millisecond) // past the old daemon's grace
	c2 := e.client("cli")
	waitFor(t, "re-attached", 5*time.Second, func() bool {
		hs := status(t, c2)
		return hs.Stream != nil && hs.Stream.ID == id && hs.Stream.Attachment == "attached"
	})
	if e.srv.Deleted(id) {
		t.Fatal("the stopped daemon ended the stream")
	}
}
