package sfusim

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestObservedSourceKeepsColdWarmAndArrivalBoundariesDistinct(t *testing.T) {
	trace := ObservedSource{Version: 1, Events: []ObservedSourceEvent{
		{AtUS: 0, Boundary: "capture_input", Clock: "browser", Identity: "draw:0"},
		{AtUS: 1000, Boundary: "encoded", Clock: "browser", Identity: "rtp:1", Key: true, EncodedBytes: 235000, ProtectedBytes: 235018},
		{AtUS: 15000, Boundary: "arrival", Clock: "server", AssembledWire: 251000},
		{AtUS: 409000, Boundary: "encoded", Clock: "browser", EncodedBytes: 1200},
		{AtUS: 11000000, Boundary: "encoded", Clock: "browser", Key: true, EncodedBytes: 235000, ProtectedBytes: 235018},
		{AtUS: 11060000, Boundary: "encoded", Clock: "browser", EncodedBytes: 1200},
	}}
	run := func(viewerDelay time.Duration) []ObservedSourceEvent {
		s := newSim(1)
		var got []ObservedSourceEvent
		if err := ReplayObservedSource(s, trace, func(e ObservedSourceEvent) {
			if s.Now() != time.Duration(e.AtUS)*time.Microsecond {
				t.Fatal("boundary moved", s.Now(), e)
			}
			got = append(got, e)
		}); err != nil {
			t.Fatal(err)
		}
		// Viewer observation work cannot move a source event or become a
		// source-arrival sample; that would fit source timing to freezes.
		s.At(viewerDelay, func() {})
		s.Run(12 * time.Second)
		return got
	}
	if a, b := run(time.Second), run(8*time.Second); !reflect.DeepEqual(a, trace.Events) || !reflect.DeepEqual(a, b) {
		t.Fatal("source times/units/identity changed with viewer delay", a, b)
	}
}

func TestObservedSourceRejectsInventedUnitsAndInvalidTimeAtomically(t *testing.T) {
	valid := ObservedSourceEvent{Boundary: "arrival", Clock: "server", AtUS: 1000}
	for _, bad := range []ObservedSourceEvent{
		{Boundary: "encoded", Clock: "browser", EncodedBytes: 235000, AssembledWire: 251000},
		{Boundary: "capture_input", Clock: "browser", EncodedBytes: 1},
		{Boundary: "arrival", Clock: "server", EncodedBytes: 1},
		{Boundary: "arrival", Clock: "server", AtUS: -1},
		{Boundary: "arrival", AtUS: 1000},
		{Boundary: "rendered", Clock: "browser"},
	} {
		s := newSim(1)
		if err := ReplayObservedSource(s, ObservedSource{Version: 1, Events: []ObservedSourceEvent{valid, bad}}, func(ObservedSourceEvent) {}); err == nil || len(s.heap) != 0 {
			t.Fatal("invalid evidence scheduled events", bad, err)
		}
	}
	s := newSim(1)
	s.Run(time.Second)
	if err := ReplayObservedSource(s, ObservedSource{Version: 1, Events: []ObservedSourceEvent{valid}}, func(ObservedSourceEvent) {}); err == nil {
		t.Fatal("past evidence was clamped to now")
	}
}

// Exercise an exported diagnostic artifact on exact observed boundary times.
// This test asserts replay fidelity; it does not score controller/gate success.
func TestObservedSourceDiagnostic(t *testing.T) {
	path := os.Getenv("SFUSIM_OBSERVED_SOURCE")
	if path == "" {
		t.Skip("set SFUSIM_OBSERVED_SOURCE to a version1 observed-source JSON")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var trace ObservedSource
	if err := json.Unmarshal(data, &trace); err != nil {
		t.Fatal(err)
	}
	s := newSim(1)
	counts := map[string]int{}
	until := time.Duration(0)
	for _, e := range trace.Events {
		until = max(until, time.Duration(e.AtUS)*time.Microsecond)
	}
	if err := ReplayObservedSource(s, trace, func(e ObservedSourceEvent) {
		if s.Now() != time.Duration(e.AtUS)*time.Microsecond {
			t.Fatal("event timing drift")
		}
		counts[e.Boundary]++
	}); err != nil {
		t.Fatal(err)
	}
	s.Run(until)
	if len(trace.Events) == 0 {
		t.Fatal("empty observed source")
	}
	t.Logf("exact source events replayed: %v; no controller verdict", counts)
}
