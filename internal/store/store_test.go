package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCapabilitiesAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	key, e := os.ReadFile(filepath.Join(dir, "owner-key"))
	if e != nil {
		t.Fatal(e)
	}
	info, _ := os.Stat(filepath.Join(dir, "owner-key"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("unguarded owner key")
	}
	if !s.Owner(string(key)) || s.Owner("invalid") {
		t.Fatal("owner auth")
	}
	v, view, pub, e := s.Create()
	if e != nil {
		t.Fatal(e)
	}
	if id, e := s.Admit("", "view", view); e != nil || id != v.ID {
		t.Fatal("view admission", e)
	}
	if _, e = s.Admit(v.ID, "publish", view); e == nil {
		t.Fatal("role confusion")
	}
	if _, e = s.Admit(v.ID, "publish", pub); e != nil {
		t.Fatal(e)
	}
	token, e := s.Session(v.ID, "view")
	if e != nil {
		t.Fatal(e)
	}
	s.DB.Close()
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	if _, e = s.Admit(v.ID, "view", token); e != nil {
		t.Fatal("session lost after restart", e)
	}
	if e = s.Delete(v.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Admit(v.ID, "view", token); e == nil {
		t.Fatal("deleted stream admitted")
	}
}
