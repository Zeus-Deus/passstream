//go:build torture

package torture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Re-exec the Go test driver in a private user/network namespace. No sudo,
// host network changes, containers, or external listeners are involved.
func TestTorture(t *testing.T) {
	root, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	if os.Getenv("PASSSTREAM_NETNS") != "1" {
		probe := exec.Command("unshare", "-rn", "sh", "-ec", "ip link set lo up && tc qdisc add dev lo root netem delay 1ms && tc qdisc del dev lo root")
		if out, e := probe.CombinedOutput(); e != nil {
			t.Fatalf("required unprivileged netns/netem unavailable: %v %s", e, out)
		}
		binary := tortureBinary(t, root)
		self, e := os.Executable()
		if e != nil {
			t.Fatal(e)
		}
		c := exec.Command("unshare", "-rn", self, "-test.run=^TestTorture$", "-test.v", "-test.timeout=45m")
		c.Env = append(os.Environ(), "PASSSTREAM_NETNS=1", "TORTURE_SERVER_BIN="+binary)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if e = c.Run(); e != nil {
			t.Fatal(e)
		}
		return
	}
	for _, args := range [][]string{{"link", "set", "lo", "up", "mtu", "1500"}} {
		if out, e := exec.Command("ip", args...).CombinedOutput(); e != nil {
			t.Fatalf("ip: %v %s", e, out)
		}
	}
	c := exec.Command("mise", "exec", "--", "node", "web/torture.mjs")
	c.Dir = root
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	e = c.Run()
	report := exec.Command("python3", "test/torture/report.py")
	report.Dir = root
	if out, err := report.CombinedOutput(); err != nil {
		t.Fatalf("report: %v %s", err, out)
	} else {
		t.Log(strings.TrimSpace(string(out)))
	}
	if e != nil {
		t.Fatalf("torture gates failed (see generated docs/REPORT.md and JSON): %v", e)
	}
}
