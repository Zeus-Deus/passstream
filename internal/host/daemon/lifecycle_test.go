package daemon_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/hosttest"
	"github.com/passstream/passstream/internal/host/proto"
)

func TestStartLiveStopAndSecrets(t *testing.T) {
	e := newEnv(t)
	c := e.client("plugin")
	id := e.startLive(c, map[string]any{"knock": false})
	if !proto.IsId22(id) {
		t.Fatalf("stream id %q", id)
	}
	if _, ok := c.WaitEvent(func(ev proto.Event) bool { return ev.Event == "link_ready" }, 5*time.Second); !ok {
		t.Fatal("no link_ready")
	}
	// start's reply precedes live (Appendix A #3): the events came after.
	link := call(t, c, "link", map[string]any{})["link"].(string)
	if !strings.HasPrefix(link, e.srv.URL+"/v#c=") || !strings.Contains(link, "&k=") || !strings.Contains(link, "&s=") {
		t.Fatalf("link %q", link)
	}
	pin := call(t, c, "pin.reveal", map[string]any{})["pin"].(string)
	if len(pin) != 4 {
		t.Fatalf("pin %q", pin)
	}
	hs := status(t, c)
	if hs.Stream == nil || hs.Stream.Phase != "live" || hs.Stream.Attachment != "attached" || !hs.Stream.HasLink {
		t.Fatalf("status %+v", hs.Stream)
	}
	if hs.Daemon.PortMode != "self_bound" || hs.Daemon.Supervision != "process_group" || hs.Daemon.Key != "ok" || hs.Daemon.PortOwner != "ok" {
		t.Fatalf("daemon %+v", hs.Daemon)
	}
	raw, _ := c.Call("status", map[string]any{})
	if strings.Contains(string(raw), pin) || strings.Contains(string(raw), link) {
		t.Fatal("HostStatus holds the PIN or the link")
	}
	if _, err := c.Call("start", map[string]any{}); code(err) != "already_live" {
		t.Fatalf("second start: %v", err)
	}
	// The creation policy: plugin defaults, explicit pin false, knock OR.
	cr := e.srv.Created()[0]
	if cr["sender_kind"] != "local" || cr["knock"] != false {
		t.Fatalf("create body %v", cr)
	}
	rec := e.record()
	if rec.Attachment == nil || rec.Browser == nil || rec.PublishToken == "" {
		t.Fatalf("record %+v", rec)
	}
	b, _ := os.ReadFile(filepath.Join(e.stateDir, "host-state.json"))
	if strings.Contains(string(b), pin) || strings.Contains(string(b), link) {
		t.Fatal("the recovery record holds a secret")
	}
	fi, _ := os.Stat(filepath.Join(e.stateDir, "host-state.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %v", fi.Mode())
	}
	browserPID := rec.Browser.PID

	// Secrets never appear in argv or logs. Launch keys use guarded files too.
	for _, s := range []string{pin, link, testKey, rec.PublishToken, rec.Attachment.Key} {
		if got := cmdlinesContaining(s, rec.Browser.PGID); len(got) > 0 {
			t.Fatalf("a secret is in an argv: %v", got)
		}
		if strings.Contains(e.log.String(), s) {
			t.Fatalf("a secret is in the log")
		}
	}

	call(t, c, "stop", map[string]any{})
	ev, ok := c.WaitEvent(func(ev proto.Event) bool { return ev.Event == "ended" }, 10*time.Second)
	if !ok || hosttest.Field(ev.Data, "reason") != "stopped" || hosttest.Field(ev.Data, "stream_id") != id {
		t.Fatalf("ended %v", ev)
	}
	if hs := status(t, c); hs.Stream != nil {
		t.Fatalf("stream after stop: %+v", hs.Stream)
	}
	if !e.srv.Deleted(id) {
		t.Fatal("not deleted on the server")
	}
	if _, err := os.Stat(filepath.Join(e.stateDir, "host-state.json")); !os.IsNotExist(err) {
		t.Fatal("record not removed")
	}
	if _, err := os.Stat(filepath.Join("/proc", itoa(browserPID))); err == nil {
		if b, _ := os.ReadFile(filepath.Join("/proc", itoa(browserPID), "stat")); !strings.Contains(string(b), ") Z") {
			t.Fatal("browser still running after stop")
		}
	}
	if _, err := c.Call("link", map[string]any{}); code(err) != "not_live" {
		t.Fatalf("link after stop: %v", err)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestHelloFirstAndClosedArgs(t *testing.T) {
	e := newEnv(t)
	c, err := hosttest.DialClient(e.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Call("status", map[string]any{}); code(err) != "protocol_mismatch" {
		t.Fatalf("no hello: %v", err)
	}
	select {
	case <-c.Closed:
	case <-time.After(2 * time.Second):
		t.Fatal("socket not closed after protocol_mismatch")
	}
	fi, _ := os.Stat(e.sock)
	di, _ := os.Stat(e.runtime)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("socket %v dir %v", fi.Mode(), di.Mode())
	}
	c2 := e.client("cli")
	cases := []struct {
		op   string
		args map[string]any
		want string
	}{
		{"lock", map[string]any{"locked": true, "extra": 1}, "invalid_argument"},
		{"lock", map[string]any{}, "invalid_argument"},
		{"start", map[string]any{"pin": "1234"}, "invalid_argument"},
		{"start", map[string]any{"pin_policy": map[string]any{"kind": "passphrase"}}, "passphrase_required"},
		{"start", map[string]any{"pin_policy": map[string]any{"kind": "digits", "length": 3}}, "invalid_argument"},
		{"start", map[string]any{"pin_policy": map[string]any{"kind": "digits", "length": 4}, "pin": "12a4"}, "invalid_argument"},
		{"start", map[string]any{"pin_policy": map[string]any{"kind": "weird"}}, "invalid_argument"},
		{"knock.approve", map[string]any{"knock_id": proto.NewId22(), "row_code": "k7p"}, "invalid_argument"},
		{"kick", map[string]any{"admission_id": proto.NewId22(), "pin": "1234"}, "invalid_argument"},
		{"frobnicate", map[string]any{}, "invalid_argument"},
		{"pause", map[string]any{}, "not_live"},
		{"link", map[string]any{}, "not_live"},
	}
	for _, tc := range cases {
		if _, err := c2.Call(tc.op, tc.args); code(err) != tc.want {
			t.Errorf("%s %v: got %v, want %s", tc.op, tc.args, err, tc.want)
		}
	}
}

func TestKeyAndServerErrors(t *testing.T) {
	e := newEnv(t)
	c := e.client("plugin")
	os.Remove(e.keyPath)
	if _, err := c.Call("start", map[string]any{}); code(err) != "key_missing" {
		t.Fatalf("missing: %v", err)
	}
	os.WriteFile(e.keyPath, []byte(testKey), 0o644)
	os.Chmod(e.keyPath, 0o644)
	if _, err := c.Call("start", map[string]any{}); code(err) != "key_unsafe" {
		t.Fatalf("unsafe: %v", err)
	}
	os.Remove(e.keyPath)
	os.Symlink("/etc/hostname", e.keyPath)
	if _, err := c.Call("start", map[string]any{}); code(err) != "key_unsafe" {
		t.Fatalf("symlink: %v", err)
	}
	os.Remove(e.keyPath)
	os.WriteFile(e.keyPath, []byte("two tokens\n"), 0o600)
	if _, err := c.Call("start", map[string]any{}); code(err) != "key_invalid" {
		t.Fatalf("invalid: %v", err)
	}
	os.WriteFile(e.keyPath, []byte("wrong-key\n"), 0o600)
	if _, err := c.Call("start", map[string]any{}); code(err) != "key_rejected" {
		t.Fatalf("rejected: %v", err)
	}
	if hs := status(t, c); hs.Daemon.Key != "rejected" || hs.Daemon.Server != "unauthorized" {
		t.Fatalf("daemon %+v", hs.Daemon)
	}
	os.WriteFile(e.keyPath, []byte(testKey+"\n"), 0o600)
	e.srv.Close()
	if _, err := c.Call("start", map[string]any{}); code(err) != "server_unreachable" {
		t.Fatalf("unreachable: %v", err)
	}
	if hs := status(t, c); hs.Daemon.Server != "unreachable" {
		t.Fatalf("daemon %+v", hs.Daemon)
	}
}

// TestServerSideEnd: a dashboard DELETE reaches the daemon through the
// events stream; it reports ended {deleted} and stops the browser.
func TestServerSideEnd(t *testing.T) {
	e := newEnv(t)
	c := e.client("plugin")
	id := e.startLive(c, map[string]any{})
	pid := e.record().Browser.PID
	time.Sleep(200 * time.Millisecond) // the events stream is connected
	e.srv.EndStream(id, "deleted")
	ev, ok := c.WaitEvent(func(ev proto.Event) bool { return ev.Event == "ended" }, 10*time.Second)
	if !ok || hosttest.Field(ev.Data, "reason") != "deleted" {
		t.Fatalf("ended %v", ev)
	}
	waitFor(t, "the browser to exit", 7*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join("/proc", itoa(pid), "stat"))
		return err != nil || strings.Contains(string(b), ") Z")
	})
}

// TestShutdownRefusedWhileLive: shutdown needs force while a share runs.
func TestShutdownRefusedWhileLive(t *testing.T) {
	e := newEnv(t)
	c := e.client("cli")
	e.startLive(c, map[string]any{})
	if _, err := c.Call("shutdown", map[string]any{}); code(err) != "conflict" {
		t.Fatalf("shutdown while live: %v", err)
	}
	call(t, c, "shutdown", map[string]any{"force": true})
	select {
	case <-e.done:
		e.cancel = nil
	case <-time.After(10 * time.Second):
		t.Fatal("no exit after shutdown")
	}
}
