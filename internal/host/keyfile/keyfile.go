// Package keyfile reads the owner API key with the PassPage guards
// (HOST-CONTROL §5.1): one open with O_NOFOLLOW|O_NONBLOCK, a regular file
// owned by the user, mode 600 or 400, at most 4 KiB, one printable token.
package keyfile

import (
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"
)

// Status values match HostStatus.daemon.key.
const (
	OK      = "ok"
	Missing = "missing"
	Unsafe  = "unsafe"
	Invalid = "invalid"
)

// Read returns the key and "ok", or "" and the status that explains why not.
// It never returns or logs the file's contents on failure.
func Read(path string) (string, string) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return "", Missing
		}
		return "", Unsafe // ELOOP (a symlink), EACCES, …
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil {
		return "", Unsafe
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || int(st.Uid) != os.Geteuid() {
		return "", Unsafe
	}
	if p := st.Mode & 0o777; p != 0o600 && p != 0o400 {
		return "", Unsafe
	}
	if st.Size > 4096 {
		return "", Invalid
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", Invalid
	}
	tok := bytes.TrimRight(b, "\r\n")
	if len(tok) == 0 || len(tok) > 1024 {
		return "", Invalid
	}
	for _, c := range tok {
		if c < 0x21 || c > 0x7e {
			return "", Invalid
		}
	}
	return string(tok), OK
}
