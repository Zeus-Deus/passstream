package server

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// listenTURNPair reserves the same port in both transport namespaces.
func listenTURNPair(address string, listen func(string, string) (net.Listener, error), packet func(string, string) (net.PacketConn, error)) (net.PacketConn, net.Listener, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 16; attempt++ {
		// TCP's ephemeral allocator excludes occupied TCP ports and TIME_WAIT
		// sockets. UDP's allocator knows nothing about that namespace.
		tcp, err := listen("tcp4", address)
		if err != nil {
			return nil, nil, err
		}
		_, selected, _ := net.SplitHostPort(tcp.Addr().String())
		udp, bindErr := packet("udp4", net.JoinHostPort(host, selected))
		if bindErr == nil {
			return udp, tcp, nil
		}
		tcp.Close()
		// Another UDP socket may own a TCP-selected port. Only an ephemeral
		// request can choose again; configured public ports must fail as given.
		if port != "0" || !errors.Is(bindErr, syscall.EADDRINUSE) {
			return nil, nil, bindErr
		}
		lastErr = bindErr
	}
	return nil, nil, fmt.Errorf("reserve shared TURN port after 16 attempts: %w", lastErr)
}
