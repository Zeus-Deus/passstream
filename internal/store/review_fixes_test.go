package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewBanHMACRetentionAndRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, _, _, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	ip := "2001:db8::1234"
	if err = s.Ban(v.ID, ip); err != nil {
		t.Fatal(err)
	}
	var h string
	if err = s.DB.QueryRow("SELECT ip_hash FROM bans WHERE stream=?", v.ID).Scan(&h); err != nil {
		t.Fatal(err)
	}
	if h == Hash(ip) || h == Hash(IPKey(ip)) {
		t.Fatal("unkeyed IP hash")
	}
	if h == s.banHash("other-stream", ip) {
		t.Fatal("ban hash links streams")
	}
	info, err := os.Stat(filepath.Join(dir, "ban-secret"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unguarded secret", err)
	}
	s.DB.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	if !s.Banned(v.ID, "2001:db8::ffff") || s.Banned(v.ID, "2001:db8:0:1::1") {
		t.Fatal("ban not stable or scoped")
	}
	if err = s.Delete(v.ID); err != nil {
		t.Fatal(err)
	}
	if s.Banned(v.ID, ip) {
		t.Fatal("ban outlived stream")
	}
	if IPKey("::ffff:192.0.2.1") != "192.0.2.1" || IPKey("192.0.2.1") == IPKey("192.0.2.2") {
		t.Fatal("IPv4 grouping")
	}
}

func TestReviewAuditRetention(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	now := time.Now()
	for _, r := range []struct {
		kind string
		at   time.Time
	}{{"audit", now.Add(-31 * 24 * time.Hour)}, {"audit", now.Add(-2 * time.Hour)}, {"audit", now}, {"history", now.Add(-60 * 24 * time.Hour)}} {
		if err = s.Record(r.kind, "", r.at.UnixMilli(), map[string]string{"test": "record"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.PruneAudit(now, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Records("audit", "", 0, 0, 100, false)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	if err = s.PruneAudit(now, time.Hour); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Records("audit", "", 0, 0, 100, false)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	rows, err = s.Records("history", "", 0, 0, 100, false)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
}

func TestReviewSQLStreamPageBounds(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	for i := 0; i < 7; i++ {
		if _, _, _, err = s.Create(); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.StreamPage("all", 0, 3)
	if err != nil || len(first) != 3 {
		t.Fatal(first, err)
	}
	second, err := s.StreamPage("all", 3, 3)
	if err != nil || len(second) != 3 {
		t.Fatal(second, err)
	}
	seen := map[string]bool{}
	for _, v := range append(first, second...) {
		if seen[v.ID] {
			t.Fatal("overlapping pages")
		}
		seen[v.ID] = true
	}
	last, err := s.StreamPage("all", 6, 3)
	if err != nil || len(last) != 1 {
		t.Fatal(last, err)
	}
}

func TestReviewDashboardRejectsExpiredIssuer(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	expired := time.Now().Add(-time.Second).UnixMilli()
	k, _, err := s.NewKey("expired issuer", "read", &expired)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := s.Login(k)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.AuthSession(token); err == nil {
		t.Fatal("session outlived issuer expiry")
	}
}
