package supervise

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/passstream/passstream/internal/host/proto"
)

// Regression tests for review round 2 (GPT-6.1-Sol).

// startReaped starts a sleeper (like startSleeper) and reaps it in the
// background; the channel closes when it exits.
func startReaped(t *testing.T, args ...string) <-chan struct{} {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"-c", "while :; do sleep 1; done", "sleeper"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-done })
	return done
}

func exitedWithin(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestLaunchValidatesProfileBeforeSweep (finding 3): a launch with a
// symlinked profile is refused before the sweep, so a process using the
// symlink's target through that path survives. Sweep on its own also
// refuses to sweep by a symlinked or foreign profile.
func TestLaunchValidatesProfileBeforeSweep(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "ordinary-browser-profile")
	os.Mkdir(outside, 0o700)
	profile := filepath.Join(dir, "chromium")
	os.Symlink(outside, profile)
	other := startReaped(t, "--user-data-dir="+profile)

	s := New(Config{Command: testCommand(), Profile: profile, Mode: ProcessGroup})
	if _, err := s.Launch(proto.NewId22(), "file:///x"); err == nil {
		t.Fatal("a symlinked profile was accepted")
	}
	s.Sweep()
	if exitedWithin(other, 300*time.Millisecond) {
		t.Fatal("a process using the refused profile was killed")
	}
}

// TestSweepStillKillsByOwnProfile: the validation keeps the sweep for a
// real profile directory of ours.
func TestSweepStillKillsByOwnProfile(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	os.Mkdir(profile, 0o700)
	target := startReaped(t, "--user-data-dir="+profile)
	New(Config{Command: testCommand(), Profile: profile, Mode: ProcessGroup}).Sweep()
	if !exitedWithin(target, 3*time.Second) {
		t.Fatal("the process using our profile survived the sweep")
	}
}

// TestProfileCleanupUnderSymlinkExchange: while Default and a symlink to an
// outside directory are swapped continuously, the cleanup never deletes
// anything outside the profile (it may refuse some rounds).
func TestProfileCleanupUnderSymlinkExchange(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "p")
	def := filepath.Join(p, "Default")
	alt := filepath.Join(p, "holding")
	outside := filepath.Join(d, "outside")
	sentinel := filepath.Join(outside, "Service Worker", "sentinel")
	os.MkdirAll(filepath.Dir(sentinel), 0o755)
	os.WriteFile(sentinel, []byte("keep"), 0o600)
	os.MkdirAll(def, 0o700)
	os.Symlink(outside, alt)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			default:
			}
			unix.Renameat2(unix.AT_FDCWD, def, unix.AT_FDCWD, alt, unix.RENAME_EXCHANGE)
		}
	}()
	s := New(Config{Profile: p})
	for i := 0; i < 500; i++ {
		s.PrepareProfile()
	}
	close(done)
	<-stopped
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "keep" {
		t.Fatalf("outside modified: %v", err)
	}
}
