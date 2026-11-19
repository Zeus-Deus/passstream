package daemon

import "testing"

// Safe mode is on for every new share unless start says otherwise; both
// directions are valid on the local socket (CONTRACT §10).
func TestSafeModeStartArgAndCommand(t *testing.T) {
	if a, err := parseStart(map[string]any{}); err != nil || !a.safe {
		t.Fatalf("default: %+v %v", a, err)
	}
	if a, err := parseStart(map[string]any{"safe": false}); err != nil || a.safe {
		t.Fatalf("safe false: %+v %v", a, err)
	}
	if _, err := parseStart(map[string]any{"safe": "no"}); err == nil {
		t.Fatal("a non-boolean safe must be refused")
	}
	for _, c := range []struct {
		args map[string]any
		ok   bool
	}{
		{map[string]any{"on": true}, true},
		{map[string]any{"on": false}, true},
		{map[string]any{}, false},
		{map[string]any{"on": "yes"}, false},
		{map[string]any{"on": true, "extra": 1}, false},
	} {
		if err := validateCommand("safe", c.args); (err == nil) != c.ok {
			t.Errorf("safe %v: %v", c.args, err)
		}
	}
}
