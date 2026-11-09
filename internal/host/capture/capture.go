// Package capture holds owner-only plaintext artifacts. It has no server client
// or disk persistence; closing a privacy gate invalidates every retained byte.
package capture

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/draw"
	"image/png"
	"sync"
	"time"

	"github.com/passstream/passstream/internal/host/proto"
)

const MaxFrame = 4 << 20
const MaxRecording = 32 << 20
const MaxPixels = 3840 * 2160
const History = 4

type Frame struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	ID     string `json:"id,omitempty"`
	At     int64  `json:"at,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
	MIME   string `json:"mime,omitempty"`
	Data   []byte `json:"data,omitempty"`
}

type Store struct {
	mu           sync.Mutex
	frames       []Frame
	open         bool
	status       string
	reason       string
	scaleMu      sync.Mutex
	scaled       map[scaleKey]Frame
	decodedID    string
	decoded      *image.NRGBA
	changed      chan struct{}
	version      uint64
	last         time.Time
	recording    []byte
	recordID     string
	complete     bool
	recordFailed bool
	durationMS   int64
}

func (s *Store) signal() {
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

// Close is deliberately unconditional: an encode begun before a gate closed
// must not repopulate the store after a resume.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open = false
	s.status, s.reason = "paused", ""
	s.frames = nil
	s.clearScale()
	s.recording, s.recordID, s.complete = nil, "", false
	s.version++
	s.signal()
}

// Prepared owns a validated PNG, so base64/header decoding stays outside the
// daemon's control lock. Only this package can construct its contents.
type Prepared struct{ frame Frame }

func Prepare(f Frame) (*Prepared, error) {
	if len(f.Data) == 0 || len(f.Data) > MaxFrame || len(f.ID) > 100 || f.ID == "" {
		return nil, proto.E("invalid_argument", "invalid frame")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(f.Data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 3840 || cfg.Height > 2160 || cfg.Width != f.Width || cfg.Height != f.Height {
		return nil, proto.E("invalid_argument", "invalid PNG dimensions")
	}
	f.Status, f.MIME = "frame", "image/png"
	f.Data = bytes.Clone(f.Data)
	return &Prepared{f}, nil
}
func (s *Store) Put(f Frame) error {
	p, e := Prepare(f)
	if e != nil {
		return e
	}
	return s.Commit(p)
}
func (s *Store) Commit(p *Prepared) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := p.frame
	if len(s.frames) > 0 && s.frames[len(s.frames)-1].ID == f.ID {
		return nil
	}
	if time.Since(s.last) < 450*time.Millisecond {
		return proto.E("rate_limited", "capture rate exceeded")
	}
	s.last, s.open, s.status, s.reason = time.Now(), true, "frame", ""
	if len(s.frames) == History {
		copy(s.frames, s.frames[1:])
		s.frames[len(s.frames)-1] = Frame{}
		s.frames = s.frames[:len(s.frames)-1]
	}
	s.frames = append(s.frames, f)
	s.clearScale()
	s.signal()
	return nil
}
func (s *Store) clearScale() { s.scaled = nil; s.decoded = nil; s.decodedID = "" }

// Allow enables owner recording independently of demand for the snapshot feed.
func (s *Store) Allow() { s.mu.Lock(); s.open = true; s.mu.Unlock() }
func (s *Store) Begin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == "idle" || s.status == "" {
		s.status = "waiting"
		s.signal()
	}
}

// Mark erases snapshot state without discarding an explicitly made recording.
func (s *Store) Mark(status, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == status && s.reason == reason && len(s.frames) == 0 {
		return
	}
	s.status, s.reason = status, reason
	s.frames = nil
	s.clearScale()
	s.version++
	s.signal()
}
func (s *Store) marker() Frame {
	status := s.status
	if status == "" {
		status = "paused"
	}
	if status == "frame" {
		status = "stale"
	}
	return Frame{Status: status, Reason: s.reason}
}

// AwaitFrame warms a cold, demand-driven snapshot publish without treating an
// earlier source-reset marker as a picture. Explicit privacy cancels the request
// separately at the daemon, before any confirmation can be accepted.
func (s *Store) AwaitFrame(ctx context.Context, wait time.Duration, width int) (Frame, error) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		s.mu.Lock()
		ready := s.status == "frame" || s.status == "too_large" || s.status == "stale"
		if s.changed == nil {
			s.signal()
		}
		ch := s.changed
		s.mu.Unlock()
		if ready {
			return s.Frame(ctx, "", 0, width)
		}
		select {
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		case <-timer.C:
			return Frame{Status: "stale", Reason: "capture_pending"}, nil
		case <-ch:
		}
	}
}
func (s *Store) Frame(ctx context.Context, after string, wait time.Duration, width int) (Frame, error) {
	s.mu.Lock()
	version := s.version
	if s.status == "waiting" && wait == 0 {
		wait = 3 * time.Second
	}
	s.mu.Unlock()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		if s.version != version || s.status != "frame" && s.status != "waiting" {
			f := s.marker()
			s.mu.Unlock()
			return f, nil
		}
		if len(s.frames) > 0 {
			f := s.frames[len(s.frames)-1]
			if f.ID != after {
				s.mu.Unlock()
				if width > 0 && width < f.Width {
					return s.scale(ctx, f, width, version)
				}
				f.Data = bytes.Clone(f.Data)
				return f, nil
			}
		}
		if s.changed == nil {
			s.signal()
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		case <-deadline.C:
			s.mu.Lock()
			f := Frame{Status: "unchanged", ID: after}
			if s.status == "waiting" {
				f = Frame{Status: "stale", Reason: "capture_pending"}
			}
			s.mu.Unlock()
			return f, nil
		case <-ch:
		}
	}
}

func (s *Store) Preview(ctx context.Context, f Frame, width int) (Frame, error) {
	if width >= f.Width {
		return f, nil
	}
	s.mu.Lock()
	v := s.version
	s.mu.Unlock()
	return s.scale(ctx, f, width, v)
}

type scaleKey struct {
	id    string
	width int
}

func (s *Store) scale(ctx context.Context, f Frame, width int, version uint64) (Frame, error) {
	// Decode/scale/encode is single-flight, including different requested widths.
	s.scaleMu.Lock()
	defer s.scaleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Frame{}, err
	}
	key := scaleKey{f.ID, width}
	s.mu.Lock()
	if s.version != version {
		m := s.marker()
		s.mu.Unlock()
		return m, nil
	}
	if cached, ok := s.scaled[key]; ok {
		s.mu.Unlock()
		cached.Data = bytes.Clone(cached.Data)
		return cached, nil
	}
	pixels := s.decoded
	if s.decodedID != f.ID {
		pixels = nil
	}
	s.mu.Unlock()
	if pixels == nil {
		src, err := png.Decode(bytes.NewReader(f.Data))
		if err != nil {
			return Frame{}, proto.E("internal", "invalid retained PNG")
		}
		if n, ok := src.(*image.NRGBA); ok {
			pixels = n
		} else {
			pixels = image.NewNRGBA(src.Bounds())
			draw.Draw(pixels, pixels.Bounds(), src, src.Bounds().Min, draw.Src)
		}
	}
	height := max(1, f.Height*width/f.Width)
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	// Direct pixel access avoids image.At/Set interface calls and per-pixel boxes.
	for y := 0; y < height; y++ {
		sy := (y * f.Height / height) * pixels.Stride
		dy := y * dst.Stride
		for x := 0; x < width; x++ {
			sx := sy + (x*f.Width/width)*4
			copy(dst.Pix[dy+x*4:dy+x*4+4], pixels.Pix[sx:sx+4])
		}
	}
	var b bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&b, dst); err != nil {
		return Frame{}, err
	}
	f.Data, f.Width, f.Height = b.Bytes(), width, height
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != version {
		return s.marker(), nil
	}
	// A newer picture may have arrived while work was in progress. Never retain
	// decoded pixels or derived PNGs from an evicted/currently idle picture.
	if len(s.frames) > 0 && s.frames[len(s.frames)-1].ID == f.ID {
		s.decodedID, s.decoded = f.ID, pixels
		if s.scaled == nil {
			s.scaled = map[scaleKey]Frame{}
		}
		if len(s.scaled) >= 4 {
			for k := range s.scaled {
				delete(s.scaled, k)
				break
			}
		}
		s.scaled[key] = f
	}
	f.Data = bytes.Clone(f.Data)
	return f, nil
}

// Record accepts bounded chunks from the current authenticated sender only.
func (s *Store) Record(id string, part []byte, final bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		return proto.E("paused", "capture is paused")
	}
	if id == "" || len(id) > 100 || len(part) > 512<<10 {
		return proto.E("invalid_argument", "invalid recording chunk")
	}
	if id != s.recordID {
		s.recording, s.recordID, s.complete = nil, id, false
		s.durationMS = 0
		s.recordFailed = false
	}
	if s.complete || s.recordFailed || len(s.recording)+len(part) > MaxRecording {
		s.recording = nil
		s.recordFailed = true
		return proto.E("payload_too_large", "recording limit exceeded")
	}
	s.recording = append(s.recording, part...)
	s.complete = final
	return nil
}

func (s *Store) Recording() ([]byte, error) { b, _, e := s.RecordingDetails(); return b, e }
func (s *Store) SetRecordingDuration(id string, ms int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordID == id && ms >= 0 && ms <= 120000 {
		s.durationMS = ms
	}
}
func (s *Store) RecordingDetails() ([]byte, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open {
		return nil, 0, proto.E("paused", "capture is paused")
	}
	if !s.complete || len(s.recording) == 0 {
		return nil, 0, proto.E("not_found", "no completed recording")
	}
	return bytes.Clone(s.recording), s.durationMS, nil
}

func Decode(s string, limit int) ([]byte, error) {
	if len(s) > base64.StdEncoding.EncodedLen(limit) {
		return nil, proto.E("payload_too_large", "capture limit exceeded")
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if len(b) > limit {
		return nil, proto.E("payload_too_large", "capture limit exceeded")
	}
	if err != nil {
		return nil, proto.E("invalid_argument", "invalid capture encoding")
	}
	return b, nil
}
