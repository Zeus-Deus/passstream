package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/daemon"
	"github.com/passstream/passstream/internal/host/hosttest"
	"github.com/passstream/passstream/internal/host/listen"
	"github.com/passstream/passstream/internal/host/procnet"
)

func TestMain(m *testing.M) {
	// The test binary is also the detached clipboard helper (wlClipboard).
	if len(os.Args) > 1 && os.Args[1] == serveCommand {
		os.Exit(Main(os.Args[1:]))
	}
	hosttest.MaybeFakeBrowser()
	os.Exit(m.Run())
}

type result struct {
	code           int
	stdout, stderr string
}

func runCLI(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	env := &Env{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, IsTTY: func() bool { return false },
		ReadSecret: func(string) (string, error) { t.Fatal("prompted"); return "", nil },
		OpenFD:     func(int) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(stdin)), nil },
		Sleep:      func(time.Duration) {}}
	code := Run(args, env)
	return result{code, out.String(), errb.String()}
}

func TestUsageNeverEchoesSecrets(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for _, args := range [][]string{
		{"share", "--pin", "4821"}, {"share", "--pin=hunter2hunter2"}, {"pin", "set", "4821"}, {"share", "--pin", "1234567"},
	} {
		r := runCLI(t, "", args...)
		if r.code != 2 {
			t.Errorf("%v: exit %d", args, r.code)
		}
		for _, s := range []string{"4821", "hunter2", "1234567"} {
			if strings.Contains(r.stderr+r.stdout, s) {
				t.Errorf("%v echoed a value: %q", args, r.stderr)
			}
		}
	}
	for _, args := range [][]string{
		{"knock", "approve", "AAAAAAAAAAAAAAAAAAAAAA"}, // no --code
		{"knock", "approve", "AAAAAAAAAAAAAAAAAAAAAA", "--code", "purple"},
		{"knock", "approve", "AAAAAAAAAAAAAAAAAAAAAA", "--code", "K7P·purple"},
		{"knock", "approve", "AAAAAAAAAAAAAAAAAAAAAA", "--code", "K7PX"},
		{"kick", "not-an-id"}, {"pin", "none", "--pin-stdin"}, {"frobnicate"}, {"share", "--pin-stdin"},
		{"pin", "set", "passphrase"}, // no TTY and no --pin-stdin
	} {
		if r := runCLI(t, "", args...); r.code != 2 {
			t.Errorf("%v: exit %d (%s)", args, r.code, r.stderr)
		}
	}
	for in, want := range map[string]string{" k7p ": "K7P", "acd": "ACD"} {
		if got, ok := NormalizeCode(in); !ok || got != want {
			t.Errorf("%q → %q %v", in, got, ok)
		}
	}
	if r := runCLI(t, "", "status", "--json"); r.code != 3 || !strings.Contains(r.stderr, `"daemon_unreachable"`) {
		t.Errorf("unreachable: %+v", r)
	}
}

// daemonFor runs a daemon whose socket is under a temp XDG_RUNTIME_DIR.
func daemonFor(t *testing.T, mutate func(*daemon.Config)) *hosttest.FakeServer {
	t.Helper()
	rt := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", rt)
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	os.WriteFile(key, []byte("k1"), 0o600)
	srv, err := hosttest.NewFakeServer("k1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	ln, err := listen.SelfBind(filepath.Join(rt, "passstream"), 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg := daemon.Config{Version: "test", ServerURL: srv.URL, KeyPath: key, RuntimeDir: filepath.Join(rt, "passstream"),
		StateDir: filepath.Join(dir, "state"), Profile: filepath.Join(dir, "profile"), IdleExit: -1, StopWait: 300 * time.Millisecond,
		Port: ln.HTTP.Addr().(*net.TCPAddr).Port, Browser: hosttest.BrowserCommand(os.Args[0], "page", "-")}
	if mutate != nil {
		mutate(&cfg)
	}
	d, err := daemon.New(cfg, ln)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() {
		runCLI(t, "", "stop")
		cancel()
		<-done
	})
	return srv
}

func TestCLIAgainstDaemon(t *testing.T) {
	daemonFor(t, nil)
	r := runCLI(t, "", "share", "--json", "--label", "demo")
	if r.code != 0 {
		t.Fatalf("share: %+v", r)
	}
	var sh map[string]any
	json.Unmarshal([]byte(r.stdout), &sh)
	link, _ := sh["link"].(string)
	if !strings.Contains(link, "/v#c=") || sh["live"] != true {
		t.Fatalf("share output %v", sh)
	}
	if r := runCLI(t, "", "share"); r.code != 1 || !strings.Contains(r.stderr, "already_live") {
		t.Fatalf("second share: %+v", r)
	}
	r = runCLI(t, "", "pin")
	if r.code != 0 || !strings.Contains(r.stdout, "PIN · 4 digits ••••") {
		t.Fatalf("pin: %+v", r)
	}
	if r := runCLI(t, "2468\n", "pin", "rotate", "--pin-stdin"); r.code != 0 {
		t.Fatalf("rotate: %+v", r)
	}
	r = runCLI(t, "", "pin", "--reveal", "--json")
	if !strings.Contains(r.stdout, `"pin":"2468"`) {
		t.Fatalf("reveal: %+v", r)
	}
	if r := runCLI(t, "", "pin", "none"); r.code != 0 {
		t.Fatalf("pin none: %+v", r)
	}
	if r := runCLI(t, "", "pin", "--json"); !strings.Contains(r.stdout, `"kind":"none"`) {
		t.Fatalf("pin after none: %+v", r)
	}
	for _, cmd := range []string{"pause", "lock", "unlock", "resume"} {
		if r := runCLI(t, "", cmd); r.code != 0 {
			t.Fatalf("%s: %+v", cmd, r)
		}
	}
	r = runCLI(t, "", "status", "--json")
	var hs map[string]any
	if json.Unmarshal([]byte(r.stdout), &hs) != nil || hosttest.Field(hs, "stream", "phase") != "live" {
		t.Fatalf("status: %+v", r)
	}
	if strings.Contains(r.stdout, "2468") || strings.Contains(r.stdout, link) {
		t.Fatal("status printed a secret")
	}
	if r := runCLI(t, "", "kick", "AAAAAAAAAAAAAAAAAAAAAA", "--json"); r.code != 1 || !strings.Contains(r.stderr, `"unknown_admission"`) {
		t.Fatalf("kick: %+v", r)
	}
	if r := runCLI(t, "", "stop"); r.code != 0 {
		t.Fatalf("stop: %+v", r)
	}
	if r := runCLI(t, "", "stop", "--json"); r.code != 1 || !strings.Contains(r.stderr, `"not_live"`) {
		t.Fatalf("stop again: %+v", r)
	}
}

func TestCLIPortNotOwnedExit4(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "net"), 0o700)
	hdr := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(hdr), 0o600)
	os.WriteFile(filepath.Join(root, "net", "tcp6"), []byte(hdr), 0o600)
	daemonFor(t, func(c *daemon.Config) { c.Proc = procnet.Root(root) })
	if r := runCLI(t, "", "share"); r.code != 4 || !strings.Contains(r.stderr, "port_not_owned") {
		t.Fatalf("share: %+v", r)
	}
}

// curtain on|off is the pause latch; annotations on|off|clear maps to the
// annotations ops. Bad arguments are usage errors before any daemon call.
func TestCurtainAndAnnotationsCommands(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for _, args := range [][]string{{"curtain"}, {"curtain", "up"}, {"curtain", "on", "off"}, {"annotations"}, {"annotations", "maybe"}} {
		if r := runCLI(t, "", args...); r.code != 2 {
			t.Errorf("%v: exit %d (%s)", args, r.code, r.stderr)
		}
	}
	daemonFor(t, nil)
	if r := runCLI(t, "", "share", "--json"); r.code != 0 {
		t.Fatalf("share: %+v", r)
	}
	status := func() map[string]any {
		var hs map[string]any
		json.Unmarshal([]byte(runCLI(t, "", "status", "--json").stdout), &hs)
		return hs
	}
	if r := runCLI(t, "", "curtain", "on"); r.code != 0 {
		t.Fatalf("curtain on: %+v", r)
	}
	if hosttest.Field(status(), "stream", "access", "latches", "paused") != true {
		t.Fatalf("curtain on did not latch pause: %v", status())
	}
	if r := runCLI(t, "", "curtain", "off"); r.code != 0 {
		t.Fatalf("curtain off: %+v", r)
	}
	if hosttest.Field(status(), "stream", "access", "latches", "paused") != false {
		t.Fatalf("curtain off did not clear pause: %v", status())
	}
	for _, a := range []string{"on", "off", "clear"} {
		if r := runCLI(t, "", "annotations", a, "--json"); r.code != 0 {
			t.Fatalf("annotations %s: %+v", a, r)
		}
	}
}
