// Package store persists hashes of credentials and bounded, allowlisted metadata.
package store

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/passstream/passstream/internal/hostview"
	_ "modernc.org/sqlite"
)

type Store struct {
	DB        *sql.DB
	banSecret []byte
}
type Declared struct {
	Pin   hostview.PinPolicy `json:"pin"`
	Knock bool               `json:"knock"`
}
type Sender struct {
	Online     bool   `json:"online"`
	Kind       string `json:"kind"`
	LastSeenAt *int64 `json:"last_seen_at"`
}
type Stream struct {
	ID         string     `json:"id"`
	CreatedAt  int64      `json:"created_at"`
	Label      *string    `json:"label"`
	Phase      string     `json:"phase"`
	EndedAt    *int64     `json:"ended_at"`
	EndReason  *string    `json:"end_reason"`
	ExpiresAt  *int64     `json:"expires_at"`
	E2EE       bool       `json:"e2ee"`
	Sender     Sender     `json:"sender"`
	Declared   Declared   `json:"declared"`
	Effective  *Effective `json:"effective"`
	Watching   uint64     `json:"watching"`
	Knocking   uint64     `json:"knocking"`
	MaxViewers uint64     `json:"-"`
}
type Defaults struct {
	Pin        hostview.PinPolicy `json:"pin"`
	Knock      bool               `json:"knock"`
	MaxViewers uint64             `json:"max_viewers"`
	ExpiresInS *uint64            `json:"expires_in_s"`
}
type Retention struct {
	KeepIP bool `json:"keep_ip"`
}
type Settings struct {
	Defaults  Defaults  `json:"defaults"`
	Retention Retention `json:"retention"`
}
type Key struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Scope      string `json:"scope"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  *int64 `json:"expires_at"`
	LastUsedAt *int64 `json:"last_used_at"`
}
type Actor struct {
	Kind  string  `json:"kind"`
	KeyID *string `json:"key_id"`
}
type AuditEntry struct {
	ID             string            `json:"id"`
	At             int64             `json:"at"`
	StreamID       *string           `json:"stream_id"`
	Actor          Actor             `json:"actor"`
	Op             string            `json:"op"`
	Target         map[string]string `json:"target"`
	Result         string            `json:"result"`
	ErrorCode      *string           `json:"error_code"`
	IdempotencyKey *string           `json:"idempotency_key"`
}

func Token() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func ID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func Hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func Open(dir string) (*Store, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", filepath.Join(dir, "passstream.db"))
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	secretPath := filepath.Join(dir, "ban-secret")
	secret, err := os.ReadFile(secretPath)
	if errors.Is(err, os.ErrNotExist) {
		secret = []byte(Token())
		err = os.WriteFile(secretPath, secret, 0600)
	}
	if err != nil || len(secret) < 32 {
		db.Close()
		return nil, errors.New("invalid ban secret")
	}
	if err = os.Chmod(secretPath, 0600); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db, banSecret: secret}
	_, e = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS api_keys(id TEXT PRIMARY KEY,hash TEXT UNIQUE NOT NULL,body BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS host_streams(id TEXT PRIMARY KEY,view_hash TEXT UNIQUE NOT NULL,publish_hash TEXT UNIQUE NOT NULL,body BLOB NOT NULL,max_viewers INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS transport_sessions(hash TEXT PRIMARY KEY,stream TEXT NOT NULL,role TEXT NOT NULL,expires INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS dashboard_sessions(hash TEXT PRIMARY KEY,key_id TEXT NOT NULL,scope TEXT NOT NULL,expires INTEGER NOT NULL,idle INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS metadata(id TEXT PRIMARY KEY,body BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS records(seq INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,stream TEXT NOT NULL,at INTEGER NOT NULL,body BLOB NOT NULL);
 CREATE INDEX IF NOT EXISTS records_lookup ON records(kind,stream,at);
 CREATE INDEX IF NOT EXISTS records_retention ON records(kind,at);
 CREATE INDEX IF NOT EXISTS streams_page ON host_streams(json_extract(body,'$.created_at') DESC,id DESC);
 CREATE TABLE IF NOT EXISTS bans(stream TEXT NOT NULL,ip_hash TEXT NOT NULL,PRIMARY KEY(stream,ip_hash));`)
	if e != nil {
		db.Close()
		return nil, e
	}
	var n int
	if e = db.QueryRow("SELECT count(*) FROM api_keys").Scan(&n); e != nil {
		db.Close()
		return nil, e
	}
	if n == 0 {
		p := filepath.Join(dir, "owner-key")
		b, e := os.ReadFile(p)
		if errors.Is(e, os.ErrNotExist) {
			b = []byte(Token())
			e = os.WriteFile(p, b, 0600)
		}
		if e == nil {
			e = os.Chmod(p, 0600)
		}
		if e != nil {
			db.Close()
			return nil, e
		}
		k := Key{ID: ID(), Label: "Initial owner", Scope: "owner", CreatedAt: time.Now().UnixMilli()}
		body, _ := json.Marshal(k)
		if _, e = db.Exec("INSERT INTO api_keys VALUES(?,?,?)", k.ID, Hash(string(b)), body); e != nil {
			db.Close()
			return nil, e
		}
	}
	return s, nil
}
func (s *Store) Put(id string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec("INSERT INTO metadata VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body", id, b)
	return e
}
func (s *Store) Read(id string, v any) error {
	var b []byte
	if e := s.DB.QueryRow("SELECT body FROM metadata WHERE id=?", id).Scan(&b); e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func DefaultSettings() Settings {
	n := uint64(4)
	return Settings{Defaults: Defaults{Pin: hostview.PinPolicy{Kind: "digits", Length: &n}, MaxViewers: 100}}
}
func (s *Store) Settings() (Settings, error) {
	v := DefaultSettings()
	e := s.Read("settings", &v)
	if errors.Is(e, sql.ErrNoRows) {
		e = nil
	}
	return v, e
}
func (s *Store) Owner(key string) bool { k, e := s.AuthKey(key); return e == nil && k.Scope == "owner" }
func (s *Store) AuthKey(key string) (Key, error) {
	var k Key
	var b []byte
	e := s.DB.QueryRow("SELECT body FROM api_keys WHERE hash=?", Hash(key)).Scan(&b)
	if e != nil {
		return k, e
	}
	if e = json.Unmarshal(b, &k); e != nil {
		return k, e
	}
	now := time.Now().UnixMilli()
	if k.ExpiresAt != nil && *k.ExpiresAt <= now {
		return k, sql.ErrNoRows
	}
	k.LastUsedAt = &now
	b, _ = json.Marshal(k)
	_, e = s.DB.Exec("UPDATE api_keys SET body=? WHERE id=?", b, k.ID)
	return k, e
}
func (s *Store) Keys() ([]Key, error) {
	rows, e := s.DB.Query("SELECT body FROM api_keys ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	v := []Key{}
	for rows.Next() {
		var b []byte
		var k Key
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &k); e != nil {
			return nil, e
		}
		v = append(v, k)
	}
	return v, rows.Err()
}
func (s *Store) NewKey(label, scope string, expires *int64) (Key, string, error) {
	k := Key{ID: ID(), Label: label, Scope: scope, CreatedAt: time.Now().UnixMilli(), ExpiresAt: expires}
	token := Token()
	b, _ := json.Marshal(k)
	_, e := s.DB.Exec("INSERT INTO api_keys VALUES(?,?,?)", k.ID, Hash(token), b)
	return k, token, e
}

var ErrLastOwner = errors.New("last owner")

func (s *Store) DeleteKey(id string) error {
	// Serialize last-owner decisions in SQLite, including simultaneous deletes.
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.Query("SELECT body FROM api_keys")
	if e != nil {
		return e
	}
	owners := 0
	found := false
	owner := false
	for rows.Next() {
		var b []byte
		var k Key
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal(b, &k); e != nil {
			rows.Close()
			return e
		}
		if k.Scope == "owner" && (k.ExpiresAt == nil || *k.ExpiresAt > time.Now().UnixMilli()) {
			owners++
		}
		if k.ID == id {
			found = true
			owner = k.Scope == "owner"
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if !found {
		return sql.ErrNoRows
	}
	if owner && owners <= 1 {
		return ErrLastOwner
	}
	if _, e = tx.Exec("DELETE FROM api_keys WHERE id=?", id); e != nil {
		return e
	}
	if _, e = tx.Exec("DELETE FROM dashboard_sessions WHERE key_id=?", id); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Create() (Stream, string, string, error) {
	cfg, e := s.Settings()
	if e != nil {
		return Stream{}, "", "", e
	}
	return s.CreateStream(Stream{Phase: "pending", E2EE: true, Sender: Sender{Kind: "browser"}, Declared: Declared{cfg.Defaults.Pin, cfg.Defaults.Knock}, MaxViewers: cfg.Defaults.MaxViewers})
}
func (s *Store) CreateStream(v Stream) (Stream, string, string, error) {
	v.ID = ID()
	v.CreatedAt = time.Now().UnixMilli()
	view, pub := Token(), Token()
	b, _ := json.Marshal(v)
	_, e := s.DB.Exec("INSERT INTO host_streams VALUES(?,?,?,?,?)", v.ID, Hash(view), Hash(pub), b, v.MaxViewers)
	return v, view, pub, e
}
func (s *Store) SaveStream(v Stream) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec("UPDATE host_streams SET body=?,max_viewers=? WHERE id=?", b, v.MaxViewers, v.ID)
	return e
}
func (s *Store) Get(id string) (Stream, error) {
	var v Stream
	var b []byte
	e := s.DB.QueryRow("SELECT body,max_viewers FROM host_streams WHERE id=?", id).Scan(&b, &v.MaxViewers)
	if e == nil {
		e = json.Unmarshal(b, &v)
	}
	return v, e
}
func (s *Store) Delete(id string) error {
	v, e := s.Get(id)
	if e != nil {
		return e
	}
	if v.EndedAt == nil {
		n := time.Now().UnixMilli()
		reason := "deleted"
		v.EndedAt = &n
		v.EndReason = &reason
		v.Phase = "ended"
		if e = s.SaveStream(v); e != nil {
			return e
		}
	}
	_, e = s.DB.Exec("DELETE FROM bans WHERE stream=?", id)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec("DELETE FROM transport_sessions WHERE stream=?", id)
	return e
}
func (s *Store) Admit(id, role, credential string) (string, error) {
	var stream string
	var e error
	if role == "publish" {
		e = s.DB.QueryRow("SELECT id FROM host_streams WHERE id=? AND publish_hash=?", id, Hash(credential)).Scan(&stream)
	} else if role == "view" {
		e = s.DB.QueryRow("SELECT id FROM host_streams WHERE view_hash=?", Hash(credential)).Scan(&stream)
	} else {
		return "", errors.New("invalid role")
	}
	if e != nil {
		e = s.DB.QueryRow("SELECT stream FROM transport_sessions WHERE hash=? AND role=? AND expires>? AND (?='' OR stream=?)", Hash(credential), role, time.Now().UnixMilli(), id, id).Scan(&stream)
	}
	if e != nil {
		return "", e
	}
	v, e := s.Get(stream)
	if e == nil && (v.EndedAt != nil || v.ExpiresAt != nil && *v.ExpiresAt <= time.Now().UnixMilli()) {
		return stream, ErrEnded
	}
	return stream, e
}

var ErrEnded = errors.New("stream ended")

func (s *Store) Session(id, role string) (string, error) {
	token := Token()
	_, e := s.DB.Exec("INSERT INTO transport_sessions VALUES(?,?,?,?)", Hash(token), id, role, time.Now().Add(24*time.Hour).UnixMilli())
	return token, e
}
func (s *Store) Login(k Key) (string, int64, error) {
	token := Token()
	now := time.Now()
	expires := now.Add(12 * time.Hour).UnixMilli()
	scope := k.Scope
	if scope == "owner" {
		scope = "streams"
	}
	_, e := s.DB.Exec("INSERT INTO dashboard_sessions VALUES(?,?,?,?,?)", Hash(token), k.ID, scope, expires, now.Add(time.Hour).UnixMilli())
	return token, expires, e
}
func (s *Store) AuthSession(token string) (Key, int64, error) {
	var k Key
	var expires int64
	now := time.Now()
	e := s.DB.QueryRow("SELECT d.key_id,d.scope,d.expires FROM dashboard_sessions d JOIN api_keys k ON k.id=d.key_id WHERE d.hash=? AND d.expires>? AND d.idle>? AND (json_extract(k.body,'$.expires_at') IS NULL OR json_extract(k.body,'$.expires_at')>?)", Hash(token), now.UnixMilli(), now.UnixMilli(), now.UnixMilli()).Scan(&k.ID, &k.Scope, &expires)
	if e == nil {
		_, e = s.DB.Exec("UPDATE dashboard_sessions SET idle=? WHERE hash=?", now.Add(time.Hour).UnixMilli(), Hash(token))
	}
	return k, expires, e
}
func (s *Store) Logout(token string) error {
	_, e := s.DB.Exec("DELETE FROM dashboard_sessions WHERE hash=?", Hash(token))
	return e
}
func (s *Store) Record(kind, stream string, at int64, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec("INSERT INTO records(kind,stream,at,body) VALUES(?,?,?,?)", kind, stream, at, b)
	return e
}

type Record struct {
	Seq  int64
	Body json.RawMessage
}

func (s *Store) Records(kind, stream string, since, cursor int64, limit int, desc bool) ([]Record, error) {
	cmp, order := ">", "ASC"
	if desc {
		cmp, order = "<", "DESC"
		if cursor == 0 {
			cursor = 1 << 62
		}
	}
	rows, e := s.DB.Query("SELECT seq,body FROM records WHERE kind=? AND (?='' OR stream=?) AND at>=? AND seq"+cmp+"? ORDER BY seq "+order+" LIMIT ?", kind, stream, stream, since, cursor, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	v := []Record{}
	for rows.Next() {
		var r Record
		if e = rows.Scan(&r.Seq, &r.Body); e != nil {
			return nil, e
		}
		v = append(v, r)
	}
	return v, rows.Err()
}
func (s *Store) Ban(stream, ip string) error {
	_, e := s.DB.Exec("INSERT OR IGNORE INTO bans VALUES(?,?)", stream, s.banHash(stream, ip))
	return e
}
func (s *Store) Banned(stream, ip string) bool {
	var n int
	return s.DB.QueryRow("SELECT 1 FROM bans WHERE stream=? AND ip_hash=?", stream, s.banHash(stream, ip)).Scan(&n) == nil
}
func (s *Store) Prune(now time.Time) error {
	_, e := s.DB.Exec("DELETE FROM transport_sessions WHERE expires<=?", now.UnixMilli())
	if e == nil {
		_, e = s.DB.Exec("DELETE FROM dashboard_sessions WHERE expires<=? OR idle<=?", now.UnixMilli(), now.UnixMilli())
	}
	return e
}

// IPKey groups IPv6 privacy addresses by /64; IPv4 stays per address.
func IPKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is6() {
		return netip.PrefixFrom(a, 64).Masked().String()
	}
	return a.String()
}
func (s *Store) banHash(stream, ip string) string {
	mac := hmac.New(sha256.New, s.banSecret)
	mac.Write([]byte(stream + "\x00" + IPKey(ip)))
	return hex.EncodeToString(mac.Sum(nil))
}
func (s *Store) ClearBans(id string) error {
	_, err := s.DB.Exec("DELETE FROM bans WHERE stream=?", id)
	return err
}
func (s *Store) PruneAudit(now time.Time, retention time.Duration) error {
	// Bound each maintenance pass even after a long downtime.
	_, err := s.DB.Exec("DELETE FROM records WHERE seq IN (SELECT seq FROM records WHERE kind='audit' AND at<? LIMIT 1000)", now.Add(-retention).UnixMilli())
	return err
}
func (s *Store) StreamPage(state string, offset int64, limit int) ([]Stream, error) {
	now := time.Now().UnixMilli()
	ended := "(json_extract(body,'$.ended_at') IS NOT NULL OR json_extract(body,'$.expires_at')<=?)"
	where := "1=1"
	args := []any{}
	if state == "live" {
		where = "NOT (json_extract(body,'$.ended_at') IS NOT NULL OR COALESCE(json_extract(body,'$.expires_at')<=?,0))"
		args = append(args, now)
	}
	if state == "ended" {
		where = ended
		args = append(args, now)
	}
	args = append(args, limit, offset)
	rows, err := s.DB.Query("SELECT body,max_viewers FROM host_streams WHERE "+where+" ORDER BY json_extract(body,'$.created_at') DESC,id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Stream{}
	for rows.Next() {
		var v Stream
		var b []byte
		if err = rows.Scan(&b, &v.MaxViewers); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
