package daemon_test

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/hosttest"
	"github.com/passstream/passstream/internal/host/proto"
)

func TestLaunchFilePermissionsAndDedicatedProfile(t *testing.T) {
	e, _, origin, id, key := launched(t)
	u, err := url.Parse(e.appURL())
	if err != nil || u.Scheme != "file" {
		t.Fatal("launch key must use a file", err)
	}
	fi, err := os.Stat(u.Path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatal("launch file must be 0600", err)
	}
	di, err := os.Stat(filepath.Dir(u.Path))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatal("launch directory must be 0700", err)
	}
	profile, err := os.Lstat(e.cfg.Profile)
	if err != nil || !profile.IsDir() || profile.Mode().Perm() != 0o700 {
		t.Fatal("dedicated profile must be a real 0700 directory", err)
	}
	launch, _ := os.ReadFile(u.Path)
	if !strings.Contains(string(launch), proto.Encode(key)) {
		t.Fatal("key missing from launch file")
	}
	argv, err := os.ReadFile(filepath.Join("/proc", itoa(e.record().Browser.PID), "cmdline"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), proto.Encode(key)) || strings.Contains(string(argv), testKey) {
		t.Fatal("launch secret in argv")
	}
	if !strings.Contains(string(argv), "--user-data-dir="+e.cfg.Profile+"\x00") {
		t.Fatal("browser did not use the dedicated test profile")
	}
	s, err := hosttest.Authenticate(ctx5(), origin, id, key)
	if err != nil {
		t.Fatal(err)
	}
	hosttest.WriteMsg(ctx5(), s.WS, map[string]any{"type": "init_ack"})
	waitFor(t, "launch file retired", time.Second, func() bool { _, err := os.Stat(u.Path); return os.IsNotExist(err) })
	if strings.Contains(e.log.String(), proto.Encode(key)) {
		t.Fatal("launch key in logs")
	}
}
