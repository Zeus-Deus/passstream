package hosttest

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// FakeBrowserArg marks a re-executed test binary as a fake browser.
const FakeBrowserArg = "__passstream_fake_browser__"

// BrowserCommand is the daemon's browser command for a fake browser in
// the given mode: "page" simulates the sender page; "idle" writes the --app
// URL to file and waits, so a test can act as the page itself.
func BrowserCommand(exe, mode, file string) []string {
	return []string{exe, FakeBrowserArg, mode, file}
}

// MaybeFakeBrowser runs the fake browser and exits when the process was
// started as one. Call it first in TestMain (or main).
func MaybeFakeBrowser() {
	if len(os.Args) < 4 || os.Args[1] != FakeBrowserArg {
		return
	}
	mode, file := os.Args[2], os.Args[3]
	app := ""
	for _, a := range os.Args[4:] {
		if v, ok := strings.CutPrefix(a, "--app="); ok {
			app = v
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// Page-mode options, comma-separated: "watch:<pid>" exits when that
	// process is gone (a fake browser in its own session never outlives its
	// harness); "api:<url>" is where the fake server's /test hooks are.
	testAPI := ""
	for _, opt := range strings.Split(file, ",") {
		if pid, ok := strings.CutPrefix(opt, "watch:"); ok && mode == "page" {
			if n, err := strconv.Atoi(pid); err == nil {
				go func() {
					for syscall.Kill(n, 0) == nil {
						time.Sleep(time.Second)
					}
					os.Exit(0)
				}()
			}
		}
		if u, ok := strings.CutPrefix(opt, "api:"); ok && mode == "page" {
			testAPI = u
		}
	}
	switch mode {
	case "idle":
		if file != "" && file != "-" {
			os.WriteFile(file+".tmp", []byte(app), 0o600)
			os.Rename(file+".tmp", file)
		}
		<-ctx.Done()
	case "page":
		p, err := NewPage(app)
		if err != nil {
			os.Exit(3)
		}
		p.TestAPI = testAPI
		p.Run(ctx)
		// A real browser window stays open after its page ends; only the
		// daemon (stop) or the user closes it.
		<-ctx.Done()
	}
	os.Exit(0)
}
