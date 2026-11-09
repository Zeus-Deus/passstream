package sfu

import (
	"github.com/pion/webrtc/v4"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTCPAdvertised(t *testing.T) {
	free := func() string {
		l, e := net.Listen("tcp4", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		a := l.Addr().String()
		l.Close()
		return a
	}
	e, err := New(free(), free())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	p, err := e.Peer("test", true)
	if err != nil {
		t.Fatal(err)
	}
	client, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.CreateDataChannel("control", nil)
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	client.SetLocalDescription(offer)
	p.PC.SetRemoteDescription(offer)
	gather := webrtc.GatheringCompletePromise(p.PC)
	answer, err := p.PC.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	p.PC.SetLocalDescription(answer)
	select {
	case <-gather:
	case <-time.After(5 * time.Second):
		t.Fatal("gather timeout")
	}
	if !strings.Contains(p.PC.LocalDescription().SDP, "tcptype passive") {
		t.Fatal("ICE TCP passive candidate not advertised")
	}
}

func TestPublicWildcardCandidates(t *testing.T) {
	e, err := NewPublic("0.0.0.0:0", "0.0.0.0:0", "198.18.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if !e.UDPAddr().IP.IsUnspecified() || e.UDPAddr().Port == 0 {
		t.Fatal("lost actual bind address", e.UDPAddr())
	}
	p, err := e.Peer("public-candidates", true)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	remote.CreateDataChannel("control", nil)
	offer, err := remote.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if err := p.PC.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	gather := webrtc.GatheringCompletePromise(p.PC)
	answer, err := p.PC.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PC.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gather:
	case <-time.After(5 * time.Second):
		t.Fatal("gather timeout")
	}
	udp, tcp := false, false
	for _, line := range strings.Split(p.PC.LocalDescription().SDP, "\r\n") {
		if !strings.HasPrefix(line, "a=candidate:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 || fields[4] != "198.18.0.1" {
			t.Fatalf("bind/private address escaped: %s", line)
		}
		udp = udp || strings.EqualFold(fields[2], "udp")
		tcp = tcp || strings.EqualFold(fields[2], "tcp") && strings.Contains(line, "tcptype passive")
	}
	if !udp || !tcp {
		t.Fatal("missing advertised UDP/passive TCP candidates", p.PC.LocalDescription().SDP)
	}
}
