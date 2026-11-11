# PassStream contract, v1 (host control)

The wire contract for the server API, the signaling additions, the local daemon and the sender page. The design and its reasons are in [HOST-CONTROL.md](HOST-CONTROL.md); where the two differ, this file wins. The e2ee library, the server, the daemon, the sender page, the viewer page, the plugin and the dashboard can be built against this file in parallel. `/llms.txt` is generated from the normative schemas, API, local IPC and agent rules and changes with them.

Status: **draft for phases 2b and 3, revised after review rounds 1 and 2** (HOST-CONTROL §12).

## 1. Conventions

### 1.1 Schema notation (normative)

Objects are written as typed records:

```
Name {
  field: Type                 // required, never null
  field: Type | null          // required, may be null
  field?: Type = default      // optional; absent means the default
  field?: Type                // optional with no default; absent means "not given"
}
```

- Types: `string`, `bool`, `uint` (a JSON integer ≥ 0, ≤ 2^53−1), `number` (a finite JSON number), `Ms` (`uint`, Unix milliseconds), `T[]`, string literal unions (`"a" | "b"`), and the named types below.
- Named ids: `Id22` (b64url, no padding, 22 characters = 16 bytes), `StreamId` (`Id22`), `ConnId` (`"c_"` + `Id22`), `Token43` (b64url, 43 characters = 32 bytes), `RowCode` (§4.3).
- `null` is allowed only where the type says `| null`. A required field that's missing, or a `null` where none is allowed, makes the message invalid (`invalid_argument` on requests; dropped and counted on streams of messages).
- Strings are UTF-8. Labels (and `geo.city`, `geo.as_org`, `device`) are at most 64 characters and 256 bytes, and `user_agent` at most 256 bytes, with no control, bidi or zero-width characters (senders clamp and strip them; receivers do it again).
- JSON field names are `snake_case`. Times are `Ms` in fields named `*_at`.

### 1.2 Compatibility

- `protocol: 1` on the signaling `join`, the daemon socket and the sender WebSocket, and `/api/v1` on HTTP. Additive changes (new optional fields, new enum values, new ops, new event and alert kinds) keep the version. Anything else bumps it.
- **Unknown fields are ignored** by every receiver. The server also **discards** unknown fields in `sender.state` and never stores or forwards them, so a sender bug can't use an unknown field to send a secret through the server.
- **Unknown enum values.** A receiver maps any value it doesn't know to `"unknown"` and handles it by category:
  - **Display-only** (`path`, `quality_limitation`, `end_reason`, `flags`, alert kinds, error codes): shown as "unknown", or ignored when it's an alert. An unknown error code is handled by its HTTP status.
  - **State that gates the UI** (`phase`, `Viewer.status`, `attachment`): treated as the most restrictive known state. An unknown `phase` isn't live and offers only `stop`. An unknown row status isn't counted as watching and offers only `kick` (when it has `admission_ids`).
  - **Security inputs the sender acts on** (`PinPolicy.kind`, `op`, any command argument): refused with `invalid_argument`. Never defaulted.
- **Command arguments are closed**, unlike everything else: an unknown argument is `invalid_argument` (§8.5), because ignoring it could drop a restriction the caller relied on.

### 1.3 Limits

Per `SenderState`: ≤ 500 admissions, ≤ 50 knocks, ≤ 100 `ends`; so ≤ 550 live row codes at once. Per `Viewer`: ≤ 3 connections. Per `ViewerList`: ≤ 100 `joining` rows (the newest).

| Message | Worst case (computed) | Limit |
|---|---|---|
| HTTP request body | | 64 KiB |
| `sender.state` (whole WebSocket message) | 500 admissions × ≤ 230 B + 50 knocks × ≤ 140 B + 100 ends × ≤ 120 B + ≤ 2 KB ≈ 136 KB | 256 KiB |
| `e2ee` envelope | 4096 bytes decoded = 5 462 b64url characters + ≤ 60 B of JSON | 6 KiB |
| A serialised `Connection` | ≤ 2 KB with the string limits in §1.1 | |
| `ViewerList` (`HostStatus`, `view`, `/viewers`) | 500 rows × (≤ 0.6 KB + 3 × 2 KB) + 150 knock and joining rows × (≤ 0.6 KB + 2 KB) ≈ 3.7 MB; under 8 MiB even if every string needs JSON escaping | 8 MiB per daemon socket line and per sender WebSocket message |
| E2EE payload (PROTOCOL.md §4) | `ADMIT` box 113 B, `PENDING` 25 B (§8.4) | 4096 B |

A message over its limit is refused (`payload_too_large`) or, on a stream of messages, dropped and counted.

### 1.4 Pre-release: no phase-1 compatibility

Nothing here is compatible with phase 1, and phase-1 code is migrated:

- Plain-text HTTP errors become the JSON envelope (§14).
- The stream's `created` (seconds) and `url` fields are removed.
- The link format changes (§3).
- `initial-key` becomes an ordinary `owner` key.
- The viewer and publisher `join` gain `protocol`.

There are no deprecated aliases.

## 2. Credentials and where they travel

**No credential or secret in this table is ever logged** by the server, the daemon, the sender page, the viewer page, the plugin or the CLI, and none is ever put in a URL path or query string. Where a row says "stored hashed", only a SHA-256 hash is kept.

| Credential | Made by | Travels | Stored | Never goes to |
|---|---|---|---|---|
| API key | Server (`POST /keys`) | Client → server, `Authorization: Bearer` only | Server: hashed. Host: `~/.config/passstream/key` (`0600`) | Argv, the sender page, the plugin |
| Session id | Server | `ps_session` cookie only | Server: hashed | JS-readable storage |
| Publish token (`Token43`) | Server, at creation | Server → daemon (201 body); daemon → page (`init`); page → server (publisher `join`). Server-served senders: in the `host_url` fragment | Server: hashed. Daemon: `host-state.json` (`0600`) | Argv, the plugin |
| View capability `c` (`Token43`) | Server, at creation | In the link fragment; viewer → server in the viewer `join` | Server: hashed. Page: recovery record | — |
| Sender public key `k` | Sender page | In the link fragment | Page | Not secret |
| Link secret `s`, identity seed | Sender page | `s` only inside the link: page → daemon (`secrets`), daemon → plugin or CLI on request, then the clipboard | Page recovery record | **The server** |
| PIN or passphrase | Sender page, or the local user | Page ↔ daemon (local WebSocket); daemon ↔ plugin or CLI (Unix socket); CLI ← TTY prompt, stdin or `--pin-fd` | Page recovery record. Daemon: memory only | **The server**, argv, toasts, `status` |
| Grant secrets, epoch and channel keys | e2ee library | Only inside E2EE boxes | Library storage; worker memory | Everything else |
| Launch key (`Token43`) and its public `key_id` (`Id22`) | Daemon | The `0600` launch file in the `0700` runtime directory → the page (`--app=file://…`); never argv. **Never sent by the page**: it's an HMAC key (§11.2) | Daemon memory; page `sessionStorage` during the handover | Any socket. Valid 60 s; retired at the handover (HOST-CONTROL §5.3) |
| Attachment key (`Token43`) and its public `key_id` | Daemon, at the first launch-key exchange | `init` only (after mutual authentication). **Never sent by the page** | Page `sessionStorage`; daemon `host-state.json` (`0600`) | Any socket after `init`. Lives as long as the stream; never rotated |
| Full share link | Sender page | Page → daemon → plugin or CLI, clipboard | Daemon memory | **The server**, toasts |

The row code (§4.3) isn't a credential or a secret.

## 3. Links

- **Viewer link:** `<base>/v#c=<capability>&k=<b64url(pk_S)>&s=<b64url(link_secret)>`, exactly this canonical form (PROTOCOL.md §3, `buildFragment`/`parseFragment`). The server returns `view_link_base` = `<base>/v#c=<capability>`; the sender appends `&k=…&s=…`. **`view_link_base` alone isn't a working link.**
- The viewer gets `stream_id` from the server's `joined` reply (§8.1). A wrong one makes every handshake fail, because the stream id is bound into CPace and every signature.
- **Server-served sender:** `host_url` = `<base>/host#id=<stream_id>&t=<publish_token>&c=<capability>&pin=<none|passphrase|digits:N>&knock=<0|1>`. Everything is in the fragment, so it never reaches the server in a request line. The page needs `c` to build the viewer link and uses `pin` and `knock` (the creation policy) on its first launch only. This sender trusts the server (HOST-CONTROL §2.1).
- **Local sender:** `http://127.0.0.1:47823/sender#i=<launch_key_id>&k=<launch_key>` (HOST-CONTROL §5.3).

### 3.1 Server transport configuration

Local defaults remain loopback HTTP `127.0.0.1:8080`, ICE UDP/passive TCP
`127.0.0.1:8443`, and TURN UDP/TCP `127.0.0.1:3478`. Public media requires
explicit `serve --public`; no public listeners or certificates are inferred.

| Flag | Public mode meaning |
|---|---|
| `--base-url` | Required non-loopback HTTPS origin; canonical links, cookie security and API Origin checks use this value, never forwarded scheme/host headers. No path, query, fragment or credentials. |
| `--listen` | Literal loopback HTTP address, also in public mode. A local HTTPS reverse proxy serves this origin, preserves Host and upgrades `/signal`; never expose the backend HTTP listener. |
| `--media-ip` | Required advertised unicast IPv4 address, distinct from binds. Private routed IPv4 is allowed. Never a hostname, wildcard or loopback. |
| `--ice-udp`, `--ice-tcp` | IPv4 unicast or `0.0.0.0` bind addresses with nonzero ports. Only `--media-ip` is published as UDP and passive TCP host candidates. |
| `--turn` | IPv4 UDP **and** TCP bind address (one port). Both URLs are advertised using `--turn-host`, never the bind host. |
| `--turn-host` | Advertised TURN hostname or IPv4 address, without scheme/port; defaults to the base origin's hostname. It must reach this server's TURN listeners. |
| `--turn-tls` | Required separate IPv4 TCP bind address/port for embedded TURN/TLS; `turns:<turn-host>:<port>?transport=tcp` is advertised. |
| `--turn-cert`, `--turn-key` | Required PEM certificate chain and matching private key paths. Startup refuses missing/malformed/mismatched keys, a leaf outside its validity interval, a hostname mismatch or an EKU excluding server authentication. TLS minimum 1.2. Clients must trust the chain. No automatic acquisition/reload; restart after renewal. |

Ports are published unchanged: routing/NAT must preserve ICE, TURN and TURN/TLS
ports. No general external port mapping or built-in 443 multiplexing. For 443-only
networks use TURN/TLS on a **second IP:443**, with `--turn-host` pointing there;
HTTPS already owns the first IP:443. An L4 mux is external infrastructure and must
preserve a consistent advertised endpoint; an HTTP reverse proxy cannot carry TURN.
The server's transport is IPv4 only; do not advertise an unreachable AAAA record.

The `joined` signaling message also includes `session`, `resumed` and
`iceServers: [{urls: string[], username: string, credential: string}]`. In public
mode its URLs are `turn:<turn-host>:<turn-port>?transport=udp`, the corresponding
`transport=tcp` URL, and `turns:<turn-host>:<tls-port>?transport=tcp`. Credentials
last 30 minutes and are transport authorization only. Clients gather candidates
concurrently (the browser keeps a candidate pool); TCP/TLS still carries UDP
allocations to the SFU. Missing TLS configuration fails startup, with no plaintext
substitution for a requested TLS path.

**TURN is private to this SFU**, not a general relay. Permissions allow only the
advertised SFU IPv4 address; the relay socket additionally enforces its exact UDP
port and filters reply sources. That tuple is translated to the actual local SFU
bind (`127.0.0.1` for a wildcard). Relay allocations deliberately bind and advertise
internal `127.0.0.2` addresses: the SFU is their only peer, so browsers reach them
through the authenticated TURN listener, never directly. This needs neither a
public ephemeral relay port range nor NAT hairpin support. Credentials cannot
reach other local services, arbitrary hosts, or ports on the media IP.

TCP/TLS TURN share a 2048-connection limit. Excess accepts close immediately.
An absolute 10-second setup deadline covers TLS and verified TURN authentication;
unauthenticated STUN traffic cannot extend it. Authenticated streams allow 60
seconds idle, with 10 seconds to finish each started frame or blocked write.
Timeout, disconnect and server shutdown release capacity.

Passive ICE TCP has separate limits in both public and loopback mode: 1024
accepted sockets and 2048 locally registered ICE endpoints. The registration
budget supports the 500-viewer product and synthetic 512-viewer cohort plus a
publisher, including full peer replacement overlap even when UDP wins. The
separate socket cap permits 1024 simultaneous direct TCP connections; these
ceilings do not certify 500-viewer media quality. UDP/TURN have separate budgets.
Remote STUN cannot create endpoints; unknown or removed ufrags close without a receive queue.
Each endpoint has 128 packet entries, each at most 8192 bytes (1 MiB queued
payload maximum, with a shared 256 MiB payload ceiling across all endpoints).
A large encoded frame spans many packets and is not truncated to this queue size. Each socket's outbound queue is
64 KiB. First STUN must complete within 10 seconds of acceptance. Each started
RFC 4571 frame, blocked write or full receive queue has an absolute 10-second
limit; established sockets allow 60 seconds idle. Removal/shutdown closes all
owned sockets, including pre-STUN connections, and releases their capacity.
These ceilings do not establish a tested concurrent-viewer capacity.

The proxy should overwrite `X-Forwarded-For` and the operator should set
`--trust-proxy` to that proxy's exact IP/CIDR for viewer IP facts. It is not an
HTTPS bypass. The local daemon remains loopback and owner-authenticated as in §11.
See README's public-hosting recipe for concrete DNS, proxy and port setup.

## 4. Shared objects

These are the same on every transport.

### 4.1 `PinPolicy`

```
PinPolicy = { kind: "digits", length: uint }   // 4–12
          | { kind: "passphrase" }             // value supplied locally: 8–256 bytes after NFC
          | { kind: "none" }
```

Strictness order: `none` < `digits 4` < `digits 5` < … < `digits 12` < `passphrase`. `knock: true` is stricter than `knock: false`. `stricter(a, b)` takes the stricter of each part. It's used only at stream creation (HOST-CONTROL §4.4).

### 4.2 `Latches`

```
Latches { locked: bool, paused: bool, frozen: bool }
```

Merging two copies is a field-by-field OR. Only the listed local commands clear a field (HOST-CONTROL §5.6).

### 4.3 `RowCode`

```
RowCode = string   // exactly 3 characters from ALPHABET = "ACDEFHJKMNPRTUVWXY3479", upper case
```

- **One canonical value.** `index = 484·i₀ + 22·i₁ + i₂`, where `iₙ` is the position of character `n` in `ALPHABET`; `0 ≤ index ≤ 10647`. In JSON it's the 3-character string; inside E2EE boxes it's `u16(index)` big-endian, and an index above 10647 is `malformed`. The two forms map one to one.
- **The colour is derived:** `COLOURS[index mod 8]` with `COLOURS = ["red", "orange", "yellow", "green", "teal", "blue", "purple", "pink"]`. It's display-only: never on the wire, never input, never compared. Displayed as `K7P · purple`.
- **Comparison** (`knock.approve`, `--code`) is on the string alone: the daemon and the sender require a canonical value (3 characters of `ALPHABET`, else `invalid_argument`) and compare it byte for byte with the knock's code (else `code_mismatch`). Only the CLI normalises user input (§12).
- Assigned by the sender per grant and knock, uniformly at random among the codes not used by the stream's live grants and pending knocks (≤ 550 at once, §1.3).

### 4.4 `SenderState` (sender → daemon and server)

```
SenderState {
  v: 1
  stream_id: StreamId
  instance: Id22                  // new per page load
  seq: uint                       // strictly increasing within an instance
  phase: "starting" | "picking" | "live" | "paused" | "ended"
  end_reason?: "stopped" | "window_closed" | "expired" | "deleted" | "state_lost" | "browser_failed" | "unknown"
  mode: "text" | "motion"
  safe: bool                      // safe mode: sensitive data is redacted before encoding
  e2ee: { status: "beta" | "on", epoch: uint, suspended: bool, revocation_not_durable: bool }
  access: Access
  admissions: Admission[]         // ≤ 500
  knocks: Knock[]                 // ≤ 50
  ends: End[]                     // ≤ 100, newest first: removals in the last 5 minutes of this instance
  health: Health | null           // null before capture
}

End = { kind: "admission", admission_id: Id22, viewer_id: Id22, cause: "kicked" | "revoked" | "stream_ended" }
    | { kind: "knock", knock_id: Id22, reason: KnockEnd }
// revoked: the grant expired or a PIN rotation revoked it. KnockEnd is in §5.3.

Access {
  pin: { required: bool, kind: "digits" | "passphrase" | "none", length: uint | null }  // length null unless digits
  pin_version: uint
  rotation_pending: bool
  reconciling: bool               // admissions blocked until PIN, policy, fence and budget agree
  latches: Latches
  knock: bool
  failures: { stream: uint, limit: uint }
}

Admission {
  admission_id: Id22
  viewer_id: Id22                 // grant-scoped: the same for every admission with one grant
  row_code: RowCode
  peer_id: ConnId                 // relay-chosen; the join key only
  connected: bool                 // receives rekeys
  via_grant: bool
  admitted_at: Ms
  kick_incomplete: bool
}

Knock { knock_id: Id22, peer_id: ConnId, row_code: RowCode, at: Ms, expires_at: Ms }

Health {
  uplink_kbps: uint | null
  encode_ms: number | null         // 0–60000; cumulative mean milliseconds per encoded frame
  encode_cpu_pct?: number | null   // 0–100; delta encode time / sample elapsed time × 100
  width?: uint | null              // 1–16384, capture width
  height?: uint | null             // 1–16384, capture height
  fps: number | null
  quality_limitation: "none" | "cpu" | "bandwidth" | "other"
}
```

- Malformed health (including non-integer uint fields, out-of-range values or an oversized object) becomes `health: null`; the rest of the sender state is still validated and accepted. Missing health remains an invalid state. The server and daemon retain the last reported health while the sender is offline; it is a last report, not a live measurement. A daemon restart has no health until the sender reports again.
- Health is at most 1 KiB on input, numeric and allowlisted; fps is 0–1000. Encode CPU is encoder utilization derived from WebRTC encode time, not process CPU. New health fields may be absent on older senders, and CPU is null until two usable samples exist. Health is relayed only to authenticated owner surfaces (read/streams/owner API keys and dashboard sessions), never viewer signaling. Unknown health fields are discarded, including any PIN, key or link field. The sender-state rate and size limits apply to health too.
- **There's no PIN value or link field.** The sender builds this object with an allowlist serializer.
- The sender sends it on every change, and at least every 5 s as a heartbeat, at most 4 per second.
- A receiver replaces its copy when `instance` differs or `seq` is greater, and notes its own clock as the copy's receipt time (`state_at`, §6).
- An admission leaves `admissions` only when it ends (it stays listed, `connected: false`, while its viewer is away). Each removal adds an `End` with its cause, so receivers never infer the cause from a diff.

### 4.5 `Connection` (server-observed)

```
Connection {
  connection_id: ConnId
  joined_at: Ms
  left_at: Ms | null
  ip: string | null               // HTTPS-observed, trusted-proxy aware, never a TURN address; null after the session unless retention.keep_ip
  geo: { country: string | null, city: string | null, asn: uint | null, as_org: string | null } | null
  user_agent: string | null       // ≤ 256 bytes
  device: string | null           // e.g. "iPhone · Safari"
  path: "udp" | "tcp" | "turn_udp" | "turn_tcp" | "turn_tls" | null
  stats: { rtt_ms: number | null, loss_pct: number | null, bitrate_kbps: uint | null, freezes: uint, temporal_layer: uint | null } | null
  stats_at: Ms | null
  reported: { label: string | null, tab_hidden: bool | null }   // viewer-reported, unverified
}
```

### 4.6 `Viewer` and `ViewerList` (joined rows)

```
Viewer {
  row_id: string                  // "v:<viewer_id>" | "k:<knock_id>" | "c:<connection_id>"
  status: "watching" | "reconnecting" | "knocking" | "joining" | "left"
  viewer_id: Id22 | null          // null for knocking and joining
  row_code: RowCode | null        // null for joining
  admission_ids: Id22[]           // newest first; [] for knocking and joining
  knock_id: Id22 | null           // set only for knocking
  sender: {
    connected: bool               // any admission connected
    via_grant: bool               // of the newest admission
    admitted_at: Ms               // earliest admission
    kick_incomplete: bool         // any admission
  } | null                        // null for knocking and joining
  connections: Connection[]       // attached by the join, newest first, ≤ 3 (all of them are in /connections)
  flags: ("shared_connection" | "no_connection" | "sender_stale")[]
  left_at: Ms | null              // set only for left
}

ViewerList {
  source: "local" | "server"      // local: sender facts are first-hand (daemon). server: relayed.
  sender_online: bool
  sender_stale: bool
  state_at: Ms | null             // when the SenderState used was received
  built_at: Ms
  watching: uint                  // distinct viewer_ids with status watching
  knocking: uint
  viewers: Viewer[]               // order: watching, knocking, reconnecting, joining, left
}
```

**Actions** use `admission_ids[0]` (a kick covers the whole grant) or `knock_id`, never `connection_id`, except for `ban`.

### 4.7 `Alert`

```
Alert = { kind: "failed_pin", count: uint, sources: uint, from_at: Ms, to_at: Ms }
      | { kind: "frozen", failures: uint }
      | { kind: "unfrozen", pin_rotated: bool }
      | { kind: "revocation_durability", not_durable: bool }
      | { kind: "state_mismatch" }
      | { kind: "joined", viewer_ids: Id22[] }
      | { kind: "left", viewer_ids: Id22[] }
      | { kind: "knock", knock_id: Id22, row_code: RowCode }
      | { kind: "knock_ended", knock_id: Id22, reason: KnockEnd }
      | { kind: "pin_changed", pin: PinPolicy, by: "plugin" | "cli" | "window" }   // local only
```

The sender emits `failed_pin` (batched; a freeze flushes at once), `frozen`, `unfrozen`, `revocation_durability`, `state_mismatch` and `knock_ended`. The daemon and the server derive `joined`, `left` and `knock` from state diffs. Every alert is display-only; no automation may act on `joined` or `knock` by identity (§13).

### 4.8 `Stream`

```
Stream {
  id: StreamId
  created_at: Ms
  label: string | null
  phase: "pending" | "starting" | "picking" | "live" | "paused" | "ended"
  ended_at: Ms | null
  end_reason: "stopped" | "window_closed" | "expired" | "deleted" | "state_lost" | "browser_failed" | "unknown" | null
  expires_at: Ms | null
  e2ee: bool
  sender: { online: bool, kind: "local" | "browser" | "whip", last_seen_at: Ms | null }
  declared: { pin: PinPolicy, knock: bool }   // fixed at creation; informational
  effective: { phase, mode, safe, access: Access, e2ee, health: Health | null } | null   // from the last SenderState; null before it
  watching: uint
  knocking: uint
}
```

`phase` is `pending` until the sender's first report, then the sender's phase, then `ended`.

### 4.9 `HistoryEntry` and `AuditEntry`

```
HistoryEntry {
  row_id: string
  viewer_id: Id22 | null
  row_code: RowCode | null
  admission_ids: Id22[]
  first_at: Ms
  last_at: Ms
  end: "left" | "kicked" | "revoked" | "denied" | "knock_timeout" | "stream_ended"
  connections: { connection_id: ConnId, joined_at: Ms, left_at: Ms | null, ip: string | null, geo, device: string | null }[]
}

AuditEntry {
  id: string
  at: Ms
  stream_id: StreamId | null
  actor: { kind: "key" | "session" | "server", key_id: string | null }
  op: string
  target: { admission_id?: Id22, knock_id?: Id22, connection_id?: ConnId }
  result: "ok" | "error"
  error_code: string | null
  idempotency_key: string | null
}
```

The server's audit covers only what reaches the server. Local commands (plugin, CLI, sender window) never pass through it. Their effects show up in `SenderState` and in local `pin_changed` alerts.

## 5. State machines

### 5.1 Sender phase

```
starting ──host created, joined /signal──▶ picking ──first encoded frame──▶ live
picking  ──pause──▶ paused        live ──pause──▶ paused
paused   ──local resume──▶ live (if capture attached) | picking (if not)
any      ──stop / stream.ended / window closed / state_lost──▶ ended   (terminal)
```

A reload re-enters `starting` and comes back to `paused` if the latch is set, never to `live`.

### 5.2 Sender attachment (daemon)

"Authenticated with K" means the full exchange of §11.2 completed with key K on that connection. Nothing else moves this machine.

```
none ──start──▶ launching (launch key minted; expires 60 s after minting)
launching ──authenticated with the launch key──▶ handover (attachment key made and fsynced on the first time; init sent)
handover ──socket drops──▶ launching (same launch key, same attachment key)
handover ──init_ack──▶ attached (launch key retired, launch file deleted)
launching | handover ──authenticated with the attachment key──▶ attached (launch key retired)
launching | handover ──60 s after minting──▶ ended (browser_failed)
attached ──socket drops──▶ detached
detached ──authenticated with the attachment key──▶ attached
attached ──authenticated with the attachment key on another socket──▶ attached (old socket gets `superseded`)
detached ──10 s, or the browser exits──▶ ended (window_closed)
daemon start with host-state.json ──▶ detached (browser re-adopted, HOST-CONTROL §5.9; else ended (window_closed))
```

An exchange that fails, times out or is abandoned leaves the machine where it was. The daemon serialises the exchanges of one stream; the last to complete is the attachment. `HostStatus.attachment` shows `handover` as `launching`.

### 5.3 Knock

```
(Tb verified, knock on) ──▶ pending ──approve(row_code ok) + commit ok──▶ admitted
pending ──▶ ended(reason)   KnockEnd = "denied" | "timeout" | "peer_closed" | "replaced"
                                       | "pin_rotated" | "stopped" | "commit_failed" | "queue_full"
```

`ended` is terminal. The `knock_id` is bound to one handshake and is never reused. `approve` on an ended or unknown id → `unknown_knock`.

### 5.4 PIN rotation (sender)

```
idle ──request fails validation──▶ idle                     (reply invalid_argument | passphrase_required; nothing written)
idle ──valid request──▶ intent_persisted (pending_rotation written) ──▶ applying
applying ──library ok──▶ committing (pin_state written, pending cleared) ──▶ idle   (reply ok)
applying ──storage──▶ retrying (same value, same version; 100 ms → 2 s) ──▶ applying   (first failure replies storage {pending: true})
applying ──other error, first call──▶ idle (pending cleared)  (reply that error)
retrying ──other error──▶ mismatch (latch locked and frozen, alert state_mismatch)
mismatch ──local rotation──▶ intent_persisted (version max(page, library) + 1)
```

`reconciling` is true in every state except `idle`. A new request outside `idle` and `mismatch` → `rotation_pending`. The startup reconciliation table is in HOST-CONTROL §5.5.

| Reply | Class | What the caller does |
|---|---|---|
| `ok` | done | — |
| `invalid_argument`, `passphrase_required` | **terminal** for that request; nothing was written | Fix the request. Resending it unchanged gets the same answer. An unchanged PIN (after NFC, `none` → `none` included) is in this class. |
| `rotation_pending` | **retryable**; nothing was written | Wait until `access.rotation_pending` is `false`, then resend. |
| `storage` with `details.pending: true` | **accepted**; the sender finishes it alone | Don't resend. Watch `access.rotation_pending`. |
| any other error | **terminal**; the intent was cleared | Read the state before deciding anything new. |

### 5.5 Remote command (server)

```
received ──not allowed remotely──▶ 403 local_owner_required
received ──duplicate id, same op and args──▶ cached response (or wait for the in-flight one)
received ──duplicate id, different op or args──▶ 409 idempotency_conflict
received ──sender offline──▶ 409 sender_offline
received ──▶ forwarded ──terminal result──▶ response (cached 10 min)
forwarded ──retryable result──▶ response (not cached; same forwarding id on retry)
forwarded ──5 s──▶ 504 sender_timeout (not cached; the command may still apply)
```

## 6. The join (normative)

```
Join(in: JoinInput, mem: JoinMemory) → (ViewerList, JoinMemory, HistoryEntry[])

JoinInput {
  source: "local" | "server"
  now: Ms                         // the caller's clock
  state: SenderState | null       // the newest accepted copy (§4.4); null before the first
  state_at: Ms | null             // the caller's clock when `state` was received; null exactly when state is null
  publisher_online: bool          // server: a publisher socket is joined for the stream. Daemon: the page is attached (§5.2).
  stream_ended: bool              // the stream has ended (DELETE, expiry, or phase "ended")
  conns: Connection[]             // every connection of the stream open or left within the last 5 min.
                                  //   Daemon: from /connections, polled every 2 s while live; [] while unreachable.
}

JoinMemory {
  rows: { row_id: string, snapshot: Viewer, left_at: Ms | null }[]   // rows from the last call
  ends: End[]                     // the union of state.ends seen within the last 5 min, by admission_id or knock_id
}
```

It's pure and deterministic: the same inputs give the same outputs, so the daemon and the server agree on the same facts. The removal cause comes only from `End` records (`state.ends`, remembered in `mem.ends` so a later state that no longer lists one still has it).

1. **Freshness.** `sender_online = publisher_online`. `sender_stale = !publisher_online || state_at == null || now − state_at > 10000`. Stale sender data is still used, and every row built from it gets the `sender_stale` flag.
2. **Group.** Admissions are grouped by `viewer_id`, one row `v:<viewer_id>` per group. `admission_ids` are sorted newest first.
3. **Claims.** An admission claims the connection named by its `peer_id`. A claim is *live* if the admission is `connected`, *historical* otherwise.
   - A connection with live claims from exactly one `viewer_id` attaches to that row. Several admissions of the same viewer on one connection, such as a grant re-admission, are one claim.
   - A connection with live claims from two or more `viewer_id`s attaches to none of them, and each of those rows gets `shared_connection`.
   - A connection with only historical claims attaches to the row whose claiming admission is newest. If two rows tie, it attaches to neither.
   - A `peer_id` that appears in several historical claims (a connection reused by a later admission) follows the same rules. It's never an error by itself.
4. **No connection.** A row with a connected admission whose `peer_id` names no open connection gets `no_connection`.
5. **Knocks.** Each knock becomes row `k:<knock_id>`. It attaches to its `peer_id` connection unless that connection has a live claim.
6. **Joining.** Every open connection that nothing claims or attaches to becomes row `c:<connection_id>` with status `joining`. It doesn't count as watching.
7. **Status** of an admission row:
   - `watching` if any admission is connected;
   - `reconnecting` if none is, but an attached connection is open;
   - `left` otherwise.
   A row whose admissions all disappear from the state (kicked, or removed by a rotation) becomes `left` from its `JoinMemory` snapshot.
8. **The 5-minute rule and history.** A `left` row keeps `left_at` (`now` at the first call that saw it left) and stays in `viewers` for 5 minutes. It then leaves `viewers` and is returned as a `HistoryEntry` (the server appends it to §9.4). Its `end`:
   - for an admission row whose admissions all ended: the strongest cause among their `End`s, in the order `kicked` > `revoked` > `stream_ended`; `left` if none has an `End`;
   - for an admission row that left only by disconnecting (its admissions are still listed): `left`;
   - for a knock row, at once when the knock leaves `state.knocks`, by its `End.reason`: `denied` → `denied`; `timeout` → `knock_timeout`; `pin_rotated`, `commit_failed` → `denied`; `stopped` → `stream_ended`; `peer_closed`, `replaced`, `queue_full`, or no `End` → `left`;
   - when `stream_ended` is true: every remaining row at once, with its cause if it has one, else `stream_ended`.
   A row id that comes back (a viewer reconnecting with its grant after its row went to history) starts a new row.
9. **Counts:** `watching` is the number of rows with status `watching`; `knocking` the number with status `knocking`.

## 7. Authentication

| Credential | How | Scopes |
|---|---|---|
| API key | `Authorization: Bearer <key>` | `read`: GETs and events. `streams`: plus create, delete and restrictive commands. `owner`: plus `/keys`. |
| Dashboard session | Cookie `ps_session` from `POST /api/v1/session` | `min(key scope, streams)`: a session never has `owner`. |
| Publish token | Inside the publisher `join` | One stream, publisher role. |
| View capability | Inside the viewer `join` | One stream, viewer role. |

- A request with an `Origin` header must match the base URL. Cookie-authenticated non-GET requests **must** carry a matching `Origin`.
- Cookie: `HttpOnly; Secure; SameSite=Strict; Path=/`. `Secure` is dropped only on loopback http. Lifetime 12 h, 1 h idle.
- Remote callers never get permissive ops, whatever their scope.

**Session:** `POST /api/v1/session` with `Authorization: Bearer <key>` and an empty body → `204` and the cookie, or `401 unauthorized`. **The key travels as a bearer token here too, never in a body** (§2): the dashboard's login form puts the typed key in that one request's header and then drops it. A request to this endpoint that also carries a cookie ignores the cookie. `GET /api/v1/session` → `{scope, expires_at}`. `DELETE /api/v1/session` → `204`, revoked server-side.

**Keys** (bearer `owner` only; a cookie gets `403 forbidden_scope`):

- `POST /api/v1/keys {label: string, scope: "read" | "streams" | "owner", expires_in_s?: uint | null = null}` → `201 {id, key, label, scope, created_at, expires_at}`. The key is shown only here.
- `GET /api/v1/keys` → `{keys: [{id, label, scope, created_at, expires_at, last_used_at}]}`.
- `DELETE /api/v1/keys/{id}` → `204`. Deleting the last `owner` key → `409 conflict`.

## 8. Signaling WebSocket `/signal`

`/signal` accepts any `Origin`, because it uses no ambient credentials: the credential is inside `join`. The local sender runs on `http://127.0.0.1:47823`.

### 8.1 `join`

```
→ {"type": "join", "role": "publish", "id": StreamId, "credential": "<publish_token>", "protocol": 1, "sender_kind": "local" | "browser", "instance": Id22}
→ {"type": "join", "role": "view", "credential": "<capability>", "protocol": 1, "label"?: string}
← {"type": "joined", "stream_id": StreamId, "connection_id": ConnId, "sender_online": bool, "sender_instance": Id22 | null, "pin_hint"?: PinPolicy}
← {"type": "error", "code": "upgrade_required" | "unauthorized" | "stream_ended"}   then close
```

- `label` is viewer-reported and sanitised.
- `pin_hint` (viewers only) is the PIN policy from the sender's latest `SenderState`, else the creation policy. It is **advisory**: the viewer uses it to decide whether to show the PIN screen before its first handshake, because probing with an empty PIN would cost a guess (PROTOCOL.md §6). The sender's signed `ADa` flag decides; a wrong hint costs at most an unneeded PIN screen, or one failed guess, which the server can cause anyway.
- **A stream that ends closes its viewers' sockets with WebSocket close 1008, reason `stream_ended`** (stop, delete, expiry), so a viewer shows "ended" instead of reconnecting into a refused join, which it can't tell from a kick. A client must not reconnect after a 1008 close. The viewer treats the close as terminal at once: it drops its keys and media in the close handler, and its screen never leaves "ended". The 1008 close is not signed by the sender. It is an availability/status assertion from the relay, which can stop relaying a stream at any time anyway, so it can't be used to weaken privacy: it can only end a viewer's session early.
- **Every signaling connection is a new relay peer.** A viewer whose socket reconnects (including a peer-connection replacement) gets a new `connection_id`; the publisher sees `peer_closed` for the old one, and the viewer re-admits with its grant on the new one. Media keys already delivered stay valid meanwhile.
- The viewer heartbeat (the unordered data channel) gains `hidden?: bool`.

### 8.2 E2EE relay envelope

The payload `b` is the b64url of one PROTOCOL.md §4 message (≤ 4096 bytes decoded). The server never parses it, except to check the size.

| Direction | Message |
|---|---|
| viewer → server | `{"type": "e2ee", "b": string}` |
| server → publisher | `{"type": "e2ee", "peer": ConnId, "b": string}`, where `peer` is the sending viewer's `connection_id` |
| publisher → server | `{"type": "e2ee", "to": ConnId, "b": string}` |
| server → viewer | `{"type": "e2ee", "b": string}` |

- Order is preserved per viewer in both directions.
- Viewer → publisher: 16 messages per second per viewer, burst 32. The excess is dropped and counted.
- **Annotation envelopes** (payload version 1, type `0x09` or `0x0A`, PROTOCOL.md §20) have their own budgets, separate from these. Admission, rekey and ACK traffic never competes with them.
  - Viewer: 10/s, burst 20.
  - Viewer IP: 32/s, burst 64.
  - Viewers per stream: 128/s, burst 256.
  - Publisher IP: 512/s, burst 1024.
  - Publisher per stream: 256/s, burst 512.
  - An annotation envelope is relayed only while its destination queue is under a quarter full. The excess is dropped and counted (`relay_annotation_rate`, `relay_annotation_queue`) and never closes a peer.
  - The sender uses a long-run aggregate budget of 160 annotation envelopes/s with a tick burst: one batch per viewer per tick, ticks spaced by viewer count.
  - Annotation clients enforce authenticated `(epoch, counter)` ordering before and after decrypt; retained media keys do not permit stale annotation epochs. The browser app uses an authenticated router-lifetime BOOTSTRAP before enabling drawing and namespace-bound causal INPUT/RECEIPT/INK/SOURCES bodies (PROTOCOL §20.1). Re-admission fences old channel output and rebases reset host counters; concurrent admissions sharing a display row retain separate causal ink identities in fixed 500-slot tables. Within that lifetime, repeated local-clear cutoffs preserve later local ink, while host clear-all/forced-origin barriers invalidate older optimistic input, even when its upstream envelope was lost. Receipts share the existing 512-byte body / 2048-byte batch / 64-record bounds and paced envelopes; the relay never parses these encrypted bodies.
- Server → viewer: a queue of 64. Overflow closes the viewer with `peer_closed` reason `overflow`.
- A message to an unknown or closed `to` is dropped. The publisher learns about it from `peer_closed`.
- While no publisher is connected, viewer `e2ee` messages are dropped.

### 8.3 Peer lifecycle

| Direction | Message |
|---|---|
| server → publisher | `{"type": "e2ee.peer_closed", "peer": ConnId, "reason": "left" \| "timeout" \| "kicked" \| "banned" \| "overflow"}` |
| server → publisher | `{"type": "e2ee.peers", "peers": ConnId[]}`, sent right after every publisher `joined` |
| publisher → server | `{"type": "e2ee.close", "to": ConnId}`: the server closes that viewer's connection |
| server → viewer | `{"type": "e2ee.sender_back", "instance": Id22}`, when a publisher joins |
| server → viewer | `{"type": "e2ee.sender_away"}`, when the publisher disconnects |

The sender page calls `host.peerClosed(peer)` for each `peer_closed`, and for every known peer missing from `e2ee.peers`. A viewer that gets `sender_back` with a different `instance` from the one it was admitted under re-runs the grant handshake after a random 0–2 s delay. With the same `instance` it keeps its session, unless it sees frames of an epoch it has no key for, for more than 2 s.

Kick and PIN rotation cancel unfinished revoked-grant handshakes with the existing transcript-bound signed `grant_invalid` REJECT; rotation answers invalidated PIN checks with signed `expired`. No new relay message or authentication shortcut is introduced. Viewer HELLO and RESPONSE progress is bounded to 5 s per phase, except an unanswered PIN HELLO waits for a known-offline host and automatically starts a fresh phase on return. An answered CHALLENGE never causes automatic PIN reuse, including across host departure. Other PIN timeouts return to manual input; grant/no-PIN checks make at most five automatic retries before a local `admission_timeout` card. Loss alone preserves the remembered grant; only an authenticated refusal clears it. Every handshake reply, including knock approval errors and replacement teardown, requires current ownership of that peer's handshake. A new HELLO does not revoke a claimed admission. Signed pending-knock deadlines and admitted-viewer missing-key recovery retain their separate behavior.

### 8.4 E2EE message additions (PROTOCOL.md changes)

| Change | Detail |
|---|---|
| Message `0x08 PENDING`, S→V | One field `box` (22 bytes = 6 bytes of plaintext + the 16-byte GCM tag): AES-256-GCM(`pending_key`, `0^12`, aad `"PassStream v1 pending"`, `u16(code_index) \|\| u32(timeout_ms)`). `pending_key = HKDF-SHA-512(ISK, salt=sid, info="PassStream v1 pending", 32)`. The message is 25 bytes: version, type, a 1-byte LEB128 length, the box. At most one per handshake, so the fixed nonce is never reused under a key. `code_index` > 10647 → `malformed`. |
| REJECT reason 8 `denied` | Knock denied, timed out, or the queue is full (`retryAfterMs` 30000). Signed with `nS` as usual. |
| Admit bundle +2 bytes (97) | `… \|\| u16(code_index)`, after the 95 bytes of PROTOCOL.md §5.5; the ADMIT `box` is 113 bytes. `code_index` > 10647 → `malformed`. No colour on the wire (§4.3). |
| Viewer state `pending` | Between sending RESPONSE and receiving ADMIT or REJECT, after a valid PENDING. The viewer waits up to `timeout_ms` + 5 s. |
| Messages `0x09 ANNOTATION` (V→S) and `0x0A ANNOTATIONS` (S→V) | End-to-end encrypted laser pointer and strokes (PROTOCOL.md §20, FEATURES.md "Track D2"). Opaque `e2ee` envelopes like every other library message, within the same per-viewer relay limits. Each viewer's messages go only to the sender, and the sender re-seals them for each other viewer. The server sees their sizes and timing only. |

The server sees these as opaque `e2ee` envelopes. Knocks are visible to it only through `SenderState.knocks`.

### 8.5 Publisher control channel

| Direction | Message |
|---|---|
| server → sender | `{"type": "control", "id": string, "op": string, "args": object, "source": {"kind": "dashboard" \| "api", "key_id": string \| null}, "auth"?: object}` |
| sender → server | `{"type": "control.result", "id": string, "ok": true, "result": object}` |
| sender → server | `{"type": "control.result", "id": string, "ok": false, "error": Error}` |
| sender → server | `{"type": "sender.state", "state": SenderState}` |
| sender → server | `{"type": "alert", "alert": Alert}` (sender-emitted kinds only, never `pin_changed`) |
| server → sender | `{"type": "stream.ended", "reason": "deleted" \| "expired"}` |

**Remote argument schemas (closed).** These are the only ops the sender runs from `control`:

| `op` | `args` | `result` |
|---|---|---|
| `stop` | `{}` | `{}` |
| `pause` | `{}` | `{}` |
| `mode` | `{mode: "text" \| "motion"}` | `{}` |
| `safe` | `{on: true}` | `{}`. Turns safe mode on (FEATURES.md "Safe mode"). |
| `lock` | `{locked: true}` | `{}` |
| `kick` | `{admission_id: Id22, lock?: bool = false}` | `{admission_ids: Id22[], locked: bool}` |
| `knock.deny` | `{knock_id: Id22}` | `{}` |
| `annotations` | `{viewers: false}` | `{viewers: false}`. Viewers may no longer draw (the default). |
| `annotations.clear` | `{}` | `{}`. Clears every annotation on every overlay. |

- `lock {locked: false}`, `annotations {viewers: true}`, `safe {on: false}`, any other op, or a local-only argument (`rotate_pin`, `pin`, `keep_pin`, `row_code`, `policy`) → `local_owner_required`, and **nothing is applied**.
- Any other unknown argument → `invalid_argument`.
- `kick` with `lock: true` sets the lock latch before it awaits the kick. A failed kick returns its error with `details: {locked: true}`.
- `id` is the server's forwarding id (§9.6). The sender caches only terminal results of the last 256 ids per page instance; a repeated id with the same op and arguments shares the in-flight or terminal result. Storage, internal and rotation-pending errors may be retried with the same id; accepted writes must be watched rather than resubmitted (§9.6).
- `auth` is reserved for v2 and ignored.

## 9. HTTP API

### 9.1 Streams

| Method and path | Scope | Body → response |
|---|---|---|
| `POST /api/v1/streams` | streams | `CreateStream` → `201 Stream & {view_link_base, publish_token, host_url}` |
| `GET /api/v1/streams?state=live\|ended\|all&limit=50&cursor=` | read | → `{streams: Stream[], next_cursor: string \| null}` |
| `GET /api/v1/streams/{id}` | read | → `Stream` |
| `DELETE /api/v1/streams/{id}` | streams | → `204`. Ends the stream, revokes publish and view, closes media, sends `stream.ended`. Idempotent. |

```
CreateStream {
  label?: string | null = null
  pin?: PinPolicy                 // absent: settings.defaults.pin
  pin_explicit?: bool = false     // true: use `pin` as given, even if weaker than the defaults
  knock?: bool                    // absent: settings.defaults.knock
  expires_in_s?: uint | null = null
  sender_kind?: "local" | "browser" = "browser"
}
```

`declared.pin` = `pin_explicit ? pin : stricter(pin, defaults.pin)`. `declared.knock = requested.knock OR defaults.knock`, including explicit PIN choices (Appendix A #7). **Defaults apply here and nowhere else.** Changing them never affects an existing stream. A daemon-launched sender uses its own creation policy (HOST-CONTROL §4.4), and `declared` is informational.

### 9.2 Viewers and connections

| Method and path | Scope | Response |
|---|---|---|
| `GET /api/v1/streams/{id}/viewers` | read | `ViewerList` with `source: "server"` |
| `GET /api/v1/streams/{id}/connections` | read | `{connections: Connection[]}`: server facts only, open or left in the last 5 min |

### 9.3 Events: `GET /api/v1/streams/{id}/events`

Server-sent events. On connect the server sends the current `stream` and `viewers`, then changes:

```
event: stream    data: Stream
event: viewers   data: ViewerList        (at most 1 per second)
event: alert     data: Alert
event: ended     data: {"reason": end_reason}
```

There's a comment line every 15 s. Events aren't replayed; a reconnecting client gets fresh snapshots and reads `/history` and `/audit` for what it missed.

### 9.4 History: `GET /api/v1/streams/{id}/history?since=<Ms>&limit=100&cursor=`

Scope `read` → `{entries: HistoryEntry[], next_cursor: string | null}`, oldest first. It holds rows that left the live list (§6 rule 8) and ended knocks. It's kept for the stream's lifetime and then as long as the stream is listed. IPs are `null` unless `retention.keep_ip`.

### 9.5 Audit: `GET /api/v1/audit?stream_id=&since=<Ms>&limit=100&cursor=`

Scope `read` → `{entries: AuditEntry[], next_cursor: string | null}`, newest first. Every command, stream creation and deletion, key change and session login is recorded. Never secrets.

### 9.6 Commands: `POST /api/v1/streams/{id}/commands`

Scope `streams`. Request:

```
Command {
  id?: string           // idempotency key, 1–64 chars of [A-Za-z0-9_-]; absent: no deduplication
  op: string
  args?: object = {}
}
```

| `op` | `args` | `result` |
|---|---|---|
| `stop` | `{}` | `{}`. Same as `DELETE`; works while the sender is offline. |
| `pause`, `mode`, `safe`, `lock`, `kick`, `knock.deny`, `annotations`, `annotations.clear` | As in §8.5 | As in §8.5 |
| `ban` | `{connection_id: ConnId}` | `{ip_banned: true, admission_ids: Id22[]}`. The server bans the IP for this stream, then sends `kick` for the admissions it attributes to that connection. |
| `resume`, `unfreeze`, `pin.rotate`, `pin.set`, `knock.approve`, `lock {locked: false}`, `annotations {viewers: true}`, `safe {on: false}` | — | `403 local_owner_required` |

**Idempotency.**

- The key is (principal, stream, `id`). The principal is the key id, or the session's key id.
- The server keeps only terminal completed responses for 10 minutes. The same key with the same `op` and canonical `args` gets the same status and body, with the header `Idempotent-Replay: true`. While the first request is still in flight, the repeat waits for it.
- The same key with a different `op` or `args` → `409 idempotency_conflict`.
- Retryable responses are never cached: HTTP 429 and 5xx, plus `sender_offline`, `not_live`, `rotation_pending` and `browser_failed`. A `storage` response with `details.pending: true` or `details.applied: true` is accepted and terminal for that command: the sender finishes it alone. A new request after a retryable attempt re-executes with the **same** forwarding id; concurrent waiters receive the attempt they joined. The server retains the fingerprint and forwarding id for 10 minutes, within the same bounded cache, to reject conflicting reuse and let the sender deduplicate an attempt that already ran.
- Scope limits: a server restart clears the server's cache, and a sender reload clears the sender's. Every remote op is restrictive and safe to repeat. A repeated `kick` whose first attempt landed answers `404 unknown_admission`; re-read the state.

**Pending restrictions.** Unacknowledged kick/lock commands (including Ban-generated kicks and retryable answers) are replayed on publisher reconnect for at most 10 minutes after their most recent forwarding attempt, then released from the 1024-entry queue. A caller retry within the command identity window keeps its forwarding id and refreshes that queue age. A terminal sender answer or stream end releases the entry immediately. After expiry, re-read state before deciding whether a new restriction is needed.

**Responses:** `200 {id, ok: true, result}`. Sender offline: `409 sender_offline` (commands aren't queued). No result in 5 s: `504 sender_timeout`. Sender errors pass through with their status (§14). Every command is written to the audit log.

### 9.7 Settings and health

- `GET /api/v1/settings` (read) / `PUT /api/v1/settings` (streams):
  ```
  Settings {
    defaults: { pin: PinPolicy, knock: bool, max_viewers: uint, expires_in_s: uint | null }
    retention: { keep_ip: bool }
  }
  ```
  `defaults` apply only at stream creation (§9.1).
- `GET /healthz`, `GET /readyz`, `GET /llms.txt`, `GET /skill.md`: no auth.

### 9.8 Endpoint and command inventory

This inventory is checked against route registration and command dispatch. API endpoints require the scopes above. The uncredentialed routes are health/readiness, this contract, its skill pointer, signaling (authenticated by `join`) and static SPA pages. The SPA fallback serves `/`, `/host`, `/v`, `/dashboard` and its client-side subpaths, `/privacy` and `/assets/*`; it also returns the home page for unknown non-API paths. The production `/sender` and `GET /local/v1/sender` are served by the local daemon, with the Host/peer/Origin checks in §11. `/api/` is a JSON not-found fallback, not an additional endpoint.

Public server routes:

- `GET /healthz`
- `GET /readyz`
- `GET /llms.txt`
- `GET /skill.md`
- `GET /signal`
- `POST /api/v1/streams`
- `GET /api/v1/streams`
- `GET /api/v1/streams/{id}`
- `DELETE /api/v1/streams/{id}`
- `POST /api/v1/streams/{id}/commands`
- `GET /api/v1/streams/{id}/viewers`
- `GET /api/v1/streams/{id}/connections`
- `GET /api/v1/streams/{id}/events`
- `GET /api/v1/streams/{id}/history`
- `GET /api/v1/audit`
- `GET /api/v1/settings`
- `PUT /api/v1/settings`
- `GET /api/v1/session`
- `POST /api/v1/session`
- `DELETE /api/v1/session`
- `GET /api/v1/keys`
- `POST /api/v1/keys`
- `DELETE /api/v1/keys/{id}`

Remote command inventory (tightening/neutral, authorized key or dashboard session):

- HTTP command `stop`
- HTTP command `pause`
- HTTP command `mode`
- HTTP command `safe`
- HTTP command `lock`
- HTTP command `kick`
- HTTP command `knock.deny`
- HTTP command `annotations`
- HTTP command `annotations.clear`
- HTTP command `ban`

Loosening commands (`resume`, unlock via `lock {locked:false}`, `unfreeze`, `pin.rotate`, `pin.set`, `knock.approve`, viewer drawing on via `annotations {viewers:true}`, safe mode off via `safe {on:false}`) and any local-only argument return `403 local_owner_required` from HTTP, including loopback callers and owner keys. Tightening commands are allowed from any channel with that channel's authentication.

## 10. Local daemon socket (plugin, CLI)

`$XDG_RUNTIME_DIR/passstream/host.sock`, held by `passstream-host.socket`: directory `0700`, socket `0600`, and the `SO_PEERCRED` uid must match. UTF-8 JSON, one object per line. Request `id`s correlate replies on one connection only; they aren't idempotency keys.

```
→ {"id": "1", "op": "hello", "args": {"client": "plugin" | "cli" | "shell", "protocol": 1}}
← {"id": "1", "ok": true, "data": {"protocol": 1, "version": "0.3.0", "server_url": "https://stream.example"}}
← {"id": "9", "ok": false, "error": Error}
← {"event": "status" | "alert" | "link_ready" | "live" | "ended", "data": …}
```

`hello` must come first; otherwise the reply is `protocol_mismatch` and the socket closes. Events go only to connections that sent `subscribe`.

| `op` | `args` (closed) | `data` |
|---|---|---|
| `status` | `{}` | `HostStatus` |
| `subscribe` | `{}` | `{}`, then events |
| `start` | `{pin_policy?: PinPolicy, pin?: string, knock?: bool = false, label?: string \| null = null, mode?: "text" \| "motion" = "text", safe?: bool = true}` | `{stream_id}`. `pin_policy` present means an explicit choice. `pin` is allowed only with `pin_policy` (and required for `passphrase`). `safe: false` starts with safe mode off. |
| `stop`, `pause`, `resume` | `{}` | `{}` |
| `mode` | `{mode}` | `{}` |
| `safe` | `{on: bool}` | `{}`. Safe mode on or off. Off is local only: the server can't send it (§8.5). Not a latch; the daemon mirrors it from `SenderState.safe`. |
| `lock` | `{locked: bool}` | `{}` |
| `unfreeze` | `{keep_pin?: bool = false, pin?: string, action_id?: string}` | `{}`. Without `keep_pin` it rotates (sender-generated unless `pin`). |
| `kick` | `{admission_id: Id22, lock?: bool = false, rotate_pin?: bool = false, pin?: string, action_id?: string}` | `{admission_ids, locked: bool}`. `pin` only with `rotate_pin`. |
| `pin.rotate` | `{pin?: string, action_id?: string}` | `{}`. Same policy, new value. |
| `pin.set` | `{policy: PinPolicy, pin?: string, action_id?: string}` | `{}`. `passphrase` requires `pin`. |
| `pin.reveal` | `{}` | `{pin: string \| null, pending_pin: string \| null}` |
| `link` | `{}` | `{link: string}`, or `link_not_ready` |
| `knock.approve` | `{knock_id: Id22, row_code: RowCode, action_id?: string}` | `{admission_id}`. A non-canonical `row_code` → `invalid_argument`; a different one → `code_mismatch` (§4.3). |
| `knock.deny` | `{knock_id: Id22}` | `{}` |
| `annotations` | `{viewers: bool}` | `{viewers: bool}`. Lets viewers draw on the shared overlay, or stops them. Host-only (`false`) by default and after every sender reload; not a latch. |
| `annotations.clear` | `{}` | `{}`. Clears every annotation. |
| `ban` | `{connection_id: ConnId, action_id?: string}` | `{ip_banned, admission_ids}`, unwrapped from the server command result. |
| `frame` | `{after?: string, wait?: uint = 0, max_width?: uint = 0}` | `LocalFrame` (§10.1). Wait ≤ 30 seconds. |
| `capture.auth` | `{}` | `{token, origin, expires_in_s: 300}`: explicit local owner HTTP credential. |
| `record.start` | `{duration?: uint = 120, max_bytes?: uint = 33554432}` | `{recording, completed, bytes, error}`. Duration 1–120 s; bytes 1024–32 MiB. |
| `record.stop`, `record.status` | `{}` | `{recording, completed, bytes, error}`. Stop waits for bounded local transfer. |
| `publish` | `{kind: "snapshot" \| "recording", service: "passfile" \| "passpage", max_width?: uint = 0}` | `PublishRequest` (§10.1), initially `pending`. The owner confirms on screen in the sender; recordings support PassFile only. |
| `publish.status` | `{request_id: string, wait?: uint = 0}` | `PublishRequest`; wait ≤ 30 s, for the current/latest request. |
| `shutdown` | `{force?: bool = false}` | `{}`. Refused while live unless `force`, which stops first. |

**Action identity.** `action_id` is 1–64 characters of `[A-Za-z0-9_-]`. The plugin and CLI generate one per Ban, Kick, unfreeze, PIN change and knock approval. The plugin sends each action once and does not automatically retry; a later click is a new action. The CLI reuses the id for its bounded Ban retries and when the caller supplies `--action-id` for a retry. Separate clicks use separate ids. The daemon forwards a Ban's action id as the HTTP command id. Other listed actions use a stable sender command id derived from (stream, action id); `action_id` itself is removed from sender args. The daemon shares concurrent attempts, caches terminal responses for 10 minutes (at most 256 per stream, oldest completed evicted first), rejects differing op/args as `idempotency_conflict`, and retains only HMAC-SHA-256 fingerprints of arguments under a random per-process key. The page uses a random per-context HMAC key too, so cached PIN arguments cannot be enumerated from an unkeyed digest. Retryable sender failures aren't cached. The page's cache also checks op/args. After a daemon restart the page's cache still deduplicates stable ids, but a page reload or cache eviction clears that protection: re-read state before retrying an uncertain non-idempotent action. Without `action_id`, each call is a new action. A request line's `id` is always correlation only. `start` is a single launch attempt; clients reconcile status before deciding to launch again.

Events: `status` (`HostStatus`, at most 4 per second), `alert` (`Alert`), `link_ready {stream_id}`, `live {stream_id}`, `ended {stream_id, reason}`.

**Latches and acknowledgements.** For `pause`, `lock {locked: true}` and `kick` with `lock`, the daemon fsyncs the latch into `host-state.json` before it forwards the command, and replies after the page's result (HOST-CONTROL §5.6). While the page is detached, `pause` and `lock {locked: true}` reply `{}` (they reach the page through `init.latches`); every other command gets `sender_offline`. PIN changes reply as in §5.4.

A failed mirror write leaves it dirty. Restrictive latch commands retry the write before forwarding or acknowledging, even if the latch is already set in memory; they return `internal` while it fails. A dirty `state` receives no `state_ack`. Authentication while dirty retries the write before `init` or superseding the old page; failure drops the socket without an authoritative error, so the page reconnects. The daemon reports its own latches only as saved on disk, retries writes with 1 s, 2 s, … up to 5 s backoff, and after three consecutive failures emits `revocation_durability {not_durable: true}` and sets `stream.e2ee.revocation_not_durable`. A successful write emits `not_durable: false` and clears it. Sender-reported durability failures remain visible independently.

```
HostStatus {
  daemon: {
    version: string
    server_url: string
    key: "ok" | "missing" | "unsafe" | "invalid" | "rejected"
    server: "ok" | "unreachable" | "unauthorized"
    port_mode: "socket_activated" | "self_bound"
    supervision: "systemd" | "process_group"   // HOST-CONTROL §5.9
    port_owner: "ok" | "not_owned"
  }
  stream: {
    id: StreamId
    label: string | null
    phase: "starting" | "picking" | "live" | "paused" | "ended"   // "starting" until the page attaches
    attachment: "launching" | "attached" | "detached"
    mode: "text" | "motion"
    safe: bool
    started_at: Ms
    live_at: Ms | null
    access: Access | null         // null until the first SenderState
    e2ee: SenderState.e2ee | null
    health: Health | null
    watching: uint
    knocking: uint
    viewers: Viewer[]             // the daemon's join, source "local"
    has_link: bool
  } | null                        // null when idle
}
```

**`HostStatus` never contains the PIN or the link.**

### 10.1 Owner capture and publishing (local only)

The server **cannot provide plaintext frames**. There is no
`GET /api/v1/streams/{id}/frame`, recording or publish endpoint. Remote agents
open the full viewer link (`/v#c=…&k=…&s=…`), enter the PIN separately, complete
CPace and any knock approval, and watch through the viewer page like any viewer.
Server API keys do not grant access to plaintext, local captures or uploads.

The local sender samples only while a frame request/waiter was seen within the
last 30 seconds, or while a snapshot publish awaits confirmation. Otherwise it
does no readback or encoding and retains no snapshot/decoded/scale buffers.
While demanded, it samples at most twice per second, compares exact RGBA pixels,
and emits a new opaque change ID only for a changed picture. PNG is native
resolution, bounded to 3840×2160 and 4 MiB. The daemon keeps the last four PNGs
in memory while demanded. PNG validation runs outside the daemon control lock.
Only one downscale runs at a time. A bounded four-entry cache keys scaled PNGs
by (frame ID, width); decoded pixels for only the current frame are retained.
Scaling works directly on the pixel buffer. `max_width` (0–3840, default 0) explicitly reduces output width,
preserving aspect ratio and the original ID; it never enlarges a picture.

```
LocalFrame = {status: "frame", id: string, at: Ms, width: uint, height: uint,
              mime: "image/png", data: string}  // standard padded base64 PNG
           | {status: "unchanged", id: string} // wait elapsed with no new picture
           | {status: "paused"}               // no bytes or previous ID
           | {status: "idle"}                 // demand expired, nothing retained
           | {status: "too_large" | "stale", reason?: string}
```

With no `after`, return the current picture. With its current ID, wait at most
`wait` seconds for a changed picture; an unknown/evicted ID gets the current
picture immediately. A pause, privacy curtain (the pause latch), encryption
suspension, disabled/muted/ended track, detach, supersede or stream end clears
all retained captures and wakes waiters with `paused`. Pending sender encodes
are generation-fenced. Resume requires a fresh sender capture; no prior frame
is returned. Oversized source/PNG and failed readback send `too_large`/`stale`
markers and erase the previous picture; `after` returns that marker immediately,
never `unchanged`. Lock/admission freeze is not a capture gate.
There is no capture feed for the server-served `/host` sender.

Recordings use the sender's plaintext capture and MediaRecorder WebM (including
capture audio when present), never the server. They are explicitly started and
stopped, capped at 120 seconds and 32 MiB. Privacy invalidation discards the whole
recording, including a completed one. There is no automatic upload. A size
overflow discards rather than publishing a truncated file. Status errors include
`capture_too_large`, `capture_failed`, `recording_too_large`, `recording_failed`,
`not_supported`, `paused`, and `upload_failed` (all local).

Local HTTP routes on the daemon's loopback listener only:

- `GET /local/v1/frame?after=&wait=0&max_width=0` → `LocalFrame`.
- `POST /local/v1/features` with `{op, args?: object = {}}` → the corresponding
  Unix result. Allowed: `frame`, `record.start`, `record.stop`, `record.status`,
  `publish`, `publish.status`. This route cannot mint credentials or confirm an upload.
- `GET /owner?stream_id=<StreamId>` → the local owner action page, opened from
  the dashboard. It never takes actions automatically from URL parameters.

The first two require `Authorization: Bearer <local-token>` issued by
`capture.auth` over the owner-UID Unix socket. The daemon keeps only its SHA-256
hash, expires it after five minutes, binds it to the active stream, and retains
at most 16. Restart/end invalidates it. Never send this token to the server.
Exact Host and peer-UID checks apply. If Origin is present, it must be the exact
local daemon origin; foreign, duplicate and `null` Origin are refused. No CORS or
cookie authentication. In `peer_check=launch_file` mode, the peer-UID check is
disabled: the token is the only authentication barrier against other native local
users. Such users can forge Host/Origin; no-CORS and Origin rules protect websites.
Optional `X-PassStream-Stream` must equal the active local
stream (the dashboard handoff uses it). No remote API key or viewer credential
is accepted. All responses have `Cache-Control: no-store`.

The owner page keeps the credential only in memory; run `passstream capture-auth`
on the host and paste the resulting token there. It offers recording and publish
request buttons. Every request, including one from the sender-page buttons,
shows its destination host and a frozen snapshot preview or recording duration
and size in the sender window, plus a shareable-link warning. Only a trusted
on-screen click there approves. The CLI, plugin and `/owner` can only request or
read status; they have no confirmation operation. Timeout or refusal sends no
bytes to the Pass service. Plugin clients may call the new
Unix ops unchanged. The dashboard link assumes port 47823; for a custom port,
open the matching loopback `/owner` URL on the host.

The trusted native click handler mints and immediately consumes a one-use
approval capability bound to its channel and request ID. No public channel
method approves by ID. Production exposes no sender core/channel global or test
secrets; instrumentation requires an explicit `--mode e2e` build, automation and
`?test`. The daemon authenticates the sender connection, not a human gesture:
arbitrary script execution in the sender origin is already full compromise.
Privileged browser automation can issue trusted clicks and must never be used
by an agent to confirm its own publish request. Stronger human-presence proof
requires a separate trusted native interface.

Publishing reads the selected service key at use time, with owner/mode/symlink
guards. Local config (`$XDG_CONFIG_HOME/passstream/config`) keys:

```
passfile_server = https://passfile.space
passpage_server = https://passpage.space
```

These are the defaults. Override them for your own servers. Keys live in
`$XDG_CONFIG_HOME/passfile/key` and `$XDG_CONFIG_HOME/passpage/key`, normally
`~/.config/passfile/key` and `~/.config/passpage/key`, mode 0600 or 0400.
They are never stored by PassStream, logged, included in argv, sent to the
PassStream server or sent to the other service. HTTPS is required except for
literal loopback HTTP development/fake servers. Redirects are refused.

PassFile: `PUT /api/v1/files?filename=snapshot.png|recording.webm`, bearer auth,
raw PNG/WebM, matching Content-Type, `X-Content-SHA256` of the bytes.
PassPage: `POST /api/shares`, bearer auth, multipart field `archive` with filename
`snapshot.tar.gz`; gzip tar contains only `index.html` and `snapshot.png`.
The service's JSON `url` must match its configured origin (scheme, hostname and
effective port). It is returned only after sender confirmation and successful
upload. One upload per confirmed local request, one in flight
per stream; no automatic retry. The upload timeout is 45 s plus 1 s per 128 KiB
of payload (301 s for 32 MiB). 32 MiB in the former 45 s budget required 5.97
Mbit/s before overhead; the new budget allows roughly 1 Mbit/s plus setup time. A failed/uncertain upload is `upload_failed`:
inspect that service before retrying because it may have accepted the artifact.
Tests point services at local fakes or isolated real local servers with temporary keys.

```
PublishRequest { request_id: string, status: "pending" | "confirmed" | "refused" | "timeout",
 kind: "snapshot" | "recording", service: "passfile" | "passpage", destination: string,
 expires_at: Ms, uploading: bool, url?: string, error?: Error }
```

`publish` reserves one pending request, freezes the selected bytes and prompts
only the authenticated sender window. Confirmation expires after 30 s. Duplicate
or late decisions cannot upload again. `confirmed` with `uploading:true` means
the upload is still running; poll `publish.status` until false, then read `url`
or `error`. Only the latest request result is retained, in memory until stream
end/replacement by a new request. Never automatically retry an uncertain upload.

## 11. Sender page ↔ daemon: `ws://127.0.0.1:47823/local/v1/sender`

The daemon checks the full-tuple peer uid (where available), `Host: 127.0.0.1:47823` and `Origin: http://127.0.0.1:47823`; a missing or `null` Origin is refused (HOST-CONTROL §5.2). Then every connection runs the mutual authentication in §11.2 before anything else.

### 11.1 Messages

**Authentication** (in this order, exactly once per connection):

| Direction | Message |
|---|---|
| page → daemon | `{"type": "hello", "protocol": 1, "key_id": Id22, "nonce": Token43}` |
| daemon → page | `{"type": "challenge", "nonce": Token43, "proof": Token43}` |
| page → daemon | `{"type": "auth", "proof": Token43}` |
| daemon → page | `Init` (below) |
| page → daemon | `{"type": "init_ack"}`, after the page has stored `init.attachment` (only when `Init` carries it) |

**After authentication:**

| Direction | Message |
|---|---|
| daemon → page | `{"type": "command", "id": string, "op": string, "args": object}`: the §10 ops `stop`, `pause`, `resume`, `mode`, `safe`, `lock`, `unfreeze`, `kick`, `pin.rotate`, `pin.set`, `knock.approve`, `knock.deny`, `annotations`, `annotations.clear`, plus `record.start`, `record.stop`, `record.status`, with the same closed args (excluding daemon-only `action_id`) |
| page → daemon | `{"type": "result", "id": string, "ok": true, "data": object}` or `{"type": "result", "id": string, "ok": false, "error": Error}` |
| daemon → page | `{"type":"capture_demand","until":Ms}`: snapshot demand lease, bounded locally; zero/expired disables and erases feed state. |
| daemon → page | `{"type":"publish_prompt","request":{request_id,kind,service,destination,bytes,duration_ms,preview,expires_at}}` or `request:null`. Preview is base64 PNG of the immutable selected snapshot. |
| page → daemon | `{"type":"publish_decision","request_id":string,"approved":bool}`: only an on-screen decision from the current authenticated sender. No CLI/HTTP confirmation op exists. |
| page → daemon | `{"type": "capture", "status": "frame", "id": string, "width": uint, "height": uint, "data": string, "ack": string}` (base64 PNG), or a marker with `status:"paused"|"idle"|"too_large"|"stale"`. Local only; never SenderState/signaling. |
| page → daemon | `{"type":"recording","id":string,"data":string,"final":bool,"ack":string,"duration_ms":uint}`. Chunks ≤ 512 KiB decoded, total ≤ 32 MiB. |
| daemon → page | `{"type":"artifact_ack","ack":string,"ok":bool}`. Acknowledge each PNG/chunk; one of each in flight. A rejected PNG does not advance the sender diff baseline. |
| page → daemon | `{"type":"owner_action","id":string,"op":string,"args":object}`. The §10.1 ops except `capture.auth`; an explicit click in the local sender. |
| daemon → page | `{"type":"owner_result","id":string,"ok":bool,"data":object,"error"?:Error}`. Never routed to the server. |
| page → daemon | `{"type": "state", "state": SenderState}` |
| daemon → page | `{"type": "state_ack", "seq": uint}`, after the daemon has fsynced the latches of the `state` with that `seq` (HOST-CONTROL §5.6) |
| page → daemon | `{"type": "latch_cleared", "latch": "locked" \| "paused" \| "frozen"}`, after the sender window's own UI cleared a latch and the page record has it |
| page → daemon | `{"type": "secrets", "pin": string \| null, "pending_pin": string \| null, "link": string}`. After every authentication and on every change. **Only on this socket.** |
| page → daemon | `{"type": "alert", "alert": Alert}` |
| daemon → page | `{"type": "view", "list": ViewerList}`: the joined table for the sender window |
| page ↔ daemon | `{"type": "ended", "reason": "stopped" \| "window_closed" \| "expired" \| "deleted" \| "state_lost" \| "browser_failed"}` |
| daemon → page | `{"type": "error", "code": "auth_failed" \| "launch_consumed" \| "launch_expired" \| "superseded" \| "protocol_mismatch"}`, then the socket closes. Terminal for that page only when it arrives after `proof_d` verified (below). |

A message out of order (anything but `hello` first, anything but `auth` after `challenge`, anything before `Init`) closes the socket.

```
Init {
  type: "init"
  attachment?: { key_id: Id22, key: Token43 }   // only after a launch-key exchange; the same value every time
  server_url: string
  signal_url: string                    // wss://…/signal, derived from server_url
  stream: { id: StreamId, publish_token: Token43, view_link_base: string }
  policy: { pin_policy: PinPolicy, pin: string | null, knock: bool, mode: "text" | "motion", safe: bool }
  latches: Latches                      // the daemon's mirror
  active_stream_ids: StreamId[]
}
```

### 11.2 Rules

- **Mutual authentication.** `K` is the 32-byte key named by `key_id` (the launch key or the attachment key). `nonce_p` (from `hello`) and `nonce_d` (from `challenge`) are 32 fresh CSPRNG bytes each; the daemon draws `nonce_d` per connection and keeps it with that connection only.
  ```
  T       = "PassStream v1 local auth" || key_id (16 bytes) || nonce_p (32) || nonce_d (32)
  proof_d = HMAC-SHA-256(K, 0x01 || T)     // daemon → page, in challenge
  proof_p = HMAC-SHA-256(K, 0x02 || T)     // page → daemon, in auth
  ```
  All values are the decoded bytes. Both sides compare in constant time.
  - **The page** sends nothing after `hello` until `proof_d` verifies. If it doesn't, the page closes the socket, never sends `auth`, `state` or `secrets` to that peer, and retries with a new connection and a new nonce (backoff 100 ms → 1 s).
  - **The daemon** accepts `auth` only on the connection whose `nonce_d` it covers, once, within 5 s of `challenge`. A wrong, late or missing `proof_p` gets `auth_failed` and changes nothing: no `Init`, no supersede, no state change. An unknown `key_id` gets `auth_failed` before any challenge. A retired or expired launch key gets `launch_consumed` or `launch_expired`.
  - **Errors before `proof_d` verifies are unauthenticated**: anyone holding the port can send them. The page treats them as a dropped connection: it shows the code, keeps its `SenderHost` and keeps retrying. Only an error after a verified `proof_d` is authoritative: then `auth_failed`, `superseded` and `protocol_mismatch` are terminal (the page shows "This window is no longer the sender", stops its tracks and drops its host). `launch_consumed` and `launch_expired` can only arrive before a challenge, so a page that has no attachment key shows "Open PassStream from the bar or the CLI" after 60 s of them.
  - **Only a completed exchange may supersede** the current attachment (§5.2). The page never sends a bearer token; neither key is sent by the page, and the attachment key is sent by the daemon only inside `Init`, after the exchange.
- **Handover.** The page writes `attachment` to `sessionStorage` before it sends `init_ack`, and uses the attachment key for every later connection. The daemon retires the launch key on `init_ack` or on the first attachment-key exchange (§5.2; HOST-CONTROL §5.3).
- **Page load versus reconnect** (HOST-CONTROL §5.3). The page decides by its own memory: with a running `SenderHost` it's a reconnect, and it never recreates the host, never uses `init.policy` and only sets latches; without one it's a page load and runs the startup order. On a reconnect, an `init.stream.id` that differs from the running host's → `ended {reason: "state_lost"}`.
- **Policy:** `init.policy` is used only to create the recovery record on a first launch. It's ignored whenever a record exists, **except `knock`**: on every `Init`, `knock = record.knock OR init.policy.knock`, persisted. A reconnect that would turn knock on for a host created without it latches `locked` and alerts `state_mismatch`. No server value reaches the page except through the daemon's creation policy.
- **Latches:** the page applies `page.latches OR init.latches` (and the library's `frozen`) before anything else, on page load and on reconnect.
- **Cleanup:** the page deletes stored records for streams not in `active_stream_ids`.

## 12. Shell IPC and CLI

- `omarchy-shell passstream start|stop|pause|resume|togglePause|curtain|curtainOff|lock|status`. `curtain` and `curtainOff` are the privacy curtain: the pause latch. No default keybind; the plugin README documents an example bind. `status` prints `{"live": bool, "phase", "watching", "knocking", "latches", "pin": {"required", "kind", "length"}, "error": ""}`.
- `passstream <command> [--json]`, for local agents (HOST-CONTROL §8.1). Each command maps to one §10 op. `curtain on|off` is `pause` / `resume` (the privacy curtain is the pause latch: on is allowed from anywhere, off only locally). `annotations on|off|clear` is `annotations {viewers}` / `annotations.clear`. `safe on|off` is `safe {on}` (on is allowed from anywhere, off only locally); `share --no-safe` starts with safe mode off.
- **Secrets never in argv.** `--pin 4|6|none|passphrase` names a policy. A value (an explicit digit PIN or a passphrase) comes from:
  - a no-echo prompt when stdin is a TTY;
  - `--pin-stdin` (one line, the trailing newline stripped);
  - `--pin-fd N`.
  A value-looking argument (`--pin 4821`, `--pin=hunter2…`) is refused with exit code 2, and the CLI doesn't echo it.
- `share` waits for `live` (up to `--timeout`, default 120 s) and prints the link. With `--no-wait` it returns at `link_ready`. On timeout it stops the stream and exits 1 with `picker_timeout`.
- `pin` shows `PIN · 4 digits ••••`. `pin --reveal` prints the PIN; on a TTY the line is erased after 10 s. With `--json` the PIN is printed plainly.
- `copy link` and `copy pin` both send the value over stdin to `wl-copy --sensitive`, never `--paste-once`. After 30 s, clear only the selection still owned by that copy; a newer selection must survive. The CLI helper owns the selection (`wl-copy --foreground`) and exits on replacement or deadline, so clearing never needs a clipboard compare. If `--sensitive` is unavailable, copy with a warning and report `sensitive: false`; never retry a failed sensitive copy without the flag. A fallback without the helper reports `clears_in_s: null` and warns. Clipboard histories may already have stored a value. `--json` returns `{copied, sensitive, clears_in_s}`.
- `knock approve <knock_id> --code <K7P>` requires `--code`. The CLI trims it and upper-cases ASCII letters; anything that isn't then exactly three characters of the row-code alphabet (a colour name, `K7P·purple`, four characters) is a usage error (exit 2). It sends the canonical string (§4.3).
- PIN changes (`pin rotate`, `pin none`, `pin set`, `unfreeze`, `kick --rotate-pin`) exit 1 with `rotation_pending` while another change is pending; with `--wait` they wait (up to `--timeout`) until `access.rotation_pending` is `false` and send again. A `storage` reply with `details.pending: true` prints "PIN change accepted, still being applied" and exits 0.
- `link` prints the link on explicit local request. `ban <connection_id> [--action-id ID]` retries `server_unreachable` and `sender_timeout` up to three attempts with the same action id. Kick, unfreeze, PIN changes and knock approval also accept `--action-id` to reuse an action across CLI runs.
- If the daemon is unreachable, the CLI starts `passstream host --detach` and retries for 5 s; failure is `daemon_unreachable` (exit 3). `host install-units [--no-enable]` installs the two user units.
- With `--json`, results go to stdout and `{"error": Error}` to stderr.
- Exit codes: `0` ok, `1` error, `2` usage, `3` daemon not reachable, `4` `port_not_owned`.

### 12.1 Local capture CLI

```
passstream frame [--after ID] [--wait 0..30] [--max-width 0..3840]
                 [--out file.png|-] [--json]
passstream record start [--duration 1..120] [--max-bytes 1024..33554432] [--json]
passstream record stop|status [--json]
passstream publish snapshot passfile|passpage [--max-width N] [--json]
passstream publish recording passfile [--json]
passstream publish status REQUEST_ID [--wait 0..30] [--json]
passstream capture-auth [--json]
```

`frame --json` returns `LocalFrame`, including base64 bytes. With `--out file`,
write PNG with mode 0600 using exclusive create (existing files/symlinks refused),
and JSON contains `out` instead of `data`. `--out -` writes only PNG bytes and
cannot combine with `--json`; a paused/unchanged result prints that marker.
Without `--out` or `--json`, print status and ID only. `record` returns status;
`publish` prints/returns `pending` and its request ID; `publish status` returns
`PublishRequest`, including the final URL/error after the owner confirms.
`capture-auth` prints local origin and token (JSON: `{origin, token,
expires_in_s:300}`). Treat it as local authority and never paste it into a
server page. These commands are exclusively authenticated local owner actions;
recording/publishing is never started by a remote agent or viewer.

### 12.2 Public CLI command inventory

The following are top-level commands. `start` is an alias for `share`; `serve` runs the server and `host` runs the daemon (and `host install-units`). `help`, `--help` and `-h` print usage without connecting. `mode text|motion` changes quality only. `safe on|off` turns safe mode on or off. Other arguments and secret-input rules are above.

- CLI command `serve`
- CLI command `host`
- CLI command `share`
- CLI command `start`
- CLI command `stop`
- CLI command `pause`
- CLI command `resume`
- CLI command `lock`
- CLI command `unlock`
- CLI command `status`
- CLI command `viewers`
- CLI command `link`
- CLI command `shutdown`
- CLI command `mode`
- CLI command `safe`
- CLI command `curtain`
- CLI command `annotations`
- CLI command `unfreeze`
- CLI command `kick`
- CLI command `ban`
- CLI command `knock`
- CLI command `pin`
- CLI command `copy`
- CLI command `frame`
- CLI command `record`
- CLI command `publish`
- CLI command `capture-auth`
- CLI command `help`

## 13. Agent rules (published in `/llms.txt`)

1. **Never decide who someone is from relay metadata.** `connections[*]` (IP, geo, ASN, device, User-Agent, path, timing) and `reported` (label, tab hidden) are server-observed or viewer-reported and can be swapped consistently by the server. Don't approve, kick, ban or otherwise pick a person from them. Show them to a human, or alert a human about anomalies. Only a human confirming a viewer's **row code** out-of-band identifies a row.
2. Act on rows by `admission_ids[0]` or `knock_id`, never by `connection_id` (except `ban`), label or IP.
3. Send an `id` with every command and retry with the same `id`. On `504`, re-read the state before deciding anything new.
4. Permissive ops (resume or curtain off, unlock, unfreeze, approve, any PIN change, letting viewers draw, safe mode off) work only through the local `passstream` CLI on the host. `403 local_owner_required` isn't something to work around.
5. Never put a PIN or passphrase in argv; use `--pin-stdin` or `--pin-fd`. Never log, echo or store the PIN or the link beyond the task.
6. Plaintext frames and publishing exist only on the owner's authenticated local API (§10.1). Remote agents must admit through the viewer page; the server cannot supply frames. An agent may request publishing only when the owner asked for it in the current
task. The owner always confirms on screen in the local sender window; an agent
must never approve that confirmation or treat local API access as owner consent.
7. `watching` in the HTTP API is relayed by the server. The local `passstream viewers` is the sender's own count.

## 14. Errors

HTTP and local errors share one envelope:

```
Error { code: string, message: string, details?: object }
HTTP body: { "error": Error }
```

`message` is for people and may change; branch on `code`.

| `code` | HTTP | Meaning |
|---|---|---|
| `invalid_argument` | 400 | Bad body, args or policy, or an unknown argument. Also an unchanged PIN in a PIN change (terminal, §5.4). |
| `passphrase_required` | 400 | The policy is a passphrase and none was supplied. |
| `unauthorized` | 401 | Missing or unknown key or session. |
| `forbidden_scope` | 403 | The key or session lacks the scope. |
| `origin_rejected` | 403 | `Origin` doesn't match the base URL. |
| `local_owner_required` | 403 | A permissive op, or a local-only argument, from a remote channel. |
| `not_found` | 404 | No such stream or key. |
| `unknown_admission`, `unknown_knock` | 404 | Sender: no such id (kicked, ended, expired or gone). |
| `conflict`, `already_live` | 409 | State conflict. |
| `idempotency_conflict` | 409 | A reused command `id` with a different op or args. |
| `sender_offline` | 409 | No publisher connected. |
| `not_live` | 409 | Sender: no active share. |
| `rotation_pending` | 409 | Sender: a PIN change is still being applied. Retryable once `access.rotation_pending` is `false` (§5.4). |
| `code_mismatch` | 409 | Sender: `row_code` doesn't match the knock. |
| `stream_ended` | 410 | The stream has ended. |
| `payload_too_large` | 413 | |
| `rate_limited` | 429 | With `Retry-After`. |
| `internal` | 500 | |
| `storage` | 503 | Sender storage failed. A kick stays `kick_incomplete`; retry it. With `details.pending: true` (a PIN change) or `details.applied: true` (a latch), the sender finishes the write alone: don't resend. |
| `sender_timeout` | 504 | No sender answer within 5 s. The command may still apply. |
| `upgrade_required` | – | Signaling protocol mismatch. |
| Local only | – | `key_missing`, `key_unsafe`, `key_invalid`, `key_rejected`, `server_unreachable`, `browser_failed`, `port_not_owned`, `picker_timeout`, `link_not_ready`, `protocol_mismatch`, `auth_failed`, `launch_consumed`, `launch_expired`, `superseded`. |

## Appendix A. Normative resolutions from the plugin build (2026-11-03)

These come from `passstream-omarchy-plugin/docs/CONTRACT-NOTES.md` items 1–22, as arbitrated by GPT-6.1-Sol's review. They are binding for the daemon, CLI and plugin.

1. A subscribed connection counts as "needed" for the idle-exit timer. Self-bound clients restart the daemon only on an explicit action or an active-stream failure, with bounded backoff.
2. The daemon sends full snapshots, sends material state changes promptly, coalesces statistics-only updates, and sends no more than 4 snapshots per second.
3. `start` replies after durable creation and a successful browser spawn, without waiting for capture. The reply precedes that stream's `live` event.
4. The port check inspects both `/proc/net/tcp` and `/proc/net/tcp6` (IPv4, IPv4-mapped and wildcard addresses). It requires exactly one matching listener socket owned by the caller, and fails closed on inspection errors. Repeated observations count once only when their nonzero inode, normalized local/remote tuple, state and UID all agree; distinct or zero inodes and contradictory observations are never coalesced.
5. Another-owner failures carry a numeric `error.details.uid`. Multiple-owner failures also carry `details.uids`.
6. Socket-activated launches verify the active socket unit. An unknown `port_mode`, or no fresh status, blocks launch.
7. The client's `knock` value is a local default. At creation it is OR-ed with the server default, so `false` cannot weaken a server-required knock.
8. Automatic link copying applies only to a stream the plugin started, once, on its first transition to live. CLI-started streams leave the clipboard alone.
9. `daemon.server_url` is the normalized public base used to build the canonical §3 viewer link.
10. Shell IPC uses `togglePause`, and the additional methods are documented. Approval requires both the knock ID and the code.
11. `status.live` means `phase === "live"`. `status.error` is a stable contract or local code, or `""`, and never contains secrets.
12. Row-code colours: red `#e5484d`, orange `#f76b15`, yellow `#ffc53d`, green `#30a46c`, teal `#12a594`, blue `#0090ff`, purple `#8e4ec6`, pink `#d6409f`. Index = COLOURS[code_index mod 8]. Colours are for display only.
13. Watching and knocking are shown separately (`watching+knocking`). Knocks never inflate the watching count.
14. Ban targets `connections[0].connection_id`. Rows without attached connections can't be banned, and joining rows can't be kicked.
15. On success, the daemon's `ban` data is the server result `{ip_banned, admission_ids}`, unwrapped from the HTTP command envelope.
16. Unknown row statuses sort last and offer only admission-based Kick. A malformed required latch invalidates the whole snapshot; it is never coerced.
17. Failure counts don't overlap. The maximum number of sources is a lower bound, shown as `N+ sources`. Alerts are not replayed on subscribe.
18. Receivers treat `phase:"ended"` as idle. The daemon converges to `stream:null` once termination completes.
19. End events are `{stream_id, reason}`, with the six known reasons `stopped`, `window_closed`, `expired`, `deleted`, `state_lost`, `browser_failed`, plus `unknown`. Unknown future or unavailable reasons map to `unknown` on every transport, remain display-only, and never block cleanup.
20. Copies use `wl-copy --sensitive` over stdin, without `--paste-once`. The clipboard is cleared after 30 s, but only while that copy still owns the clipboard selection. Ending its `wl-copy --foreground` owner clears that selection; replacement, even by an identical value, must survive.
21. All bar panels share one popup surface, so the panel can't be hidden from capture. Clients warn **before** a reveal and hide the secret after 10 s. Capture exclusion applies to the separately namespaced live border.
22. Clicking a notification card only opens Review. An explicit "Approve CODE" binds the current canonical code to that knock ID, which is never reused, and the sender checks it again.

Implementation note for the daemon (orchestrator): before every browser launch the daemon deletes the dedicated Chromium profile's service-worker storage (`<profile>/Default/Service Worker/`). This closes the residual self-bound-mode risk where a foreign listener's page registers a persistent service worker.
