# Public hosting


Use a server with a reachable IPv4 address, a DNS name (for example
`stream.example.com`) pointing to it, and a trusted TLS certificate/chain and
private key for that name. Provide certificate paths yourself; PassStream does
not acquire certificates. The following uses documentation IP `203.0.113.10`;
replace it with your server's reachable address:

```sh
./build/passstream serve --data ./data \
  --public --base-url https://stream.example.com \
  --listen 127.0.0.1:8080 --trust-proxy 127.0.0.1/32 \
  --media-ip 203.0.113.10 \
  --ice-udp 0.0.0.0:8443 --ice-tcp 0.0.0.0:8443 \
  --turn 0.0.0.0:3478 --turn-tls 0.0.0.0:5349 \
  --turn-cert /path/to/fullchain.pem --turn-key /path/to/privkey.pem
```

Run an HTTPS reverse proxy on the same machine. For example, a Caddyfile with
operator-provided certificates (no automatic certificate acquisition):

```caddyfile
stream.example.com {
    tls /path/to/fullchain.pem /path/to/privkey.pem
    reverse_proxy 127.0.0.1:8080 {
        header_up X-Forwarded-For {remote_host}
    }
}
```

Caddy preserves Host and handles WebSocket upgrades, including `/signal`.
Keep backend HTTP loopback-only. `--base-url` is the exact public HTTPS origin;
links, Secure cookies and API Origin checks use it, not proxy headers. Point
sender/CLI configuration at this HTTPS origin. Limit `--trust-proxy` to your
actual proxy; the example overwrites forwarded IPs at the edge.

Provide these routes/firewall openings on the server or its port-preserving NAT:

| Public port | Destination | Purpose |
|---|---|---|
| TCP 443 | HTTPS reverse proxy | Pages, API and WebSocket signaling |
| UDP 8443 | PassStream ICE mux | Preferred direct media |
| TCP 8443 | PassStream passive ICE mux | Direct ICE TCP |
| UDP and TCP 3478 | PassStream TURN | Authenticated TURN UDP/TCP |
| TCP 5349 | PassStream TURN/TLS | Authenticated TLS relay |

`--media-ip` is the address viewers reach; `0.0.0.0` is only a bind. For a server
behind NAT, bind the local IPv4 address or wildcard, advertise its reachable
IPv4, and preserve these ports. TURN defaults to the origin hostname;
`--turn-host turn.example.com` advertises a separate hostname whose certificate
must match and whose DNS/routes reach all configured TURN ports. Do not publish
an unreachable AAAA record: media transport here is IPv4 only.

For networks allowing only TLS on **443**, use a **second IP** for TURN/TLS:
set `--turn-host turn.example.com --turn-tls <second-local-ip>:443`, point that
hostname to the second public IP, provide its matching certificate, and expose
TURN UDP/TCP 3478 there too. Bind the HTTPS proxy specifically to the first IP
(e.g. Caddy `bind <first-local-ip>`), so it does not occupy both IPs. TURN cannot
share an HTTP reverse proxy; sharing one IP:443 requires external L4 multiplexing,
which this binary does not implement. A listener on 5349 alone does not overcome
a firewall that allows only 443.

No ephemeral public TURN relay range is needed. Internal allocations stay on
`127.0.0.2` and can reach only this SFU's exact UDP endpoint; the server translates
the advertised endpoint to its local bind, avoiding NAT hairpin requirements.
TURN TCP/TLS share a 2048-connection cap. Connections must authenticate within
10 seconds (including TLS handshake); partial TURN requests and blocked writes
have a 10-second deadline, and authenticated idle streams close after 60 seconds.
Passive ICE TCP (public and loopback) separately caps accepted sockets at 1024
and locally registered ICE endpoints at 2048. Its receive queues hold 128 packets
of at most 8192
bytes per endpoint (at most 1 MiB payload), with a shared 256 MiB payload ceiling;
outbound queues hold 64 KiB per socket. The registration budget preserves TCP
fallback for 512 viewers plus a publisher and full peer replacement overlap.
The separate 1024-socket budget accommodates the supported cohort; UDP and TURN
are separate. These resource ceilings do not certify 500-viewer media quality.
Unknown STUN ufrags are refused without creating a queue. First STUN, started
RFC 4571 frames, full receive queues and blocked writes have 10-second limits;
established idle sockets close after 60 seconds. Shutdown closes silent sockets too.
Startup refuses insecure public origins, missing advertise addresses and missing,
expired, mismatched or incorrectly named TURN/TLS certificates. Certificate renewal
requires restarting PassStream. Requested TLS connections have no plaintext fallback.

