package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for review round 2 (GPT-6.1-Sol): the clipboard clear
// can't erase a newer copy and can't hang.

// fakeCompositor is wl-copy and wl-paste scripts on PATH that model the
// compositor with files: clip is the selection, owner the pid of the
// wl-copy --foreground that serves it. That wl-copy exits 0 once it's no
// longer the owner (another copy took the selection), and the selection
// clears when its owner is stopped. hang makes wl-paste stall, keeping its
// stdout open through a child; fail makes wl-copy fail.
type fakeCompositor struct{ dir, clip, owner, args, hang, fail string }

func newFakeCompositor(t *testing.T) *fakeCompositor {
	t.Helper()
	d := t.TempDir()
	f := &fakeCompositor{dir: d, clip: filepath.Join(d, "clip"), owner: filepath.Join(d, "owner"),
		args: filepath.Join(d, "args"), hang: filepath.Join(d, "hang"), fail: filepath.Join(d, "fail")}
	os.WriteFile(f.clip, []byte("older"), 0o600)
	os.WriteFile(f.owner, []byte("none"), 0o600)
	copyScript := `#!/bin/sh
echo "$@" >> '` + f.args + `'
[ -e '` + f.fail + `' ] && exit 1
case "$1" in --foreground) ;; *) cat > '` + f.clip + `'; exit 0 ;; esac
cat > '` + f.clip + `.tmp' && mv '` + f.clip + `.tmp' '` + f.clip + `'
echo $$ > '` + f.owner + `'
trap '[ "$(cat '` + f.owner + `')" = $$ ] && : > '` + f.clip + `'; exit 0' TERM
while [ "$(cat '` + f.owner + `')" = $$ ]; do sleep 0.02; done
exit 0
`
	pasteScript := `#!/bin/sh
[ -e '` + f.hang + `' ] && { sleep 60; exit 0; }
cat '` + f.clip + `'
`
	os.WriteFile(filepath.Join(d, "wl-copy"), []byte(copyScript), 0o700)
	os.WriteFile(filepath.Join(d, "wl-paste"), []byte(pasteScript), 0o700)
	t.Setenv("PATH", d+":"+os.Getenv("PATH"))
	return f
}

func (f *fakeCompositor) read(t *testing.T) string {
	b, _ := os.ReadFile(f.clip)
	return string(b)
}

// userCopies is a newer copy taking the selection.
func (f *fakeCompositor) userCopies(v string) {
	os.WriteFile(f.owner, []byte("user"), 0o600)
	os.WriteFile(f.clip, []byte(v), 0o600)
}

func shortTimings(t *testing.T) {
	p, c, g, w := pasteTimeout, confirmFor, stopGrace, waitDelay
	pasteTimeout, confirmFor, stopGrace, waitDelay = 100*time.Millisecond, 300*time.Millisecond, time.Second, 50*time.Millisecond
	t.Cleanup(func() { pasteTimeout, confirmFor, stopGrace, waitDelay = p, c, g, w })
}

type statusBuf struct {
	bytes.Buffer
	closed chan struct{}
}

func newStatus() *statusBuf      { return &statusBuf{closed: make(chan struct{})} }
func (s *statusBuf) Close() error { close(s.closed); return nil }

func serveAsync(value string, d time.Duration, sensitive bool) (*statusBuf, <-chan int) {
	st := newStatus()
	done := make(chan int, 1)
	go func() { done <- runServe(strings.NewReader(value), st, d, sensitive) }()
	return st, done
}

func waitStatus(t *testing.T, st *statusBuf) string {
	t.Helper()
	select {
	case <-st.closed:
		return st.String()
	case <-time.After(3 * time.Second):
		t.Fatal("no status from the helper")
		return ""
	}
}

func waitExit(t *testing.T, done <-chan int, within time.Duration) int {
	t.Helper()
	select {
	case c := <-done:
		return c
	case <-time.After(within):
		t.Fatalf("the helper didn't exit within %v", within)
		return -1
	}
}

// TestClipboardServeClearsByExiting: the value is served for d and the
// selection clears when the owner stops. The value never reaches argv.
func TestClipboardServeClearsByExiting(t *testing.T) {
	shortTimings(t)
	f := newFakeCompositor(t)
	st, done := serveAsync("4821", 400*time.Millisecond, true)
	if s := waitStatus(t, st); s != string(statusReady) {
		t.Fatalf("status %q", s)
	}
	if got := f.read(t); got != "4821" {
		t.Fatalf("while served: %q", got)
	}
	if c := waitExit(t, done, 3*time.Second); c != exitOK {
		t.Fatalf("exit %d", c)
	}
	if got := f.read(t); got != "" {
		t.Fatalf("after the deadline: %q", got)
	}
	args, _ := os.ReadFile(f.args)
	if string(args) != "--foreground --sensitive\n" {
		t.Fatalf("wl-copy argv %q", args)
	}
}

// TestClipboardServeNeverErasesNewerCopy (finding 8): a copy made at any
// point after ours, including right before the deadline, survives; the
// owner exits when replaced and there's nothing left to clear.
func TestClipboardServeNeverErasesNewerCopy(t *testing.T) {
	shortTimings(t)
	for _, when := range []time.Duration{0, 150 * time.Millisecond, 390 * time.Millisecond} {
		f := newFakeCompositor(t)
		start := time.Now()
		st, done := serveAsync("4821", 400*time.Millisecond, false)
		waitStatus(t, st)
		time.Sleep(time.Until(start.Add(when)))
		f.userCopies("user's newer clipboard")
		if c := waitExit(t, done, 3*time.Second); c != exitOK {
			t.Fatalf("exit %d", c)
		}
		time.Sleep(100 * time.Millisecond)
		if got := f.read(t); got != "user's newer clipboard" {
			t.Fatalf("copy at +%v: the newer copy became %q", when, got)
		}
	}
}

// TestClipboardServeBoundedWhenPasteStalls (finding 8): a stalled wl-paste
// (one that keeps its stdout open) can't keep the helper alive: the
// confirmation gives up, the copy is still stopped at the deadline, and
// the helper exits.
func TestClipboardServeBoundedWhenPasteStalls(t *testing.T) {
	shortTimings(t)
	f := newFakeCompositor(t)
	os.WriteFile(f.hang, nil, 0o600)
	start := time.Now()
	st, done := serveAsync("4821", 600*time.Millisecond, false)
	if s := waitStatus(t, st); s != string(statusReady) {
		t.Fatalf("status %q", s)
	}
	waitExit(t, done, 3*time.Second)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the helper took %v", el)
	}
	if got := f.read(t); got != "" {
		t.Fatalf("after the deadline: %q", got)
	}
}

// TestClipboardServeReportsFailure: a failing wl-copy is reported, not
// treated as a scheduled clear.
func TestClipboardServeReportsFailure(t *testing.T) {
	shortTimings(t)
	f := newFakeCompositor(t)
	os.WriteFile(f.fail, nil, 0o600)
	st, done := serveAsync("4821", time.Second, false)
	if s := waitStatus(t, st); s != string(statusFailed) {
		t.Fatalf("status %q", s)
	}
	if c := waitExit(t, done, 2*time.Second); c != exitError {
		t.Fatalf("exit %d", c)
	}
}

// TestWLClipboardDetachedHelper runs the real path: the CLI starts the
// detached helper (this test binary, see TestMain), hands it the value
// over a pipe, returns once it's served, and the helper clears it later on
// its own. Usage errors don't start anything.
func TestWLClipboardDetachedHelper(t *testing.T) {
	f := newFakeCompositor(t)
	start := time.Now()
	clears, err := wlClipboard{}.Copy("https://s.example/v#c=x", true, 500*time.Millisecond)
	if err != nil || !clears {
		t.Fatalf("copy: clears=%v err=%v", clears, err)
	}
	if got := f.read(t); got != "https://s.example/v#c=x" {
		t.Fatalf("after Copy returned: %q", got)
	}
	for f.read(t) != "" {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the detached helper never cleared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if args, _ := os.ReadFile(f.args); strings.Contains(string(args), "s.example") {
		t.Fatal("the value reached argv")
	}

	os.WriteFile(f.fail, nil, 0o600)
	if _, err := (wlClipboard{}).Copy("4821", false, time.Second); err == nil {
		t.Fatal("a failing wl-copy was reported as copied")
	}
	if code := Run([]string{serveCommand, "--after-ms", "x"}, &Env{Stdin: strings.NewReader("v")}); code != exitUsage {
		t.Fatalf("bad delay: exit %d", code)
	}
}
