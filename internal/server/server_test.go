package server

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func free(t *testing.T) string {
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	l.Close()
	return a
}
func TestAPIAndResumableSignal(t *testing.T) {
	dir := t.TempDir()
	s, e := New(Config{Data: dir, Listen: free(t), ICEUDP: free(t), ICETCP: free(t), TURN: free(t), BaseURL: "http://localhost"})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	key, _ := os.ReadFile(filepath.Join(dir, "owner-key"))
	request := func(method, path, key string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(method, h.URL+path, nil)
		r.Header.Set("Authorization", "Bearer "+key)
		v, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	bad := request("POST", "/api/v1/streams", "bad")
	bad.Body.Close()
	if bad.StatusCode != 401 {
		t.Fatal(bad.StatusCode)
	}
	res := request("POST", "/api/v1/streams", string(key))
	var v struct {
		ID      string `json:"id"`
		URL     string `json:"view_link_base"`
		Publish string `json:"publish_token"`
	}
	if e = json.NewDecoder(res.Body).Decode(&v); e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 201 {
		t.Fatal(res.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	join := func(m message) (*websocket.Conn, message) {
		t.Helper()
		c, _, e := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.URL, "http")+"/signal", nil)
		if e != nil {
			t.Fatal(e)
		}
		if e = wsjson.Write(ctx, c, m); e != nil {
			t.Fatal(e)
		}
		var reply message
		if e = wsjson.Read(ctx, c, &reply); e != nil {
			t.Fatal(e)
		}
		return c, reply
	}
	c, first := join(message{Type: "join", Protocol: 1, Role: "view", Credential: strings.Split(v.URL, "#c=")[1]})
	if first.Resumed || first.Session == "" {
		t.Fatal("bad first session")
	}
	c.CloseNow()
	c, next := join(message{Type: "join", Protocol: 1, Role: "view", Session: first.Session})
	defer c.CloseNow()
	if !next.Resumed || next.Session != first.Session {
		t.Fatal("session did not resume")
	}
	frames := uint64(7)
	if e = wsjson.Write(ctx, c, message{Type: "ping", FreshFrames: &frames, At: time.Now().UnixMilli()}); e != nil {
		t.Fatal(e)
	}
	var pong message
	if e = wsjson.Read(ctx, c, &pong); e != nil || pong.Type != "pong" {
		t.Fatal("decoder-bearing ping did not preserve signaling liveness", e, pong.Type)
	}
	res = request("DELETE", "/api/v1/streams/"+v.ID, string(key))
	res.Body.Close()
	if res.StatusCode != 204 {
		t.Fatal(res.StatusCode)
	}
	if _, e = s.Store.Admit("", "view", first.Session); e == nil {
		t.Fatal("deleted session accepted")
	}
	res = request("GET", "/llms.txt", "")
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), "POST /api/v1/streams") {
		t.Fatal("contract missing API")
	}
}
