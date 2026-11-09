package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Regression tests for review round 1 (GPT-6.1-Sol, request changes).

type fakeClipboard struct {
	sensitiveOK bool
	copyErr     error
	copies      []string // "value|sensitive"
	scheduled   []string
	scheduleErr error
	after       time.Duration
}

func (f *fakeClipboard) SensitiveSupported() bool { return f.sensitiveOK }
func (f *fakeClipboard) Copy(v string, s bool, keep time.Duration) (bool, error) {
	f.scheduled, f.after = append(f.scheduled, v), keep
	if s {
		v += "|sensitive"
	}
	f.copies = append(f.copies, v)
	return f.copyErr == nil && f.scheduleErr == nil, f.copyErr
}

func copyWith(t *testing.T, cb *fakeClipboard, json bool) (error, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := &Env{Stdout: &out, Stderr: &errb, Clipboard: cb}
	err := copyValue(&opts{json: json}, env, "pin", "4821")
	return err, out.String(), errb.String()
}

// TestCopyIsSensitiveAndScheduledForClearing (finding 8): copies use
// --sensitive and schedule the 30 s conditional clear.
func TestCopyIsSensitiveAndScheduledForClearing(t *testing.T) {
	cb := &fakeClipboard{sensitiveOK: true}
	err, out, errs := copyWith(t, cb, true)
	if err != nil || errs != "" {
		t.Fatalf("%v %q", err, errs)
	}
	if len(cb.copies) != 1 || cb.copies[0] != "4821|sensitive" {
		t.Fatalf("copies %v", cb.copies)
	}
	if len(cb.scheduled) != 1 || cb.scheduled[0] != "4821" || cb.after != 30*time.Second {
		t.Fatalf("clear %v after %v", cb.scheduled, cb.after)
	}
	var res map[string]any
	json.Unmarshal([]byte(out), &res)
	if res["sensitive"] != true || res["clears_in_s"] != 30.0 || strings.Contains(out, "4821") {
		t.Fatalf("output %s", out)
	}
}

// TestCopyReportsMissingSensitiveSupport (finding 8): an old wl-copy isn't
// a silent fallback; and a failed sensitive copy isn't retried without it.
func TestCopyReportsMissingSensitiveSupport(t *testing.T) {
	cb := &fakeClipboard{sensitiveOK: false}
	err, out, errs := copyWith(t, cb, true)
	if err != nil || !strings.Contains(errs, "can't mark the copy sensitive") || !strings.Contains(out, `"sensitive":false`) {
		t.Fatalf("%v out=%q err=%q", err, out, errs)
	}
	cb = &fakeClipboard{sensitiveOK: true, copyErr: errors.New("boom")}
	if err, _, _ := copyWith(t, cb, false); err == nil || len(cb.copies) != 1 {
		t.Fatalf("failed copy: %v copies=%v scheduled=%v", err, cb.copies, cb.scheduled)
	}
	cb = &fakeClipboard{sensitiveOK: true, scheduleErr: errors.New("no")}
	if err, out, errs := copyWith(t, cb, true); err != nil || !strings.Contains(errs, "can't schedule the clipboard clear") || !strings.Contains(out, `"clears_in_s":null`) {
		t.Fatalf("unscheduled clear: %v %q %q", err, out, errs)
	}
}

// TestJSONUsageErrors (orchestrator decision): with --json, usage errors
// are {"error": Error} too, also when the flags themselves don't parse.
func TestJSONUsageErrors(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for _, args := range [][]string{
		{"share", "--json", "--pin", "4821"}, {"frobnicate", "--json"}, {"kick", "x", "--json"},
		{"status", "--json", "--bogus"}, {"ban", "c_x", "--json", "--action-id", "a b"},
	} {
		r := runCLI(t, "", args...)
		var e struct {
			Error struct{ Code, Message string }
		}
		if r.code != 2 || json.Unmarshal([]byte(r.stderr), &e) != nil || e.Error.Code != "invalid_argument" || e.Error.Message == "" {
			t.Errorf("%v: exit %d stderr %q", args, r.code, r.stderr)
		}
		if strings.Contains(r.stderr, "4821") {
			t.Errorf("%v echoed the value", args)
		}
	}
	if r := runCLI(t, "", "frobnicate"); r.code != 2 || strings.HasPrefix(r.stderr, "{") {
		t.Errorf("plain usage error: %+v", r)
	}
}

// TestBanSendsOneActionIDAcrossRetries (finding 7): the CLI makes one
// action id per ban (or takes --action-id) and retries a transient
// failure with the same id.
func TestBanSendsOneActionIDAcrossRetries(t *testing.T) {
	srv := daemonFor(t, nil)
	if r := runCLI(t, "", "share", "--json"); r.code != 0 {
		t.Fatalf("share: %+v", r)
	}
	runCLI(t, "", "ban", "c_AAAAAAAAAAAAAAAAAAAAAA")
	runCLI(t, "", "ban", "c_AAAAAAAAAAAAAAAAAAAAAA", "--action-id", "agent-step-4")
	runCLI(t, "", "ban", "c_AAAAAAAAAAAAAAAAAAAAAA", "--action-id", "agent-step-4")
	// Two transient failures, then success: three attempts, one id.
	srv.FailCommands(2)
	runCLI(t, "", "ban", "c_AAAAAAAAAAAAAAAAAAAAAA")
	ids := srv.CommandIDs()
	if len(ids) != 6 || !strings.HasPrefix(ids[0], "ban-") || ids[1] != "agent-step-4" || ids[2] != "agent-step-4" ||
		!strings.HasPrefix(ids[3], "ban-") || ids[3] == ids[0] || ids[4] != ids[3] || ids[5] != ids[3] {
		t.Fatalf("command ids %v", ids)
	}
}
