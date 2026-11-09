package cli

import (
	"strings"
	"testing"
)

func TestCaptureCLIUsageBounds(t *testing.T) {
	for _, args := range [][]string{{"frame", "--wait", "31"}, {"frame", "--json", "--out", "-"}, {"record", "start", "--duration", "121"}, {"record", "stop", "--duration", "2"}, {"publish", "recording", "passpage"}, {"publish", "snapshot", "passfile", "--server", "https://arbitrary.example"}, {"capture-auth", "unexpected"}} {
		r := runCLI(t, "", append(args, "--json")...)
		if r.code != 2 || !strings.Contains(r.stderr, `"invalid_argument"`) {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

func TestCaptureCLIJSONPausedAndCredential(t *testing.T) {
	daemonFor(t, nil)
	r := runCLI(t, "", "share", "--no-wait", "--json")
	if r.code != 0 {
		t.Fatal(r)
	}
	r = runCLI(t, "", "frame", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"status":"paused"`) || strings.Contains(r.stdout, `"data"`) {
		t.Fatal(r)
	}
	r = runCLI(t, "", "capture-auth", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"expires_in_s":300`) || !strings.Contains(r.stdout, `"token"`) {
		t.Fatal(r)
	}
}
