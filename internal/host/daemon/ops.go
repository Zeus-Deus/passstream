package daemon

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/passstream/passstream/internal/host/proto"
	"github.com/passstream/passstream/internal/host/serverapi"
)

// dispatch runs one daemon-socket op (CONTRACT §10).
func (d *Daemon) dispatch(c *client, op string, args map[string]any) (any, error) {
	switch op {
	case "frame", "capture.auth", "record.start", "record.stop", "record.status", "publish", "publish.status":
		return d.featureDispatch(d.ctx, op, args, nil)
	case "status":
		if err := closed(args); err != nil {
			return nil, err
		}
		return d.hostStatus(), nil
	case "subscribe":
		if err := closed(args); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.sub = true
		if d.cfg.StatusDelay > 0 {
			c.holdUntil = time.Now().Add(d.cfg.StatusDelay)
		}
		c.mu.Unlock()
		return map[string]any{}, nil
	case "start":
		return d.start(c, args)
	case "stop":
		if err := closed(args); err != nil {
			return nil, err
		}
		return d.stop()
	case "link":
		if err := closed(args); err != nil {
			return nil, err
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		st := d.st
		if st == nil || st.ending {
			return nil, proto.E("not_live", "no active share")
		}
		if !st.secrets.have {
			return nil, proto.E("link_not_ready", "the link isn't ready yet")
		}
		return map[string]any{"link": st.secrets.link}, nil
	case "pin.reveal":
		if err := closed(args); err != nil {
			return nil, err
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		st := d.st
		if st == nil || st.ending {
			return nil, proto.E("not_live", "no active share")
		}
		if !st.secrets.have {
			if st.att != attAttached {
				return nil, proto.E("sender_offline", "the sender window hasn't reported the PIN")
			}
			return nil, proto.E("link_not_ready", "the PIN isn't ready yet")
		}
		return map[string]any{"pin": st.secrets.pin, "pending_pin": st.secrets.pending}, nil
	case "ban":
		if err := closed(args, "connection_id", "action_id"); err != nil {
			return nil, err
		}
		cid, _ := args["connection_id"].(string)
		if !proto.IsConnID(cid) {
			return nil, proto.E("invalid_argument", "connection_id must be a connection id")
		}
		// action_id names one user action; the client reuses it on its
		// retries and it becomes the server's command id (idempotency key).
		aid, _ := args["action_id"].(string)
		if _, present := args["action_id"]; present && !serverapi.ValidActionID(aid) {
			return nil, proto.E("invalid_argument", "action_id must be 1-64 characters of A-Z a-z 0-9 _ -")
		}
		d.mu.Lock()
		st := d.st
		d.mu.Unlock()
		if st == nil || st.ending {
			return nil, proto.E("not_live", "no active share")
		}
		api, err := d.api()
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
		defer cancel()
		res, err := api.Ban(ctx, st.rec.StreamID, cid, aid)
		d.noteServer(err)
		return res, err
	case "shutdown":
		if err := closed(args, "force"); err != nil {
			return nil, err
		}
		force, ok := args["force"].(bool)
		if _, present := args["force"]; present && !ok {
			return nil, proto.E("invalid_argument", "force must be a boolean")
		}
		d.mu.Lock()
		live := d.st != nil
		d.mu.Unlock()
		if live {
			if !force {
				return nil, proto.E("conflict", "a share is running; use force to stop it")
			}
			if _, err := d.stop(); err != nil {
				return nil, err
			}
		}
		return map[string]any{}, nil
	case "pause", "resume", "mode", "lock", "unfreeze", "kick", "pin.rotate", "pin.set", "knock.approve", "knock.deny", "annotations", "annotations.clear":
	default:
		return nil, proto.E("invalid_argument", "unknown op")
	}
	// action_id belongs to this transport; the sender receives a stable command id.
	action, err := localActionID(op, args)
	if err != nil {
		return nil, err
	}
	args = withoutActionID(args)
	if err := validateCommand(op, args); err != nil {
		return nil, err
	}
	c.mu.Lock()
	by := "plugin"
	if c.kind == "cli" {
		by = "cli"
	}
	c.mu.Unlock()
	return d.actionForward(op, args, by, action)
}

func reqBool(args map[string]any, k string, required bool) error {
	v, ok := args[k]
	if !ok {
		if required {
			return proto.E("invalid_argument", "missing %s", k)
		}
		return nil
	}
	if _, ok := v.(bool); !ok {
		return proto.E("invalid_argument", "%s must be a boolean", k)
	}
	return nil
}

// validateCommand applies the closed argument schemas of CONTRACT §10.
func validateCommand(op string, args map[string]any) error {
	pinOK := func() error {
		if v, ok := args["pin"]; ok {
			if _, err := pinArg(v); err != nil {
				return err
			}
		}
		return nil
	}
	switch op {
	case "pause", "resume", "annotations.clear":
		return closed(args)
	case "annotations":
		if err := closed(args, "viewers"); err != nil {
			return err
		}
		return reqBool(args, "viewers", true)
	case "mode":
		if err := closed(args, "mode"); err != nil {
			return err
		}
		if m := args["mode"]; m != "text" && m != "motion" {
			return proto.E("invalid_argument", "mode must be text or motion")
		}
	case "lock":
		if err := closed(args, "locked"); err != nil {
			return err
		}
		return reqBool(args, "locked", true)
	case "unfreeze":
		if err := closed(args, "keep_pin", "pin"); err != nil {
			return err
		}
		if err := reqBool(args, "keep_pin", false); err != nil {
			return err
		}
		if args["keep_pin"] == true && args["pin"] != nil {
			return proto.E("invalid_argument", "pin isn't allowed with keep_pin")
		}
		return pinOK()
	case "kick":
		if err := closed(args, "admission_id", "lock", "rotate_pin", "pin"); err != nil {
			return err
		}
		if id, _ := args["admission_id"].(string); !proto.IsId22(id) {
			return proto.E("invalid_argument", "admission_id must be an Id22")
		}
		if err := reqBool(args, "lock", false); err != nil {
			return err
		}
		if err := reqBool(args, "rotate_pin", false); err != nil {
			return err
		}
		if _, ok := args["pin"]; ok && args["rotate_pin"] != true {
			return proto.E("invalid_argument", "pin is allowed only with rotate_pin")
		}
		return pinOK()
	case "pin.rotate":
		if err := closed(args, "pin"); err != nil {
			return err
		}
		return pinOK()
	case "pin.set":
		if err := closed(args, "policy", "pin"); err != nil {
			return err
		}
		p, err := proto.ParsePinPolicy(args["policy"])
		if err != nil {
			return err
		}
		if err := pinOK(); err != nil {
			return err
		}
		var pin *string
		if s, ok := args["pin"].(string); ok {
			pin = &s
		}
		return checkPinForPolicy(p, pin)
	case "knock.approve":
		if err := closed(args, "knock_id", "row_code"); err != nil {
			return err
		}
		if id, _ := args["knock_id"].(string); !proto.IsId22(id) {
			return proto.E("invalid_argument", "knock_id must be an Id22")
		}
		if code, _ := args["row_code"].(string); !proto.IsRowCode(code) {
			return proto.E("invalid_argument", "row_code must be a canonical row code")
		}
	case "knock.deny":
		if err := closed(args, "knock_id"); err != nil {
			return err
		}
		if id, _ := args["knock_id"].(string); !proto.IsId22(id) {
			return proto.E("invalid_argument", "knock_id must be an Id22")
		}
	}
	return nil
}

// forward sends a command to the sender page and waits for its result.
// Restrictive latches are fsynced into the mirror before forwarding; while
// the page is detached pause and lock {locked: true} succeed through the
// mirror alone (CONTRACT §10 "Latches and acknowledgements").
func (d *Daemon) forward(op string, args map[string]any, by, action string, expected *stream) (any, error) {
	d.mu.Lock()
	st := d.st
	if st == nil || st.ending || expected != nil && expected != st {
		d.mu.Unlock()
		return nil, proto.E("not_live", "no active share")
	}
	latchOp, setLatch := false, false
	switch {
	case op == "pause":
		latchOp, setLatch, st.rec.Latches.Paused = true, !st.rec.Latches.Paused, true
		st.captures.Close()
		d.refusePublishLocked(st, "refused")
	case op == "lock" && args["locked"] == true, op == "kick" && args["lock"] == true:
		latchOp, setLatch, st.rec.Latches.Locked = true, !st.rec.Latches.Locked, true
	}
	// A latch is forwarded or acknowledged only once it's on disk: also when
	// memory already has it but an earlier write failed (st.dirty).
	if latchOp && (setLatch || st.dirty) {
		if err := d.saveLocked(st); err != nil {
			d.scheduleLocked()
			d.mu.Unlock()
			return nil, err
		}
		d.scheduleLocked()
	}
	page := st.page
	if page == nil || st.att != attAttached {
		d.mu.Unlock()
		if op == "pause" || (op == "lock" && args["locked"] == true) {
			return map[string]any{}, nil
		}
		return nil, proto.E("sender_offline", "the sender window isn't attached")
	}
	id := d.commandIDLocked()
	if action != "" {
		id = actionForwardID(st.rec.StreamID, action)
	}
	ch := make(chan pageResult, 1)
	st.pending[id] = ch
	policy := st.rec.Policy
	if st.state != nil {
		pi := st.state.Access.Pin
		policy = proto.PinPolicy{Kind: pi.Kind}
		if pi.Length != nil {
			policy.Length = int(*pi.Length)
		}
	}
	d.mu.Unlock()

	b, _ := json.Marshal(map[string]any{"type": "command", "id": id, "op": op, "args": args})
	page.send(b)
	var res pageResult
	var ok bool
	select {
	case res, ok = <-ch:
	case <-time.After(d.cfg.CommandTimeout):
		d.mu.Lock()
		delete(st.pending, id)
		d.mu.Unlock()
		return nil, proto.E("sender_timeout", "no answer from the sender window; the command may still apply")
	}
	if !ok {
		return nil, proto.E("sender_timeout", "the sender window went away; the command may still apply")
	}
	if res.err != nil {
		return nil, res.err
	}
	d.mu.Lock()
	if d.st == st {
		clear := false
		switch {
		case op == "resume":
			clear, st.rec.Latches.Paused = st.rec.Latches.Paused, false
		case op == "lock" && args["locked"] == false:
			clear, st.rec.Latches.Locked = st.rec.Latches.Locked, false
		case op == "unfreeze":
			clear, st.rec.Latches.Frozen = st.rec.Latches.Frozen, false
		}
		if clear {
			d.saveLocked(st)
		}
		pinChange := op == "pin.rotate" || op == "pin.set" || (op == "unfreeze" && args["keep_pin"] != true) || (op == "kick" && args["rotate_pin"] == true)
		if pinChange {
			if op == "pin.set" {
				policy, _ = proto.ParsePinPolicy(args["policy"])
			}
			d.broadcastLocked("alert", map[string]any{"kind": "pin_changed", "pin": policy, "by": by}, false)
		}
		d.scheduleLocked()
	}
	d.mu.Unlock()
	return res.data, nil
}

// stop is HOST-CONTROL §5.3 "Stop is locally enforceable": the page gets
// stop (≤ 2 s), then the server delete and the browser kill run anyway.
func (d *Daemon) stop() (any, error) {
	d.mu.Lock()
	st := d.st
	if st == nil {
		d.mu.Unlock()
		return nil, proto.E("not_live", "no active share")
	}
	if st.ending {
		d.mu.Unlock()
		return map[string]any{}, nil
	}
	page := st.page
	var ch chan pageResult
	if page != nil && st.att == attAttached {
		id := d.commandIDLocked()
		ch = make(chan pageResult, 1)
		st.pending[id] = ch
		b, _ := json.Marshal(map[string]any{"type": "command", "id": id, "op": "stop", "args": map[string]any{}})
		page.send(b)
	}
	d.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-time.After(d.cfg.StopWait):
		}
	}
	d.end(st, "stopped")
	return map[string]any{}, nil
}

// commandIDLocked returns a command id unique across daemon incarnations
// ("d<incarnation>.<n>"): the page caches results by id, so a restarted
// daemon must never reuse one. Callers hold d.mu.
func (d *Daemon) commandIDLocked() string {
	d.cmdSeq++
	return "d" + d.incarnation + "." + strconv.FormatUint(d.cmdSeq, 10)
}
