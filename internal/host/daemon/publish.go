package daemon

import (
	"context"
	"encoding/json"
	"time"

	"github.com/passstream/passstream/internal/host/capture"
	"github.com/passstream/passstream/internal/host/proto"
)

type publishRequest struct {
	id, kind, service, destination, status string
	expires                                time.Time
	data, preview                          []byte
	durationMS                             int64
	timer                                  *time.Timer
	changed                                chan struct{}
	prompted, uploading                    bool
	url                                    string
	err                                    *proto.Error
	cancel                                 context.CancelFunc
}

func (r *publishRequest) result() map[string]any {
	m := map[string]any{"request_id": r.id, "status": r.status, "kind": r.kind, "service": r.service, "destination": r.destination, "expires_at": r.expires.UnixMilli(), "uploading": r.uploading}
	if r.url != "" {
		m["url"] = r.url
	}
	if r.err != nil {
		m["error"] = r.err
	}
	return m
}
func (r *publishRequest) notify() { close(r.changed); r.changed = make(chan struct{}) }
func (d *Daemon) requestPublish(ctx context.Context, st *stream, args map[string]any) (any, error) {
	if e := closed(args, "kind", "service", "max_width"); e != nil {
		return nil, e
	}
	kind, _ := args["kind"].(string)
	service, _ := args["service"].(string)
	if kind != "snapshot" && kind != "recording" || service != "passfile" && service != "passpage" || kind == "recording" && service != "passfile" {
		return nil, proto.E("invalid_argument", "publish snapshot passfile|passpage or recording passfile")
	}
	width, e := featureInt(args, "max_width", 0, 3840)
	if e != nil {
		return nil, e
	}
	destination, e := d.cfg.Pass.Destination(service)
	if e != nil {
		return nil, e
	}
	d.mu.Lock()
	if d.st != st || st.ending || st.page == nil || !captureSafe(st) {
		d.mu.Unlock()
		return nil, proto.E("paused", "capture is paused or sender is offline")
	}
	if st.featureBusy {
		d.mu.Unlock()
		return nil, proto.E("conflict", "a publish request is already in progress")
	}
	ttl := d.cfg.PublishConfirmTimeout
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	r := &publishRequest{id: proto.Encode(proto.RandBytes(16)), kind: kind, service: service, destination: destination, status: "pending", expires: time.Now().Add(ttl), changed: make(chan struct{})}
	st.publish = r
	st.featureBusy = true
	r.timer = time.AfterFunc(ttl, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.st == st && st.publish == r {
			d.refusePublishLocked(st, "timeout")
		}
	})
	if kind == "snapshot" {
		d.updateCaptureLocked(st)
	}
	d.mu.Unlock()
	var data, preview []byte
	var duration int64
	if kind == "snapshot" {
		var f capture.Frame
		f, e = st.captures.AwaitFrame(ctx, 3*time.Second, width)
		if e == nil && f.Status != "frame" {
			e = proto.E(f.Status, "no current snapshot")
		}
		if e == nil {
			data = f.Data
			var p capture.Frame
			p, e = st.captures.Preview(ctx, f, 320)
			preview = p.Data
		}
	} else {
		data, duration, e = st.captures.RecordingDetails()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.st != st || st.publish != r || r.status != "pending" {
		return r.result(), nil
	}
	if e != nil || !captureSafe(st) {
		d.refusePublishLocked(st, "refused")
		if e != nil {
			return nil, e
		}
		return r.result(), nil
	}
	r.data, r.preview, r.durationMS, r.prompted = data, preview, duration, true
	prompt := map[string]any{"request_id": r.id, "kind": r.kind, "service": r.service, "destination": r.destination, "bytes": len(data), "duration_ms": duration, "preview": preview, "expires_at": r.expires.UnixMilli()}
	b, _ := json.Marshal(map[string]any{"type": "publish_prompt", "request": prompt})
	st.page.send(b)
	return r.result(), nil
}
func (d *Daemon) refusePublishLocked(st *stream, status string) {
	r := st.publish
	if r == nil || r.status != "pending" {
		return
	}
	r.status = status
	r.data = nil
	r.preview = nil
	r.timer.Stop()
	st.featureBusy = false
	r.notify()
	if st.page != nil {
		st.page.send([]byte(`{"type":"publish_prompt","request":null}`))
	}
	d.updateCaptureLocked(st)
}

// No Unix or HTTP operation calls this. Only the current mutually authenticated
// sender window can approve the immutable artifact shown by its prompt.
func (d *Daemon) publishDecision(st *stream, p *pageConn, m map[string]any) {
	id, _ := m["request_id"].(string)
	approved, valid := m["approved"].(bool)
	d.mu.Lock()
	r := st.publish
	if d.st != st || st.page != p || r == nil || r.id != id || r.status != "pending" || !r.prompted || !valid {
		d.mu.Unlock()
		return
	}
	if !time.Now().Before(r.expires) {
		d.refusePublishLocked(st, "timeout")
		d.mu.Unlock()
		return
	}
	if !approved || !captureSafe(st) {
		d.refusePublishLocked(st, "refused")
		d.mu.Unlock()
		return
	}
	r.status, r.uploading = "confirmed", true
	r.timer.Stop()
	r.preview = nil
	r.notify()
	p.send([]byte(`{"type":"publish_prompt","request":null}`))
	d.updateCaptureLocked(st)
	data := r.data
	r.data = nil
	uploadCtx, cancel := context.WithCancel(d.ctx)
	r.cancel = cancel
	d.mu.Unlock()
	go func() {
		defer cancel()
		link, err := d.cfg.Pass.Upload(uploadCtx, r.service, r.kind, data)
		d.mu.Lock()
		defer d.mu.Unlock()
		r.uploading = false
		r.url = link
		if err != nil {
			r.err = proto.AsError(err)
		}
		r.notify()
		if d.st == st && st.publish == r {
			st.featureBusy = false
		}
	}()
}
func (d *Daemon) publishStatus(ctx context.Context, st *stream, args map[string]any) (any, error) {
	if e := closed(args, "request_id", "wait"); e != nil {
		return nil, e
	}
	id, _ := args["request_id"].(string)
	wait, e := featureInt(args, "wait", 0, 30)
	if e != nil {
		return nil, e
	}
	timer := time.NewTimer(time.Duration(wait) * time.Second)
	defer timer.Stop()
	for {
		d.mu.Lock()
		r := st.publish
		if r == nil || r.id != id {
			d.mu.Unlock()
			return nil, proto.E("not_found", "no such publish request")
		}
		result := r.result()
		ch := r.changed
		pending := r.status == "pending" || r.uploading
		d.mu.Unlock()
		if !pending || wait == 0 {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return result, nil
		case <-ch:
		}
	}
}
