package procnet_test

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/procnet"
)

// TestChromiumPeerUID is the implementation gate HOST-CONTROL §11 Q1: does
// /proc/net/tcp show the user's uid for the sockets of Chromium's
// (sandboxed, out-of-process) network service? It launches the system
// Chromium headless against a loopback listener, then checks the reversed
// tuple of the page's request, its WebSocket upgrade, and a navigation from
// a file:// launch page (the launch-file fallback).
func TestChromiumPeerUID(t *testing.T) {
	exe, err := exec.LookPath("chromium")
	if err != nil || testing.Short() {
		t.Skip("needs chromium (and not -short)")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	type seen struct {
		path string
		uid  int
		err  error
	}
	results := make(chan seen, 16)
	mux := http.NewServeMux()
	lookup := func(r *http.Request) {
		client, _ := netip.ParseAddrPort(r.RemoteAddr)
		la := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
		uid, err := procnet.Root("").PeerUID(client, la.AddrPort())
		results <- seen{r.URL.Path + "?" + r.URL.RawQuery, uid, err}
	}
	mux.HandleFunc("/sender", func(w http.ResponseWriter, r *http.Request) {
		lookup(r)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><script src="/a.js?via=%s"></script>`, r.URL.Query().Get("via"))
	})
	mux.HandleFunc("/a.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		fmt.Fprintf(w, `globalThis.uidProbeSocket = new WebSocket("ws://127.0.0.1:%d/ws?via=%s")`, port, r.URL.Query().Get("via"))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		lookup(r)
		http.Error(w, "no", 400)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	dir := t.TempDir()
	launch := filepath.Join(dir, "launch.html")
	os.WriteFile(launch, []byte(fmt.Sprintf(`<!doctype html><meta http-equiv="refresh" content="0;url=http://127.0.0.1:%d/sender?via=file#i=x&k=y">`, port)), 0o600)
	var pgids []int
	run := func(name string, args ...string) {
		profile := filepath.Join(dir, name)
		cmd := exec.Command(exe, append([]string{"--headless=new", "--user-data-dir=" + profile, "--no-first-run",
			"--no-default-browser-check", "--disable-gpu"}, args...)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pgids = append(pgids, cmd.Process.Pid)
		t.Cleanup(func() {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			cmd.Wait()
		})
	}
	want := map[string]bool{"/sender?": true, "/ws?via=": true, "/sender?via=file": true, "/ws?via=file": true}
	run("direct", fmt.Sprintf("--app=http://127.0.0.1:%d/sender", port))
	run("file", "--app=file://"+launch)
	deadline := time.After(30 * time.Second)
	for len(want) > 0 {
		select {
		case s := <-results:
			t.Logf("Q1: %s → uid %d (err %v), our uid %d", s.path, s.uid, s.err, os.Geteuid())
			if s.err != nil || s.uid != os.Geteuid() {
				t.Errorf("Q1 fails for %s: uid %d err %v", s.path, s.uid, s.err)
			}
			delete(want, s.path)
		case <-deadline:
			t.Fatalf("chromium never requested %v", keys(want))
		}
	}
	// HOST-CONTROL §11 Q8: every process of each launch, found through the
	// parent chain, must be in the launch's process group (or name the
	// profile) for the self-bound kill sweep to find it.
	for _, pg := range pgids {
		procs := descendants(pg)
		outside := 0
		for _, p := range procs {
			if p.pgrp != pg && !strings.Contains(p.cmd, dir) {
				outside++
				t.Errorf("Q8: pid %d left the group: %.120s", p.pid, p.cmd)
			}
			if strings.Contains(p.cmd, "NetworkService") {
				t.Logf("Q1: network service pid %d: sandbox %s", p.pid, flagValue(p.cmd, "--service-sandbox-type="))
			}
		}
		t.Logf("Q8: launch %d has %d processes, %d outside its process group", pg, len(procs), outside)
	}
}

type proc struct {
	pid, ppid, pgrp int
	cmd             string
}

func descendants(root int) []proc {
	all := map[int]proc{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		var pid int
		if _, err := fmt.Sscan(e.Name(), &pid); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		i := strings.LastIndexByte(string(b), ')')
		f := strings.Fields(string(b[i+1:]))
		var p proc
		p.pid = pid
		fmt.Sscan(f[1], &p.ppid)
		fmt.Sscan(f[2], &p.pgrp)
		c, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		p.cmd = strings.ReplaceAll(string(c), "\x00", " ")
		all[pid] = p
	}
	var out []proc
	var walk func(int)
	walk = func(pid int) {
		for _, p := range all {
			if p.ppid == pid {
				out = append(out, p)
				walk(p.pid)
			}
		}
	}
	if p, ok := all[root]; ok {
		out = append(out, p)
	}
	walk(root)
	return out
}

func flagValue(cmd, prefix string) string {
	for _, f := range strings.Fields(cmd) {
		if strings.HasPrefix(f, prefix) {
			return strings.TrimPrefix(f, prefix)
		}
	}
	return "(unset)"
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
