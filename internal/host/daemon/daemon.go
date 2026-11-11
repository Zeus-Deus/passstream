// Package daemon is `passstream host`: the local daemon that serves the
// sender page on 127.0.0.1:47823, answers the plugin and the CLI on the Unix
// socket, supervises the sender browser and keeps the recovery record
// (HOST-CONTROL §2, §5; CONTRACT §10, §11).
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/passstream/passstream/internal/host/keyfile"
	"github.com/passstream/passstream/internal/host/listen"
	"github.com/passstream/passstream/internal/host/passglue"
	"github.com/passstream/passstream/internal/host/procnet"
	"github.com/passstream/passstream/internal/host/proto"
	"github.com/passstream/passstream/internal/host/serverapi"
	"github.com/passstream/passstream/internal/host/state"
	"github.com/passstream/passstream/internal/host/supervise"
)

// DefaultPort is the sender origin's port. It's fixed because the sender's
// origin storage is keyed by it (HOST-CONTROL §5.1).
const DefaultPort = 47823

// Peer checks on the sender WebSocket (HOST-CONTROL §5.2, §11 Q1). The
// launch key always travels in a 0600 launch file, never in argv.
const (
	PeerTuple      = "tuple"       // check 1 via /proc/net/tcp
	PeerLaunchFile = "launch_file" // no check 1
)

// Config configures the daemon. Zero values get defaults from Defaults.
type Config struct {
	Version   string
	ServerURL string // normalised base; "" when unconfigured
	Pass      passglue.Config
	KeyPath   string
	// RuntimeDir is $XDG_RUNTIME_DIR/passstream; StateDir is
	// $XDG_STATE_HOME/passstream.
	RuntimeDir string
	StateDir   string
	Port       int
	// Browser is the browser command and extra flags; Profile its
	// dedicated --user-data-dir.
	Browser []string
	Profile string
	// LocalPolicy and LocalKnock are the local defaults (config file).
	LocalPolicy *proto.PinPolicy
	LocalKnock  bool
	PeerCheck   string
	IdleExit    time.Duration // <0 disables

	// Overridable for tests.
	APITransport http.RoundTripper // nil: the default transport
	Proc         procnet.Root      // the port-owner check
	PeerProc     procnet.Root      // the peer-uid check; "" means Proc
	ProcRoot     string            // for the supervisor
	Assets       fs.FS             // the built SPA (web/dist)
	Supervision  string            // "" = systemd when socket-activated, else process_group
	UnitActive   func() bool
	Logf         func(format string, a ...any)
	// OnRequest sees every request line on the daemon socket. It's a test
	// hook (the plugin harness records what the plugin sent); the daemon
	// itself never logs requests.
	OnRequest func(line []byte)
	// StatusDelay holds status replies and a new subscriber's events
	// (a plugin-harness hook for restart races).
	CaptureIdle           time.Duration
	PublishConfirmTimeout time.Duration
	StatusDelay           time.Duration
	LaunchTTL             time.Duration
	DetachGrace           time.Duration
	CommandTimeout        time.Duration
	AuthTimeout           time.Duration
	StopWait              time.Duration
	PollEvery             time.Duration
	SaveRetry             time.Duration // first retry of a failed record write
}

func (c *Config) defaults() {
	if c.CaptureIdle == 0 {
		c.CaptureIdle = 30 * time.Second
	}
	if c.PublishConfirmTimeout == 0 {
		c.PublishConfirmTimeout = 30 * time.Second
	}
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.PeerCheck == "" {
		c.PeerCheck = PeerTuple
	}
	if c.IdleExit == 0 {
		c.IdleExit = 60 * time.Second
	}
	if c.LaunchTTL == 0 {
		c.LaunchTTL = 60 * time.Second
	}
	if c.DetachGrace == 0 {
		c.DetachGrace = 10 * time.Second
	}
	if c.CommandTimeout == 0 {
		c.CommandTimeout = 8 * time.Second
	}
	if c.AuthTimeout == 0 {
		c.AuthTimeout = 5 * time.Second
	}
	if c.StopWait == 0 {
		c.StopWait = 2 * time.Second
	}
	if c.PollEvery == 0 {
		c.PollEvery = 2 * time.Second
	}
	if c.SaveRetry == 0 {
		c.SaveRetry = time.Second
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if len(c.Browser) == 0 {
		c.Browser = []string{"chromium"}
	}
	if c.Version == "" {
		c.Version = "dev"
	}
}

// Daemon is one running `passstream host`.
type Daemon struct {
	cfg  Config
	ln   *listen.Listeners
	sup  *supervise.Supervisor
	http *http.Server
	uid  int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu         sync.Mutex
	st         *stream
	clients    map[*client]bool
	keyStatus  string
	keyAt      time.Time
	server     string // ok | unreachable | unauthorized
	portOwner  string
	portAt     time.Time
	idleSince  time.Time
	startBusy  bool
	statusLast []byte
	statusMat  string
	statusAt   time.Time
	statusTmr  *time.Timer
	cmdSeq     uint64
	// incarnation prefixes command ids: a restarted daemon's counter starts
	// again at 1, and the page must never mistake a new command for an old one.
	incarnation   string
	featureTokens map[string]featureToken
}

// New prepares a daemon on the given listeners.
func New(cfg Config, ln *listen.Listeners) (*Daemon, error) {
	cfg.defaults()
	if cfg.Supervision == "" {
		cfg.Supervision = supervise.ProcessGroup
		if ln.Mode == listen.SocketActivated {
			cfg.Supervision = supervise.Systemd
		}
	}
	if cfg.UnitActive == nil {
		cfg.UnitActive = unitActive
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{
		cfg: cfg, ln: ln, uid: os.Geteuid(), ctx: ctx, cancel: cancel,
		clients: map[*client]bool{}, server: "ok", idleSince: time.Now(),
		incarnation: proto.Encode(proto.RandBytes(9)),
	}
	d.sup = supervise.New(supervise.Config{Command: cfg.Browser, Profile: cfg.Profile, Mode: cfg.Supervision, Proc: cfg.ProcRoot, Logf: cfg.Logf})
	if err := listen.PrivateDir(cfg.RuntimeDir); err != nil {
		return nil, err
	}
	if err := listen.PrivateDir(cfg.StateDir); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Daemon) logf(format string, a ...any) { d.cfg.Logf(format, a...) }

func (d *Daemon) statePath() string { return filepath.Join(d.cfg.StateDir, "host-state.json") }

func nowMs() int64 { return time.Now().UnixMilli() }

// Run serves until ctx ends, the idle timer fires or shutdown is requested.
func (d *Daemon) Run(ctx context.Context) error {
	go func() {
		select {
		case <-ctx.Done():
			d.cancel()
		case <-d.ctx.Done():
		}
	}()
	d.recover()
	d.http = &http.Server{Handler: d.senderHandler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		ErrorLog: nil}
	d.wg.Add(2)
	go func() { defer d.wg.Done(); d.http.Serve(d.ln.HTTP) }()
	go func() { defer d.wg.Done(); d.serveUnix() }()
	d.logf("host: serving (%s, %s, peer check %s)", d.ln.Mode, d.cfg.Supervision, d.cfg.PeerCheck)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-d.ctx.Done():
			d.shutdown()
			return nil
		case <-tick.C:
			if d.idle() {
				d.logf("host: idle exit")
				d.cancel()
			}
		}
	}
}

// Stop asks the daemon to exit (without ending a stream: the browser and
// the record survive, as after a crash).
func (d *Daemon) Stop() { d.cancel() }

func (d *Daemon) idle() bool {
	if d.cfg.IdleExit < 0 {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st != nil || len(d.clients) > 0 || d.startBusy {
		d.idleSince = time.Now()
		return false
	}
	return time.Since(d.idleSince) >= d.cfg.IdleExit
}

func (d *Daemon) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.mu.Lock()
	if st := d.st; st != nil {
		st.stopWorkers()
		if st.page != nil {
			st.page.close()
		}
	}
	for c := range d.clients {
		c.close()
	}
	d.mu.Unlock()
	d.ln.Unix.Close()
	if d.http != nil {
		d.http.Shutdown(ctx)
	}
	d.ln.HTTP.Close()
	d.wg.Wait()
}

// unitActive is the socket-activated pre-launch check (HOST-CONTROL §2.2).
func unitActive() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return runQuiet(ctx, "systemctl", "--user", "is-active", "--quiet", "passstream-host.socket") == nil
}

// keyState reads the key file (callers cache the status for 2 s).
func (d *Daemon) keyState() (string, string) {
	key, st := keyfile.Read(d.cfg.KeyPath)
	return key, st
}

func (d *Daemon) daemonStatus() proto.DaemonStatus {
	d.mu.Lock()
	refreshKey := time.Since(d.keyAt) > 2*time.Second
	refreshPort := time.Since(d.portAt) > 2*time.Second
	d.mu.Unlock()
	if refreshKey {
		_, st := d.keyState()
		d.mu.Lock()
		if !(st == keyfile.OK && d.keyStatus == "rejected") {
			d.keyStatus = st
		}
		d.keyAt = time.Now()
		d.mu.Unlock()
	}
	if refreshPort {
		po := "ok"
		if d.cfg.Proc.CheckOwner(d.cfg.Port, d.uid) != nil {
			po = "not_owned"
		}
		d.mu.Lock()
		d.portOwner, d.portAt = po, time.Now()
		d.mu.Unlock()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return proto.DaemonStatus{
		Version: d.cfg.Version, ServerURL: d.cfg.ServerURL, Key: d.keyStatus, Server: d.server,
		PortMode: d.ln.Mode, Supervision: d.cfg.Supervision, PortOwner: d.portOwner,
	}
}

// hostStatus builds HostStatus. It never contains the PIN or the link.
func (d *Daemon) hostStatus() proto.HostStatus {
	ds := d.daemonStatus()
	d.mu.Lock()
	defer d.mu.Unlock()
	return proto.HostStatus{Daemon: ds, Stream: d.streamStatusLocked()}
}

func (d *Daemon) streamStatusLocked() *proto.StreamStatus {
	st := d.st
	if st == nil {
		return nil
	}
	s := &proto.StreamStatus{
		ID: st.rec.StreamID, Label: st.rec.Label, Phase: "starting", Attachment: st.attachmentName(),
		Mode: st.rec.Mode, Safe: st.rec.Safe, StartedAt: st.rec.StartedAt, LiveAt: st.rec.LiveAt, Viewers: []proto.Viewer{},
		HasLink: st.secrets.have,
	}
	if ss := st.state; ss != nil {
		s.Phase = ss.Phase
		s.Mode = ss.Mode
		s.Safe = ss.Safe
		acc := ss.Access
		// The daemon's own latches only as they're on disk: a latch whose
		// write failed was refused to its caller (HOST-CONTROL §5.6).
		acc.Latches = acc.Latches.Or(st.saved)
		s.Access = &acc
		e := ss.E2EE
		s.E2EE = &e
		s.Health = ss.Health
	}
	if st.notDurable && s.E2EE != nil {
		// The daemon's mirror of the latches isn't on disk: the same
		// "revocation not durable" warning the page raises for its own record.
		s.E2EE.RevocationNotDurable = true
	}
	if st.ending {
		s.Phase = "ended"
	}
	s.Viewers = st.viewers.Viewers
	if s.Viewers == nil {
		s.Viewers = []proto.Viewer{}
	}
	s.Watching, s.Knocking = int(st.viewers.Watching), int(st.viewers.Knocking)
	return s
}

// changed schedules a status event: material changes promptly, stats-only
// changes within a second, never more than 4 per second (Appendix A #2).
func (d *Daemon) changed() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scheduleLocked()
}

func (d *Daemon) scheduleLocked() {
	if d.statusTmr != nil {
		return
	}
	wait := 250*time.Millisecond - time.Since(d.statusAt)
	if wait < 0 {
		wait = 0
	}
	d.statusTmr = time.AfterFunc(wait, d.flushStatus)
}

func (d *Daemon) flushStatus() {
	hs := d.hostStatus()
	full, _ := json.Marshal(hs)
	mat := materialKey(hs)
	d.mu.Lock()
	d.statusTmr = nil
	if string(full) == string(d.statusLast) {
		d.mu.Unlock()
		return
	}
	if mat == d.statusMat && time.Since(d.statusAt) < time.Second {
		// Statistics only: coalesce to one per second.
		d.statusTmr = time.AfterFunc(time.Second-time.Since(d.statusAt), d.flushStatus)
		d.mu.Unlock()
		return
	}
	d.statusLast, d.statusMat, d.statusAt = full, mat, time.Now()
	var view []byte
	if st := d.st; st != nil && st.page != nil {
		view, _ = json.Marshal(map[string]any{"type": "view", "list": st.viewers})
		st.page.send(view)
	}
	d.broadcastLocked("status", json.RawMessage(full), true)
	d.mu.Unlock()
}

// materialKey is the status without health and connection statistics.
func materialKey(hs proto.HostStatus) string {
	if hs.Stream == nil {
		b, _ := json.Marshal(hs)
		return string(b)
	}
	s := *hs.Stream
	s.Health = nil
	vs := make([]proto.Viewer, len(s.Viewers))
	for i, v := range s.Viewers {
		cs := make([]proto.Connection, len(v.Connections))
		for j, c := range v.Connections {
			c.Stats, c.StatsAt = nil, nil
			cs[j] = c
		}
		v.Connections = cs
		vs[i] = v
	}
	s.Viewers = vs
	b, _ := json.Marshal(proto.HostStatus{Daemon: hs.Daemon, Stream: &s})
	return string(b)
}

// broadcastLocked sends an event to every subscribed client.
func (d *Daemon) broadcastLocked(name string, data any, isStatus bool) {
	b, err := json.Marshal(proto.Event{Event: name, Data: data})
	if err != nil {
		return
	}
	b = append(b, '\n')
	for c := range d.clients {
		if c.subscribed() {
			if isStatus && c.holdUntil.After(time.Now()) {
				continue
			}
			c.send(b)
		}
	}
}

func (d *Daemon) event(name string, data any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.broadcastLocked(name, data, false)
}

// ErrRunning is returned when another daemon holds the lock.
var ErrRunning = errors.New("another passstream host is running")

// Paths resolves the XDG locations.
type Paths struct {
	RuntimeDir, StateDir, ConfigDir string
}

// DefaultPaths reads XDG_RUNTIME_DIR, XDG_STATE_HOME and XDG_CONFIG_HOME
// with their standard fallbacks. The browser profile lives in
// StateDir/chromium (HOST-CONTROL §5.3).
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		return Paths{}, fmt.Errorf("XDG_RUNTIME_DIR is not set")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" && filepath.IsAbs(v) {
			return v
		}
		return def
	}
	return Paths{
		RuntimeDir: filepath.Join(rt, "passstream"),
		StateDir:   filepath.Join(env("XDG_STATE_HOME", filepath.Join(home, ".local", "state")), "passstream"),
		ConfigDir:  filepath.Join(env("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "passstream"),
	}, nil
}

// api returns a server client, or the reason there's none.
func (d *Daemon) api() (*serverapi.Client, error) {
	if d.cfg.ServerURL == "" {
		return nil, proto.E("server_unreachable", "no server URL configured (passstream host --server or ~/.config/passstream/config)")
	}
	key, st := d.keyState()
	d.mu.Lock()
	if !(st == keyfile.OK && d.keyStatus == "rejected") {
		d.keyStatus = st
	}
	d.keyAt = time.Now()
	d.mu.Unlock()
	if st != keyfile.OK {
		return nil, proto.E("key_"+st, "the owner key at %s is %s", d.cfg.KeyPath, st)
	}
	c := serverapi.New(d.cfg.ServerURL, key)
	if d.cfg.APITransport != nil {
		c.HTTP.Transport = d.cfg.APITransport
	}
	return c, nil
}

// noteServer records the outcome of a server call for HostStatus.
func (d *Daemon) noteServer(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	prev := d.server + d.keyStatus
	switch e := proto.AsError(err); {
	case err == nil:
		d.server = "ok"
		if d.keyStatus == "rejected" {
			d.keyStatus = keyfile.OK
		}
	case e.Code == "key_rejected":
		d.server, d.keyStatus = "unauthorized", "rejected"
	case e.Code == "server_unreachable":
		d.server = "unreachable"
	}
	if d.server+d.keyStatus != prev {
		d.scheduleLocked()
	}
}

// localAddr is the accepted connection's local address.
func localAddr(r *http.Request) net.Addr {
	a, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	return a
}

// saveFailAlert is how many failed writes in a row raise the
// not-durable error state.
const saveFailAlert = 3

// saveLocked writes the recovery record (fsync) while holding d.mu. A
// failed write leaves the mirror dirty: the in-memory record is ahead of
// the disk, so every latch command and state_ack must write again before
// it's acknowledged, and a timer keeps retrying. After saveFailAlert
// failures in a row the stream reports revocation_not_durable (and the
// matching alert) until a write succeeds.
func (d *Daemon) saveLocked(st *stream) error {
	if err := state.Save(d.statePath(), st.rec); err != nil {
		d.logf("host: can't write the recovery record: %v", err)
		st.dirty = true
		st.saveFails++
		if st.saveFails >= saveFailAlert && !st.notDurable {
			st.notDurable = true
			d.broadcastLocked("alert", map[string]any{"kind": "revocation_durability", "not_durable": true}, false)
			d.scheduleLocked()
		}
		if st.saveRetry == nil && !st.ending {
			wait := time.Duration(st.saveFails) * d.cfg.SaveRetry
			if wait > 5*d.cfg.SaveRetry {
				wait = 5 * d.cfg.SaveRetry
			}
			st.saveRetry = time.AfterFunc(wait, func() { d.retrySave(st) })
		}
		return proto.E("internal", "can't write the recovery record")
	}
	st.dirty, st.saveFails, st.saved = false, 0, st.rec.Latches
	if st.saveRetry != nil {
		st.saveRetry.Stop()
		st.saveRetry = nil
	}
	if st.notDurable {
		st.notDurable = false
		d.broadcastLocked("alert", map[string]any{"kind": "revocation_durability", "not_durable": false}, false)
		d.scheduleLocked()
	}
	return nil
}

func (d *Daemon) retrySave(st *stream) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st.saveRetry = nil
	if d.st == st && !st.ending && st.dirty && d.ctx.Err() == nil {
		d.saveLocked(st)
	}
}
