package hostcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/passstream/passstream/internal/host/daemon"
)

func TestConfigFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config")
	os.WriteFile(p, []byte("# comment\nserver = https://stream.example\nport = 47999\npin = 6\nknock = true\nbrowser = chromium --ozone-platform=wayland\npeer_check = launch_file\n"), 0o600)
	c, err := LoadFileConfig(p)
	if err != nil || c.Server != "https://stream.example" || c.Port != 47999 || c.Pin.Length != 6 || !c.Knock || len(c.Browser) != 2 || c.PeerCheck != "launch_file" {
		t.Fatalf("%+v %v", c, err)
	}
	// launch_key_in_argv is gone: the launch file is the only delivery.
	for _, bad := range []string{"pin = 4821\n", "port = 80\n", "nonsense\n", "colour = red\n", "launch_key_in_argv = true\n"} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadFileConfig(p); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if c, err := LoadFileConfig(filepath.Join(t.TempDir(), "none")); err != nil || c.Server != "" {
		t.Fatal("a missing config is empty")
	}
}

func TestProfileUnderXDGState(t *testing.T) {
	home, state := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_STATE_HOME", state)
	p, err := daemon.DefaultPaths()
	if err != nil || ProfileDir(p) != filepath.Join(state, "passstream", "chromium") {
		t.Fatalf("%s %v", ProfileDir(p), err)
	}
	t.Setenv("XDG_STATE_HOME", "")
	p, _ = daemon.DefaultPaths()
	if ProfileDir(p) != filepath.Join(home, ".local", "state", "passstream", "chromium") {
		t.Fatalf("default %s", ProfileDir(p))
	}
}

func TestInstallUnitsWritesOnlyUnderXDGConfig(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if code := Main([]string{"install-units", "--no-enable"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	sock, err := os.ReadFile(filepath.Join(cfg, "systemd", "user", "passstream-host.socket"))
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := os.ReadFile(filepath.Join(cfg, "systemd", "user", "passstream-host.service"))
	for _, want := range []string{"ListenStream=127.0.0.1:47823", "ListenStream=%t/passstream/host.sock", "SocketMode=0600", "DirectoryMode=0700"} {
		if !strings.Contains(string(sock), want) {
			t.Errorf("socket unit lacks %s", want)
		}
	}
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	if !strings.Contains(string(svc), "ExecStart="+exe+" host") || !strings.Contains(string(svc), "KillMode=process") {
		t.Errorf("service unit:\n%s", svc)
	}
}
