package daemon_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/capture"
	"github.com/passstream/passstream/internal/host/hosttest"
	"github.com/passstream/passstream/internal/host/proto"
)

func frameOf(t *testing.T, c *hosttest.Client) capture.Frame {
	t.Helper()
	b, e := c.Call("frame", map[string]any{})
	if e != nil {
		t.Fatal(e)
	}
	var f capture.Frame
	if json.Unmarshal(b, &f) != nil {
		t.Fatal("bad frame result")
	}
	return f
}
func captureWire(t *testing.T, id string) map[string]any {
	t.Helper()
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 16, 8)))
	return map[string]any{"type": "capture", "status": "frame", "id": id, "data": base64.StdEncoding.EncodeToString(b.Bytes()), "width": 16, "height": 8}
}

func TestLocalFramesPrivacyDisconnectAndNoServerLeak(t *testing.T) {
	e, c, origin, lid, lkey := launched(t)
	s, _, _ := attachedPage(t, c, origin, lid, lkey)
	state := senderState(e.record().StreamID, proto.NewId22(), 1, proto.Latches{}, nil)
	_ = hosttest.WriteMsg(ctx5(), s.WS, state)
	readUntil(t, s.WS, "state_ack")
	// A request, rather than an unsolicited sender image, enables the feed.
	first := make(chan capture.Frame, 1)
	go func() {
		b, _ := c.Call("frame", map[string]any{})
		var f capture.Frame
		json.Unmarshal(b, &f)
		first <- f
	}()
	readUntil(t, s.WS, "capture_demand")
	wire := captureWire(t, "snapshot_private_needle")
	_ = hosttest.WriteMsg(ctx5(), s.WS, wire)
	if f := <-first; f.Status != "frame" {
		t.Fatal(f)
	}
	if f := frameOf(t, c); f.ID != "snapshot_private_needle" || f.Width != 16 || len(f.Data) == 0 {
		t.Fatal(f)
	}
	statusRaw, _ := c.Call("status", nil)
	if strings.Contains(string(statusRaw), "snapshot_private_needle") || strings.Contains(e.log.String(), wire["data"].(string)) {
		t.Fatal("capture leaked into status/logs")
	}
	// A sender privacy/curtain transition invalidates retained bytes immediately.
	_ = hosttest.WriteMsg(ctx5(), s.WS, map[string]any{"type": "capture", "status": "paused"})
	waitFor(t, "privacy marker", time.Second, func() bool { return frameOf(t, c).Status == "paused" })
	state = senderState(e.record().StreamID, proto.NewId22(), 2, proto.Latches{Paused: true}, nil)
	_ = hosttest.WriteMsg(ctx5(), s.WS, state)
	readUntil(t, s.WS, "state_ack")
	_ = hosttest.WriteMsg(ctx5(), s.WS, captureWire(t, "late_encode"))
	// A following state ack fences processing of the preceding late encode.
	state = senderState(e.record().StreamID, proto.NewId22(), 3, proto.Latches{Paused: true}, nil)
	_ = hosttest.WriteMsg(ctx5(), s.WS, state)
	readUntil(t, s.WS, "state_ack")
	if f := frameOf(t, c); f.Status != "paused" || len(f.Data) != 0 {
		t.Fatal("paused frame leaked")
	}
	s.WS.CloseNow()
	waitFor(t, "detach", time.Second, func() bool { return status(t, c).Stream.Attachment == "detached" })
	if f := frameOf(t, c); f.Status != "paused" {
		t.Fatal(f)
	}
}

func TestLocalHTTPRequiresOwnerCredentialAndRejectsRemoteOrigin(t *testing.T) {
	_, c, origin, lid, lkey := launched(t)
	_, _, _ = attachedPage(t, c, origin, lid, lkey)
	b, e := c.Call("capture.auth", map[string]any{})
	if e != nil {
		t.Fatal(e)
	}
	var cred struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(b, &cred)
	for _, tc := range []struct {
		token, origin, host string
		want                int
	}{
		{"", "", "", 401}, {"viewer_session", "", "", 401}, {testKey, "", "", 401},
		{cred.Token, "https://remote.example", "", 403}, {cred.Token, "null", "", 403},
		{cred.Token, origin, "foreign.example", 403}, {cred.Token, origin, "", 200},
	} {
		r, _ := http.NewRequest("GET", origin+"/local/v1/frame", nil)
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.host != "" {
			r.Host = tc.host
		}
		resp, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("want %d got %d %s", tc.want, resp.StatusCode, body)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("CORS enabled")
		}
	}
	// A viewer cannot request a local owner credential over the sender WS.
	ws, e := hosttest.Dial(ctx5(), origin, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.CloseNow()
	_ = hosttest.WriteMsg(ctx5(), ws, map[string]any{"type": "owner_action", "op": "capture.auth"})
	if _, e := hosttest.ReadMsg(ctx5(), ws); e == nil {
		t.Fatal("unauthenticated local owner request accepted")
	}
}

func TestFeatureArgumentBoundsRefuseBeforePublishing(t *testing.T) {
	_, c, origin, lid, lkey := launched(t)
	_, _, _ = attachedPage(t, c, origin, lid, lkey)
	for _, tc := range []struct {
		op   string
		args map[string]any
	}{{"frame", map[string]any{"wait": 31}}, {"frame", map[string]any{"max_width": -1}}, {"frame", map[string]any{"after": true}}, {"record.start", map[string]any{"duration": 121}}, {"record.start", map[string]any{"max_bytes": float64(capture.MaxRecording + 1)}}, {"record.stop", map[string]any{"unknown": true}}, {"publish", map[string]any{"kind": "recording", "service": "passpage"}}, {"publish", map[string]any{"kind": "snapshot", "service": "unknown"}}} {
		if _, e := c.Call(tc.op, tc.args); e == nil || proto.AsError(e).Code != "invalid_argument" {
			t.Fatalf("invalid %s args accepted: %v", tc.op, e)
		}
	}
}
