package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/passstream/passstream/internal/host/capture"
	"github.com/passstream/passstream/internal/host/proto"
)

type featureToken struct {
	stream  *stream
	expires time.Time
}

func isFeatureOp(op string) bool {
	switch op {
	case "frame", "capture.auth", "record.start", "record.stop", "record.status", "publish", "publish.status":
		return true
	}
	return false
}

func featureInt(args map[string]any, k string, def, max int) (int, error) {
	v, present := args[k]
	if !present {
		return def, nil
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f > float64(max) || f != float64(int(f)) {
		return 0, proto.E("invalid_argument", "invalid %s", k)
	}
	return int(f), nil
}

func (d *Daemon) featureStream(expected *stream) (*stream, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.st
	if st == nil || st.ending || expected != nil && expected != st {
		return nil, proto.E("not_live", "no active share")
	}
	return st, nil
}

func (d *Daemon) featureDispatch(ctx context.Context, op string, args map[string]any, expected *stream) (any, error) {
	st, err := d.featureStream(expected)
	if err != nil {
		return nil, err
	}
	switch op {
	case "capture.auth":
		if err := closed(args); err != nil {
			return nil, err
		}
		token := proto.Encode(proto.RandBytes(32))
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.st != st || st.ending {
			return nil, proto.E("not_live", "no active share")
		}
		if d.featureTokens == nil {
			d.featureTokens = map[string]featureToken{}
		}
		for k, v := range d.featureTokens {
			if time.Now().After(v.expires) || v.stream != st {
				delete(d.featureTokens, k)
			}
		}
		if len(d.featureTokens) >= 16 {
			return nil, proto.E("rate_limited", "too many local credentials")
		}
		h := sha256.Sum256([]byte(token))
		d.featureTokens[string(h[:])] = featureToken{st, time.Now().Add(5 * time.Minute)}
		return map[string]any{"token": token, "origin": d.origin(), "expires_in_s": 300}, nil
	case "frame":
		if err := closed(args, "after", "wait", "max_width"); err != nil {
			return nil, err
		}
		after, _ := args["after"].(string)
		if v, ok := args["after"]; ok && (v != after || len(after) > 100) {
			return nil, proto.E("invalid_argument", "invalid after")
		}
		wait, e := featureInt(args, "wait", 0, 30)
		if e != nil {
			return nil, e
		}
		width, e := featureInt(args, "max_width", 0, 3840)
		if e != nil {
			return nil, e
		}
		d.mu.Lock()
		if d.st != st || st.ending {
			d.mu.Unlock()
			return nil, proto.E("not_live", "no active share")
		}
		d.touchCaptureLocked(st, time.Duration(wait)*time.Second)
		d.mu.Unlock()
		return st.captures.Frame(ctx, after, time.Duration(wait)*time.Second, width)
	case "record.start":
		if err := closed(args, "duration", "max_bytes"); err != nil {
			return nil, err
		}
		duration, e := featureInt(args, "duration", 120, 120)
		if e != nil {
			return nil, e
		}
		size, e := featureInt(args, "max_bytes", capture.MaxRecording, capture.MaxRecording)
		if e != nil {
			return nil, e
		}
		if duration < 1 || size < 1024 {
			return nil, proto.E("invalid_argument", "recording bounds must be positive (at least 1024 bytes)")
		}
		return d.forward(op, map[string]any{"duration": duration, "max_bytes": size}, "cli", "", st)
	case "record.stop", "record.status":
		if err := closed(args); err != nil {
			return nil, err
		}
		return d.forward(op, args, "cli", "", st)
	case "publish":
		return d.requestPublish(ctx, st, args)
	case "publish.status":
		return d.publishStatus(ctx, st, args)

	}
	return nil, proto.E("invalid_argument", "unknown local feature op")
}

func (d *Daemon) onCapture(st *stream, p *pageConn, m map[string]any) {
	// Copy/validate the large payload before taking the daemon's control lock.
	status, _ := m["status"].(string)
	var prepared *capture.Prepared
	var part []byte
	var err error
	id, _ := m["id"].(string)
	if m["type"] == "recording" {
		enc, _ := m["data"].(string)
		part, err = capture.Decode(enc, 512<<10)
	} else if status == "frame" {
		enc, _ := m["data"].(string)
		var data []byte
		data, err = capture.Decode(enc, capture.MaxFrame)
		if err == nil {
			w, e1 := featureInt(m, "width", 0, 3840)
			h, e2 := featureInt(m, "height", 0, 2160)
			if e1 != nil {
				err = e1
			} else if e2 != nil {
				err = e2
			}
			if err == nil {
				prepared, err = capture.Prepare(capture.Frame{ID: id, At: nowMs(), Width: w, Height: h, Data: data})
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st != st || st.page != p || st.ending {
		return
	}
	accepted := false
	defer func() {
		ack, ok := m["ack"].(string)
		if ok && len(ack) <= 100 {
			reply, _ := json.Marshal(map[string]any{"type": "artifact_ack", "ack": ack, "ok": accepted})
			p.send(reply)
		}
	}()
	if status == "paused" {
		st.captures.Close()
		d.refusePublishLocked(st, "refused")
		return
	}
	if !captureSafe(st) {
		return
	}
	if m["type"] == "recording" {
		if err == nil {
			final, _ := m["final"].(bool)
			accepted = st.captures.Record(id, part, final) == nil
			if accepted && final {
				ms, _ := featureInt(m, "duration_ms", 0, 120000)
				st.captures.SetRecordingDuration(id, int64(ms))
			}
		}
		return
	}
	if !time.Now().Before(d.captureUntilLocked(st)) {
		return
	}
	switch status {
	case "idle":
		st.captures.Mark("idle", "")
	case "too_large", "stale":
		st.captures.Mark(status, "capture_"+status)
	case "frame":
		if err != nil {
			status := "stale"
			if proto.AsError(err).Code == "payload_too_large" {
				status = "too_large"
			}
			st.captures.Mark(status, "invalid_capture")
			return
		}
		if prepared != nil {
			accepted = st.captures.Commit(prepared) == nil
		}
	}
}

func (d *Daemon) ownerFeature(st *stream, p *pageConn, id, op string, args map[string]any) {
	if !d.isCurrent(st, p) {
		return
	}
	data, err := d.featureDispatch(d.ctx, op, args, st)
	if !d.isCurrent(st, p) {
		return
	}
	m := map[string]any{"type": "owner_result", "id": id, "ok": err == nil, "data": data}
	if err != nil {
		m["error"] = proto.AsError(err)
	}
	b, _ := json.Marshal(m)
	p.send(b)
}

func (d *Daemon) serveFeatures(w http.ResponseWriter, r *http.Request) {
	writeErr := func(status int, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"error": proto.AsError(err)})
	}
	if origins := r.Header.Values("Origin"); len(origins) > 1 || len(origins) == 1 && origins[0] != d.origin() {
		writeErr(403, proto.E("origin_rejected", "local origin required"))
		return
	}
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	h := sha256.Sum256([]byte(token))
	d.mu.Lock()
	cred, ok := d.featureTokens[string(h[:])]
	ok = ok && cred.stream == d.st && !cred.stream.ending && time.Now().Before(cred.expires)
	d.mu.Unlock()
	if !ok || !bearer || len(r.Header.Values("Authorization")) != 1 {
		writeErr(401, proto.E("unauthorized", "local owner credential required"))
		return
	}
	if expected := r.Header.Get("X-PassStream-Stream"); expected != "" && expected != cred.stream.rec.StreamID {
		writeErr(409, proto.E("not_live", "this stream is not active on the local host"))
		return
	}
	var op string
	args := map[string]any{}
	if r.URL.Path == "/local/v1/frame" && r.Method == "GET" {
		op = "frame"
		for k, v := range r.URL.Query() {
			if len(v) != 1 {
				writeErr(400, proto.E("invalid_argument", "duplicate parameter"))
				return
			}
			if k == "after" {
				args[k] = v[0]
			} else if k == "wait" || k == "max_width" {
				n, e := strconv.Atoi(v[0])
				if e != nil {
					writeErr(400, proto.E("invalid_argument", "invalid integer"))
					return
				}
				args[k] = float64(n)
			} else {
				writeErr(400, proto.E("invalid_argument", "unknown parameter"))
				return
			}
		}
	} else if r.URL.Path == "/local/v1/features" && r.Method == "POST" {
		var req struct {
			Op   string         `json:"op"`
			Args map[string]any `json:"args"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF {
			writeErr(400, proto.E("invalid_argument", "invalid feature request"))
			return
		}
		op, args = req.Op, req.Args
		if !isFeatureOp(op) || op == "capture.auth" {
			writeErr(403, proto.E("local_owner_required", "unsupported owner action"))
			return
		}
	} else {
		writeErr(405, proto.E("invalid_argument", "method not allowed"))
		return
	}
	data, err := d.featureDispatch(r.Context(), op, args, cred.stream)
	if err != nil {
		status := 409
		switch proto.AsError(err).Code {
		case "invalid_argument":
			status = 400
		case "not_found":
			status = 404
		case "rate_limited":
			status = 429
		case "internal":
			status = 500
		case "storage":
			status = 503
		case "sender_timeout":
			status = 504
		case "upload_failed":
			status = 502
		}
		writeErr(status, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
