//go:build torture

package torture

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// This is a route smoke, not a congestion/capacity or physical-device gate.
// The caller holds /tmp/passstream-media.lock, as for every torture run.
func TestPublicTransport(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PASSSTREAM_PUBLIC_NETNS") != "1" {
		binary := tortureBinary(t, root)
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		c := exec.Command("unshare", "-rn", self, "-test.run=^TestPublicTransport$", "-test.v", "-test.timeout=6m")
		c.Env = append(os.Environ(), "PASSSTREAM_PUBLIC_NETNS=1", "TORTURE_SERVER_BIN="+binary)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Run(); err != nil {
			t.Fatalf("required isolated public route smoke: %v", err)
		}
		return
	}
	for _, args := range [][]string{{"link", "set", "lo", "up"}, {"link", "add", "public0", "type", "dummy"}, {"addr", "add", "198.18.0.1/24", "dev", "public0"}, {"link", "set", "public0", "up"}} {
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("private netns setup: %v %s", err, out)
		}
	}
	c := exec.Command("mise", "exec", "--", "node", "test/torture/public-transport.mjs")
	c.Dir = root
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
}

// This is a browser failure probe, not a media/quality gate. The caller holds
// the same media lock, even though this does not start a sender or viewer.
func TestPublicTransportFailureRedaction(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("mise", "exec", "--", "node", "--test", "test/torture/public-transport-redaction.test.mjs")
	c.Dir = root
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
}
