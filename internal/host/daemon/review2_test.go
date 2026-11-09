package daemon_test

import (
	"errors"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/hosttest"
	"github.com/passstream/passstream/internal/host/proto"
)

// TestReconnectInitWaitsForDurableLatches (round 2, finding 1): while the
// mirror's latches aren't on disk, an attachment reconnect gets no init
// (it would carry them) and the socket drops without an error code, so the
// page retries; the current page stays attached and the status reports only
// the latches on disk. Once the write succeeds, init carries them.
func TestReconnectInitWaitsForDurableLatches(t *testing.T) {
	e, c, origin, lid, lkey := launched(t, func(e *env) { e.cfg.SaveRetry = 30 * time.Millisecond })
	s, aid, akey := attachedPage(t, c, origin, lid, lkey)
	msgs := pageMsgs(s.WS)
	id := hosttest.Field(s.Init, "stream", "id").(string)
	inst := proto.NewId22()
	hosttest.WriteMsg(ctx5(), s.WS, senderState(id, inst, 1, proto.Latches{}, nil))
	if next(msgs, "state_ack", time.Second) == nil {
		t.Fatal("initial ack missing")
	}

	fix := e.breakState()
	if _, err := c.Call("pause", map[string]any{}); code(err) != "internal" {
		t.Fatalf("pause with a failing write: %v", err)
	}
	hosttest.WriteMsg(ctx5(), s.WS, senderState(id, inst, 2, proto.Latches{Locked: true}, nil))
	if next(msgs, "state_ack", 150*time.Millisecond) != nil {
		t.Fatal("non-durable state ack")
	}
	if l := status(t, c).Stream.Access.Latches; l.Paused {
		t.Fatalf("status reports a latch that isn't on disk: %+v", l)
	}

	s2, err := hosttest.Authenticate(ctx5(), origin, aid, akey)
	if err == nil {
		s2.WS.CloseNow()
		t.Fatalf("init sent with non-durable latches %v (disk %+v)", s2.Init["latches"], e.record().Latches)
	}
	var de *hosttest.ErrDaemon
	if errors.As(err, &de) {
		t.Fatalf("a durability failure must look like a dropped connection, got %q", de.Code)
	}
	if m := next(msgs, "error", 150*time.Millisecond); m != nil {
		t.Fatalf("the current page was superseded by a refused reconnect: %v", m)
	}

	fix()
	waitFor(t, "retry saved both latches", 2*time.Second, func() bool {
		r := e.record()
		return r.Latches.Paused && r.Latches.Locked
	})
	s3, err := hosttest.Authenticate(ctx5(), origin, aid, akey)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.WS.CloseNow()
	if hosttest.Field(s3.Init, "latches", "paused") != true || hosttest.Field(s3.Init, "latches", "locked") != true {
		t.Fatalf("init after the write: %v", s3.Init["latches"])
	}
	if !status(t, c).Stream.Access.Latches.Paused {
		t.Fatal("status lost the durable pause")
	}
}
