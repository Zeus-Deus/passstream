package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/passstream/passstream/internal/host/capture"
	"github.com/passstream/passstream/internal/host/listen"
	"github.com/passstream/passstream/internal/host/proto"
	"github.com/passstream/passstream/internal/host/serverapi"
	"github.com/passstream/passstream/internal/host/state"
	"github.com/passstream/passstream/internal/host/supervise"
	"github.com/passstream/passstream/internal/hostview"
)

// Attachment states (CONTRACT §5.2).
const (
	attLaunching = "launching"
	attHandover  = "handover"
	attAttached  = "attached"
	attDetached  = "detached"
)

// launchKey is minted per Go-live and lives in memory only.
type launchKey struct {
	id     string
	idb    []byte
	key    []byte
	minted time.Time
	file   string // guarded launch file
}

// secrets are the PIN and link, in memory only (HOST-CONTROL §5.7).
type secrets struct {
	have    bool
	pin     *string
	pending *string
	link    string
}

type stream struct {
	captures         capture.Store
	featureBusy      bool
	frameDemandUntil time.Time
	demandTimer      *time.Timer
	demandPage       *pageConn
	publish          *publishRequest
	rec              *state.Record
	att              string
	launch           *launchKey
	retired          map[string]string // launch key id -> error code
	explicitPin      *string           // from start; memory only
	page             *pageConn
	exchange         chan struct{} // serialises the exchanges of this stream (a 1-slot lock)

	state    *proto.SenderState
	stateAt  int64
	secrets  secrets
	liveSent bool
	conns    []proto.Connection
	mem      hostview.JoinMemory
	viewers  proto.ViewerList
	actions  map[string]*localAction
	pending  map[string]chan pageResult

	// dirty: rec has changes the disk doesn't (a failed save); saveRetry
	// retries it and notDurable is the reported error state. saved is the
	// latches as last written: the only ones the daemon reports itself.
	dirty      bool
	saved      proto.Latches
	saveFails  int
	saveRetry  *time.Timer
	notDurable bool

	ending      bool
	endDone     chan struct{}
	launchTimer *time.Timer
	detachTimer *time.Timer
	workers     context.CancelFunc
	workerCtx   context.Context
}

func (st *stream) attachmentName() string {
	switch st.att {
	case attHandover, attLaunching:
		return "launching"
	case attAttached:
		return "attached"
	}
	return "detached"
}

func (st *stream) stopWorkers() {
	if st.demandTimer != nil {
		st.demandTimer.Stop()
	}
	if st.publish != nil {
		st.publish.timer.Stop()
		if st.publish.cancel != nil {
			st.publish.cancel()
		}
		st.publish.data = nil
	}
	if st.workers != nil {
		st.workers()
	}
	if st.launchTimer != nil {
		st.launchTimer.Stop()
	}
	if st.detachTimer != nil {
		st.detachTimer.Stop()
	}
	if st.saveRetry != nil {
		st.saveRetry.Stop()
		st.saveRetry = nil
	}
}

func newStream(rec *state.Record) *stream {
	ctx, cancel := context.WithCancel(context.Background())
	st := &stream{rec: rec, retired: map[string]string{}, pending: map[string]chan pageResult{},
		exchange: make(chan struct{}, 1), workerCtx: ctx, workers: cancel, endDone: make(chan struct{})}
	st.viewers = proto.ViewerList{Source: "local", Viewers: []proto.Viewer{}}
	st.saved = rec.Latches // a new record is written before use, a recovered one was read
	return st
}

// recover reloads host-state.json at startup, re-adopts the browser and
// puts the attachment in detached (HOST-CONTROL §5.3 "Daemon side").
func (d *Daemon) recover() {
	rec, err := state.Load(d.statePath())
	if err != nil {
		d.logf("host: ignoring an unreadable recovery record: %v", err)
		return
	}
	if rec == nil {
		return
	}
	st := newStream(rec)
	st.att = attDetached
	d.mu.Lock()
	d.st = st
	d.mu.Unlock()
	pidfd, ok := d.sup.Adopt(rec.Browser)
	if !ok || rec.Attachment == nil {
		// The browser is gone, or the launch key died with the old daemon
		// before an attachment key existed: the launch can't finish.
		d.logf("host: stream %s can't be recovered (browser gone or no attachment)", rec.StreamID)
		reason := "window_closed"
		if rec.Attachment == nil {
			reason = "browser_failed" // the launch never attached
		}
		go d.end(st, reason)
		return
	}
	supervise.Watch(st.workerCtx, pidfd, func() { d.browserExited(st) })
	d.mu.Lock()
	st.detachTimer = time.AfterFunc(d.cfg.DetachGrace, func() { d.detachExpired(st) })
	d.mu.Unlock()
	d.startWorkers(st)
	d.logf("host: recovered stream %s, waiting for the sender page", rec.StreamID)
	d.changed()
}

// startArgs is the validated start request.
type startArgs struct {
	policy   *proto.PinPolicy
	pin      *string
	knock    bool
	label    *string
	mode     string
	byClient string
}

func parseStart(args map[string]any) (startArgs, error) {
	var a startArgs
	if err := closed(args, "pin_policy", "pin", "knock", "label", "mode"); err != nil {
		return a, err
	}
	a.mode = "text"
	if v, ok := args["pin_policy"]; ok {
		p, err := proto.ParsePinPolicy(v)
		if err != nil {
			return a, err
		}
		a.policy = &p
	}
	if v, ok := args["pin"]; ok {
		s, err := pinArg(v)
		if err != nil {
			return a, err
		}
		a.pin = &s
	}
	if v, ok := args["knock"]; ok {
		b, ok := v.(bool)
		if !ok {
			return a, proto.E("invalid_argument", "knock must be a boolean")
		}
		a.knock = b
	}
	if v, ok := args["label"]; ok && v != nil {
		s, ok := v.(string)
		if !ok {
			return a, proto.E("invalid_argument", "label must be a string or null")
		}
		s = proto.Clean(s, 64, 256)
		a.label = &s
	}
	if v, ok := args["mode"]; ok {
		if v != "text" && v != "motion" {
			return a, proto.E("invalid_argument", "mode must be text or motion")
		}
		a.mode = v.(string)
	}
	if a.pin != nil && a.policy == nil {
		return a, proto.E("invalid_argument", "pin is allowed only with pin_policy")
	}
	if a.policy != nil {
		if err := checkPinForPolicy(*a.policy, a.pin); err != nil {
			return a, err
		}
	}
	return a, nil
}

// pinArg bounds a PIN or passphrase value. The sender page does the exact
// checks (NFC, 8–256 bytes); the daemon only refuses what can't be one.
func pinArg(v any) (string, error) {
	s, ok := v.(string)
	if !ok || s == "" || len(s) > 1024 {
		return "", proto.E("invalid_argument", "pin must be a non-empty string")
	}
	return s, nil
}

func checkPinForPolicy(p proto.PinPolicy, pin *string) error {
	switch p.Kind {
	case "passphrase":
		if pin == nil {
			return proto.E("passphrase_required", "a passphrase policy needs a passphrase")
		}
	case "none":
		if pin != nil {
			return proto.E("invalid_argument", "no pin with policy none")
		}
	case "digits":
		if pin != nil {
			if len(*pin) != p.Length {
				return proto.E("invalid_argument", "the pin must have %d digits", p.Length)
			}
			for _, c := range *pin {
				if c < '0' || c > '9' {
					return proto.E("invalid_argument", "the pin must be digits")
				}
			}
		}
	}
	return nil
}

// start is the launch sequence (HOST-CONTROL §5.4). It replies after
// durable creation and a successful browser spawn (Appendix A #3).
func (d *Daemon) start(c *client, args map[string]any) (any, error) {
	a, err := parseStart(args)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.st != nil || d.startBusy {
		d.mu.Unlock()
		return nil, proto.E("already_live", "a share is already running")
	}
	d.startBusy = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.startBusy = false
		d.mu.Unlock()
	}()

	api, err := d.api()
	if err != nil {
		d.changed()
		return nil, err
	}
	if err := d.portCheck(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(d.ctx, 20*time.Second)
	defer cancel()
	set, err := api.Settings(ctx)
	d.noteServer(err)
	if err != nil {
		return nil, err
	}
	// Creation policy (HOST-CONTROL §4.4, Appendix A #7).
	policy := proto.DefaultPolicy
	if d.cfg.LocalPolicy != nil {
		policy = *d.cfg.LocalPolicy
	}
	explicit := a.policy != nil
	if explicit {
		policy = *a.policy
	} else {
		policy = proto.Stricter(policy, set.Defaults.Pin)
	}
	knock := a.knock || d.cfg.LocalKnock || set.Defaults.Knock
	if policy.Kind == "passphrase" && a.pin == nil {
		return nil, proto.E("passphrase_required", "the policy is a passphrase and none was supplied")
	}
	created, err := api.Create(ctx, serverapi.CreateStream{Label: a.label, Pin: policy, PinExplicit: true, Knock: knock, SenderKind: "local"})
	d.noteServer(err)
	if err != nil {
		return nil, err
	}
	rec := &state.Record{V: state.Version, StreamID: created.ID, ServerURL: d.cfg.ServerURL, PublishToken: created.PublishToken,
		ViewLinkBase: created.ViewLinkBase, Label: a.label, Mode: a.mode, Policy: policy, Knock: knock, StartedAt: nowMs()}
	st := newStream(rec)
	st.att = attLaunching
	st.explicitPin = a.pin
	fail := func(e error) (any, error) {
		d.mu.Lock()
		st.stopWorkers()
		d.mu.Unlock()
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		api.Delete(dctx, created.ID)
		dcancel()
		state.Remove(d.statePath())
		d.mu.Lock()
		if d.st == st {
			d.st = nil
		}
		if st.launch != nil && st.launch.file != "" {
			os.Remove(st.launch.file)
		}
		d.mu.Unlock()
		d.changed()
		return nil, e
	}
	if err := state.Save(d.statePath(), rec); err != nil {
		d.logf("host: can't write the recovery record: %v", err)
		return fail(proto.E("internal", "can't write the recovery record"))
	}
	lk, appURL, err := d.mintLaunch(created.ID)
	if err != nil {
		return fail(proto.E("browser_failed", "can't prepare the launch"))
	}
	d.mu.Lock()
	st.launch = lk
	d.st = st
	d.mu.Unlock()
	// The daemon checks the port again right before it spawns a browser.
	if err := d.portCheck(); err != nil {
		return fail(err)
	}
	b, err := d.sup.Launch(created.ID, appURL)
	if err != nil {
		d.logf("host: browser launch failed: %v", err)
		return fail(proto.E("browser_failed", "the sender browser didn't start"))
	}
	pidfd, err := d.sup.Open(b)
	if err != nil {
		d.sup.Stop(b)
		return fail(proto.E("browser_failed", "the sender browser exited at once"))
	}
	d.mu.Lock()
	rec.Browser = b
	err = d.saveLocked(st)
	if err == nil {
		st.launchTimer = time.AfterFunc(d.cfg.LaunchTTL, func() { d.launchExpired(st, lk) })
	}
	d.mu.Unlock()
	if err != nil {
		d.sup.Stop(b)
		return fail(err)
	}
	supervise.Watch(st.workerCtx, pidfd, func() { d.browserExited(st) })
	d.startWorkers(st)
	d.logf("host: stream %s created, browser started", created.ID)
	d.changed()
	return map[string]any{"stream_id": created.ID}, nil
}

// portCheck is HOST-CONTROL §2.2 check 2 with Appendix A #4–6.
func (d *Daemon) portCheck() error {
	err := d.cfg.Proc.CheckOwner(d.cfg.Port, d.uid)
	d.mu.Lock()
	d.portOwner, d.portAt = "ok", time.Now()
	if err != nil {
		d.portOwner = "not_owned"
	}
	d.mu.Unlock()
	if err != nil {
		return err
	}
	switch d.ln.Mode {
	case listen.SocketActivated:
		if !d.cfg.UnitActive() {
			return proto.E("port_not_owned", "passstream-host.socket isn't active").WithDetails(map[string]any{"reason": "socket_unit_inactive"})
		}
	case listen.SelfBound:
	default:
		return proto.E("port_not_owned", "unknown port mode")
	}
	return nil
}

// mintLaunch makes the launch key and the --app URL: a 0600 launch file
// that redirects to the sender with the key in the fragment (HOST-CONTROL
// §5.2). The launch file is the only delivery, so no argv ever holds the key.
func (d *Daemon) mintLaunch(streamID string) (*launchKey, string, error) {
	idb := proto.RandBytes(16)
	key := proto.RandBytes(32)
	lk := &launchKey{id: proto.Encode(idb), idb: idb, key: key, minted: time.Now()}
	target := fmt.Sprintf("http://127.0.0.1:%d/sender#i=%s&k=%s", d.cfg.Port, lk.id, proto.Encode(key))
	name := filepath.Join(d.cfg.RuntimeDir, "launch-"+proto.Encode(proto.RandBytes(12))+".html")
	page := `<!doctype html><meta charset="utf-8"><meta name="referrer" content="no-referrer">` +
		`<meta http-equiv="refresh" content="0;url=` + target + `"><title>PassStream</title>`
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", err
	}
	_, err = f.WriteString(page)
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		os.Remove(name)
		return nil, "", err
	}
	lk.file = name
	return lk, (&url.URL{Scheme: "file", Path: name}).String(), nil
}

// retireLaunchLocked forgets the launch key (HOST-CONTROL §5.3 step 3).
func (d *Daemon) retireLaunchLocked(st *stream, code string) {
	if st.launch == nil {
		return
	}
	st.retired[st.launch.id] = code
	if st.launch.file != "" {
		os.Remove(st.launch.file)
	}
	st.launch = nil
	if st.launchTimer != nil {
		st.launchTimer.Stop()
	}
}

func (d *Daemon) launchExpired(st *stream, lk *launchKey) {
	if d.ctx.Err() != nil {
		return
	}
	d.mu.Lock()
	if d.st != st || st.launch != lk {
		d.mu.Unlock()
		return
	}
	d.retireLaunchLocked(st, "launch_expired")
	attached := st.att == attAttached
	d.mu.Unlock()
	if !attached {
		d.logf("host: the sender page didn't attach within %s", d.cfg.LaunchTTL)
		d.end(st, "browser_failed")
	}
}

func (d *Daemon) detachExpired(st *stream) {
	if d.ctx.Err() != nil {
		return
	}
	d.mu.Lock()
	ok := d.st == st && st.att == attDetached && !st.ending
	d.mu.Unlock()
	if ok {
		d.logf("host: the sender page didn't come back within %s", d.cfg.DetachGrace)
		d.end(st, "window_closed")
	}
}

func (d *Daemon) browserExited(st *stream) {
	if d.ctx.Err() != nil {
		return
	}
	d.mu.Lock()
	ok := d.st == st && !st.ending
	d.mu.Unlock()
	if ok {
		d.logf("host: the sender browser exited")
		d.end(st, "window_closed")
	}
}

// end terminates the stream: tell the page, delete it on the server (best
// effort), stop the browser, delete the record, then report it. The stream
// shows phase "ended" until all of that is done (Appendix A #18).
func (d *Daemon) end(st *stream, reason string) { d.endFrom(st, nil, reason) }

// endFrom is end on behalf of a page socket: nothing happens unless that
// page is still the stream's current one (a superseded page can't end its
// replacement). A nil page is the daemon itself.
func (d *Daemon) endFrom(st *stream, from *pageConn, reason string) {
	d.mu.Lock()
	if from != nil && (d.st != st || st.page != from || st.ending) {
		d.mu.Unlock()
		return
	}
	if st.ending {
		d.mu.Unlock()
		<-st.endDone // a concurrent end finishes the termination
		return
	}
	if d.st != st {
		d.mu.Unlock()
		return
	}
	st.ending = true
	st.captures.Close()
	for token, credential := range d.featureTokens {
		if credential.stream == st {
			delete(d.featureTokens, token)
		}
	}
	defer close(st.endDone)
	st.stopWorkers()
	page := st.page
	for id, ch := range st.pending {
		close(ch)
		delete(st.pending, id)
	}
	d.scheduleLocked()
	d.mu.Unlock()
	if page != nil {
		b, _ := json.Marshal(map[string]any{"type": "ended", "reason": reason})
		page.send(b)
		time.Sleep(50 * time.Millisecond)
		page.close()
	}
	if reason != "deleted" && reason != "expired" {
		if api, err := d.api(); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = api.Delete(ctx, st.rec.StreamID)
			cancel()
			if err != nil {
				d.logf("host: server delete failed: %s", proto.AsError(err).Code)
			}
		}
	}
	d.sup.Stop(st.rec.Browser)
	d.mu.Lock()
	d.retireLaunchLocked(st, "launch_consumed")
	if err := state.Remove(d.statePath()); err != nil {
		d.logf("host: can't remove the recovery record: %v", err)
	}
	d.st = nil
	d.idleSince = time.Now()
	d.broadcastLocked("ended", map[string]any{"stream_id": st.rec.StreamID, "reason": reason}, false)
	d.scheduleLocked()
	d.mu.Unlock()
	d.logf("host: stream %s ended (%s)", st.rec.StreamID, reason)
}

// startWorkers polls the server's connections (for the join) and follows
// its events (for a server-side end) while the stream exists.
func (d *Daemon) startWorkers(st *stream) {
	ctx := st.workerCtx
	go func() {
		t := time.NewTicker(d.cfg.PollEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			api, err := d.api()
			var conns []proto.Connection
			if err == nil {
				c, cancel := context.WithTimeout(ctx, 5*time.Second)
				conns, err = api.Connections(c, st.rec.StreamID)
				cancel()
				if ctx.Err() != nil {
					return
				}
				d.noteServer(err)
			}
			d.mu.Lock()
			if d.st != st {
				d.mu.Unlock()
				return
			}
			st.conns = conns // [] while unreachable (CONTRACT §6)
			d.rejoinLocked(st)
			d.mu.Unlock()
		}
	}()
	go func() {
		backoff := time.Second
		ended := func(reason string) { go d.end(st, reason) }
		for ctx.Err() == nil {
			api, err := d.api()
			if err == nil {
				// The server doesn't replay "ended": reconcile on every
				// (re)connect, then follow the events, whose first "stream"
				// snapshot also reports a terminal phase.
				c, cancel := context.WithTimeout(ctx, 10*time.Second)
				reason, done, e := api.StreamEnded(c, st.rec.StreamID)
				cancel()
				d.noteServer(e)
				if e == nil && done {
					ended(reason)
					return
				}
				began := time.Now()
				err = api.Events(ctx, st.rec.StreamID, ended)
				if time.Since(began) > 30*time.Second {
					backoff = time.Second // it was a working connection
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()
}

// rejoinLocked rebuilds the local viewer table (CONTRACT §6, source local).
func (d *Daemon) rejoinLocked(st *stream) {
	var at *int64
	if st.state != nil {
		v := st.stateAt
		at = &v
	}
	list, mem, _ := hostview.Join(hostview.JoinInput{Source: "local", Now: nowMs(), State: st.state, StateAt: at,
		PublisherOnline: st.att == attAttached, Conns: st.conns}, st.mem)
	st.viewers, st.mem = list, mem
	d.scheduleLocked()
}
