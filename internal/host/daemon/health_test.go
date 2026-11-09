package daemon_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/hosttest"
	"github.com/passstream/passstream/internal/host/proto"
)

func TestHealthStatusUsesAllowlistedSenderFields(t *testing.T) {
	e, c, origin, lid, lkey := launched(t)
	s, _, _ := attachedPage(t, c, origin, lid, lkey)
	st := senderState(e.record().StreamID, proto.NewId22(), 1, proto.Latches{}, nil)
	b, _ := json.Marshal(st)
	var wire map[string]any
	json.Unmarshal(b, &wire)
	wire["state"].(map[string]any)["health"] = map[string]any{"uplink_kbps": 2400, "encode_ms": 4.5, "encode_cpu_pct": 13.5, "fps": 30, "width": 1920, "height": 1080, "quality_limitation": "cpu", "pin": "SECRET_PIN", "link": "SECRET_LINK", "key": "SECRET_KEY"}
	if err := hosttest.WriteMsg(ctx5(), s.WS, wire); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "health status", time.Second, func() bool { h := status(t, c).Stream.Health; return h != nil && h.Width != nil })
	h := status(t, c).Stream.Health
	if *h.EncodeCPUPct != 13.5 || *h.Width != 1920 || *h.Height != 1080 || *h.FPS != 30 || *h.UplinkKbps != 2400 || *h.EncodeMS != 4.5 {
		t.Fatal(h)
	}
	raw, _ := c.Call("status", map[string]any{})
	if strings.Contains(string(raw), "SECRET") || strings.Contains(e.log.String(), "SECRET") {
		t.Fatal("health secret leaked")
	}
	s.WS.CloseNow()
	waitFor(t, "detached", time.Second, func() bool { return status(t, c).Stream.Attachment == "detached" })
	if status(t, c).Stream.Health == nil {
		t.Fatal("last health lost while detached")
	}
}

func TestInvalidHealthKeepsSenderStateAndLatches(t *testing.T) {
	e, c, origin, lid, lkey := launched(t)
	s, _, _ := attachedPage(t, c, origin, lid, lkey)
	for i, health := range []any{map[string]any{"width": 1920.5}, map[string]any{"fps": 1001}, "bad"} {
		st := senderState(e.record().StreamID, proto.NewId22(), i+1, proto.Latches{Locked: true, Paused: true}, nil)
		b, _ := json.Marshal(st)
		var wire map[string]any
		json.Unmarshal(b, &wire)
		state := wire["state"].(map[string]any)
		state["health"] = health
		if err := hosttest.WriteMsg(ctx5(), s.WS, wire); err != nil {
			t.Fatal(err)
		}
		// Receiving state_ack proves the entire report was accepted and mirrored.
		readUntil(t, s.WS, "state_ack")
		got := status(t, c).Stream
		if got.Health != nil || !got.Access.Latches.Locked || !got.Access.Latches.Paused {
			t.Fatal(got)
		}
	}
}

func TestUnknownPageEndReasonStillCleansUp(t *testing.T) {
	_, c, origin, lid, lkey := launched(t)
	s, _, _ := attachedPage(t, c, origin, lid, lkey)
	if err := hosttest.WriteMsg(ctx5(), s.WS, map[string]any{"type": "ended", "reason": "future-reason"}); err != nil {
		t.Fatal(err)
	}
	ev, ok := c.WaitEvent(func(ev proto.Event) bool { return ev.Event == "ended" }, 7*time.Second)
	if !ok || hosttest.Field(ev.Data, "reason") != "unknown" {
		t.Fatal(ev)
	}
	waitFor(t, "idle after unknown end", time.Second, func() bool { return status(t, c).Stream == nil })
}
