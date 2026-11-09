package main

import (
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/webrtc/v4"
)

func main() {
	p, _ := webrtc.NewPeerConnection(webrtc.Configuration{})
	p.Close()
	b, _ := gcc.NewSendSideBWE()
	b.Close()
}
