//go:build torture

package torture

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var suiteBuild struct {
	sync.Once
	binary string
	err    error
}

// Every top-level torture test uses the same immutable binary. The default
// builds instrumentation explicitly; an override must embed an e2e-mode SPA.
func tortureBinary(t *testing.T, root string) string {
	t.Helper()
	if binary := os.Getenv("TORTURE_SERVER_BIN"); binary != "" {
		if !filepath.IsAbs(binary) {
			binary = filepath.Join(root, binary)
		}
		return binary
	}
	suiteBuild.Do(func() {
		suiteBuild.binary = filepath.Join(root, "build", fmt.Sprintf("torture-%d", os.Getpid()))
		for _, args := range [][]string{{"npm", "--prefix", "web", "run", "build", "--", "--mode", "e2e"}, {"go", "build", "-trimpath", "-ldflags=-s -w", "-o", suiteBuild.binary, "./cmd/passstream"}} {
			build := exec.Command("mise", append([]string{"exec", "--"}, args...)...)
			build.Dir = root
			build.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := build.CombinedOutput(); err != nil {
				suiteBuild.err = fmt.Errorf("instrumented torture build: %w\n%s", err, out)
				return
			}
		}
	})
	if suiteBuild.err != nil {
		t.Fatal(suiteBuild.err)
	}
	return suiteBuild.binary
}
