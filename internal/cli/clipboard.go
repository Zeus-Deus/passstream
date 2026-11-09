package cli

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Clipboard copies values and takes them off the clipboard later (CONTRACT
// Appendix A #20).
type Clipboard interface {
	// SensitiveSupported reports whether wl-copy knows --sensitive.
	SensitiveSupported() bool
	// Copy puts value on the clipboard over stdin, never argv, for at most
	// keep, without keeping this process alive. clears is false when only
	// a plain copy was possible: the value then stays until replaced.
	Copy(value string, sensitive bool, keep time.Duration) (clears bool, err error)
}

// serveCommand is the hidden subcommand of the detached clipboard owner:
// `__clipboard-serve --after-ms N [--sensitive]`, the value on stdin (never
// argv) and a status pipe on fd 3.
//
// It runs `wl-copy --foreground`, which owns the selection while it runs
// and exits as soon as anything else takes the selection. After N ms the
// helper stops it, and the compositor clears a selection whose owner went
// away. So the clear never reads the clipboard and can't erase a newer
// copy: once something replaced the value, there's nothing left to stop.
// (A clipboard manager that re-offers a selection when its owner exits
// keeps the value; a history may already have stored it. --sensitive asks
// both not to.)
const serveCommand = "__clipboard-serve"

// maxClipValue bounds what the helper reads: a link or a passphrase.
const maxClipValue = 4096

// Helper timings; variables so tests can shorten them.
var (
	pasteTimeout = time.Second     // one wl-paste while confirming the copy
	confirmFor   = 3 * time.Second // how long the helper tries to confirm it
	readyTimeout = 5 * time.Second // how long the CLI waits for the helper
	stopGrace    = 2 * time.Second // SIGTERM to SIGKILL for wl-copy
	copyTimeout  = 5 * time.Second // a plain wl-copy (it forks to serve)
	waitDelay    = 200 * time.Millisecond
)

// Status bytes from the helper to the CLI.
const (
	statusReady  = 'k' // the copy is served and will be stopped after N ms
	statusFailed = 'f' // wl-copy failed: nothing was copied
)

func (e *Env) clipboard() Clipboard {
	if e.Clipboard != nil {
		return e.Clipboard
	}
	return wlClipboard{}
}

func (e *Env) clearAfter() time.Duration {
	if e.ClearAfter > 0 {
		return e.ClearAfter
	}
	return 30 * time.Second
}

// wlClipboard is wl-clipboard.
type wlClipboard struct{}

func (wlClipboard) SensitiveSupported() bool {
	ctx, cancel := context.WithTimeout(context.Background(), copyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wl-copy", "--help")
	cmd.WaitDelay = waitDelay
	out, _ := cmd.CombinedOutput()
	return bytes.Contains(out, []byte("--sensitive"))
}

var errNoHelper = errors.New("can't start the clipboard helper")

func (c wlClipboard) Copy(value string, sensitive bool, keep time.Duration) (bool, error) {
	if len(value) == 0 || len(value) > maxClipValue {
		return false, errors.New("value too long")
	}
	err := serve(value, sensitive, keep)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, errNoHelper) {
		return false, err
	}
	// No helper: a plain copy, which stays until something replaces it.
	return false, plainCopy(value, sensitive)
}

func wlCopyArgs(sensitive bool, more ...string) []string {
	if sensitive {
		more = append(more, "--sensitive")
	}
	return more
}

func plainCopy(value string, sensitive bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), copyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wl-copy", wlCopyArgs(sensitive)...)
	cmd.Stdin = strings.NewReader(value)
	cmd.WaitDelay = waitDelay
	return cmd.Run()
}

// serve starts the detached owner in its own session, hands it the value
// over a pipe and waits for its status byte.
func serve(value string, sensitive bool, keep time.Duration) error {
	exe, err := os.Executable()
	if err != nil {
		return errNoHelper
	}
	in, w, err := os.Pipe()
	if err != nil {
		return errNoHelper
	}
	sr, sw, err := os.Pipe()
	if err != nil {
		in.Close()
		w.Close()
		return errNoHelper
	}
	defer sr.Close()
	args := append([]string{serveCommand, "--after-ms", strconv.FormatInt(keep.Milliseconds(), 10)}, wlCopyArgs(sensitive)...)
	cmd := exec.Command(exe, args...)
	cmd.Stdin = in // *os.File pipes: no copying goroutines that would need Wait
	cmd.ExtraFiles = []*os.File{sw}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	in.Close()
	sw.Close()
	if err != nil {
		w.Close()
		return errNoHelper
	}
	// Smaller than a pipe buffer, so this never blocks.
	_, werr := w.Write([]byte(value))
	w.Close()
	b := make([]byte, 1)
	sr.SetReadDeadline(time.Now().Add(readyTimeout))
	n, _ := sr.Read(b)
	if werr == nil && n == 1 && b[0] == statusReady {
		cmd.Process.Release()
		return nil
	}
	// Failed, or silent past the deadline: stop the helper and its wl-copy
	// (one process group).
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	cmd.Wait()
	return errors.New("wl-copy failed")
}

// clipboardServe is the helper's entry point.
func clipboardServe(args []string, env *Env) int {
	d := env.clearAfter()
	sensitive := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--after-ms" && i+1 < len(args):
			ms, err := strconv.Atoi(args[i+1])
			if err != nil || ms <= 0 || ms > 10*60*1000 {
				return exitUsage
			}
			d = time.Duration(ms) * time.Millisecond
			i++
		case args[i] == "--sensitive":
			sensitive = true
		default:
			return exitUsage
		}
	}
	status := os.NewFile(3, "status")
	if status == nil {
		return exitUsage
	}
	defer status.Close()
	return runServe(env.Stdin, status, d, sensitive)
}

// runServe owns the selection for d: it starts `wl-copy --foreground`,
// confirms the copy (bounded), reports on status and closes it, then stops
// wl-copy after d unless it already exited because something newer took
// the selection. Every external call is bounded, so the helper always ends
// within d plus a few seconds.
func runServe(stdin io.Reader, status io.WriteCloser, d time.Duration, sensitive bool) int {
	report := func(b byte) {
		status.Write([]byte{b})
		status.Close()
	}
	value, err := io.ReadAll(io.LimitReader(stdin, maxClipValue+1))
	if err != nil || len(value) == 0 || len(value) > maxClipValue {
		report(statusFailed)
		return exitUsage
	}
	start := time.Now()
	cp := exec.Command("wl-copy", wlCopyArgs(sensitive, "--foreground")...)
	cp.Stdin = bytes.NewReader(value)
	// If the helper dies, wl-copy goes too: the clipboard clears early
	// rather than never.
	cp.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	cp.WaitDelay = waitDelay
	if err := cp.Start(); err != nil {
		report(statusFailed)
		return exitError
	}
	exited := make(chan error, 1)
	go func() { exited <- cp.Wait() }()

	// Confirm that the clipboard serves the value. wl-copy failing exits
	// non-zero at once; an exit 0 means something newer already took the
	// selection. A wl-paste that can't confirm in time (stalled, missing)
	// leaves the copy unconfirmed but still bounded by d.
	confirmBy := time.Now().Add(confirmFor)
	for {
		select {
		case err := <-exited:
			if err != nil {
				report(statusFailed)
				return exitError
			}
			report(statusReady)
			return exitOK
		default:
		}
		if cur, err := wlPaste(); err == nil && subtle.ConstantTimeCompare(cur, value) == 1 {
			break
		}
		if time.Now().After(confirmBy) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	report(statusReady)

	timer := time.NewTimer(time.Until(start.Add(d)))
	defer timer.Stop()
	select {
	case <-exited:
		return exitOK // replaced: nothing of ours is left to clear
	case <-timer.C:
	}
	cp.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(stopGrace):
		cp.Process.Kill()
		<-exited
	}
	return exitOK
}

func wlPaste() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pasteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wl-paste", "--no-newline")
	cmd.WaitDelay = waitDelay
	return cmd.Output()
}
