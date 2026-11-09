package supervise

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/proto"
)

// The "browser" is sh running sleep; the supervisor's arguments become
// sh's positional parameters, and the profile path is in its argv.
func testCommand() []string { return []string{"/bin/sh", "-c", "sleep 300 & sleep 300; wait"} }

func exercise(t *testing.T, mode string) {
	dir := t.TempDir()
	s := New(Config{Command: testCommand(), Profile: filepath.Join(dir, "profile"), Mode: mode})
	os.MkdirAll(filepath.Join(dir, "profile", "Default", "Service Worker", "Database"), 0o700)
	id := proto.NewId22()
	b, err := s.Launch(id, "file:///nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "profile", "Default", "Service Worker")); !os.IsNotExist(err) {
		t.Fatal("service-worker storage survived the launch")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "profile")); fi.Mode().Perm() != 0o700 {
		t.Fatal("profile isn't 0700")
	}
	fd, ok := s.Adopt(b)
	if !ok {
		t.Fatal("can't adopt a running browser")
	}
	exited := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Watch(ctx, fd, func() { close(exited) })
	other := *b
	other.StartTime++
	if _, ok := s.Adopt(&other); ok {
		t.Fatal("adopted a process with another start time")
	}
	began := time.Now()
	s.Stop(b)
	select {
	case <-exited:
	case <-time.After(7 * time.Second):
		t.Fatal("the pidfd never reported the exit")
	}
	if len(s.find(b, true)) != 0 {
		t.Fatal("processes left after stop")
	}
	t.Logf("%s: stopped in %v", mode, time.Since(began).Round(time.Millisecond))
	if mode == Systemd {
		if exec.Command("systemctl", "--user", "is-active", "--quiet", b.Unit).Run() == nil {
			t.Fatal("unit still active")
		}
	}
}

func TestProcessGroup(t *testing.T) { exercise(t, ProcessGroup) }

// TestSystemdUnit runs a real transient user unit; opt in with
// PASSSTREAM_TEST_SYSTEMD=1 (it touches the user's systemd manager).
func TestSystemdUnit(t *testing.T) {
	if os.Getenv("PASSSTREAM_TEST_SYSTEMD") != "1" {
		t.Skip("set PASSSTREAM_TEST_SYSTEMD=1 to run a transient user unit")
	}
	exercise(t, Systemd)
}

// TestSweepCatchesEscapees: a child that leaves the process group but
// names the profile is still killed.
func TestSweepCatchesEscapees(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "profile")
	// The escapee is a shell in its own session that keeps the exact
	// --user-data-dir argument ($2), as a Chromium child would.
	s := New(Config{Command: []string{"/bin/sh", "-c", `setsid sh -c 'while :; do sleep 1; done' escapee "$2" & sleep 300`, "sh"}, Profile: profile, Mode: ProcessGroup})
	b, err := s.Launch(proto.NewId22(), "file:///x")
	if err != nil {
		t.Fatal(err)
	}
	var escapee int
	waitUntil(t, "the escapee", func() bool {
		for _, v := range s.find(nil, true) {
			if st, err := s.stat(v.pid); err == nil && st.pgrp != b.PGID {
				escapee = v.pid
			}
		}
		return escapee != 0
	})
	s.Stop(b)
	if n := len(s.find(b, true)); n != 0 {
		t.Fatalf("%d processes survived", n)
	}
	if _, err := s.stat(escapee); err == nil {
		t.Fatal("the escapee survived")
	}
}

func waitUntil(t *testing.T, what string, f func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !f(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// startSleeper runs a process in its own session with the given argv tail.
func startSleeper(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"-c", "while :; do sleep 1; done", "sleeper"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

func alive(cmd *exec.Cmd) bool {
	return cmd.Process.Signal(syscall.Signal(0)) == nil && cmd.ProcessState == nil
}

// TestSweepSparesUnrelatedProcesses (review round 1, finding 3): the sweep
// matches an exact --user-data-dir=<profile> argument only. Processes whose
// argv merely contains the profile path, or a longer path that starts with
// it, are left alone; one with the exact argument is killed.
func TestSweepSparesUnrelatedProcesses(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	s := New(Config{Command: testCommand(), Profile: profile, Mode: ProcessGroup})
	unrelated := []*exec.Cmd{
		startSleeper(t, "--user-data-dir="+profile+"-backup-document"),
		startSleeper(t, profile),
		startSleeper(t, "--data="+profile+"/x"),
	}
	target := startSleeper(t, "--app=x", "--user-data-dir="+profile)
	exited := make(chan struct{})
	go func() { target.Wait(); close(exited) }()
	s.Sweep()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("the process using the profile survived the sweep")
	}
	for i, c := range unrelated {
		if !alive(c) {
			t.Fatalf("unrelated process %d was killed", i)
		}
	}
}

// TestKillRevalidatesIdentity: a victim whose start time or exe no longer
// matches what the scan saw (a reused pid) isn't signalled.
func TestKillRevalidatesIdentity(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	s := New(Config{Command: testCommand(), Profile: profile, Mode: ProcessGroup})
	c := startSleeper(t, "--user-data-dir="+profile)
	var v victim
	waitUntil(t, "the scan", func() bool {
		for _, x := range s.find(nil, true) {
			if x.pid == c.Process.Pid {
				v = x
			}
		}
		return v.pid != 0
	})
	s.kill(victim{v.pid, v.start + 1, v.exe})
	s.kill(victim{v.pid, v.start, "/not/the/exe"})
	time.Sleep(100 * time.Millisecond)
	if !alive(c) {
		t.Fatal("a process with another identity was signalled")
	}
	s.kill(v)
	waitUntil(t, "the kill", func() bool { _, err := s.stat(v.pid); return err != nil })
}

// TestProfileCleanupNeverFollowsSymlinks (review round 1, finding 2): the
// service-worker cleanup works through O_NOFOLLOW directory descriptors. A
// symlinked profile, or a symlinked Default, is refused and nothing outside
// the profile is touched; a symlink inside the deleted tree is unlinked,
// not followed.
func TestProfileCleanupNeverFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	sentinel := filepath.Join(outside, "Service Worker", "sentinel")
	os.MkdirAll(filepath.Dir(sentinel), 0o755)
	os.WriteFile(sentinel, []byte("x"), 0o644)
	check := func(what string) {
		t.Helper()
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("%s: the outside sentinel is gone", what)
		}
		if fi, _ := os.Stat(outside); fi.Mode().Perm() != 0o755 {
			t.Fatalf("%s: the outside directory was chmodded", what)
		}
	}

	// The profile's Default is a symlink to the outside directory.
	p1 := filepath.Join(dir, "p1")
	os.MkdirAll(p1, 0o700)
	os.Symlink(outside, filepath.Join(p1, "Default"))
	if err := New(Config{Profile: p1}).PrepareProfile(); err == nil {
		t.Fatal("a symlinked Default was accepted")
	}
	check("symlinked Default")

	// The profile itself is a symlink.
	p2 := filepath.Join(dir, "p2")
	os.Symlink(outside, p2)
	if err := New(Config{Profile: p2}).PrepareProfile(); err == nil {
		t.Fatal("a symlinked profile was accepted")
	}
	check("symlinked profile")
	if _, err := New(Config{Command: testCommand(), Profile: p2}).Launch(proto.NewId22(), "file:///x"); err == nil {
		t.Fatal("launched with a symlinked profile")
	}

	// A symlink inside Service Worker is removed without following it.
	p3 := filepath.Join(dir, "p3")
	sw := filepath.Join(p3, "Default", "Service Worker")
	os.MkdirAll(filepath.Join(sw, "Database"), 0o700)
	os.Symlink(filepath.Join(outside, "Service Worker"), filepath.Join(sw, "Database", "link"))
	os.Symlink(outside, filepath.Join(sw, "dirlink"))
	if err := New(Config{Profile: p3}).PrepareProfile(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sw); !os.IsNotExist(err) {
		t.Fatal("service-worker storage survived")
	}
	check("symlinks inside the tree")
}
