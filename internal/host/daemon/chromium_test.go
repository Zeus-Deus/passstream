package daemon_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/daemon"
	"github.com/passstream/passstream/internal/host/proto"
)

// TestChromiumSmoke launches the real sender page (web/dist) in headless
// Chromium through the daemon, completes the HMAC attach and stops cleanly:
// every Chromium process is gone within 7 s of stop (HOST-CONTROL §5.9).
func TestChromiumSmoke(t *testing.T) {
	exe, err := exec.LookPath("chromium")
	if err != nil || testing.Short() {
		t.Skip("needs chromium (and not -short)")
	}
	dist, _ := filepath.Abs("../../../web/dist")
	if _, err := os.Stat(filepath.Join(dist, "index.html")); err != nil {
		t.Skip("needs a built web/dist (npm --prefix web run build)")
	}
	for _, mode := range []string{daemon.PeerTuple, daemon.PeerLaunchFile} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t, func(e *env) {
				e.cfg.Assets = os.DirFS(dist)
				e.cfg.PeerCheck = mode
				e.cfg.Browser = []string{exe, "--headless=new", "--disable-gpu"}
			})
			c := e.client("cli")
			call(t, c, "subscribe", map[string]any{})
			began := time.Now()
			id := call(t, c, "start", map[string]any{})["stream_id"].(string)
			waitFor(t, "the sender page to attach", 30*time.Second, func() bool {
				hs := status(t, c)
				return hs.Stream != nil && hs.Stream.Attachment == "attached"
			})
			t.Logf("%s: attached %v after start", mode, time.Since(began).Round(time.Millisecond))
			rec := e.record()
			if rec.Attachment == nil || rec.Attachment.AttachedAt == 0 {
				t.Fatalf("record %+v", rec.Attachment)
			}
			// The launch file is deleted at the handover, and the
			// browser's argv names only the file.
			files, _ := filepath.Glob(filepath.Join(e.runtime, "launch-*.html"))
			if len(files) != 0 {
				t.Fatalf("launch file left behind: %v", files)
			}
			for _, l := range cmdlinesContaining(e.cfg.Profile, rec.Browser.PGID) {
				if strings.Contains(l, "#i=") || strings.Contains(l, "&k=") {
					t.Fatalf("launch key in argv: %.200s", l)
				}
			}
			for _, s := range []string{testKey, rec.PublishToken, rec.Attachment.Key} {
				if got := cmdlinesContaining(s, rec.Browser.PGID); len(got) > 0 {
					t.Fatalf("a secret is in an argv")
				}
			}
			pg := rec.Browser.PGID
			procs := browserProcs(e.cfg.Profile, pg)
			t.Logf("%s: %d browser processes", mode, len(procs))
			stopAt := time.Now()
			call(t, c, "stop", map[string]any{})
			if ev, ok := c.WaitEvent(func(ev proto.Event) bool { return ev.Event == "ended" }, 15*time.Second); !ok || ev.Data.(map[string]any)["stream_id"] != id {
				t.Fatal("no ended event")
			}
			waitFor(t, "every Chromium process to exit", 7*time.Second-time.Since(stopAt), func() bool {
				return len(browserProcs(e.cfg.Profile, pg)) == 0
			})
			t.Logf("%s: all browser processes gone %v after stop", mode, time.Since(stopAt).Round(time.Millisecond))
		})
	}
}

// browserProcs lists live (non-zombie) processes in the browser's group or
// naming its profile.
func browserProcs(profile string, pgid int) []int {
	var out []int
	ents, _ := os.ReadDir("/proc")
	for _, en := range ents {
		pid, err := strconv.Atoi(en.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", en.Name(), "stat"))
		if err != nil {
			continue
		}
		i := strings.LastIndexByte(string(b), ')')
		f := strings.Fields(string(b[i+1:]))
		if f[0] == "Z" {
			continue
		}
		pg, _ := strconv.Atoi(f[2])
		cl, _ := os.ReadFile(filepath.Join("/proc", en.Name(), "cmdline"))
		if pg == pgid || strings.Contains(string(cl), profile) {
			out = append(out, pid)
		}
	}
	return out
}
