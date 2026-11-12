package hosttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/passstream/passstream/internal/host/localauth"
	"github.com/passstream/passstream/internal/host/proto"
)

// ParseLaunchURL reads the launch key from a sender URL fragment
// (http://127.0.0.1:P/sender#i=<id>&k=<key>), following a launch file.
func ParseLaunchURL(app string) (origin string, keyID, key []byte, err error) {
	if strings.HasPrefix(app, "file://") {
		u, e := url.Parse(app)
		if e != nil {
			return "", nil, nil, e
		}
		b, e := os.ReadFile(u.Path)
		if e != nil {
			return "", nil, nil, e
		}
		m := regexp.MustCompile(`url=(http://[^"]+)"`).FindSubmatch(b)
		if m == nil {
			return "", nil, nil, errors.New("no redirect in the launch file")
		}
		app = string(m[1])
	}
	u, err := url.Parse(app)
	if err != nil {
		return "", nil, nil, err
	}
	q, err := url.ParseQuery(u.Fragment)
	if err != nil {
		return "", nil, nil, err
	}
	keyID, ok1 := proto.Decode(q.Get("i"), 16)
	key, ok2 := proto.Decode(q.Get("k"), 32)
	if !ok1 || !ok2 || u.Path != "/sender" {
		return "", nil, nil, errors.New("bad launch URL")
	}
	return u.Scheme + "://" + u.Host, keyID, key, nil
}

// Session is one authenticated sender WebSocket (the protocol half of a page).
type Session struct {
	WS   *websocket.Conn
	Init map[string]any
}

// ErrDaemon is an error message from the daemon.
type ErrDaemon struct {
	Code          string
	Authenticated bool // arrived after proof_d verified
}

func (e *ErrDaemon) Error() string { return "daemon error " + e.Code }

// Dial opens the sender WebSocket with the right Origin.
func Dial(ctx context.Context, origin string, header http.Header) (*websocket.Conn, error) {
	h := http.Header{"Origin": {origin}}
	for k, v := range header {
		h[k] = v
	}
	ws, _, err := websocket.Dial(ctx, strings.Replace(origin, "http://", "ws://", 1)+"/local/v1/sender", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(16 << 20)
	return ws, nil
}

// ReadMsg reads one JSON message.
func ReadMsg(ctx context.Context, ws *websocket.Conn) (map[string]any, error) {
	_, b, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// WriteMsg writes one JSON message.
func WriteMsg(ctx context.Context, ws *websocket.Conn, v any) error {
	b, _ := json.Marshal(v)
	return ws.Write(ctx, websocket.MessageText, b)
}

// Authenticate runs the page side of CONTRACT §11.2: hello, verify proof_d
// (sending nothing more if it fails), auth, then read Init.
func Authenticate(ctx context.Context, origin string, keyID, key []byte) (*Session, error) {
	ws, err := Dial(ctx, origin, nil)
	if err != nil {
		return nil, err
	}
	nonceP := proto.RandBytes(32)
	if err := WriteMsg(ctx, ws, map[string]any{"type": "hello", "protocol": 1, "key_id": proto.Encode(keyID), "nonce": proto.Encode(nonceP)}); err != nil {
		ws.CloseNow()
		return nil, err
	}
	m, err := ReadMsg(ctx, ws)
	if err != nil {
		ws.CloseNow()
		return nil, err
	}
	if m["type"] == "error" {
		ws.CloseNow()
		return nil, &ErrDaemon{Code: fmt.Sprint(m["code"])}
	}
	nonceD, ok1 := proto.Decode(fmt.Sprint(m["nonce"]), 32)
	proofD, ok2 := proto.Decode(fmt.Sprint(m["proof"]), 32)
	if m["type"] != "challenge" || !ok1 || !ok2 || !localauth.Verify(key, localauth.RoleDaemon, keyID, nonceP, nonceD, proofD) {
		ws.CloseNow()
		return nil, errors.New("the daemon's proof didn't verify")
	}
	proofP := localauth.Proof(key, localauth.RolePage, keyID, nonceP, nonceD)
	if err := WriteMsg(ctx, ws, map[string]any{"type": "auth", "proof": proto.Encode(proofP)}); err != nil {
		ws.CloseNow()
		return nil, err
	}
	m, err = ReadMsg(ctx, ws)
	if err != nil {
		ws.CloseNow()
		return nil, err
	}
	if m["type"] == "error" {
		ws.CloseNow()
		return nil, &ErrDaemon{Code: fmt.Sprint(m["code"]), Authenticated: true}
	}
	if m["type"] != "init" {
		ws.CloseNow()
		return nil, fmt.Errorf("expected init, got %v", m["type"])
	}
	return &Session{WS: ws, Init: m}, nil
}

// Page simulates the sender page: authenticate, hand over to the
// attachment key, report state and secrets, run commands, reconnect.
type Page struct {
	Origin string
	// TestAPI is where the /test/* hooks live (default: init.server_url).
	TestAPI   string
	launchID  []byte
	launchKey []byte

	mu         sync.Mutex
	attID      []byte
	attKey     []byte
	started    bool // a SenderHost exists (page load done)
	streamID   string
	serverURL  string
	linkBase   string
	scenario   Scenario
	instance   string
	seq        uint64
	phase      string
	mode       string
	safe       bool
	latches    proto.Latches
	policy     proto.PinPolicy
	pin        *string
	pinVer     uint64
	knock      bool
	failures   uint64
	adms       []proto.Admission
	knocks     []proto.Knock
	ends       []proto.End
	link       string
	ws         *websocket.Conn
	signal     *websocket.Conn
	results    map[string]map[string]any
	stopped    bool
	Superseded bool
	Ended      string
	codes      map[string]bool
}

// NewPage prepares a page for a launch URL.
func NewPage(app string) (*Page, error) {
	origin, id, key, err := ParseLaunchURL(app)
	if err != nil {
		return nil, err
	}
	return &Page{Origin: origin, launchID: id, launchKey: key, instance: proto.NewId22(), phase: "starting", mode: "text", safe: true,
		results: map[string]map[string]any{}, codes: map[string]bool{}}, nil
}

// Run connects and reconnects (100 ms → 1 s) until ctx ends, the page is
// superseded or the stream ends.
func (p *Page) Run(ctx context.Context) error {
	backoff := 100 * time.Millisecond
	started := time.Now()
	for ctx.Err() == nil {
		p.mu.Lock()
		id, key := p.attID, p.attKey
		if id == nil {
			id, key = p.launchID, p.launchKey
		}
		p.mu.Unlock()
		s, err := Authenticate(ctx, p.Origin, id, key)
		if err == nil {
			backoff = 100 * time.Millisecond
			err = p.serve(ctx, s)
			p.mu.Lock()
			done := p.Superseded || p.Ended != ""
			p.mu.Unlock()
			if done {
				return err
			}
		} else {
			var de *ErrDaemon
			if errors.As(err, &de) && de.Authenticated {
				p.mu.Lock()
				p.Superseded = true
				p.mu.Unlock()
				return err
			}
			if p.attID == nil && time.Since(started) > 60*time.Second {
				return err
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		if backoff < time.Second {
			backoff *= 2
		}
	}
	return ctx.Err()
}

func (p *Page) send(v any) {
	p.mu.Lock()
	ws := p.ws
	p.mu.Unlock()
	if ws != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		WriteMsg(ctx, ws, v)
		cancel()
	}
}

func (p *Page) serve(ctx context.Context, s *Session) error {
	init := s.Init
	if a, ok := init["attachment"].(map[string]any); ok {
		id, ok1 := proto.Decode(fmt.Sprint(a["key_id"]), 16)
		key, ok2 := proto.Decode(fmt.Sprint(a["key"]), 32)
		if !ok1 || !ok2 {
			return errors.New("bad attachment")
		}
		p.mu.Lock()
		p.attID, p.attKey = id, key // "sessionStorage" before init_ack
		p.mu.Unlock()
		WriteMsg(ctx, s.WS, map[string]any{"type": "init_ack"})
	}
	stream, _ := init["stream"].(map[string]any)
	var il proto.Latches
	b, _ := json.Marshal(init["latches"])
	json.Unmarshal(b, &il)
	p.mu.Lock()
	p.ws = s.WS
	if p.started && p.streamID != stream["id"] {
		p.mu.Unlock()
		p.send(map[string]any{"type": "ended", "reason": "state_lost"})
		return errors.New("state_lost")
	}
	p.latches = p.latches.Or(il) // a reconnect can set a latch, never clear one
	first := !p.started
	if first {
		p.started = true
		p.streamID = fmt.Sprint(stream["id"])
		p.serverURL = fmt.Sprint(init["server_url"])
		p.linkBase = fmt.Sprint(stream["view_link_base"])
		pol, _ := init["policy"].(map[string]any)
		pp, err := proto.ParsePinPolicy(pol["pin_policy"])
		if err != nil {
			pp = proto.DefaultPolicy
		}
		p.policy = pp
		p.knock, _ = pol["knock"].(bool)
		if m, ok := pol["mode"].(string); ok {
			p.mode = m
		}
		if s, ok := pol["safe"].(bool); ok {
			p.safe = s
		}
		if v, ok := pol["pin"].(string); ok {
			p.pin = &v
		} else {
			p.pin = genPin(pp, nil)
		}
		p.pinVer = 1
		k, _ := proto.NewToken43()
		sk, _ := proto.NewToken43()
		p.link = p.linkBase + "&k=" + k + "&s=" + sk
		p.phase = "picking"
	}
	if p.latches.Paused && (p.phase == "live" || p.phase == "picking") {
		p.phase = "paused"
	}
	p.mu.Unlock()
	if first {
		p.loadScenario()
		if err := p.openRealSignal(ctx, init); err != nil {
			return err
		}
	}
	p.sendState()
	p.sendSecrets()
	if first {
		go p.script(ctx)
	}
	hb := time.NewTicker(5 * time.Second)
	defer hb.Stop()
	msgs := make(chan map[string]any)
	errc := make(chan error, 1)
	go func() {
		for {
			m, err := ReadMsg(ctx, s.WS)
			if err != nil {
				errc <- err
				return
			}
			msgs <- m
		}
	}()
	for {
		select {
		case <-hb.C:
			p.sendState()
		case err := <-errc:
			p.mu.Lock()
			if p.ws == s.WS {
				p.ws = nil
			}
			p.mu.Unlock()
			return err
		case m := <-msgs:
			switch m["type"] {
			case "command":
				p.command(m)
			case "error":
				if m["code"] == "superseded" {
					p.mu.Lock()
					p.Superseded = true
					p.mu.Unlock()
					s.WS.CloseNow()
					return errors.New("superseded")
				}
			case "ended":
				p.mu.Lock()
				p.Ended = fmt.Sprint(m["reason"])
				p.mu.Unlock()
				return nil
			}
		}
	}
}

func (p *Page) loadScenario() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", p.testAPI()+"/test/scenario", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var sc Scenario
	if json.NewDecoder(resp.Body).Decode(&sc) == nil {
		p.mu.Lock()
		p.scenario = sc
		p.mu.Unlock()
	}
}

func (p *Page) testAPI() string {
	if p.TestAPI != "" {
		return p.TestAPI
	}
	return p.serverURL
}

func genPin(p proto.PinPolicy, not *string) *string {
	if p.Kind != "digits" {
		return nil
	}
	for {
		b := make([]byte, p.Length)
		for i := range b {
			b[i] = byte('0' + rand.IntN(10))
		}
		s := string(b)
		if not == nil || *not != s {
			return &s
		}
	}
}

// stateLocked is the allowlisted SenderState.
func (p *Page) stateLocked() proto.SenderState {
	p.seq++
	phase := p.phase
	if p.scenario.Phase != "" && phase != "ended" {
		phase = p.scenario.Phase
	}
	pin := proto.PinInfo{Required: p.policy.Kind != "none", Kind: p.policy.Kind}
	if p.policy.Kind == "digits" {
		n := uint64(p.policy.Length)
		pin.Length = &n
	}
	var h *proto.Health
	if phase == "live" {
		k := uint64(2400)
		fps := 30.0
		h = &proto.Health{UplinkKbps: &k, FPS: &fps, QualityLimitation: "none"}
	}
	return proto.SenderState{V: 1, StreamID: p.streamID, Instance: p.instance, Seq: p.seq, Phase: phase, Mode: p.mode, Safe: p.safe,
		E2EE: proto.E2EE{Status: "beta", Epoch: 1},
		Access: proto.Access{Pin: pin, PinVersion: p.pinVer, Latches: p.latches, Knock: p.knock,
			Failures: proto.Failures{Stream: p.failures, Limit: 20}},
		Admissions: append([]proto.Admission{}, p.adms...), Knocks: append([]proto.Knock{}, p.knocks...),
		Ends: append([]proto.End{}, p.ends...), Health: h}
}

func (p *Page) sendState() {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return
	}
	st := p.stateLocked()
	p.mu.Unlock()
	p.send(map[string]any{"type": "state", "state": st})
	p.mu.Lock()
	signal := p.signal
	p.mu.Unlock()
	if signal != nil {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		WriteMsg(c, signal, map[string]any{"type": "sender.state", "state": st})
	}
}

func (p *Page) sendSecrets() {
	p.mu.Lock()
	m := map[string]any{"type": "secrets", "pin": p.pin, "pending_pin": nil, "link": p.link}
	p.mu.Unlock()
	p.send(m)
}

func (p *Page) alert(a map[string]any) { p.send(map[string]any{"type": "alert", "alert": a}) }

func (p *Page) step() time.Duration {
	s := p.scenario.Step
	if s <= 0 {
		s = 0.2
	}
	return time.Duration(s * float64(time.Second))
}

func (p *Page) code() string {
	for {
		b := []byte{proto.RowAlphabet[rand.IntN(22)], proto.RowAlphabet[rand.IntN(22)], proto.RowAlphabet[rand.IntN(22)]}
		if !p.codes[string(b)] {
			p.codes[string(b)] = true
			return string(b)
		}
	}
}

func (p *Page) conn(label *string, hidden bool, admissions []string) string {
	id := "c_" + proto.NewId22()
	ip := fmt.Sprintf("203.0.113.%d", 2+rand.IntN(240))
	dev := "iPhone · Safari"
	path := "udp"
	rtt, loss := 48.0, 0.4
	kbps := uint64(1800)
	now := time.Now().UnixMilli()
	c := proto.Connection{ConnectionID: id, JoinedAt: now, IP: &ip, Device: &dev, Path: &path,
		Stats: &proto.Stats{RTTMS: &rtt, LossPct: &loss, BitrateKbps: &kbps}, StatsAt: &now,
		Reported: proto.Reported{Label: label, TabHidden: &hidden}}
	return p.postConn(c, admissions)
}

func (p *Page) postConn(c proto.Connection, admissions []string) string {
	b, _ := json.Marshal(map[string]any{"stream_id": p.streamID, "capability": strings.Split(p.linkBase, "#c=")[1], "connection": c, "admissions": admissions})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", p.testAPI()+"/test/conn", strings.NewReader(string(b)))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		defer resp.Body.Close()
		var out struct {
			ID string `json:"connection_id"`
		}
		if json.NewDecoder(resp.Body).Decode(&out) == nil && out.ID != "" {
			return out.ID
		}
	}
	return c.ConnectionID
}

func (p *Page) addViewer(name string, hidden bool) proto.Admission {
	var label *string
	if name != "" {
		label = &name
	}
	aid, vid := proto.NewId22(), proto.NewId22()
	cid := p.conn(label, hidden, []string{aid})
	p.mu.Lock()
	a := proto.Admission{AdmissionID: aid, ViewerID: vid, RowCode: p.code(), PeerID: cid, Connected: true, AdmittedAt: time.Now().UnixMilli()}
	p.adms = append(p.adms, a)
	p.mu.Unlock()
	return a
}

// script plays the scenario (live, viewers, knock, failed-pins, freeze).
func (p *Page) script(ctx context.Context) {
	p.mu.Lock()
	sc := p.scenario
	p.mu.Unlock()
	picker := time.Duration(sc.PickerMs) * time.Millisecond
	if picker == 0 {
		picker = 300 * time.Millisecond
	}
	if !sc.NoLive {
		time.Sleep(picker)
		p.mu.Lock()
		if p.phase == "picking" {
			p.phase = "live"
		}
		p.mu.Unlock()
		p.sendState()
	}
	sleep := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(p.step()):
			return true
		}
	}
	if sc.Has("live") {
		p.addViewer("Ana", false)
		p.addViewer("Bo", true)
		p.addViewer("", false)
		p.conn(nil, false, nil) // a connection no admission claims: "joining"
		p.sendState()
	}
	if sc.Has("knock") {
		if !sleep() {
			return
		}
		cid := p.conn(ptr("Hana"), false, nil)
		p.mu.Lock()
		now := time.Now().UnixMilli()
		p.knocks = append(p.knocks, proto.Knock{KnockID: proto.NewId22(), PeerID: cid, RowCode: p.code(), At: now, ExpiresAt: now + 120_000})
		p.mu.Unlock()
		p.sendState()
	}
	if sc.Has("viewers") {
		if !sleep() {
			return
		}
		var batch []proto.Admission
		for _, n := range []string{"Eli", "Femi", "Gus"} {
			batch = append(batch, p.addViewer(n, false))
		}
		p.sendState()
		if !sleep() {
			return
		}
		p.mu.Lock()
		for i := range p.adms {
			if p.adms[i].AdmissionID == batch[1].AdmissionID {
				p.adms[i].Connected = false
			}
		}
		p.mu.Unlock()
		p.leaveConn(batch[1].PeerID)
		p.sendState()
		if !sleep() {
			return
		}
		p.addViewer("Hal", false)
		p.sendState()
	}
	if sc.Has("failed-pins") {
		for i := 0; i < 3; i++ {
			if !sleep() {
				return
			}
			p.mu.Lock()
			p.failures += 3
			p.mu.Unlock()
			t := time.Now().UnixMilli()
			p.alert(map[string]any{"kind": "failed_pin", "count": 3, "sources": 1 + i%2, "from_at": t - 4000, "to_at": t})
			p.sendState()
		}
		if sc.Has("freeze") {
			p.mu.Lock()
			p.latches.Frozen = true
			f := p.failures
			p.mu.Unlock()
			p.alert(map[string]any{"kind": "frozen", "failures": f})
			p.sendState()
		}
	}
}

func (p *Page) leaveConn(cid string) {
	now := time.Now().UnixMilli()
	p.postConn(proto.Connection{ConnectionID: cid, JoinedAt: now - 1000, LeftAt: &now}, nil)
}

func ptr(s string) *string { return &s }

func errResult(code string, details map[string]any) map[string]any {
	e := map[string]any{"code": code, "message": code}
	if details != nil {
		e["details"] = details
	}
	return map[string]any{"ok": false, "error": e}
}

// command runs one daemon command with sender semantics (latches first).
func (p *Page) command(m map[string]any) {
	id := fmt.Sprint(m["id"])
	op := fmt.Sprint(m["op"])
	args, _ := m["args"].(map[string]any)
	p.mu.Lock()
	if cached, ok := p.results[id]; ok {
		p.mu.Unlock()
		p.reply(id, cached)
		return
	}
	res := map[string]any{"ok": true, "data": map[string]any{}}
	pinChanged := false
	if p.scenario.FailOp == op {
		res = errResult(p.scenario.FailCode, nil)
	} else {
		switch op {
		case "stop":
			p.phase = "ended"
		case "pause":
			p.latches.Paused = true
			if p.phase == "live" || p.phase == "picking" {
				p.phase = "paused"
			}
		case "resume":
			p.latches.Paused = false
			if p.phase == "paused" {
				p.phase = "live"
			}
		case "mode":
			p.mode = fmt.Sprint(args["mode"])
		case "safe":
			p.safe = args["on"] == true
		case "lock":
			p.latches.Locked = args["locked"] == true
		case "kick":
			if args["lock"] == true {
				p.latches.Locked = true
			}
			aid := fmt.Sprint(args["admission_id"])
			var vid string
			for _, a := range p.adms {
				if a.AdmissionID == aid {
					vid = a.ViewerID
				}
			}
			if vid == "" {
				var d map[string]any
				if args["lock"] == true {
					d = map[string]any{"locked": true}
				}
				res = errResult("unknown_admission", d)
				break
			}
			var keep []proto.Admission
			var ids []string
			for _, a := range p.adms {
				if a.ViewerID == vid {
					ids = append(ids, a.AdmissionID)
					p.ends = append([]proto.End{{Kind: "admission", AdmissionID: a.AdmissionID, ViewerID: vid, Cause: "kicked"}}, p.ends...)
				} else {
					keep = append(keep, a)
				}
			}
			p.adms = keep
			if args["rotate_pin"] == true {
				p.rotateLocked(args["pin"])
				pinChanged = true
			}
			res["data"] = map[string]any{"admission_ids": ids, "locked": p.latches.Locked}
		case "unfreeze":
			p.latches.Frozen = false
			p.failures = 0
			if args["keep_pin"] != true {
				p.rotateLocked(args["pin"])
				pinChanged = true
			}
		case "pin.rotate":
			if args["pin"] != nil && p.pin != nil && fmt.Sprint(args["pin"]) == *p.pin {
				res = errResult("invalid_argument", nil)
				break
			}
			p.rotateLocked(args["pin"])
			pinChanged = true
		case "pin.set":
			pp, err := proto.ParsePinPolicy(args["policy"])
			if err != nil {
				res = errResult("invalid_argument", nil)
				break
			}
			if pp.Kind == "none" && p.policy.Kind == "none" {
				res = errResult("invalid_argument", nil)
				break
			}
			p.policy = pp
			p.rotateLocked(args["pin"])
			pinChanged = true
		case "knock.approve", "knock.deny":
			kid := fmt.Sprint(args["knock_id"])
			idx := -1
			for i, k := range p.knocks {
				if k.KnockID == kid {
					idx = i
				}
			}
			if idx < 0 {
				res = errResult("unknown_knock", nil)
				break
			}
			k := p.knocks[idx]
			if op == "knock.approve" && args["row_code"] != k.RowCode {
				res = errResult("code_mismatch", nil)
				break
			}
			p.knocks = append(p.knocks[:idx:idx], p.knocks[idx+1:]...)
			if op == "knock.deny" {
				p.ends = append([]proto.End{{Kind: "knock", KnockID: kid, Reason: "denied"}}, p.ends...)
				go p.alert(map[string]any{"kind": "knock_ended", "knock_id": kid, "reason": "denied"})
				break
			}
			a := proto.Admission{AdmissionID: proto.NewId22(), ViewerID: proto.NewId22(), RowCode: k.RowCode, PeerID: k.PeerID,
				Connected: true, AdmittedAt: time.Now().UnixMilli()}
			p.adms = append(p.adms, a)
			res["data"] = map[string]any{"admission_id": a.AdmissionID}
		case "annotations":
			// Viewer drawing policy lives only in the page (not a latch).
			res["data"] = map[string]any{"viewers": args["viewers"] == true}
		case "annotations.clear":
		default:
			res = errResult("invalid_argument", nil)
		}
	}
	p.results[id] = res
	p.mu.Unlock()
	// A PIN change sends secrets and state before it replies (HOST-CONTROL
	// §5.5 step 6), so a reveal right after the reply sees the new PIN.
	if pinChanged {
		p.sendSecrets()
	}
	p.sendState()
	p.reply(id, res)
}

func (p *Page) rotateLocked(v any) {
	if s, ok := v.(string); ok {
		p.pin = &s
	} else {
		p.pin = genPin(p.policy, p.pin)
	}
	if p.policy.Kind == "none" {
		p.pin = nil
	}
	p.pinVer++
}

func (p *Page) reply(id string, res map[string]any) {
	m := map[string]any{"type": "result", "id": id}
	for k, v := range res {
		m[k] = v
	}
	p.send(m)
	p.mu.Lock()
	signal := p.signal
	p.mu.Unlock()
	if signal != nil {
		out := map[string]any{"type": "control.result", "id": id, "ok": res["ok"]}
		if res["ok"] == true {
			out["result"] = res["data"]
		} else {
			out["error"] = res["error"]
		}
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		WriteMsg(c, signal, out)
	}
}
