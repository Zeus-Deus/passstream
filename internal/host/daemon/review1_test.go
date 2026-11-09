package daemon_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/passstream/passstream/internal/host/hosttest"
	"github.com/passstream/passstream/internal/host/proto"
)

// Regression tests for review round 1 (GPT-6.1-Sol, request changes).

// pageMsgs reads a page socket in the background, so a test can check that
// a message did *not* arrive without a read deadline (which would close a
// coder/websocket connection).
func pageMsgs(ws *websocket.Conn) <-chan map[string]any {
	ch := make(chan map[string]any, 256)
	go func() {
		defer close(ch)
		for {
			m, err := hosttest.ReadMsg(context.Background(), ws)
			if err != nil {
				return
			}
			ch <- m
		}
	}()
	return ch
}

// next returns the next message of the given type within d, or nil.
func next(ch <-chan map[string]any, typ string, d time.Duration) map[string]any {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return nil
			}
			if m["type"] == typ {
				return m
			}
		case <-timer.C:
			return nil
		}
	}
}

// breakState makes the state directory unwritable until the returned func
// runs (also at cleanup, so TempDir can be removed).
func (e *env) breakState() func() {
	e.t.Helper()
	if os.Geteuid() == 0 {
		e.t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(e.stateDir, 0o500); err != nil {
		e.t.Fatal(err)
	}
	fix := func() { os.Chmod(e.stateDir, 0o700) }
	e.t.Cleanup(fix)
	return fix
}

// TestLatchNeverAckedWhileNotDurable (finding 1): a failed record write
// keeps the mirror dirty. A repeated pause, whose latch memory already has,
// still writes first, and isn't forwarded or acknowledged while the write
// fails. A state with an already-mirrored latch gets no state_ack either.
// The daemon retries the write, reports revocation_not_durable after
// repeated failures, and clears it once the record is durable.
func TestLatchNeverAckedWhileNotDurable(t *testing.T) {
	e, c, origin, lid, lkey := launched(t, func(e *env) { e.cfg.SaveRetry = 40 * time.Millisecond })
	s, _, _ := attachedPage(t, c, origin, lid, lkey)
	msgs := pageMsgs(s.WS)
	id := hosttest.Field(s.Init, "stream", "id").(string)
	inst := proto.NewId22()
	hosttest.WriteMsg(ctx5(), s.WS, senderState(id, inst, 1, proto.Latches{}, nil))
	if next(msgs, "state_ack", 3*time.Second) == nil {
		t.Fatal("no first state_ack")
	}

	fix := e.breakState()
	for i := 0; i < 2; i++ {
		if _, err := c.Call("pause", map[string]any{}); code(err) != "internal" {
			t.Fatalf("pause %d with a failing write: %v", i, err)
		}
	}
	if m := next(msgs, "command", 300*time.Millisecond); m != nil {
		t.Fatalf("a non-durable pause was forwarded: %v", m)
	}
	if e.record().Latches.Paused {
		t.Fatal("the disk can't have the latch yet")
	}
	// A page state that adds a latch, then the same state again: neither is
	// acknowledged while the mirror isn't on disk.
	hosttest.WriteMsg(ctx5(), s.WS, senderState(id, inst, 2, proto.Latches{Locked: true}, nil))
	hosttest.WriteMsg(ctx5(), s.WS, senderState(id, inst, 3, proto.Latches{Locked: true}, nil))
	if m := next(msgs, "state_ack", 500*time.Millisecond); m != nil {
		t.Fatalf("state_ack while not durable: %v", m)
	}
	// Repeated failures surface as revocation_not_durable.
	if _, ok := c.WaitEvent(func(ev proto.Event) bool {
		return ev.Event == "alert" && hosttest.Field(ev.Data, "kind") == "revocation_durability" && hosttest.Field(ev.Data, "not_durable") == true
	}, 5*time.Second); !ok {
		t.Fatal("no revocation_durability alert")
	}
	if hs := status(t, c); hs.Stream.E2EE == nil || !hs.Stream.E2EE.RevocationNotDurable {
		t.Fatalf("status doesn't report the non-durable mirror: %+v", hs.Stream.E2EE)
	}

	// The retry makes it durable without any new command.
	fix()
	waitFor(t, "the retried write", 3*time.Second, func() bool {
		r := e.record()
		return r.Latches.Paused && r.Latches.Locked
	})
	if _, ok := c.WaitEvent(func(ev proto.Event) bool {
		return ev.Event == "alert" && hosttest.Field(ev.Data, "kind") == "revocation_durability" && hosttest.Field(ev.Data, "not_durable") == false
	}, 3*time.Second); !ok {
		t.Fatal("no recovery alert")
	}
	waitFor(t, "status recovery", 3*time.Second, func() bool { return !status(t, c).Stream.E2EE.RevocationNotDurable })
	hosttest.WriteMsg(ctx5(), s.WS, senderState(id, inst, 4, proto.Latches{Locked: true}, nil))
	if m := next(msgs, "state_ack", 3*time.Second); m == nil || m["seq"] != 4.0 {
		t.Fatalf("state_ack after recovery: %v", m)
	}
	// And a pause now goes through.
	done := make(chan error, 1)
	go func() { _, err := c.Call("pause", map[string]any{}); done <- err }()
	cmd := next(msgs, "command", 3*time.Second)
	if cmd == nil || cmd["op"] != "pause" {
		t.Fatalf("pause not forwarded: %v", cmd)
	}
	hosttest.WriteMsg(ctx5(), s.WS, map[string]any{"type": "result", "id": cmd["id"], "ok": true, "data": map[string]any{}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestLatchWhileDetachedNeedsDurability (finding 1): detached, pause and
// lock succeed through the mirror alone, so they must fail while it can't
// be written.
func TestLatchWhileDetachedNeedsDurability(t *testing.T) {
	e, c, origin, lid, lkey := launched(t)
	s, _, _ := attachedPage(t, c, origin, lid, lkey)
	s.WS.CloseNow()
	waitFor(t, "detached", 3*time.Second, func() bool { return status(t, c).Stream.Attachment == "detached" })
	fix := e.breakState()
	if _, err := c.Call("lock", map[string]any{"locked": true}); code(err) != "internal" {
		t.Fatalf("lock: %v", err)
	}
	if _, err := c.Call("lock", map[string]any{"locked": true}); code(err) != "internal" {
		t.Fatalf("repeated lock: %v", err)
	}
	fix()
	if _, err := c.Call("lock", map[string]any{"locked": true}); err != nil {
		t.Fatal(err)
	}
	if !e.record().Latches.Locked {
		t.Fatal("lock acknowledged but not on disk")
	}
}

// TestSupersededPageCantEndItsReplacement (finding 4): during the old
// socket's grace period, its "ended", "result" and "state" are ignored.
func TestSupersededPageCantEndItsReplacement(t *testing.T) {
	_, c, origin, lid, lkey := launched(t)
	s1, aid, akey := attachedPage(t, c, origin, lid, lkey)
	old := pageMsgs(s1.WS)
	id := hosttest.Field(s1.Init, "stream", "id").(string)
	s2, err := hosttest.Authenticate(ctx5(), origin, aid, akey)
	if err != nil {
		t.Fatal(err)
	}
	if m := next(old, "error", 2*time.Second); m == nil || m["code"] != "superseded" {
		t.Fatalf("old page: %v", m)
	}
	// At once, inside the 50 ms grace.
	hosttest.WriteMsg(ctx5(), s1.WS, map[string]any{"type": "ended", "reason": "state_lost"})
	hosttest.WriteMsg(ctx5(), s1.WS, senderState(id, proto.NewId22(), 1, proto.Latches{}, func(s *proto.SenderState) { s.Phase = "live" }))
	hosttest.WriteMsg(ctx5(), s1.WS, map[string]any{"type": "result", "id": "d1", "ok": true, "data": map[string]any{}})

	hosttest.WriteMsg(ctx5(), s2.WS, map[string]any{"type": "init_ack"})
	time.Sleep(400 * time.Millisecond)
	if ev, ok := c.WaitEvent(func(ev proto.Event) bool { return ev.Event == "ended" }, 10*time.Millisecond); ok {
		t.Fatalf("the superseded page ended the stream: %v", ev)
	}
	hs := status(t, c)
	if hs.Stream == nil || hs.Stream.Phase == "ended" || hs.Stream.Attachment != "attached" || hs.Stream.Phase == "live" {
		t.Fatalf("stream after the old page's messages: %+v", hs.Stream)
	}
}

// TestServerEndMissedWhileDisconnected (finding 6): the server ends the
// stream while the events connection is down and doesn't replay "ended";
// the daemon finds out on reconnect.
func TestServerEndMissedWhileDisconnected(t *testing.T) {
	e := newEnv(t)
	c := e.client("plugin")
	id := e.startLive(c, map[string]any{})
	time.Sleep(200 * time.Millisecond)
	e.srv.SetSilentEnd(true)
	e.srv.EndStream(id, "expired")
	e.srv.DropEvents(id)
	ev, ok := c.WaitEvent(func(ev proto.Event) bool { return ev.Event == "ended" }, 10*time.Second)
	if !ok || hosttest.Field(ev.Data, "reason") != "expired" {
		t.Fatalf("ended %v", ev)
	}
}

// TestBanActionIDIsTheCommandID (finding 7): a ban's action_id is the
// server command id on every retry; a malformed one is refused.
func TestBanActionIDIsTheCommandID(t *testing.T) {
	e := newEnv(t)
	c := e.client("plugin")
	e.startLive(c, map[string]any{})
	for i := 0; i < 2; i++ {
		c.Call("ban", map[string]any{"connection_id": "c_AAAAAAAAAAAAAAAAAAAAAA", "action_id": "act-7"})
	}
	if _, err := c.Call("ban", map[string]any{"connection_id": "c_AAAAAAAAAAAAAAAAAAAAAA", "action_id": "bad id"}); code(err) != "invalid_argument" {
		t.Fatalf("malformed action_id: %v", err)
	}
	ids := e.srv.CommandIDs()
	if len(ids) != 2 || ids[0] != "act-7" || ids[1] != "act-7" {
		t.Fatalf("command ids %v", ids)
	}
}
