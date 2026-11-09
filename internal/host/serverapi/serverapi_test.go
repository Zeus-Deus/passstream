package serverapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeBase(t *testing.T) {
	good := map[string]string{"https://Stream.Example/": "https://stream.example", "http://127.0.0.1:8080": "http://127.0.0.1:8080",
		"http://localhost:9": "http://localhost:9", "https://[::1]:443": "https://[::1]:443"}
	for in, want := range good {
		if got, err := NormalizeBase(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"http://stream.example", "https://u:p@x.example", "https://x.example/path", "https://x.example?q",
		"https://x.example#f", "ftp://x.example", "https://*.example", "https://x.example\n", "x.example"} {
		if _, err := NormalizeBase(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if SignalURL("https://s.example") != "wss://s.example/signal" || SignalURL("http://127.0.0.1:8") != "ws://127.0.0.1:8/signal" {
		t.Fatal("signal url")
	}
}

func TestHostileServer(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(201)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	tok := strings.Repeat("A", 43)
	id := strings.Repeat("A", 22)
	body = `{"id":"` + id + `","view_link_base":"https://evil.example/v#c=` + tok + `","publish_token":"` + tok + `"}`
	if _, err := c.Create(context.Background(), CreateStream{}); err == nil {
		t.Fatal("a view link on another origin was accepted")
	}
	body = `{"id":"` + id + `","view_link_base":"` + srv.URL + `/v#c=` + tok + `","publish_token":"` + tok + `"}`
	if _, err := c.Create(context.Background(), CreateStream{}); err != nil {
		t.Fatal(err)
	}
	body = strings.Repeat("x", 70<<10)
	if _, err := c.Create(context.Background(), CreateStream{}); err == nil {
		t.Fatal("an oversized body was accepted")
	}
	if _, err := New(srv.URL, "wrong").Settings(context.Background()); err == nil || !strings.Contains(err.Error(), "key_rejected") {
		t.Fatalf("401: %v", err)
	}
}

// TestEventsEndedSnapshot (review round 1, finding 6): on connect the
// server sends the current stream and never replays "ended"; a terminal
// snapshot ends the stream with its reason.
func TestEventsEndedSnapshot(t *testing.T) {
	for body, want := range map[string]string{
		"event: stream\ndata: {\"id\":\"x\",\"phase\":\"ended\",\"end_reason\":\"expired\"}\n\n": "expired",
		"event: stream\ndata: {\"phase\":\"ended\",\"end_reason\":null}\n\n":                     "unknown",
		"event: stream\ndata: {\"phase\":\"ended\",\"end_reason\":\"future\"}\n\n":               "unknown",
		"data: {\"phase\":\"ended\",\n\ndata: x\nevent: stream\ndata: {\"phase\":\"live\"}\n\n":  "",
		": hi\n\nevent: ended\ndata: {\"reason\":\"stopped\"}\n\n":                               "stopped",
	} {
		var got []string
		err := readEvents(strings.NewReader(body), func(r string) { got = append(got, r) })
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if want == "" && len(got) != 0 || want != "" && (len(got) != 1 || got[0] != want) {
			t.Fatalf("%q: ended %v, want %q", body, got, want)
		}
	}
}

// TestEventsAreBounded (review round 1, finding 5): an event the daemon
// reads is bounded as a whole, not per line, and irrelevant events'
// bodies are discarded whatever their size.
func TestEventsAreBounded(t *testing.T) {
	// A 9 MiB "ended" made of small data lines fails the connection.
	var b strings.Builder
	b.WriteString("event: ended\n")
	for b.Len() < 9<<20 {
		b.WriteString("data: " + strings.Repeat("x", 1000) + "\n")
	}
	b.WriteString("\nevent: ended\ndata: {\"reason\":\"stopped\"}\n\n")
	called := false
	if err := readEvents(strings.NewReader(b.String()), func(string) { called = true }); err == nil || called {
		t.Fatalf("a 9 MiB event was accepted: err=%v called=%v", err, called)
	}
	// So does one over-long line in a "stream" event.
	big := "event: stream\ndata: {\"phase\":\"ended\",\"pad\":\"" + strings.Repeat("y", maxEvent) + "\"}\n\n"
	if err := readEvents(strings.NewReader(big), func(string) { called = true }); err == nil || called {
		t.Fatalf("an over-long stream line was accepted: err=%v called=%v", err, called)
	}
	// Huge viewers events (many lines, or one 9 MiB line) are skipped and
	// the following "ended" still counts.
	var v strings.Builder
	v.WriteString("event: viewers\n")
	for v.Len() < 9<<20 {
		v.WriteString("data: " + strings.Repeat("v", 1000) + "\n")
	}
	v.WriteString("\nevent: viewers\ndata: " + strings.Repeat("w", 9<<20) + "\n\n")
	v.WriteString("event: ended\ndata: {\"reason\":\"expired\"}\n\n")
	var got []string
	if err := readEvents(strings.NewReader(v.String()), func(r string) { got = append(got, r) }); err != nil || len(got) != 1 || got[0] != "expired" {
		t.Fatalf("after big viewers events: err=%v ended=%v", err, got)
	}
}

// TestBanActionID (review round 1, finding 7): the caller's action id is
// the server command id on every attempt, so retries are deduplicated.
func TestBanActionID(t *testing.T) {
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var cmd struct{ ID string }
		json.NewDecoder(r.Body).Decode(&cmd)
		ids = append(ids, cmd.ID)
		w.Write([]byte(`{"id":"` + cmd.ID + `","ok":true,"result":{"ip_banned":true,"admission_ids":[]}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	for i := 0; i < 2; i++ {
		if _, err := c.Ban(context.Background(), "s", "c_x", "act-1"); err != nil {
			t.Fatal(err)
		}
	}
	c.Ban(context.Background(), "s", "c_x", "")
	c.Ban(context.Background(), "s", "c_x", "")
	if ids[0] != "act-1" || ids[1] != "act-1" || ids[2] == ids[3] || ids[2] == "" {
		t.Fatalf("command ids %v", ids)
	}
	for _, bad := range []string{"", strings.Repeat("a", 65), "a b", "é"} {
		if ValidActionID(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// TestStreamEnded: GET /streams/{id} reconciles a missed end; 404 is gone.
func TestStreamEnded(t *testing.T) {
	body, status := `{"phase":"ended","end_reason":"expired"}`, 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	if r, ok, err := c.StreamEnded(context.Background(), "s"); err != nil || !ok || r != "expired" {
		t.Fatalf("%q %v %v", r, ok, err)
	}
	body = `{"phase":"live","end_reason":null}`
	if _, ok, err := c.StreamEnded(context.Background(), "s"); err != nil || ok {
		t.Fatalf("live: %v %v", ok, err)
	}
	body, status = `{"error":{"code":"not_found","message":"x"}}`, 404
	if r, ok, err := c.StreamEnded(context.Background(), "s"); err != nil || !ok || r != "deleted" {
		t.Fatalf("404: %q %v %v", r, ok, err)
	}
}

func TestBrowserFailedEndReasonSurvivesServerRelay(t *testing.T) {
	if got := endReason("browser_failed"); got != "browser_failed" {
		t.Fatal(got)
	}
}
