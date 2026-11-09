package daemon_test

import (
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/hosttest"
)

// TestCommandIDsUniqueAcrossRestarts (phase 2c review, HIGH): the page caches
// command results by id, so a restarted daemon (its counter starts again)
// must not reuse an id the page already answered. Repro: resume = d1, kill,
// re-attach, pause = d1 again got the cached "resume" success.
func TestCommandIDsUniqueAcrossRestarts(t *testing.T) {
	e, c, origin, lid, lkey := launched(t)
	s, aid, akey := attachedPage(t, c, origin, lid, lkey)
	msgs := pageMsgs(s.WS)
	answer := func(ch <-chan map[string]any, ws func(map[string]any)) string {
		cmd := next(ch, "command", 3*time.Second)
		if cmd == nil {
			t.Fatal("no command forwarded")
		}
		ws(map[string]any{"type": "result", "id": cmd["id"], "ok": true, "data": map[string]any{}})
		return cmd["id"].(string)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Call("resume", map[string]any{}); done <- err }()
	first := answer(msgs, func(m map[string]any) { hosttest.WriteMsg(ctx5(), s.WS, m) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	c.Close()
	e.restart()
	c = e.client("cli")
	s2, err := hosttest.Authenticate(ctx5(), origin, aid, akey)
	if err != nil {
		t.Fatal(err)
	}
	hosttest.WriteMsg(ctx5(), s2.WS, map[string]any{"type": "init_ack"})
	waitFor(t, "re-attached", 5*time.Second, func() bool { return status(t, c).Stream.Attachment == "attached" })
	msgs2 := pageMsgs(s2.WS)
	go func() { _, err := c.Call("pause", map[string]any{}); done <- err }()
	second := answer(msgs2, func(m map[string]any) { hosttest.WriteMsg(ctx5(), s2.WS, m) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("command id %q reused across a daemon restart", first)
	}
	p1, p2 := first[:strings.LastIndexByte(first, '.')], second[:strings.LastIndexByte(second, '.')]
	if p1 == p2 {
		t.Fatalf("incarnation prefix %q survived the restart", p1)
	}
}
