package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/passstream/passstream/internal/host/capture"
	"github.com/passstream/passstream/internal/host/passglue"
	"github.com/passstream/passstream/internal/host/proto"
	"github.com/passstream/passstream/internal/host/state"
)

func TestPublishAlwaysRequiresSenderConfirmation(t *testing.T) {
	for _, decision := range []string{"confirm", "refuse", "timeout", "wrong-page"} {
		t.Run(decision, func(t *testing.T) {
			var calls atomic.Int64
			var original bytes.Buffer
			png.Encode(&original, image.NewRGBA(image.Rect(0, 0, 16, 8)))
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				data, _ := io.ReadAll(r.Body)
				if !bytes.Equal(data, original.Bytes()) {
					t.Error("published a different artifact than the confirmed preview")
				}
				fmt.Fprintf(w, `{"url":"http://%s/s/result"}`, r.Host)
			}))
			defer service.Close()
			key := filepath.Join(t.TempDir(), "key")
			os.WriteFile(key, []byte("fake_publish_key"), 0600)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			page := &pageConn{out: make(chan []byte, 128), done: make(chan struct{})}
			st := &stream{rec: &state.Record{StreamID: "test"}, state: &proto.SenderState{Phase: "live"}, page: page}
			d := &Daemon{ctx: ctx, st: st, cfg: Config{PublishConfirmTimeout: 100 * time.Millisecond, Pass: passglue.Config{PassFile: service.URL, PassFileKey: key}}}
			st.captures.Put(capture.Frame{ID: "previewed", Width: 16, Height: 8, Data: original.Bytes()})
			raw, err := d.requestPublish(ctx, st, map[string]any{"kind": "snapshot", "service": "passfile"})
			if err != nil {
				t.Fatal(err)
			}
			result := raw.(map[string]any)
			id := result["request_id"].(string)
			if result["status"] != "pending" || calls.Load() != 0 {
				t.Fatal("unconfirmed request uploaded")
			}
			if _, err := d.featureDispatch(ctx, "publish.confirm", map[string]any{"request_id": id}, st); err == nil {
				t.Fatal("CLI/API can confirm")
			}
			switch decision {
			case "confirm":
				for range 2 {
					d.publishDecision(st, page, map[string]any{"request_id": id, "approved": true})
				}
			case "refuse":
				d.publishDecision(st, page, map[string]any{"request_id": id, "approved": false})
			case "wrong-page":
				d.publishDecision(st, &pageConn{}, map[string]any{"request_id": id, "approved": true})
			}
			raw, err = d.publishStatus(ctx, st, map[string]any{"request_id": id, "wait": float64(1)})
			if err != nil {
				t.Fatal(err)
			}
			result = raw.(map[string]any)
			want := "timeout"
			if decision == "confirm" {
				want = "confirmed"
			}
			if decision == "refuse" {
				want = "refused"
			}
			if result["status"] != want {
				t.Fatal(result)
			}
			if decision == "confirm" {
				if calls.Load() != 1 || result["url"] != service.URL+"/s/result" {
					t.Fatal(result, calls.Load())
				}
			} else if calls.Load() != 0 {
				t.Fatal("refused/timed out publish uploaded")
			}
			d.publishDecision(st, page, map[string]any{"request_id": id, "approved": true})
			if decision != "confirm" && calls.Load() != 0 {
				t.Fatal("late approval uploaded")
			}
		})
	}
}

func TestCaptureLeaseDropsRetainedFrames(t *testing.T) {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 16, 8)))
	st := &stream{rec: &state.Record{}, state: &proto.SenderState{Phase: "live"}}
	d := &Daemon{st: st, cfg: Config{CaptureIdle: 20 * time.Millisecond}}
	d.mu.Lock()
	d.touchCaptureLocked(st, 0)
	d.mu.Unlock()
	st.captures.Put(capture.Frame{ID: "a", Width: 16, Height: 8, Data: b.Bytes()})
	f, _ := st.captures.Frame(context.Background(), "", 0, 0)
	if f.Status != "frame" {
		t.Fatal(f)
	}
	time.Sleep(50 * time.Millisecond)
	f, _ = st.captures.Frame(context.Background(), "a", 0, 0)
	if f.Status != "idle" || f.Data != nil {
		t.Fatal("expired feed retained a picture", f.Status)
	}
}

func TestRejectedCaptureCannotLeaveAnUnchangedPriorFrame(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		var pngData bytes.Buffer
		png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 16, 8)))
		page := &pageConn{out: make(chan []byte, 16), done: make(chan struct{})}
		st := &stream{rec: &state.Record{}, state: &proto.SenderState{Phase: "live"}, page: page, frameDemandUntil: time.Now().Add(time.Minute)}
		d := &Daemon{st: st}
		st.captures.Put(capture.Frame{ID: "old", Width: 16, Height: 8, Data: pngData.Bytes()})
		data := pngData.Bytes()
		width := 3841
		want := "stale"
		if oversized {
			data = make([]byte, capture.MaxFrame+1)
			width = 16
			want = "too_large"
		}
		d.onCapture(st, page, map[string]any{"type": "capture", "status": "frame", "id": "bad", "width": float64(width), "height": float64(8), "data": base64.StdEncoding.EncodeToString(data)})
		f, e := st.captures.Frame(context.Background(), "old", 0, 0)
		if e != nil || f.Status != want || f.Data != nil {
			t.Fatal("invalid capture retained an old picture", f.Status, e)
		}
	}
}
