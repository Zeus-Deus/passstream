package passglue

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type serviceLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *serviceLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *serviceLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// Opt in to a binary built read-only from the real sibling source checkout.
// Runtime data, credentials and listeners are all private temporary fixtures.
func TestLocalPassFileServer(t *testing.T) {
	binary := os.Getenv("PASSFILE_TEST_BINARY")
	if binary == "" {
		t.Skip("set PASSFILE_TEST_BINARY to a locally built PassFile binary")
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	address := ln.Addr().String()
	ln.Close()
	base := "http://" + address
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	var logs serviceLog
	cmd := exec.Command(binary, "serve", "--listen", address, "--base-url", base, "--data", data, "--min-free-bytes", "0", "--min-free-percent", "0")
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "XDG_RUNTIME_DIR="+filepath.Join(dir, "run"))
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Signal(syscall.SIGTERM); cmd.Wait() }()
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		r, e := http.Get(base + "/readyz")
		if e == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				ready = true
				break
			}
		}
	}
	if !ready {
		t.Fatal("local PassFile did not become ready", logs.String())
	}
	keyPath := filepath.Join(data, "initial-key")
	key, e := os.ReadFile(keyPath)
	if e != nil {
		t.Fatal(e)
	}
	var pngBytes bytes.Buffer
	png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 32, 16)))
	cfg := Config{PassFile: base, PassFileKey: keyPath}
	for _, kind := range []string{"snapshot", "recording"} {
		payload := pngBytes.Bytes()
		if kind == "recording" {
			payload = []byte{0x1a, 0x45, 0xdf, 0xa3, 0x81, 0x01}
		}
		link, e := cfg.Upload(context.Background(), "passfile", kind, payload)
		if e != nil {
			t.Fatal(e)
		}
		if !strings.HasPrefix(link, base+"/s/") {
			t.Fatal("unexpected local result origin")
		}
	}
	req, _ := http.NewRequest("GET", base+"/api/v1/files", nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	var result struct {
		Files []struct {
			Filename string `json:"filename"`
			Verified bool   `json:"client_checksum_verified"`
		} `json:"files"`
	}
	if e := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); e != nil {
		t.Fatal(e)
	}
	if len(result.Files) != 2 {
		t.Fatal("local server did not retain both uploads")
	}
	names := map[string]bool{}
	for _, f := range result.Files {
		if !f.Verified {
			t.Fatal("real service did not verify checksum")
		}
		names[f.Filename] = true
	}
	if !names["snapshot.png"] || !names["recording.webm"] {
		t.Fatal("wrong filenames")
	}
	if strings.Contains(logs.String(), strings.TrimSpace(string(key))) {
		t.Fatal("test key reached service logs")
	}
	t.Log("real local PassFile accepted snapshot and recording PUTs, verified SHA-256, returned same-origin links, and kept its key out of logs")
}

func TestLocalPassPageServer(t *testing.T) {
	node := os.Getenv("PASSPAGE_TEST_NODE")
	source := os.Getenv("PASSPAGE_LOCAL_SOURCE")
	loader := os.Getenv("PASSPAGE_TEST_LOADER")
	if node == "" || source == "" || loader == "" {
		t.Skip("set PASSPAGE_TEST_NODE, PASSPAGE_LOCAL_SOURCE and PASSPAGE_TEST_LOADER for isolated real PassPage")
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	address := ln.Addr().(*net.TCPAddr)
	port := address.Port
	ln.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	const testKey = "FAKE_LOCAL_PASSPAGE_KEY_847235_NEVER_LOG"
	os.WriteFile(keyPath, []byte(testKey), 0600)
	fixture, e := filepath.Abs("../../../test/e2e/local-passpage-server.mjs")
	if e != nil {
		t.Fatal(e)
	}
	var logs serviceLog
	cmd := exec.Command(node, "--import", loader, fixture)
	cmd.Env = append(os.Environ(), "NODE_ENV=test", "DATABASE_URL="+filepath.Join(dir, "page.sqlite"), "UPLOAD_ROOT="+filepath.Join(dir, "uploads"), "PORT="+strconv.Itoa(port), "HOST=127.0.0.1", "PASSPAGE_BASE_URL="+base, "PASSPAGE_DOMAIN=127.0.0.1", "SESSION_SECRET=local-test-session-secret-847235", "PASSPAGE_TEST_KEY_PATH="+keyPath, "GITHUB_CLIENT_ID=", "GITHUB_CLIENT_SECRET=", "RESEND_API_KEY=", "XDG_CONFIG_HOME="+filepath.Join(dir, "config"), "XDG_STATE_HOME="+filepath.Join(dir, "state"), "XDG_RUNTIME_DIR="+filepath.Join(dir, "run"))
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Signal(syscall.SIGTERM); cmd.Wait() }()
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		resp, e := http.Get(base + "/llms.txt")
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
	}
	if !ready {
		t.Fatal("isolated local PassPage did not become ready", logs.String())
	}
	var data bytes.Buffer
	png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 32, 16)))
	link, e := (Config{PassPage: base, PassPageKey: keyPath}).Upload(context.Background(), "passpage", "snapshot", data.Bytes())
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(link, base+"/v/") {
		t.Fatal("wrong local result origin")
	}
	resp, e := http.Get(link + "snapshot.png")
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(body, data.Bytes()) {
		t.Fatal("real PassPage did not serve the exact snapshot")
	}
	if strings.Contains(logs.String(), testKey) {
		t.Fatal("local PassPage logged its key")
	}
	t.Log("real isolated PassPage accepted multipart gzip tar, served the exact snapshot and returned a same-origin link without logging its test key")
}
