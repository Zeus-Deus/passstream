package capture

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, id string) Frame {
	t.Helper()
	var b bytes.Buffer
	if e := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 16, 8))); e != nil {
		t.Fatal(e)
	}
	return Frame{ID: id, Width: 16, Height: 8, Data: b.Bytes()}
}

func TestInitiallyClosedAndPrivacyErasesArtifacts(t *testing.T) {
	var s Store
	f, _ := s.Frame(context.Background(), "", 0, 0)
	if f.Status != "paused" || f.Data != nil {
		t.Fatal(f)
	}
	if e := s.Put(fixture(t, "one")); e != nil {
		t.Fatal(e)
	}
	if e := s.Record("record", []byte("webm"), true); e != nil {
		t.Fatal(e)
	}
	s.Close()
	f, _ = s.Frame(context.Background(), "", time.Second, 0)
	if f.Status != "paused" || f.Data != nil || len(s.frames) != 0 {
		t.Fatal(f)
	}
	if _, e := s.Recording(); e == nil {
		t.Fatal("recording survived privacy")
	}
}

func TestIdenticalIDAndRateBound(t *testing.T) {
	var s Store
	f := fixture(t, "one")
	if e := s.Put(f); e != nil {
		t.Fatal(e)
	}
	if e := s.Put(f); e != nil {
		t.Fatal(e)
	}
	f.ID = "two"
	if e := s.Put(f); e == nil {
		t.Fatal("rate unbounded")
	}
	if len(s.frames) != 1 {
		t.Fatal(len(s.frames))
	}
}

func TestRetainsOnlyFourAndCopiesBytes(t *testing.T) {
	var s Store
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		s.last = time.Time{}
		f := fixture(t, id)
		if e := s.Put(f); e != nil {
			t.Fatal(e)
		}
		f.Data[0] = 0
	}
	if len(s.frames) != History || s.frames[0].ID != "3" {
		t.Fatal(s.frames)
	}
	f, _ := s.Frame(context.Background(), "", 0, 0)
	f.Data[0] = 0
	if s.frames[len(s.frames)-1].Data[0] == 0 {
		t.Fatal("caller mutated retained frame")
	}
}

func TestDownscaleAndUnchanged(t *testing.T) {
	var s Store
	_ = s.Put(fixture(t, "one"))
	f, e := s.Frame(context.Background(), "", 0, 8)
	if e != nil || f.Width != 8 || f.Height != 4 {
		t.Fatal(f, e)
	}
	c, e := png.DecodeConfig(bytes.NewReader(f.Data))
	if e != nil || c.Width != 8 || c.Height != 4 {
		t.Fatal(c, e)
	}
	f, _ = s.Frame(context.Background(), "one", 0, 0)
	if f.Status != "unchanged" || f.Data != nil {
		t.Fatal(f)
	}
}

func TestWaitWakesOnChangeOrPrivacy(t *testing.T) {
	for _, closeGate := range []bool{false, true} {
		var s Store
		_ = s.Put(fixture(t, "one"))
		done := make(chan Frame, 1)
		go func() { f, _ := s.Frame(context.Background(), "one", time.Second, 0); done <- f }()
		time.Sleep(10 * time.Millisecond)
		if closeGate {
			s.Close()
		} else {
			s.mu.Lock()
			s.last = time.Time{}
			s.mu.Unlock()
			_ = s.Put(fixture(t, "two"))
		}
		select {
		case f := <-done:
			if closeGate && f.Status != "paused" || !closeGate && f.ID != "two" {
				t.Fatal(f)
			}
		case <-time.After(time.Second):
			t.Fatal("waiter stuck")
		}
	}
}

func TestWaitCancellation(t *testing.T) {
	var s Store
	_ = s.Put(fixture(t, "one"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e := s.Frame(ctx, "one", time.Second, 0)
	if e == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestRejectMalformedAndOversizedFrames(t *testing.T) {
	for _, f := range []Frame{{ID: "x", Data: []byte("secret needle")}, {ID: "x", Data: make([]byte, MaxFrame+1)}, {ID: strings.Repeat("x", 101), Data: []byte("x")}} {
		var s Store
		if s.Put(f) == nil {
			t.Fatal("accepted invalid PNG")
		}
	}
	var s Store
	f := fixture(t, "x")
	f.Width++
	if s.Put(f) == nil {
		t.Fatal("accepted false dimensions")
	}
}

func TestRecordingBoundsAndFinality(t *testing.T) {
	var s Store
	_ = s.Put(fixture(t, "one"))
	for i := 0; i < 64; i++ {
		if e := s.Record("record", make([]byte, 512<<10), false); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.Record("record", []byte{1}, true); e == nil {
		t.Fatal("accepted oversized recording")
	}
	if e := s.Record("record", []byte{1}, true); e == nil {
		t.Fatal("accepted truncated recording after overflow")
	}
	if _, e := s.Recording(); e == nil {
		t.Fatal("returned oversized recording")
	}
	if e := s.Record("new", []byte("webm"), true); e != nil {
		t.Fatal(e)
	}
	b, e := s.Recording()
	if e != nil || string(b) != "webm" {
		t.Fatal(e)
	}
	b[0] = 0
	if s.recording[0] == 0 {
		t.Fatal("mutated recording")
	}
}

func TestDecodeBoundAndCanonicalEncoding(t *testing.T) {
	for _, v := range []string{"!!!!", "YR==", strings.Repeat("a", 100)} {
		if _, e := Decode(v, 4); e == nil {
			t.Fatal("accepted invalid encoding")
		}
	}
	b, e := Decode("YQ==", 4)
	if e != nil || string(b) != "a" {
		t.Fatal(e)
	}
}

func TestTooLargeMarkerReplacesPreviousPicture(t *testing.T) {
	var s Store
	if e := s.Put(fixture(t, "old")); e != nil {
		t.Fatal(e)
	}
	s.Mark("too_large", "capture_too_large")
	for _, after := range []string{"", "old"} {
		f, e := s.Frame(context.Background(), after, time.Second, 0)
		if e != nil || f.Status != "too_large" || f.Data != nil || f.ID != "" {
			t.Fatal(f, e)
		}
	}
}

func TestScaleSerializesAndCachesByPictureAndWidth(t *testing.T) {
	var s Store
	s.Put(fixture(t, "first"))
	s.scaleMu.Lock()
	done := make(chan Frame, 1)
	go func() { f, _ := s.Frame(context.Background(), "", 0, 8); done <- f }()
	select {
	case <-done:
		t.Fatal("scale bypassed serialization")
	case <-time.After(10 * time.Millisecond):
	}
	s.scaleMu.Unlock()
	first := <-done
	key := scaleKey{"first", 8}
	s.mu.Lock()
	cached := s.scaled[key]
	cached.Data = []byte("cache sentinel")
	s.scaled[key] = cached
	s.mu.Unlock()
	next, e := s.Frame(context.Background(), "", 0, 8)
	if e != nil || string(next.Data) != "cache sentinel" {
		t.Fatal("scale result was not cached", e)
	}
	first.Data[0] = 0
	s.last = time.Time{}
	s.Put(fixture(t, "second"))
	next, e = s.Frame(context.Background(), "", 0, 8)
	if e != nil || next.ID != "second" || string(next.Data) == "cache sentinel" {
		t.Fatal("reused a different picture's scale")
	}
	s.Mark("idle", "")
	if len(s.scaled) != 0 || s.decoded != nil {
		t.Fatal("idle feed retained decoded/derived pictures")
	}
}

func TestDirectPixelDownscalePreservesSampledColorsAndAlpha(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			source.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 51), G: uint8(y * 59), B: uint8((x + y) * 23), A: uint8(64 + x*20 + y*10)})
		}
	}
	var raw bytes.Buffer
	png.Encode(&raw, source)
	var s Store
	if err := s.Put(Frame{ID: "colors", Width: 4, Height: 4, Data: raw.Bytes()}); err != nil {
		t.Fatal(err)
	}
	frame, err := s.Frame(context.Background(), "", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	scaled, err := png.Decode(bytes.NewReader(frame.Data))
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			if got, want := color.NRGBAModel.Convert(scaled.At(x, y)), source.NRGBAAt(x*2, y*2); got != want {
				t.Fatalf("pixel %d,%d: %v != %v", x, y, got, want)
			}
		}
	}
}

func BenchmarkScale4K(b *testing.B) {
	im := image.NewNRGBA(image.Rect(0, 0, 3840, 2160))
	for i := 0; i < len(im.Pix); i += 4 {
		im.Pix[i] = byte(i / 128)
		im.Pix[i+1] = byte(i / 4096)
		im.Pix[i+3] = 255
	}
	var raw bytes.Buffer
	png.Encode(&raw, im)
	for _, cached := range []bool{false, true} {
		name := "cold"
		if cached {
			name = "cached"
		}
		b.Run(name, func(b *testing.B) {
			var s Store
			s.Put(Frame{ID: "4k", Width: 3840, Height: 2160, Data: raw.Bytes()})
			if cached {
				s.Frame(context.Background(), "", 0, 1280)
			}
			b.ResetTimer()
			for range b.N {
				if !cached {
					s.mu.Lock()
					s.clearScale()
					s.mu.Unlock()
				}
				if _, err := s.Frame(context.Background(), "", 0, 1280); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
