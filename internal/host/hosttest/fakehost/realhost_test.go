package main

import (
	"testing"
	"time"
)

func TestRealStatusPreservesPortModeExceptExplicitFixtures(t *testing.T) {
	for _, fixture := range []string{"ordinary", "delay", "foreign", "socket-unit"} {
		t.Run(fixture, func(t *testing.T) {
			h := &harness{mode: "socket_activated"}
			switch fixture {
			case "foreign":
				h.cfg.Proc = "foreign-table"
			case "socket-unit":
				h.cfg.StatusDelay = time.Second
			case "delay":
				h.cfg.StatusDelay = time.Second
				h.mode = "self_bound"
			}
			status := map[string]any{"port_mode": "self_bound", "port_owner": "ok"}
			h.fixtureStatus(status)
			want := "self_bound"
			if fixture == "foreign" || fixture == "socket-unit" {
				want = "socket_activated"
			}
			if status["port_mode"] != want {
				t.Fatal(status)
			}
		})
	}
}
