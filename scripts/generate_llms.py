#!/usr/bin/env python3
"""Generate the public agent contract from the binding wire specification."""
from pathlib import Path
import re
import sys
root = Path(__file__).resolve().parents[1]
contract = (root / "docs/CONTRACT.md").read_text()
parts = re.split(r"(?=^## \d+\. )", contract, flags=re.M)
selected = [part for part in parts if re.match(r"## (1|2|3|4|7|8|9|10|11|12|13|14)\. ", part)]
header = """# PassStream server API, protocol 1

The API and relay implement the following wire contract. Viewer cryptography and
the local host daemon are separate client components; the server never receives
the PIN, link secret, identity seed, or media/grant keys. Do not send them here.
Explicit --public enables IPv4 ICE and embedded TURN UDP/TCP/TLS behind a local
HTTPS reverse proxy. Bind and advertised addresses differ; see contract §3.1.
Safe defaults remain loopback. Bootstrap bearer key: DATA/owner-key (mode 0600).
The agent rules below apply to every remote integration.

Pages: the viewer is /v#c=…&k=…&s=…; the production sender is /sender, served by
the local `passstream host` daemon; /host is the server-served browser sender
(it trusts this server). Video is VP9 profile 0 only (non-flexible L1T3; VP8/H.264/AV1 refused) and
tab/system audio is Opus (when the capture has it), both under SFrame E2EE,
labelled beta; the SFU forwards audio opaquely and never decrypts either.
Viewers without RTCRtpScriptTransform are refused ("browser not supported")
and never get plaintext. Agents that need to watch a
stream must admit like a viewer; this server can't decrypt anything for them.

Operational bounds: default 100 viewers per stream (settings: 1–500), SSE 32 per
stream/eight per key-id (reauthorized every second); commands 128 cached/eight
in-flight per key-id, 4096 cached/1024 in-flight total. Id-less commands are never
cached. HTTP per key-id: 20/s burst 60, including login and dashboard sessions.
Publisher output: separate 64-message relay and control/lifecycle queues, control
first. Relay backpressure at half capacity closes only the flooding viewer;
those closures coalesce into fresh e2ee.peers snapshots outside the control queue.
Control overflow counts publisher_control_overflow and publisher_control_drop
(failed enqueues and buffered messages abandoned by resync), and closes the
publisher with WebSocket 1013/resync. Reconnect gets a fresh peer list and replays
unacknowledged kick/lock controls, including ban-derived kicks, with stable ids
before queued traffic. Replay bypasses the 64-slot queue, survives HTTP timeouts
and disconnects, and clears on terminal sender acknowledgement or stream end
(retryable failures retain it). Memory-only replay is capped at 1024 per
stream; exhaustion returns rate_limited before applying a new command/ban.
Pending controls prevent idle eviction; server restart clears them. Ban retains
derived kicks while offline; other new sender commands return sender_offline.
Waiters share the attempt they joined, including its 504; only a request arriving
after completion starts a retry with the same forwarding id.
Handshakes: IP 2/s burst 20, stream 10/s burst 50. All IPv6 IP budgets and bans
use the masked /64; IPv4 uses the individual address.
Relay: viewer 16/s burst 32, viewer IP 64/s burst 128, publisher IP 2048/s
burst 4096, stream 512/s burst 1024. Annotation envelopes (types 0x09/0x0A) use
separate budgets (viewer 10/s burst 20, viewer IP 32/s burst 64, viewers per
stream 128/s burst 256, publisher IP 512/s burst 1024, publisher per stream
256/s burst 512) and at most a quarter of any queue, never displacing rekeys.
Knock alerts: IP 1/s burst 5, stream 5/s burst 50; snapshots are always retained
within the sender-state budget of 4/s burst four (excess reports silently drop).
Coalesce reports and resend the latest state within that budget.
SenderState optionally accepts end_reason: stopped, window_closed, expired, deleted,
state_lost or browser_failed. An ended state without it defaults to window_closed.
Viewer PLI requests: IP 2/s burst 4, stream 5/s burst 10, plus SFU coalescing.
Bans persist stream-scoped HMACs (DATA/ban-secret, 0600) and are deleted on end.
Recent disconnected IPs stay private in memory for five minutes for ban actions.
--audit-retention defaults to 720h (minimum 1h); up to 1000 expired audit records
are removed each second. Historical stream listing is paginated in SQL.
Canonical --base-url and returned links: lower-case host, bracketed IPv6, explicit
port if supplied, no trailing slash/path/query/fragment; HTTP or HTTPS only.
GeoIP attribution is in response meta.geoip_attribution. Missing offline databases
produce null geography. Retention changes that enable keep_ip need an owner
bearer key. --trust-proxy accepts comma-separated IPs/CIDRs and trusts X-Forwarded-For
only through the configured proxy chain, walking from right to left.
Transport resumption fields session, replace, resumed and iceServers remain
available on signaling; these authorize only a transport, never E2EE admission.
A peer-connection replacement redoes sender admission using the client's grant.

"""
media = (root / "scripts/llms-media.md").read_text() + "\n"
output = root / "internal/server/llms.txt"
text = header + media + "\n".join(selected)
if "--check" in sys.argv:
    if output.read_text() != text:
        sys.exit("llms.txt is stale: run python3 scripts/generate_llms.py")
else:
    output.write_text(text)
