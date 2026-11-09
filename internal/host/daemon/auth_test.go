package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/passstream/passstream/internal/host/hosttest"
	"github.com/passstream/passstream/internal/host/localauth"
	"github.com/passstream/passstream/internal/host/procnet"
	"github.com/passstream/passstream/internal/host/proto"
)

// launched starts a share with an idle fake browser and returns the
// launch key the browser was given.
func launched(t *testing.T, opts ...opt) (*env, *hosttest.Client, string, []byte, []byte) {
	t.Helper()
	e := newEnv(t, append([]opt{withBrowser("idle")}, opts...)...)
	c := e.client("plugin")
	call(t, c, "subscribe", map[string]any{})
	call(t, c, "start", map[string]any{})
	origin, id, key, err := hosttest.ParseLaunchURL(e.appURL())
	if err != nil {
		t.Fatal(err)
	}
	if origin != e.origin() {
		t.Fatalf("origin %s", origin)
	}
	return e, c, origin, id, key
}

// ctx5 is a 5-second deadline for one protocol step (a timer, not a
// cancel func, so the vet check for leaked cancels stays quiet).
func ctx5() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	time.AfterFunc(6*time.Second, cancel)
	return ctx
}

func attachmentOf(t *testing.T, s *hosttest.Session) ([]byte, []byte) {
	t.Helper()
	a, ok := s.Init["attachment"].(map[string]any)
	if !ok {
		t.Fatalf("init without attachment: %v", s.Init)
	}
	id, _ := proto.Decode(a["key_id"].(string), 16)
	key, _ := proto.Decode(a["key"].(string), 32)
	return id, key
}

func TestLaunchHandoverLostInitIsRetryable(t *testing.T) {
	e, c, origin, lid, lkey := launched(t)
	s1, err := hosttest.Authenticate(ctx5(), origin, lid, lkey)
	if err != nil {
		t.Fatal(err)
	}
	aid1, akey1 := attachmentOf(t, s1)
	if hosttest.Field(s1.Init, "stream", "publish_token") == nil || s1.Init["signal_url"] != "ws"+e.srv.URL[4:]+"/signal" {
		t.Fatalf("init %v", s1.Init)
	}
	if rec := e.record(); rec.Attachment == nil || rec.Attachment.KeyID != proto.Encode(aid1) {
		t.Fatal("the attachment key isn't durable before init")
	}
	// The init is "lost": the socket drops before init_ack.
	s1.WS.CloseNow()
	waitFor(t, "launching again", 3*time.Second, func() bool { return status(t, c).Stream.Attachment == "launching" })
	s2, err := hosttest.Authenticate(ctx5(), origin, lid, lkey)
	if err != nil {
		t.Fatalf("the launch key must still work before init_ack: %v", err)
	}
	aid2, akey2 := attachmentOf(t, s2)
	if string(aid1) != string(aid2) || string(akey1) != string(akey2) {
		t.Fatal("a second init minted a different attachment key")
	}
	hosttest.WriteMsg(ctx5(), s2.WS, map[string]any{"type": "init_ack"})
	waitFor(t, "attached", 3*time.Second, func() bool { return status(t, c).Stream.Attachment == "attached" })
	// init_ack retired the launch key.
	_, err = hosttest.Authenticate(ctx5(), origin, lid, lkey)
	var de *hosttest.ErrDaemon
	if !errors.As(err, &de) || de.Code != "launch_consumed" || de.Authenticated {
		t.Fatalf("retired launch key: %v", err)
	}
	// The attachment key supersedes the attached socket.
	s3, err := hosttest.Authenticate(ctx5(), origin, aid1, akey1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s3.Init["attachment"]; ok {
		t.Fatal("an attachment-key init must not carry the key again")
	}
	// The old socket may get a view first; then superseded, then the close.
	if m := readUntil(t, s2.WS, "error"); m["code"] != "superseded" {
		t.Fatalf("old socket got %v", m)
	}
}

func TestAttachmentKeyRetiresLaunchKeyWithoutAck(t *testing.T) {
	_, c, origin, lid, lkey := launched(t)
	s1, err := hosttest.Authenticate(ctx5(), origin, lid, lkey)
	if err != nil {
		t.Fatal(err)
	}
	aid, akey := attachmentOf(t, s1)
	s1.WS.CloseNow() // stored the key, then lost the socket before acking
	if _, err := hosttest.Authenticate(ctx5(), origin, aid, akey); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "attached", 3*time.Second, func() bool { return status(t, c).Stream.Attachment == "attached" })
	_, err = hosttest.Authenticate(ctx5(), origin, lid, lkey)
	var de *hosttest.ErrDaemon
	if !errors.As(err, &de) || de.Code != "launch_consumed" {
		t.Fatalf("launch key after the attachment key: %v", err)
	}
}

// rawHello opens a socket and sends hello with the given nonce.
func rawHello(t *testing.T, origin string, kid, nonceP []byte) (*websocket.Conn, map[string]any) {
	t.Helper()
	ws, err := hosttest.Dial(ctx5(), origin, nil)
	if err != nil {
		t.Fatal(err)
	}
	hosttest.WriteMsg(ctx5(), ws, map[string]any{"type": "hello", "protocol": 1, "key_id": proto.Encode(kid), "nonce": proto.Encode(nonceP)})
	m, err := hosttest.ReadMsg(ctx5(), ws)
	if err != nil {
		t.Fatal(err)
	}
	return ws, m
}

func TestReplayedAndWrongProofsChangeNothing(t *testing.T) {
	e, c, origin, lid, lkey := launched(t)
	s, err := hosttest.Authenticate(ctx5(), origin, lid, lkey)
	if err != nil {
		t.Fatal(err)
	}
	aid, akey := attachmentOf(t, s)
	hosttest.WriteMsg(ctx5(), s.WS, map[string]any{"type": "init_ack"})
	waitFor(t, "attached", 3*time.Second, func() bool { return status(t, c).Stream.Attachment == "attached" })

	// An eavesdropped exchange: the attacker saw nonce_p, nonce_d and
	// proof_p of an earlier connection (here: one it ran itself with the key).
	np := proto.RandBytes(32)
	ws1, ch1 := rawHello(t, origin, aid, np)
	nd1, _ := proto.Decode(ch1["nonce"].(string), 32)
	oldProof := localauth.Proof(akey, localauth.RolePage, aid, np, nd1)
	ws1.CloseNow()

	cases := map[string]func(nd []byte) []byte{
		"replayed proof, same nonce_p": func([]byte) []byte { return oldProof },
		"daemon's own proof reflected": func(nd []byte) []byte { return localauth.Proof(akey, localauth.RoleDaemon, aid, np, nd) },
		"wrong key":                    func(nd []byte) []byte { return localauth.Proof(proto.RandBytes(32), localauth.RolePage, aid, np, nd) },
	}
	for name, mk := range cases {
		ws, ch := rawHello(t, origin, aid, np)
		if ch["type"] != "challenge" {
			t.Fatalf("%s: %v", name, ch)
		}
		nd, _ := proto.Decode(ch["nonce"].(string), 32)
		if string(nd) == string(nd1) {
			t.Fatal("nonce_d reused")
		}
		hosttest.WriteMsg(ctx5(), ws, map[string]any{"type": "auth", "proof": proto.Encode(mk(nd))})
		m, err := hosttest.ReadMsg(ctx5(), ws)
		if err != nil || m["type"] != "error" || m["code"] != "auth_failed" {
			t.Fatalf("%s: got %v %v", name, m, err)
		}
		ws.CloseNow()
	}
	// The attached page was never superseded and still runs commands.
	go func() {
		for {
			m, err := hosttest.ReadMsg(ctx5(), s.WS)
			if err != nil {
				return
			}
			if m["type"] == "command" {
				hosttest.WriteMsg(ctx5(), s.WS, map[string]any{"type": "result", "id": m["id"], "ok": true, "data": map[string]any{}})
				return
			}
		}
	}()
	if _, err := c.Call("mode", map[string]any{"mode": "motion"}); err != nil {
		t.Fatalf("the original attachment was disturbed: %v", err)
	}
	if hs := status(t, c); hs.Stream.Attachment != "attached" {
		t.Fatal("not attached")
	}
	_ = e
}

func TestUnknownKeyAndLateAuth(t *testing.T) {
	e, _, origin, _, lkey := launched(t, func(e *env) { e.cfg.AuthTimeout = 300 * time.Millisecond })
	ws, m := rawHello(t, origin, proto.RandBytes(16), proto.RandBytes(32))
	if m["type"] != "error" || m["code"] != "auth_failed" {
		t.Fatalf("unknown key id: %v", m)
	}
	ws.CloseNow()
	_, id, _, _ := hosttest.ParseLaunchURL(e.appURL())
	np := proto.RandBytes(32)
	ws, ch := rawHello(t, origin, id, np)
	nd, _ := proto.Decode(ch["nonce"].(string), 32)
	time.Sleep(500 * time.Millisecond) // past the auth window
	hosttest.WriteMsg(ctx5(), ws, map[string]any{"type": "auth", "proof": proto.Encode(localauth.Proof(lkey, localauth.RolePage, id, np, nd))})
	if m, err := hosttest.ReadMsg(ctx5(), ws); err == nil && m["type"] == "init" {
		t.Fatal("a late proof was accepted")
	}
}

func TestLaunchKeyExpiresAndEndsTheLaunch(t *testing.T) {
	e, c, origin, lid, lkey := launched(t, func(e *env) { e.cfg.LaunchTTL = 400 * time.Millisecond })
	ev, ok := c.WaitEvent(func(ev proto.Event) bool { return ev.Event == "ended" }, 5*time.Second)
	if !ok || hosttest.Field(ev.Data, "reason") != "browser_failed" {
		t.Fatalf("ended %v", ev)
	}
	_, err := hosttest.Authenticate(ctx5(), origin, lid, lkey)
	if err == nil {
		t.Fatal("expired launch key authenticated")
	}
	if !e.srv.Deleted(hosttest.Field(ev.Data, "stream_id").(string)) {
		t.Fatal("not deleted on the server")
	}
	if files, _ := filepath.Glob(filepath.Join(e.runtime, "launch-*.html")); len(files) != 0 {
		t.Fatalf("launch file left: %v", files)
	}
}

func TestConcurrentLaunchExchangesShareOneAttachmentKey(t *testing.T) {
	_, _, origin, lid, lkey := launched(t)
	var wg sync.WaitGroup
	keys := make([]string, 6)
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := hosttest.Authenticate(ctx5(), origin, lid, lkey)
			if err != nil {
				keys[i] = "err:" + err.Error()
				return
			}
			a := s.Init["attachment"].(map[string]any)
			keys[i] = a["key_id"].(string)
		}(i)
	}
	wg.Wait()
	for _, k := range keys {
		if k != keys[0] || len(k) != 22 {
			t.Fatalf("attachment keys differ: %v", keys)
		}
	}
}

func TestOriginHostAndPeerChecks(t *testing.T) {
	e, _, origin, lid, lkey := launched(t)
	url := fmt.Sprintf("ws://127.0.0.1:%d/local/v1/sender", e.port)
	try := func(h http.Header) int {
		_, resp, err := websocket.Dial(ctx5(), url, &websocket.DialOptions{HTTPHeader: h})
		if err == nil {
			return 101
		}
		if resp == nil {
			return 0
		}
		return resp.StatusCode
	}
	for name, h := range map[string]http.Header{
		"no origin":   {},
		"null origin": {"Origin": {"null"}},
		"other port":  {"Origin": {"http://127.0.0.1:1"}},
		"localhost":   {"Origin": {fmt.Sprintf("http://localhost:%d", e.port)}},
		"https":       {"Origin": {"https://127.0.0.1:" + fmt.Sprint(e.port)}},
		"evil site":   {"Origin": {"https://evil.example"}},
	} {
		if got := try(h); got != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, got)
		}
	}
	// A rebound Host is refused on the upgrade and on the page.
	up, _ := http.NewRequest("GET", origin+"/local/v1/sender", nil)
	up.Host = fmt.Sprintf("evil.example:%d", e.port)
	up.Header = http.Header{"Origin": {origin}, "Connection": {"Upgrade"}, "Upgrade": {"websocket"},
		"Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}}
	if resp, err := http.DefaultClient.Do(up); err != nil || resp.StatusCode != 403 {
		t.Fatalf("rebinding host on the upgrade: %v %v", resp, err)
	}
	req, _ := http.NewRequest("GET", origin+"/sender", nil)
	req.Host = "attacker.example"
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("rebinding host on /sender: %v", resp)
	}
	if resp, _ := http.Get(origin + "/api/v1/streams"); resp.StatusCode != 404 {
		t.Fatal("there's no API on TCP")
	}
	if _, err := hosttest.Authenticate(ctx5(), origin, lid, lkey); err != nil {
		t.Fatalf("the real origin: %v", err)
	}
}

// TestPeerUIDFailsClosed: with no matching /proc/net/tcp entry (another
// uid, or an unreadable table) the upgrade is refused.
func TestPeerUIDFailsClosed(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "net"), 0o700)
	hdr := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(hdr), 0o600)
	os.WriteFile(filepath.Join(root, "net", "tcp6"), []byte(hdr), 0o600)
	e, _, origin, lid, lkey := launched(t, func(e *env) { e.cfg.PeerProc = procnet.Root(root) })
	_ = e
	if _, err := hosttest.Authenticate(ctx5(), origin, lid, lkey); err == nil {
		t.Fatal("authenticated without a peer-uid match")
	}
	resp, err := http.Get(origin + "/sender")
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("/sender without a peer-uid match: %v", resp)
	}
}

// TestImpersonatingListener runs the page side against a listener that
// doesn't know the key: the page never sends auth (so it never reveals a
// proof), and its next hello uses a fresh nonce.
func TestImpersonatingListener(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []map[string]any
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			m, err := hosttest.ReadMsg(r.Context(), ws)
			if err != nil {
				return
			}
			mu.Lock()
			got = append(got, m)
			mu.Unlock()
			if m["type"] == "hello" {
				hosttest.WriteMsg(r.Context(), ws, map[string]any{"type": "challenge",
					"nonce": proto.Encode(proto.RandBytes(32)), "proof": proto.Encode(proto.RandBytes(32))})
			}
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()
	origin := "http://" + ln.Addr().String()
	kid, key := proto.RandBytes(16), proto.RandBytes(32)
	for i := 0; i < 2; i++ {
		if _, err := hosttest.Authenticate(ctx5(), origin, kid, key); err == nil {
			t.Fatal("authenticated against an impersonator")
		}
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	nonces := map[any]bool{}
	for _, m := range got {
		if m["type"] != "hello" {
			t.Fatalf("the page sent %v to an unverified peer", m["type"])
		}
		nonces[m["nonce"]] = true
	}
	if len(got) != 2 || len(nonces) != 2 {
		t.Fatalf("hellos %v", got)
	}
}
