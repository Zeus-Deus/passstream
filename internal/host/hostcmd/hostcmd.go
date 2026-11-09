// Package hostcmd is the `passstream host` subcommand: run the daemon
// (socket-activated or self-bound), --detach for clients without systemd,
// and install-units.
package hostcmd

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/passstream/passstream/deploy/systemd"
	"github.com/passstream/passstream/internal/host/daemon"
	"github.com/passstream/passstream/internal/host/listen"
	"github.com/passstream/passstream/internal/host/passglue"
	"github.com/passstream/passstream/internal/host/proto"
	"github.com/passstream/passstream/internal/host/serverapi"
	"github.com/passstream/passstream/web"
)

// Version is set by main.
var Version = "dev"

// FileConfig is ~/.config/passstream/config: `key = value` lines, '#'
// comments. Keys: server, port, browser, pin, knock, peer_check,
// passfile_server and passpage_server.
type FileConfig struct {
	PassFile  string
	PassPage  string
	Server    string
	Port      int
	Browser   []string
	Pin       *proto.PinPolicy
	Knock     bool
	PeerCheck string
}

// ParsePolicyName maps "none", "passphrase" or a digit count 4–12 to a
// policy. Anything else (a PIN value included) is refused.
func ParsePolicyName(s string) (proto.PinPolicy, bool) {
	switch s {
	case "none", "passphrase":
		return proto.PinPolicy{Kind: s}, true
	}
	if len(s) <= 2 {
		if n, err := strconv.Atoi(s); err == nil && n >= 4 && n <= 12 && strconv.Itoa(n) == s {
			return proto.PinPolicy{Kind: "digits", Length: n}, true
		}
	}
	return proto.PinPolicy{}, false
}

// LoadFileConfig reads the config file; a missing file is an empty config.
func LoadFileConfig(path string) (FileConfig, error) {
	var c FileConfig
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return c, fmt.Errorf("%s:%d: expected key = value", path, n)
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`)
		switch k {
		case "passfile_server", "passpage_server":
			base, err := passglue.Base(v)
			if err != nil {
				return c, fmt.Errorf("%s:%d: invalid Pass service URL", path, n)
			}
			if k == "passfile_server" {
				c.PassFile = base
			} else {
				c.PassPage = base
			}
		case "server":
			c.Server = v
		case "port":
			p, err := strconv.Atoi(v)
			if err != nil || p < 1024 || p > 65535 {
				return c, fmt.Errorf("%s:%d: bad port", path, n)
			}
			c.Port = p
		case "browser":
			c.Browser = strings.Fields(v)
		case "pin":
			p, ok := ParsePolicyName(v)
			if !ok {
				return c, fmt.Errorf("%s:%d: pin must be none, passphrase or 4-12", path, n)
			}
			c.Pin = &p
		case "knock":
			c.Knock = v == "true"
		case "peer_check":
			if v != daemon.PeerTuple && v != daemon.PeerLaunchFile {
				return c, fmt.Errorf("%s:%d: peer_check must be tuple or launch_file", path, n)
			}
			c.PeerCheck = v
		default:
			return c, fmt.Errorf("%s:%d: unknown key %q", path, n, k)
		}
	}
	return c, sc.Err()
}

// Main runs `passstream host ...` and returns the exit code.
func Main(args []string) int {
	if len(args) > 0 && args[0] == "install-units" {
		return installUnits(args[1:])
	}
	fl := flag.NewFlagSet("host", flag.ContinueOnError)
	server := fl.String("server", "", "server base URL (overrides the config file)")
	detach := fl.Bool("detach", false, "start in the background (no systemd) and return once the socket answers")
	idle := fl.Duration("idle-exit", 60*time.Second, "exit after this long without a stream or a client (negative: never)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	paths, err := daemon.DefaultPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "passstream host:", err)
		return 1
	}
	fc, err := LoadFileConfig(filepath.Join(paths.ConfigDir, "config"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "passstream host:", err)
		return 2
	}
	if *server != "" {
		fc.Server = *server
	}
	base := ""
	if fc.Server != "" {
		if base, err = serverapi.NormalizeBase(fc.Server); err != nil {
			fmt.Fprintln(os.Stderr, "passstream host: server:", proto.AsError(err).Message)
			return 2
		}
	}
	if *detach {
		return detachSelf(paths, args)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	lock, err := listen.Lock(paths.RuntimeDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "passstream host:", err)
		return 1
	}
	defer lock.Close()
	ln, err := listen.FromSystemd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "passstream host: socket activation:", err)
		return 1
	}
	port := fc.Port
	if port == 0 {
		port = daemon.DefaultPort
	}
	if ln == nil {
		if ln, err = listen.SelfBind(paths.RuntimeDir, port); err != nil {
			fmt.Fprintln(os.Stderr, "passstream host:", err)
			return 1
		}
	} else if a, ok := ln.HTTP.Addr().(*net.TCPAddr); ok {
		port = a.Port
	}
	assets, _ := fs.Sub(web.Dist, "dist")
	cfg := daemon.Config{
		Pass:    passglue.Config{PassFile: fc.PassFile, PassPage: fc.PassPage, PassFileKey: filepath.Join(filepath.Dir(paths.ConfigDir), "passfile", "key"), PassPageKey: filepath.Join(filepath.Dir(paths.ConfigDir), "passpage", "key")},
		Version: Version, ServerURL: base, KeyPath: filepath.Join(paths.ConfigDir, "key"),
		RuntimeDir: paths.RuntimeDir, StateDir: paths.StateDir, Port: port,
		Browser: fc.Browser, Profile: ProfileDir(paths),
		LocalPolicy: fc.Pin, LocalKnock: fc.Knock, PeerCheck: fc.PeerCheck, IdleExit: *idle,
		Assets: assets, Logf: log.Printf,
	}
	d, err := daemon.New(cfg, ln)
	if err != nil {
		ln.Close()
		fmt.Fprintln(os.Stderr, "passstream host:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := d.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "passstream host:", err)
		return 1
	}
	return 0
}

// ProfileDir is the sender browser's dedicated profile:
// $XDG_STATE_HOME/passstream/chromium (HOST-CONTROL §5.3).
func ProfileDir(p daemon.Paths) string { return filepath.Join(p.StateDir, "chromium") }

// detachSelf starts the daemon in its own session (setsid, stdio on
// /dev/null, log in the state dir) and waits up to 5 s for the socket.
func detachSelf(paths daemon.Paths, args []string) int {
	sock := filepath.Join(paths.RuntimeDir, "host.sock")
	if canDial(sock) {
		return 0
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "passstream host:", err)
		return 1
	}
	var rest []string
	for _, a := range args {
		if a != "--detach" && a != "-detach" {
			rest = append(rest, a)
		}
	}
	if err := listen.PrivateDir(paths.StateDir); err != nil {
		fmt.Fprintln(os.Stderr, "passstream host:", err)
		return 1
	}
	logf, err := os.OpenFile(filepath.Join(paths.StateDir, "host.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "passstream host:", err)
		return 1
	}
	null, _ := os.Open(os.DevNull)
	cmd := exec.Command(exe, append([]string{"host"}, rest...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "passstream host:", err)
		return 1
	}
	cmd.Process.Release()
	logf.Close()
	null.Close()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if canDial(sock) {
			return 0
		}
	}
	fmt.Fprintln(os.Stderr, "passstream host: the daemon didn't come up; see", filepath.Join(paths.StateDir, "host.log"))
	return 1
}

func canDial(sock string) bool {
	c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// installUnits writes the two user units and, unless --no-enable, reloads
// the user manager and enables the socket.
func installUnits(args []string) int {
	fl := flag.NewFlagSet("install-units", flag.ContinueOnError)
	noEnable := fl.Bool("no-enable", false, "only write the unit files")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "passstream host install-units:", err)
		return 1
	}
	home, _ := os.UserHomeDir()
	cfgHome := os.Getenv("XDG_CONFIG_HOME")
	if cfgHome == "" || !filepath.IsAbs(cfgHome) {
		cfgHome = filepath.Join(home, ".config")
	}
	port := daemon.DefaultPort
	if fc, err := LoadFileConfig(filepath.Join(cfgHome, "passstream", "config")); err == nil && fc.Port != 0 {
		port = fc.Port
	}
	dir := filepath.Join(cfgHome, "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "passstream host install-units:", err)
		return 1
	}
	socket := strings.ReplaceAll(systemd.Socket, "127.0.0.1:47823", fmt.Sprintf("127.0.0.1:%d", port))
	service := strings.ReplaceAll(systemd.Service, "ExecStart=/usr/bin/passstream host", "ExecStart="+systemdQuote(exe)+" host")
	for name, body := range map[string]string{"passstream-host.socket": socket, "passstream-host.service": service} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "passstream host install-units:", err)
			return 1
		}
	}
	fmt.Println("wrote", filepath.Join(dir, "passstream-host.socket"), "and passstream-host.service")
	if *noEnable {
		fmt.Println("enable with: systemctl --user daemon-reload && systemctl --user enable --now passstream-host.socket")
		return 0
	}
	for _, a := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", "passstream-host.socket"}} {
		cmd := exec.Command("systemctl", a...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "passstream host install-units: systemctl", strings.Join(a, " "), "failed (is a self-bound daemon holding the port? run `passstream shutdown` first)")
			return 1
		}
	}
	fmt.Println("enabled passstream-host.socket; `loginctl enable-linger` reserves the port from boot (recommended)")
	return 0
}

func systemdQuote(p string) string {
	if strings.ContainsAny(p, " \t\"'\\") {
		return strconv.Quote(p)
	}
	return p
}
