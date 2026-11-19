package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s", "host-state.json")
	r := &Record{V: Version, StreamID: "AAAAAAAAAAAAAAAAAAAAAA", Attachment: &Attachment{KeyID: "k", Key: "v"}}
	if err := Save(p, r); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	got, err := Load(p)
	if err != nil || got.Attachment.Key != "v" {
		t.Fatalf("load %v %v", got, err)
	}
	os.Chmod(p, 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("a readable record must be refused")
	}
	os.Remove(p)
	os.Symlink(filepath.Join(dir, "elsewhere"), p)
	if _, err := Load(p); err == nil {
		t.Fatal("a symlinked record must be refused")
	}
	os.Remove(p)
	if r, err := Load(p); r != nil || err != nil {
		t.Fatal("no record is (nil, nil)")
	}
}

// A record saved before safe mode existed restores it on; a saved off stays off.
func TestLoadDefaultsSafeModeOn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "host-state.json")
	if err := os.WriteFile(p, []byte(`{"v":1,"stream_id":"AAAAAAAAAAAAAAAAAAAAAA"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := Load(p); err != nil || !r.Safe {
		t.Fatalf("legacy record: %+v %v", r, err)
	}
	if err := Save(p, &Record{V: Version, StreamID: "AAAAAAAAAAAAAAAAAAAAAA", Safe: false}); err != nil {
		t.Fatal(err)
	}
	if r, err := Load(p); err != nil || r.Safe {
		t.Fatalf("saved off: %+v %v", r, err)
	}
}
