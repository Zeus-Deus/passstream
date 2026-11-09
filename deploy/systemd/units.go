// Package systemd embeds the user units installed by
// `passstream host install-units`.
package systemd

import _ "embed"

// Socket is passstream-host.socket.
//
//go:embed passstream-host.socket
var Socket string

// Service is passstream-host.service; ExecStart names /usr/bin/passstream
// and the installer substitutes the running binary's path.
//
//go:embed passstream-host.service
var Service string
