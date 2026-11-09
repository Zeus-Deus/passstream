package daemon

import (
	"encoding/json"
	"time"
)

func captureSafe(st *stream) bool {
	return !st.ending && !st.rec.Latches.Paused && st.state != nil && st.state.Phase == "live" && !st.state.Access.Latches.Paused && !st.state.E2EE.Suspended
}
func (d *Daemon) captureUntilLocked(st *stream) time.Time {
	until := st.frameDemandUntil
	if r := st.publish; r != nil && r.status == "pending" && r.kind == "snapshot" && r.expires.After(until) {
		until = r.expires
	}
	return until
}
func (d *Daemon) touchCaptureLocked(st *stream, wait time.Duration) {
	idle := d.cfg.CaptureIdle
	if idle == 0 {
		idle = 30 * time.Second
	}
	st.frameDemandUntil = time.Now().Add(idle + wait)
	d.updateCaptureLocked(st)
}
func (d *Daemon) updateCaptureLocked(st *stream) {
	if st.demandTimer != nil {
		st.demandTimer.Stop()
		st.demandTimer = nil
	}
	until := d.captureUntilLocked(st)
	if st.page != nil {
		b, _ := json.Marshal(map[string]any{"type": "capture_demand", "until": max(int64(0), until.UnixMilli())})
		st.page.send(b)
		st.demandPage = st.page
	}
	if !captureSafe(st) {
		st.captures.Close()
		return
	}
	st.captures.Allow()
	if !time.Now().Before(until) {
		st.captures.Mark("idle", "")
		return
	}
	st.captures.Begin()
	st.demandTimer = time.AfterFunc(time.Until(until), func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.st == st && !st.ending {
			d.updateCaptureLocked(st)
		}
	})
}
