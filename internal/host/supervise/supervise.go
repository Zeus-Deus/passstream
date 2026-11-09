// Package supervise launches, watches, re-adopts and stops the sender
// browser (HOST-CONTROL §5.3 "Stop is locally enforceable" and §5.9): in a
// transient systemd user unit when the daemon runs under systemd, otherwise
// in its own session and process group, watched through a pidfd.
package supervise

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/passstream/passstream/internal/host/state"
)

// Supervision modes reported in HostStatus.daemon.supervision.
const (
	Systemd      = "systemd"
	ProcessGroup = "process_group"
)

// Config says how to start the browser.
type Config struct {
	// Command is the browser and any extra flags, e.g. ["chromium"].
	Command []string
	// Profile is the dedicated --user-data-dir.
	Profile string
	// Mode is Systemd or ProcessGroup.
	Mode string
	// Proc is the procfs root ("/proc").
	Proc string
	// Logf logs without secrets.
	Logf func(format string, a ...any)
}

// Supervisor runs at most one sender browser at a time.
type Supervisor struct {
	cfg Config
	mu  sync.Mutex
}

// New returns a supervisor.
func New(cfg Config) *Supervisor {
	if cfg.Proc == "" {
		cfg.Proc = "/proc"
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Supervisor{cfg: cfg}
}

// Mode reports the supervision mode.
func (s *Supervisor) Mode() string { return s.cfg.Mode }

// UnitName is the transient unit of a stream's browser.
func UnitName(streamID string) string { return "passstream-sender-" + streamID + ".service" }

// Args are the browser arguments. The only value in them that matters is
// the --app file:// path; the launch key is inside that guarded file,
// never in argv (HOST-CONTROL §5.7).
func (s *Supervisor) Args(appURL string) []string {
	args := append([]string{}, s.cfg.Command[1:]...)
	return append(args,
		"--app="+appURL,
		"--user-data-dir="+s.cfg.Profile,
		"--class=passstream-sender",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-sync",
		"--password-store=basic",
	)
}

// PrepareProfile creates the dedicated profile (0700) and deletes its
// service-worker storage, so nothing registered by an earlier page, or by a
// foreign listener's page in self-bound mode, survives into this launch
// (CONTRACT Appendix A, implementation note).
//
// The profile itself must be a real directory owned by us: a symlink or a
// foreign directory is refused. The cleanup never follows a symlink: it
// works on directory descriptors opened with O_NOFOLLOW one component at a
// time, and a symlink inside the tree is unlinked, not followed. The
// profile's ancestors are the user's own paths and may be symlinks.
//
// Out of scope: a process of the same user that moves a directory out of
// the profile while the cleanup is inside it. The cleanup keeps working
// through the descriptor it already holds, so it can delete that
// directory's contents at their new place. Only the user can do that, and
// the user can delete those files anyway; no other user can reach the
// 0700 profile.
func (s *Supervisor) PrepareProfile() error {
	fd, err := s.openProfile()
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return cleanProfile(fd)
}

// openProfile creates the profile if needed and opens it, refusing a
// symlink or a directory that isn't ours. It deletes nothing.
func (s *Supervisor) openProfile() (int, error) {
	if err := os.MkdirAll(filepath.Dir(s.cfg.Profile), 0o700); err != nil {
		return -1, err
	}
	if err := os.Mkdir(s.cfg.Profile, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return -1, err
	}
	fd, err := unix.Open(s.cfg.Profile, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("the profile isn't a plain directory: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if int(st.Uid) != os.Geteuid() || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(fd)
		return -1, errors.New("the profile directory isn't ours")
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// cleanProfile deletes Default/Service Worker under the open profile.
func cleanProfile(fd int) error {
	def, err := unix.Openat(fd, "Default", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("the profile's Default isn't a plain directory: %w", err)
	}
	defer unix.Close(def)
	return removeAt(def, "Service Worker")
}

// profileOwned reports whether the profile path is ours to sweep by: absent,
// or a real directory (not a symlink) owned by us. A process naming a
// symlinked or foreign profile is someone else's browser.
func (s *Supervisor) profileOwned() bool {
	if s.cfg.Profile == "" {
		return false
	}
	var st unix.Stat_t
	err := unix.Lstat(s.cfg.Profile, &st)
	if errors.Is(err, unix.ENOENT) {
		return true
	}
	return err == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR && int(st.Uid) == os.Geteuid()
}

// removeAt deletes name under the directory dirfd, recursively, without
// following symlinks: directories are entered through O_NOFOLLOW
// descriptors and anything else (a symlink included) is unlinked.
func removeAt(dirfd int, name string) error {
	return removeAtDepth(dirfd, name, 0)
}

func removeAtDepth(dirfd int, name string, depth int) error {
	if depth > 64 {
		return errors.New("profile tree too deep")
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(dirfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		return nil
	}
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name) // owns fd
	defer f.Close()
	for {
		names, err := f.Readdirnames(256)
		for _, n := range names {
			if e := removeAtDepth(fd, n, depth+1); e != nil {
				return e
			}
		}
		if err != nil {
			break // io.EOF, or a read error that the rmdir below reports
		}
		if len(names) == 0 {
			break
		}
	}
	if err := unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// Launch starts the browser for a stream and returns its record.
func (s *Supervisor) Launch(streamID, appURL string) (*state.Browser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cfg.Command) == 0 {
		return nil, errors.New("no browser command")
	}
	// Validate before the sweep, which kills whatever uses the profile: a
	// profile the launch would refuse must not cost anyone a process.
	fd, err := s.openProfile()
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	defer unix.Close(fd)
	s.sweepProfileLocked()
	if err := cleanProfile(fd); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	if s.cfg.Mode == Systemd {
		return s.launchUnit(streamID, appURL)
	}
	return s.launchGroup(appURL)
}

func devNull() (*os.File, error) { return os.OpenFile(os.DevNull, os.O_RDWR, 0) }

func (s *Supervisor) launchGroup(appURL string) (*state.Browser, error) {
	cmd := exec.Command(s.cfg.Command[0], s.Args(appURL)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = cleanEnv()
	null, err := devNull()
	if err != nil {
		return nil, err
	}
	defer null.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	go cmd.Wait() // reap; exit is observed through the pidfd
	st, err := s.stat(pid)
	if err != nil {
		return nil, fmt.Errorf("browser exited at once")
	}
	return &state.Browser{Kind: ProcessGroup, PID: pid, StartTime: st.start, PGID: pid}, nil
}

func (s *Supervisor) launchUnit(streamID, appURL string) (*state.Browser, error) {
	unit := UnitName(streamID)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	exec.CommandContext(ctx, "systemctl", "--user", "stop", unit).Run()
	args := []string{"--user", "--unit=" + unit, "--collect", "--quiet", "--property=TimeoutStopSec=5", "--"}
	args = append(args, s.cfg.Command[0])
	args = append(args, s.Args(appURL)...)
	cmd := exec.CommandContext(ctx, "systemd-run", args...)
	cmd.Env = cleanEnv()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("systemd-run failed")
	}
	pid, err := mainPID(ctx, unit)
	if err != nil {
		return nil, err
	}
	st, err := s.stat(pid)
	if err != nil {
		return nil, fmt.Errorf("browser exited at once")
	}
	return &state.Browser{Kind: Systemd, Unit: unit, PID: pid, StartTime: st.start, PGID: st.pgrp}, nil
}

func mainPID(ctx context.Context, unit string) (int, error) {
	for i := 0; i < 20; i++ {
		out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "-p", "MainPID", "--value", unit).Output()
		if err == nil {
			if pid, _ := strconv.Atoi(strings.TrimSpace(string(out))); pid > 0 {
				return pid, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0, errors.New("no main pid for the sender unit")
}

func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LISTEN_") || strings.HasPrefix(kv, "NOTIFY_SOCKET=") || strings.HasPrefix(kv, "PASSSTREAM_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

type procStat struct {
	pgrp  int
	start uint64
}

// stat reads fields 5 (pgrp) and 22 (starttime) of /proc/<pid>/stat for a
// process of our uid.
func (s *Supervisor) stat(pid int) (procStat, error) {
	dir := filepath.Join(s.cfg.Proc, strconv.Itoa(pid))
	fi, err := os.Stat(dir)
	if err != nil {
		return procStat{}, err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		return procStat{}, errors.New("not our process")
	}
	b, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return procStat{}, err
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return procStat{}, errors.New("bad stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return procStat{}, errors.New("bad stat")
	}
	if f[0] == "Z" || f[0] == "X" {
		return procStat{}, errors.New("exited")
	}
	pgrp, _ := strconv.Atoi(f[5-3])
	start, err := strconv.ParseUint(f[22-3], 10, 64)
	if err != nil {
		return procStat{}, err
	}
	return procStat{pgrp: pgrp, start: start}, nil
}

// Adopt opens a pidfd on a recorded browser after a daemon restart and
// accepts it only if it's still the same process of our uid (and, under
// systemd, its unit is still active).
func (s *Supervisor) Adopt(b *state.Browser) (int, bool) {
	if b == nil || b.PID <= 0 {
		return -1, false
	}
	fd, err := unix.PidfdOpen(b.PID, 0)
	if err != nil {
		return -1, false
	}
	st, err := s.stat(b.PID)
	if err != nil || st.start != b.StartTime {
		unix.Close(fd)
		return -1, false
	}
	if b.Kind == Systemd {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if exec.CommandContext(ctx, "systemctl", "--user", "is-active", "--quiet", b.Unit).Run() != nil {
			unix.Close(fd)
			return -1, false
		}
	}
	return fd, true
}

// Open opens a pidfd for a browser just launched.
func (s *Supervisor) Open(b *state.Browser) (int, error) {
	fd, err := unix.PidfdOpen(b.PID, 0)
	if err != nil {
		return -1, err
	}
	if st, err := s.stat(b.PID); err != nil || st.start != b.StartTime {
		unix.Close(fd)
		return -1, errors.New("browser already gone")
	}
	return fd, nil
}

// Watch calls onExit once when the pidfd becomes readable (the process
// exited). It closes the pidfd when it returns or when ctx ends.
func Watch(ctx context.Context, pidfd int, onExit func()) {
	go func() {
		defer unix.Close(pidfd)
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		for ctx.Err() == nil {
			n, err := unix.Poll(fds, 500)
			if err != nil && !errors.Is(err, unix.EINTR) {
				return
			}
			if n > 0 {
				if ctx.Err() == nil {
					onExit()
				}
				return
			}
		}
	}()
}

func waitPidfd(pidfd int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	for time.Now().Before(deadline) {
		n, err := unix.Poll(fds, int(time.Until(deadline)/time.Millisecond)+1)
		if n > 0 {
			return true
		}
		if err != nil && !errors.Is(err, unix.EINTR) {
			return false
		}
	}
	return false
}

// Stop kills the browser: systemctl stop for a unit; for a process group
// SIGTERM, up to 5 s, SIGKILL, then a sweep of the group and of every
// browser process naming the profile (HOST-CONTROL §5.9).
func (s *Supervisor) Stop(b *state.Browser) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b != nil && b.Kind == Systemd && b.Unit != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		exec.CommandContext(ctx, "systemctl", "--user", "stop", b.Unit).Run()
		cancel()
	} else if b != nil && b.PGID > 0 {
		fd, err := unix.PidfdOpen(b.PID, 0)
		same := err == nil
		if same {
			if st, e := s.stat(b.PID); e != nil || st.start != b.StartTime {
				same = false
			}
		}
		if same {
			// The leader is alive and verified through its pidfd, so the
			// group id is still the browser's.
			syscall.Kill(-b.PGID, syscall.SIGTERM)
			if !waitPidfd(fd, 5*time.Second) {
				syscall.Kill(-b.PGID, syscall.SIGKILL)
			}
		}
		if fd >= 0 {
			unix.Close(fd)
		}
		if !same {
			// The leader is gone (e.g. recovery after a crash), so its pid
			// and group id may be reused: sweep by profile only. Every
			// Chromium process names the profile (measured, CONTRACT-NOTES-HOST).
			b = nil
		}
	}
	s.sweepLocked(b)
}

// Sweep kills any browser process of ours using the profile, e.g. a
// leftover browser before a launch.
func (s *Supervisor) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepProfileLocked()
}

func (s *Supervisor) sweepProfileLocked() { s.sweepLocked(nil) }

func (s *Supervisor) sweepLocked(b *state.Browser) {
	deadline := time.Now().Add(2 * time.Second)
	byProfile := s.profileOwned()
	if !byProfile && s.cfg.Profile != "" {
		s.cfg.Logf("supervise: the profile isn't a plain directory of ours; not sweeping by it")
	}
	for {
		victims := s.find(b, byProfile)
		if len(victims) == 0 {
			return
		}
		for _, v := range victims {
			s.kill(v)
		}
		if time.Now().After(deadline) {
			s.cfg.Logf("supervise: %d browser processes survived the sweep", len(victims))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// victim is a process found by a scan, with the identity it had then.
type victim struct {
	pid   int
	start uint64
	exe   string
}

// kill signals a victim through a pidfd, after checking that the pidfd's
// process is still the one the scan found (same start time and exe), so a
// pid reused since the scan is never signalled.
func (s *Supervisor) kill(v victim) {
	fd, err := unix.PidfdOpen(v.pid, 0)
	if err != nil {
		return
	}
	defer unix.Close(fd)
	st, err := s.stat(v.pid)
	if err != nil || st.start != v.start || s.exe(v.pid) != v.exe {
		return
	}
	unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
}

// exe is the resolved /proc/<pid>/exe, or "" when it can't be read.
func (s *Supervisor) exe(pid int) string {
	e, err := os.Readlink(filepath.Join(s.cfg.Proc, strconv.Itoa(pid), "exe"))
	if err != nil {
		return ""
	}
	return e
}

// find lists processes of our uid that are either in the verified
// browser's group (started at or after it), or have exactly
// --user-data-dir=<profile> as one of their arguments. A substring match
// isn't enough: a process merely mentioning the path is never a victim.
// The binary isn't compared with the configured command: Arch's
// /usr/bin/chromium is a launcher and every browser process runs
// /usr/lib/chromium/chromium. Each victim's start time and exe are checked
// again through a pidfd before it's signalled (kill).
func (s *Supervisor) find(b *state.Browser, byProfile bool) []victim {
	ents, err := os.ReadDir(s.cfg.Proc)
	if err != nil {
		return nil
	}
	self := os.Getpid()
	want := []byte("--user-data-dir=" + s.cfg.Profile)
	var out []victim
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		st, err := s.stat(pid)
		if err != nil {
			continue
		}
		exe := s.exe(pid)
		if exe == "" {
			continue // not ours to judge (gone, or a kernel thread)
		}
		if b != nil && b.PGID > 0 && st.pgrp == b.PGID && st.start >= b.StartTime {
			out = append(out, victim{pid, st.start, exe})
			continue
		}
		if !byProfile {
			continue
		}
		cl, err := os.ReadFile(filepath.Join(s.cfg.Proc, e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := bytes.Split(bytes.TrimRight(cl, "\x00"), []byte{0})
		for _, a := range args[min(1, len(args)):] {
			if bytes.Equal(a, want) {
				out = append(out, victim{pid, st.start, exe})
				break
			}
		}
	}
	return out
}

// Running reports whether the recorded browser still runs.
func (s *Supervisor) Running(b *state.Browser) bool {
	if b == nil {
		return false
	}
	st, err := s.stat(b.PID)
	return err == nil && st.start == b.StartTime
}
