package procnet_test

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/passstream/passstream/internal/host/procnet"
	"github.com/passstream/passstream/internal/host/proto"
)

const hdr = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

// line formats one /proc/net/tcp row (port 47823 = 0xBACF).
func line(local, remote, st string, uid string, inode uint64) string {
	return fmt.Sprintf("   0: %s %s %s 00000000:00000000 00:00000000 00000000 %s 0 %d 1 0000000000000000 100 0 0 10 0\n", local, remote, st, uid, inode)
}

func root(t *testing.T, tcp, tcp6 string) procnet.Root {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "net"), 0o700)
	os.WriteFile(filepath.Join(dir, "net", "tcp"), []byte(hdr+tcp), 0o600)
	if tcp6 != "-" {
		os.WriteFile(filepath.Join(dir, "net", "tcp6"), []byte(hdr+tcp6), 0o600)
	}
	return procnet.Root(dir)
}

const (
	lo4    = "0100007F:BACF"
	any4   = "00000000:BACF"
	any6   = "00000000000000000000000000000000:BACF"
	mapped = "0000000000000000FFFF00000100007F:BACF"
	other6 = "00000000000000000000000001000000:BACF" // ::1, doesn't receive 127.0.0.1
	zero4  = "00000000:0000"
	zero6  = "00000000000000000000000000000000:0000"
)

func details(err error) map[string]any {
	if e, ok := err.(*proto.Error); ok {
		return e.Details
	}
	return nil
}

func TestCheckOwner(t *testing.T) {
	cases := []struct {
		name      string
		tcp, tcp6 string
		wantUID   any
		ok        bool
	}{
		{"one of ours", line(lo4, zero4, "0A", "1000", 12346), "", nil, true},
		{"wildcard v4", line(any4, zero4, "0A", "1000", 12347), "", nil, true},
		{"none", "", "", nil, false},
		{"another uid", line(lo4, zero4, "0A", "1001", 12348), "", 1001, false},
		{"dual-stack wildcard of another uid", line(lo4, zero4, "0A", "1000", 12349), line(any6, zero6, "0A", "1001", 12350), 1001, false},
		{"v4-mapped of another uid", "", line(mapped, zero6, "0A", "1001", 12351), 1001, false},
		{"::1 doesn't count", line(lo4, zero4, "0A", "1000", 12352), line(other6, zero6, "0A", "1001", 12353), nil, true},
		{"established isn't a listener", line(lo4, "0100007F:1234", "01", "1001", 12354) + line(lo4, zero4, "0A", "1000", 12355), "", nil, true},
		{"two of ours", line(lo4, zero4, "0A", "1000", 12356) + line(any4, zero4, "0A", "1000", 12357), "", 1000, false},
	}
	for _, c := range cases {
		err := root(t, c.tcp, c.tcp6).CheckOwner(47823, 1000)
		if (err == nil) != c.ok {
			t.Errorf("%s: err %v", c.name, err)
			continue
		}
		if c.wantUID != nil && details(err)["uid"] != c.wantUID {
			t.Errorf("%s: details %v", c.name, details(err))
		}
	}
	err := root(t, line(lo4, zero4, "0A", "1000", 12358)+line(any4, zero4, "0A", "1001", 12359), "").CheckOwner(47823, 1000)
	if d := details(err); d["uid"] != 1001 || len(d["uids"].([]int)) != 2 {
		t.Errorf("several owners: %v", d)
	}
	// tcp6 unreadable while IPv6 exists: fail closed.
	r := root(t, line(lo4, zero4, "0A", "1000", 12360), "-")
	os.MkdirAll(filepath.Join(string(r), "sys", "net", "ipv6"), 0o700)
	if r.CheckOwner(47823, 1000) == nil {
		t.Error("missing tcp6 with IPv6 present must fail")
	}
	// tcp6 absent and IPv6 absent: fine.
	if err := root(t, line(lo4, zero4, "0A", "1000", 12361), "-").CheckOwner(47823, 1000); err != nil {
		t.Errorf("no IPv6: %v", err)
	}
	if procnet.Root(t.TempDir()).CheckOwner(47823, 1000) == nil {
		t.Error("unreadable tables must fail")
	}
}

func TestPeerUID(t *testing.T) {
	client := netip.MustParseAddrPort("127.0.0.1:4660") // 0x1234
	server := netip.MustParseAddrPort("127.0.0.1:47823")
	theirs := line("0100007F:1234", lo4, "01", "1000", 12362)
	ours := line(lo4, "0100007F:1234", "01", "1001", 12363) // the accepted end, the daemon's
	uid, err := root(t, theirs+ours, "").PeerUID(client, server)
	if err != nil || uid != 1000 {
		t.Fatalf("uid %d err %v", uid, err)
	}
	// A v4-mapped client socket in tcp6.
	uid, err = root(t, ours, line("0000000000000000FFFF00000100007F:1234", "0000000000000000FFFF00000100007F:BACF", "01", "1000", 12364)).PeerUID(client, server)
	if err != nil || uid != 1000 {
		t.Fatalf("mapped: uid %d err %v", uid, err)
	}
	if _, err := root(t, ours, "").PeerUID(client, server); err == nil {
		t.Fatal("no client entry must fail")
	}
	if _, err := root(t, theirs+line("0100007F:1234", lo4, "01", "1000", 54321), "").PeerUID(client, server); err == nil {
		t.Fatal("two matches must fail")
	}
	if _, err := root(t, line("0100007F:1234", lo4, "06", "1000", 12365), "").PeerUID(client, server); err == nil {
		t.Fatal("a TIME_WAIT entry isn't established")
	}
}

// These are the three rows returned by one failing Chromium lookup under
// loopback socket churn. The slot and timer changed; socket identity did not.
func TestPeerUIDRepeatedObservation(t *testing.T) {
	data, err := os.ReadFile("testdata/repeated-peer.tcp")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "net", "tcp"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "net", "tcp6"), []byte(hdr), 0o600); err != nil {
		t.Fatal(err)
	}
	uid, err := procnet.Root(dir).PeerUID(netip.MustParseAddrPort("127.0.0.1:46010"), netip.MustParseAddrPort("127.0.0.1:42279"))
	if err != nil || uid != 1000 {
		t.Fatalf("uid %d err %v", uid, err)
	}
}

func TestPeerUIDSocketIdentity(t *testing.T) {
	client := netip.MustParseAddrPort("127.0.0.1:4660")
	server := netip.MustParseAddrPort("127.0.0.1:47823")
	one := line("0100007F:1234", lo4, "01", "1000", 12345)
	unknown := line("0100007F:1234", lo4, "01", "1000", 0)
	cases := []struct {
		name, tcp, tcp6 string
		ok              bool
	}{
		{"repeated observation", one + one, "", true},
		{"mapped repeated observation", one, line("0000000000000000FFFF00000100007F:1234", mapped, "01", "1000", 12345), true},
		{"distinct sockets with same UID", one + line("0100007F:1234", lo4, "01", "1000", 54321), "", false},
		{"distinct sockets with different UIDs", one + line("0100007F:1234", lo4, "01", "1001", 54321), "", false},
		{"contradictory UID for same inode", one + line("0100007F:1234", lo4, "01", "1001", 12345), "", false},
		{"contradictory mapped UID", one, line("0000000000000000FFFF00000100007F:1234", mapped, "01", "1001", 12345), false},
		{"unknown identities remain ambiguous", unknown + unknown, "", false},
		{"known and unknown identities remain ambiguous", one + unknown, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uid, err := root(t, c.tcp, c.tcp6).PeerUID(client, server)
			if c.ok {
				if err != nil || uid != 1000 {
					t.Fatalf("uid %d err %v", uid, err)
				}
			} else if err == nil || uid != -1 {
				t.Fatalf("ambiguous socket lookup: uid %d err %v", uid, err)
			}
		})
	}
}

func TestCheckOwnerSocketIdentity(t *testing.T) {
	one := line(lo4, zero4, "0A", "1000", 12345)
	unknown := line(lo4, zero4, "0A", "1000", 0)
	cases := []struct {
		name, tcp, tcp6 string
		ok              bool
	}{
		{"repeated listener observation", one + one, "", true},
		{"different remote family is not identical", one, line(mapped, zero6, "0A", "1000", 12345), false},
		{"repeated mapped listener", "", line(mapped, zero6, "0A", "1000", 12345) + line(mapped, zero6, "0A", "1000", 12345), true},
		{"distinct listeners with same UID", one + line(lo4, zero4, "0A", "1000", 54321), "", false},
		{"distinct listeners with different UIDs", one + line(lo4, zero4, "0A", "1001", 54321), "", false},
		{"contradictory UID for same inode", one + line(lo4, zero4, "0A", "1001", 12345), "", false},
		{"contradictory address for same inode", one + line(any4, zero4, "0A", "1000", 12345), "", false},
		{"unknown identities remain ambiguous", unknown + unknown, "", false},
		{"known and unknown identities remain ambiguous", one + unknown, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := root(t, c.tcp, c.tcp6).CheckOwner(47823, 1000)
			if (err == nil) != c.ok {
				t.Fatalf("err %v, want success %v", err, c.ok)
			}
		})
	}
}

func TestReadSocketIdentity(t *testing.T) {
	one := line(lo4, zero4, "0A", "1000", 12345)
	cases := []struct {
		name, second string
		want         int
	}{
		{"identical observation", one, 1},
		{"different inode", line(lo4, zero4, "0A", "1000", 54321), 2},
		{"different UID", line(lo4, zero4, "0A", "1001", 12345), 2},
		{"different state", line(lo4, zero4, "01", "1000", 12345), 2},
		{"different local tuple", line(any4, zero4, "0A", "1000", 12345), 2},
		{"different remote tuple", line(lo4, "0100007F:1234", "0A", "1000", 12345), 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			es, err := root(t, one+c.second, "").Read()
			if err != nil || len(es) != c.want {
				t.Fatalf("entries %+v err %v", es, err)
			}
			if es[0].Inode != 12345 {
				t.Fatalf("lost inode: %+v", es[0])
			}
		})
	}
}

func TestReadInvalidInode(t *testing.T) {
	fields := strings.Fields(line(lo4, zero4, "0A", "1000", 12345))
	for _, inode := range []string{"missing", "-1", "not-an-inode", "18446744073709551616"} {
		t.Run(inode, func(t *testing.T) {
			row := append([]string(nil), fields...)
			row[9] = inode
			if inode == "missing" {
				row = row[:9]
			}
			if _, err := root(t, strings.Join(row, " ")+"\n", "").Read(); err == nil {
				t.Fatal("invalid socket identity must fail closed")
			}
		})
	}
}
