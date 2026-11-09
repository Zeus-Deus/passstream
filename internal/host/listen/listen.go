// Package listen gets the daemon's two listeners: from systemd socket
// activation (sd_listen_fds, in pure Go) or by binding them itself
// (HOST-CONTROL §2.2 and §5.9), plus the single-instance lock.
package listen

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Port modes reported in HostStatus.daemon.port_mode.
const (
	SocketActivated = "socket_activated"
	SelfBound       = "self_bound"
)

// Listeners are the Unix control socket and the loopback HTTP listener.
type Listeners struct {
	Unix *net.UnixListener
	HTTP *net.TCPListener
	Mode string
}

// Close closes both.
func (l *Listeners) Close() {
	if l.Unix != nil {
		l.Unix.Close()
	}
	if l.HTTP != nil {
		l.HTTP.Close()
	}
}

const listenFdsStart = 3

// FromSystemd takes the listeners passed by systemd (LISTEN_PID, LISTEN_FDS).
// It returns (nil, nil) when the process wasn't socket-activated, and it
// unsets the variables so children don't inherit them.
func FromSystemd() (*Listeners, error) {
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil, nil
	}
	n, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	os.Unsetenv("LISTEN_PID")
	os.Unsetenv("LISTEN_FDS")
	os.Unsetenv("LISTEN_FDNAMES")
	if err != nil || n < 1 || n > 16 {
		return nil, fmt.Errorf("bad LISTEN_FDS")
	}
	l := &Listeners{Mode: SocketActivated}
	for fd := listenFdsStart; fd < listenFdsStart+n; fd++ {
		syscall.CloseOnExec(fd)
		f := os.NewFile(uintptr(fd), "LISTEN_FD_"+strconv.Itoa(fd))
		ln, err := net.FileListener(f)
		f.Close()
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("fd %d: %w", fd, err)
		}
		switch t := ln.(type) {
		case *net.UnixListener:
			t.SetUnlinkOnClose(false) // systemd owns the path
			l.Unix = t
		case *net.TCPListener:
			l.HTTP = t
		default:
			ln.Close()
		}
	}
	if l.Unix == nil || l.HTTP == nil {
		l.Close()
		return nil, errors.New("socket activation must pass the Unix socket and the TCP listener")
	}
	return l, nil
}

// PrivateDir creates dir 0700 (or verifies it): owned by us, not a symlink.
func PrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is not our directory", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// Lock takes the single-instance flock on dir/host.lock. The returned file
// must stay open for the daemon's lifetime.
func Lock(dir string) (*os.File, error) {
	if err := PrivateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "host.lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another passstream host is running")
	}
	return f, nil
}

// SelfBind binds both listeners itself: the Unix socket 0600 in the 0700
// runtime dir (replacing a stale socket, which the held lock proves stale),
// and 127.0.0.1:port.
func SelfBind(dir string, port int) (*Listeners, error) {
	if err := PrivateDir(dir); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "host.sock")
	if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(sock)
	}
	old := syscall.Umask(0o177)
	u, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		u.Close()
		return nil, err
	}
	t, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		u.Close()
		return nil, fmt.Errorf("bind 127.0.0.1:%d: %w", port, err)
	}
	return &Listeners{Unix: u, HTTP: t, Mode: SelfBound}, nil
}
