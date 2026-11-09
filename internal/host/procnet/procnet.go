// Package procnet reads /proc/net/tcp and /proc/net/tcp6 for the two
// ownership checks of HOST-CONTROL §2.2 and §5.2: who listens on the
// sender port, and which uid owns the client end of an accepted connection.
package procnet

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/passstream/passstream/internal/host/proto"
)

// TCP states in /proc/net/tcp.
const (
	Established = 0x01
	Listen      = 0x0A
)

// Entry is one socket observation, with IPv4-mapped addresses normalized.
type Entry struct {
	Local  netip.AddrPort
	Remote netip.AddrPort
	State  int
	UID    int
	Inode  uint64
}

// Root is the procfs root ("/proc" outside tests).
type Root string

// Read parses both tables and coalesces repeated, identical observations of a
// nonzero socket inode. tcp6 may be absent only when IPv6 is absent (ENOENT on
// the file and on /proc/sys/net/ipv6); any other error fails.
func (r Root) Read() ([]Entry, error) {
	if r == "" {
		r = "/proc"
	}
	v4, err := parseFile(filepath.Join(string(r), "net", "tcp"), false)
	if err != nil {
		return nil, err
	}
	v6, err := parseFile(filepath.Join(string(r), "net", "tcp6"), true)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if _, e2 := os.Stat(filepath.Join(string(r), "sys", "net", "ipv6")); !errors.Is(e2, fs.ErrNotExist) {
			return nil, err
		}
		v6 = nil
	}
	// A procfs walk under socket churn can observe the same socket more than
	// once (see testdata/repeated-peer.tcp). Count socket identities, not rows.
	// Only a nonzero inode with an identical tuple, state and UID is evidence
	// of repetition. Keep unknown identities and conflicting observations so
	// they cannot collapse an ambiguous ownership lookup into a unique match.
	seen := make(map[Entry]bool)
	var out []Entry
	for _, e := range append(v4, v6...) {
		if e.Inode != 0 {
			if seen[e] {
				continue
			}
			seen[e] = true
		}
		out = append(out, e)
	}
	return out, nil
}

func parseFile(path string, v6 bool) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			return nil, fmt.Errorf("%s: short line", path)
		}
		l, err1 := parseAddr(fields[1], v6)
		rm, err2 := parseAddr(fields[2], v6)
		st, err3 := strconv.ParseUint(fields[3], 16, 8)
		uid, err4 := strconv.Atoi(fields[7])
		inode, err5 := strconv.ParseUint(fields[9], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
			return nil, fmt.Errorf("%s: malformed line", path)
		}
		out = append(out, Entry{Local: l, Remote: rm, State: int(st), UID: uid, Inode: inode})
	}
	return out, sc.Err()
}

// parseAddr decodes "0100007F:B8BF": the address words are in host byte
// order (little endian on every platform this targets), the port is hex.
func parseAddr(s string, v6 bool) (netip.AddrPort, error) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return netip.AddrPort{}, errors.New("no port")
	}
	raw, err := hex.DecodeString(s[:i])
	if err != nil {
		return netip.AddrPort{}, err
	}
	port, err := strconv.ParseUint(s[i+1:], 16, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if (!v6 && len(raw) != 4) || (v6 && len(raw) != 16) {
		return netip.AddrPort{}, errors.New("bad address length")
	}
	b := make([]byte, len(raw))
	for w := 0; w < len(raw); w += 4 {
		binary.BigEndian.PutUint32(b[w:], binary.LittleEndian.Uint32(raw[w:]))
	}
	a, _ := netip.AddrFromSlice(b)
	return netip.AddrPortFrom(a.Unmap(), uint16(port)), nil
}

// receives reports whether a listener on a receives 127.0.0.1 traffic.
func receives(a netip.Addr) bool {
	a = a.Unmap()
	return a == netip.MustParseAddr("127.0.0.1") || a == netip.IPv4Unspecified() || a == netip.IPv6Unspecified()
}

// ListenerUIDs returns the uids of the listeners that would receive
// 127.0.0.1:port (CONTRACT Appendix A #4).
func ListenerUIDs(es []Entry, port int) []int {
	var out []int
	for _, e := range es {
		if e.State == Listen && int(e.Local.Port()) == port && receives(e.Local.Addr()) {
			out = append(out, e.UID)
		}
	}
	return out
}

// CheckOwner requires exactly one listener on 127.0.0.1:port owned by uid.
// Failures are port_not_owned with details.uid and, for several, details.uids.
func (r Root) CheckOwner(port, uid int) error {
	es, err := r.Read()
	if err != nil {
		return proto.E("port_not_owned", "can't inspect the listeners on port %d", port).WithDetails(map[string]any{"reason": "unreadable"})
	}
	uids := ListenerUIDs(es, port)
	switch {
	case len(uids) == 0:
		return proto.E("port_not_owned", "nothing listens on 127.0.0.1:%d", port).WithDetails(map[string]any{"reason": "none"})
	case len(uids) > 1:
		sort.Ints(uids)
		other := uids[0]
		for _, u := range uids {
			if u != uid {
				other = u
				break
			}
		}
		return proto.E("port_not_owned", "%d listeners on port %d", len(uids), port).WithDetails(map[string]any{"uid": other, "uids": uids})
	case uids[0] != uid:
		return proto.E("port_not_owned", "port %d is held by uid %d", port, uids[0]).WithDetails(map[string]any{"uid": uids[0]})
	}
	return nil
}

// PeerUID finds the client end of an accepted connection: the ESTABLISHED
// entry whose local address is the client's and whose remote address is the
// server's, the reverse of the accepted tuple. Exactly one socket must match.
func (r Root) PeerUID(client, server netip.AddrPort) (int, error) {
	es, err := r.Read()
	if err != nil {
		return -1, err
	}
	client = netip.AddrPortFrom(client.Addr().Unmap(), client.Port())
	server = netip.AddrPortFrom(server.Addr().Unmap(), server.Port())
	uid, n := -1, 0
	for _, e := range es {
		if e.State == Established && e.Local == client && e.Remote == server {
			uid = e.UID
			n++
		}
	}
	if n != 1 {
		return -1, fmt.Errorf("%d matching sockets", n)
	}
	return uid, nil
}
