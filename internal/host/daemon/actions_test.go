package daemon_test

import (
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/hosttest"
)

func TestLocalActionReplaysWithoutRotatingAgain(t *testing.T) {
	e := newEnv(t)
	c := e.client("plugin")
	e.startLive(c, map[string]any{})
	args := map[string]any{"action_id": "one-click"}
	call(t, c, "pin.rotate", args)
	first := call(t, c, "pin.reveal", map[string]any{})
	version := status(t, c).Stream.Access.PinVersion
	call(t, c, "pin.rotate", args)
	second := call(t, c, "pin.reveal", map[string]any{})
	if hosttest.Field(first, "pin") != hosttest.Field(second, "pin") || status(t, c).Stream.Access.PinVersion != version {
		t.Fatal("retried action rotated twice")
	}
	if _, err := c.Call("unfreeze", args); code(err) != "idempotency_conflict" {
		t.Fatal(err)
	}
	if _, err := c.Call("pin.rotate", map[string]any{"action_id": "bad id"}); code(err) != "invalid_argument" {
		t.Fatal(err)
	}
	call(t, c, "pin.rotate", map[string]any{"action_id": "another-click"})
	if status(t, c).Stream.Access.PinVersion != version+1 {
		t.Fatal("new click did not execute")
	}
}

func TestLocalActionRetriesWithStableSenderID(t *testing.T) {
	_, c, origin, lid, lkey := launched(t, func(e *env) { e.cfg.CommandTimeout = 50 * time.Millisecond })
	s, _, _ := attachedPage(t, c, origin, lid, lkey)
	msgs := pageMsgs(s.WS)
	done := make(chan error, 1)
	args := map[string]any{"action_id": "retry-click"}
	go func() { _, err := c.Call("pin.rotate", args); done <- err }()
	first := next(msgs, "command", time.Second)
	if first == nil {
		t.Fatal("no command")
	}
	if err := <-done; code(err) != "sender_timeout" {
		t.Fatal(err)
	}
	if _, err := c.Call("unfreeze", args); code(err) != "idempotency_conflict" {
		t.Fatal("transient failure lost the action fingerprint", err)
	}
	go func() { _, err := c.Call("pin.rotate", args); done <- err }()
	second := next(msgs, "command", time.Second)
	if second == nil || second["id"] != first["id"] {
		t.Fatal("forwarding identity changed")
	}
	if _, ok := second["args"].(map[string]any)["action_id"]; ok {
		t.Fatal("transport action_id forwarded as an argument")
	}
	hosttest.WriteMsg(ctx5(), s.WS, map[string]any{"type": "result", "id": second["id"], "ok": true, "data": map[string]any{}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
