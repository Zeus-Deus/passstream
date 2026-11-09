// Package state is the daemon's recovery record, host-state.json
// (HOST-CONTROL §5.3): 0600, written atomically with fsync, and holding no
// PIN, link or link secret.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/passstream/passstream/internal/host/proto"
)

// Version of the record format.
const Version = 1

// Attachment is the stream's attachment key and who holds it.
type Attachment struct {
	KeyID        string `json:"key_id"`
	Key          string `json:"key"`
	PageInstance string `json:"page_instance,omitempty"`
	AttachedAt   int64  `json:"attached_at,omitempty"`
}

// Browser identifies the supervised sender browser (HOST-CONTROL §5.9).
type Browser struct {
	Kind      string `json:"kind"` // "systemd" | "process_group"
	Unit      string `json:"unit,omitempty"`
	PID       int    `json:"pid"`
	StartTime uint64 `json:"start_time"`
	PGID      int    `json:"pgid"`
}

// Record is host-state.json.
type Record struct {
	V            int             `json:"v"`
	StreamID     string          `json:"stream_id"`
	ServerURL    string          `json:"server_url"`
	PublishToken string          `json:"publish_token"`
	ViewLinkBase string          `json:"view_link_base"`
	Label        *string         `json:"label"`
	Mode         string          `json:"mode"`
	Policy       proto.PinPolicy `json:"pin_policy"`
	Knock        bool            `json:"knock"`
	Latches      proto.Latches   `json:"latches"`
	Attachment   *Attachment     `json:"attachment"`
	Browser      *Browser        `json:"browser"`
	StartedAt    int64           `json:"started_at"`
	LiveAt       *int64          `json:"live_at"`
}

// Save writes the record atomically: a 0600 temp file, fsync, rename, and
// fsync of the directory, so the record is durable when Save returns.
func Save(path string, r *Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Load reads the record; (nil, nil) when there is none. A record that isn't
// a private regular file of ours is refused.
func Load(path string) (*Record, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, nil
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || int(st.Uid) != os.Geteuid() || st.Mode&0o077 != 0 {
		return nil, fmt.Errorf("%s is not a private file", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.V != Version || !proto.IsId22(r.StreamID) {
		return nil, fmt.Errorf("%s: unknown record", path)
	}
	return &r, nil
}

// Remove deletes the record (the stream ended).
func Remove(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err == nil {
		syncDir(filepath.Dir(path))
	}
	return err
}
