# Host control: who sees what, and who may command the sender

Status: **design, phases 2b and 3, revised after review rounds 1 and 2 (§12).** Wire formats and schemas are in [CONTRACT.md](CONTRACT.md), which is normative where the two differ. This builds on [PLAN.md](PLAN.md) ("Access and encryption", "Host visibility", "Omarchy first", "Dashboard") and on the E2EE library in [PROTOCOL.md](PROTOCOL.md).

This is pre-release. Nothing here keeps compatibility with phase-1 link, fragment, error or stream formats; phase-1 code is migrated to the formats below (CONTRACT §1.4).

## 0. Decisions at a glance

1. **The Omarchy sender page is served by a local `passstream host` daemon, not by the server.** It's the same binary with the same embedded SPA, on `http://127.0.0.1:47823`. The server stops being a source of code for the one party that holds the PIN, the link secret and the identity seed. Without this, no command authentication would mean anything: a server that serves the sender's JavaScript can make the sender do anything.
2. **The port is held by systemd.** `passstream-host.socket` (systemd `--user` socket activation) owns `127.0.0.1:47823` and the Unix control socket, so the port stays reserved for this user across daemon crashes and restarts. The plugin and the CLI also check that the listener belongs to their own UID before they launch the browser, and refuse otherwise (§2.2). Without a systemd user manager the daemon binds the port and supervises the browser itself (§5.9).
3. **Monotonic command rule.** A command that only *reduces* exposure (stop, pause, lock, kick, deny a knock) may come from anywhere, the server included. A command that *increases* exposure (resume, unlock, unfreeze, approve a knock, any PIN change) is accepted **only over the local channel**. The sender page enforces this by channel: it never runs a permissive command that arrived from the server.
4. **No owner signing key in v1** (§4.6).
5. **What a row guarantees:** a command targets the admission the owner selected. It doesn't guarantee that the row's IP, place or name belong to that person: the server can swap those consistently. For a human check, each admission has a **row code** (three characters; a colour derived from them is a display aid) that the sender assigns inside the E2EE session and that the viewer's own screen shows, so the owner can ask "what code do you see?" (§3.3). Automation may not decide anything from relay metadata.
6. **Restrictive state survives everything.** Lock, pause and the freeze gate are persisted latches, persisted before the command that set them is acknowledged, and restored before any admission or any captured frame is sent. Reloads, peer-connection replacement, worker recovery and a fresh `init` never unlock or resume (§5.6).
7. **PIN changes are a persisted intent**, reconciled with the E2EE library's revocation generation and guess budget at startup. Admissions stay blocked until all of them agree (§5.5).
8. **Policy from the server applies only at stream creation**, and only where it's stricter than the local default. An explicit local choice (`--pin none`) wins. The server never causes a PIN regeneration (§4.4).
9. **Secrets at the edges:** no PIN or passphrase in argv (prompt, stdin or `--pin-fd`), the toast doesn't show the PIN, the PIN is masked everywhere until a 10 s reveal, and "copy link" and "copy PIN" are separate (§5.7).
10. **The sender WebSocket never sends a bearer token.** It uses mutual HMAC challenge-response, daemon first (§5.2). The separate `/owner` page sends a short-lived, Unix-issued bearer token to the local HTTP capture API (CONTRACT §10.1). These are distinct authentication protocols.
11. **Admission transport** is specified: an E2EE relay envelope on the signaling WebSocket, peer-close notifications, knock (pending approval) and grant reconnect. Each `knock_id` is bound to one verified handshake (§6).
12. **One join function** (`internal/hostview`, Go) merges sender facts with server facts, keyed by the sender-assigned `admission_id`, with every field's provenance. Its rules are normative in CONTRACT §6.

## 1. What must hold

- The server never learns the PIN, the link secret, the identity seed, grant secrets or media keys. Credentials that do go to the server are listed one by one in CONTRACT §2, and none is ever logged.
- The server can't make the sender admit anyone, weaken the PIN, reset the guess budget, unfreeze, unlock or resume.
- The server *can* deny service: stop, pause, kick, lock, deny knocks, drop traffic. It can already do all of that by dropping packets, so v1 doesn't authenticate those commands.
- **A command reaches the admission the owner selected.** A row's telemetry may be wrong; the action isn't redirected by it.
- Restrictive state (lock, pause, freeze, a pending PIN change) is never lost or loosened by a crash, reload or recovery path.
- The plugin, dashboard and agents see the same viewer table, each labelled with how far it can be trusted.

**The boundary is the user account.** Everything here protects against the server, other local users and websites. It doesn't protect against native code running as the same UID: such code can read the browser profile, the daemon's memory and the key file anyway (§9).

## 2. Components

```
 ┌───────────── host machine (one user) ─────────────────────────────────┐
 │                                                                        │
 │  Quattro bar plugin (QML)        passstream CLI / local agents          │
 │        │  Unix socket, JSON lines       │                               │
 │        └──────────────┬─────────────────┘                               │
 │                       ▼                                                 │
 │  systemd --user: passstream-host.socket  (holds both listeners)         │
 │       · %t/passstream/host.sock             (plugin, CLI)               │
 │       · 127.0.0.1:47823                     (sender SPA + 1 WS)         │
 │                       │ activates                                       │
 │     passstream-host.service  (daemon, same binary)                      │
 │       · holds the API key, caches PIN/link in memory, joins views       │
 │              ▲ same-origin WS /local/v1/sender                          │
 │              │                                                          │
 │     passstream-sender-<stream>.service  (transient unit)                │
 │       Chromium --app, dedicated profile                                 │
 │       sender page from 127.0.0.1:47823: SenderHost, PIN, keys           │
 └──────────────┼───────────────────────────────┬──────────────────────────┘
                │ signaling WS (publisher)       │ HTTPS API (key)
                ▼                                ▼ (daemon)
          passstream serve  (remote or tailnet server, SFU)
                ▲                     ▲
       viewers (SPA from server)   dashboard (SPA from server), remote agents
```

The daemon calls the server API with the owner's key. The sender page talks to the server only through the publisher signaling WebSocket (media negotiation, the E2EE relay, the remote control channel and state reports). The plugin never talks to the server.

### 2.1 Why serve the sender locally

- **Security.** The sender's code comes from the binary on the host's disk. A compromised server can still serve bad JavaScript to *viewers* (the documented web ceiling), but it can't take over the sender, which holds the PIN, the guess budget and the admission decisions. That's what makes the monotonic rule worth enforcing.
- **IPC without browser hurdles.** Same-origin loopback needs no Local Network Access prompt, no CORS, no extension and no native messaging.
- **No secrets at launch.** The window is opened with a launch key that expires after 60 s and is never sent anywhere: the page proves it holds it (§5.2, §5.3).

The server's `/signal` must accept the publisher from Origin `http://127.0.0.1:47823`. That's safe because `/signal` uses no ambient credentials (CONTRACT §8.1). The SPA runs with a different `connect-src` when it's served locally.

The server-served `/host` page stays for the torture suite, browser hosts on other platforms and headless agent senders. It's a "trusted server" sender: the same rules apply, but they protect nothing against a malicious server there, and its UI says so.

### 2.2 Holding the port

The browser can't authenticate whoever answers on `127.0.0.1:47823`. If another local user bound the port after a daemon crash, the sender window's next load would run that user's JavaScript on the sender's origin, with access to the sender's stored seed, link secret and PIN. Three measures close this:

1. **Socket activation.** `passstream host install-units` writes and enables two user units:
   - `passstream-host.socket`: `ListenStream=127.0.0.1:47823`, `ListenStream=%t/passstream/host.sock` (`SocketMode=0600`, `DirectoryMode=0700`), `WantedBy=sockets.target`.
   - `passstream-host.service`: `ExecStart=passstream host`, `KillMode=process`.
   The user's systemd binds both sockets when it starts (at login) and keeps them bound while the daemon crashes, restarts or exits when idle. The daemon takes them from `LISTEN_FDS` and never binds itself in this mode. Another UID can't bind the same address while it's held, and Linux refuses `SO_REUSEPORT` sharing across UIDs.
2. **Owner check before launch.** Before the plugin or the CLI asks the daemon to open a browser, and the daemon checks again before it spawns one:
   - read `/proc/net/tcp` for sockets in state `LISTEN` (`0A`) on port 47823 with local address `127.0.0.1` or `0.0.0.0`;
   - require **exactly one socket**, owned by the caller's UID; repeated procfs observations count once only if their nonzero inode, normalized local/remote tuple, state and UID all agree;
   - also require `systemctl --user is-active passstream-host.socket` in socket-activated mode.
   Anything else (no listener, two listeners, another UID, unreadable `/proc`) fails with `port_not_owned` and names the owning UID. No browser is launched.
3. **Mutual authentication on every connection.** The `/sender` page never sends its authentication key. Every sender WebSocket starts with an HMAC challenge-response over a 32-byte key the page and the daemon share (the launch key on a first launch, then the attachment key), bound to a nonce from each side and to that one connection (§5.2, CONTRACT §11.2). **The daemon proves first.** The page sends nothing but its key id and nonce until the daemon's proof verifies, and the daemon sends no `init`, runs no command and replaces no attachment until the page's proof verifies. A listener that isn't our daemon learns only a public key id and a nonce, which it can't replay. A client that isn't our page can't take over the attachment, with or without the peer-UID check. The page never reloads itself.

**Residual window.** Before the user's systemd manager starts (from boot until the first login, unless lingering is on), another local user can bind 47823 first. `passstream-host.socket` then fails to start, and check 2 refuses every launch with `port_not_owned`, so no sender page is loaded from that listener. `loginctl enable-linger` makes the user manager start at boot, which shrinks the window to early boot. The setting is recommended, not required.

**Fallback** (no systemd user manager, or the socket unit can't be installed): the daemon binds both listeners itself, supervises the browser and is restarted by its clients (§5.9). `host status` reports `port_mode: "self_bound"`. Check 2 still runs before every launch. Check 3 guards every reconnect: another UID that grabs the port while the daemon is down sees a key id and a nonce, and once the real daemon is back it can't replace the sender, because replacement needs the page's proof, which needs the key. That holds with the launch-file fallback too, where the peer-UID check is absent (§5.2). The remaining exposure is an open sender window that the *user* reloads while the daemon is down and another UID holds the port: the reload loads that UID's code on the sender origin. The plugin and sender window show "Port not reserved: don't reload the sender window while PassStream is restarting."

## 3. Sources of truth and the viewer join

### 3.1 Who knows what

| Fact | Source | Trust |
|---|---|---|
| Admission exists, `admission_id`, `viewer_id`, row code, admitted at, via grant or PIN, connected (receives rekeys), `kick_incomplete` | **Sender** (`SenderHost.viewers()`) | Authenticated. The only source for "who is admitted". |
| PIN required, kind and length; latches (locked, paused, frozen); knock mode; failure counts; knock queue with row codes; E2EE epoch and suspension; rotation pending; text/motion | **Sender** | Authoritative. PIN *value* and link are local-only. |
| `peer_id` an admission came in on | Sender records it, but **the relay chose it** | Routing only. The join key, never an authorization input. |
| Connection id, IP (HTTPS-observed, trusted-proxy aware, never a TURN address), ASN/geo, User-Agent, path, RTT/loss/bitrate/freezes/layer, join/leave times | **Server** | As honest as the server. Can be wrong, swapped or invented, and consistently so. |
| Display name, tab hidden | **Viewer**, relayed by the server | Unverified. Labelled "viewer-reported". |

### 3.2 The join

`internal/hostview.Join(in JoinInput, mem JoinMemory) (ViewerList, JoinMemory, []HistoryEntry)` is pure and shared by the server and the daemon. Its inputs are the sender state with its receipt time, whether the publisher is online, whether the stream has ended, and the connections; removal causes come from the sender's own `ends` records, never from a diff. Its rules (grouping by `viewer_id`, duplicate and historical `peer_id`s, freshness, the 5-minute `left` window and history) are normative in CONTRACT §6. In short: rows are built from sender admissions grouped by `viewer_id`, telemetry is attached by `peer_id`, ambiguity is flagged and attaches nothing, and connections no admission claims become `joining` rows.

**The watching count is the number of distinct `viewer_id`s with a connected admission, per the sender.** The server can't inflate or hide it in the plugin or the sender window. In the dashboard it can, and the dashboard says so (§7).

### 3.3 What a row guarantees, and the row code

**The guarantee is narrow: commands target the selected admission.** Every action names an `admission_id` or `knock_id` the owner picked from a row. A kick is never addressed by IP, `peer_id` or label. A malicious server can swap *all* the relayed facts between two rows (IP, place, device, name, timing) and keep them consistent, so the owner may pick the wrong row. What it can't do is make a kick on the row the owner picked hit a different admission.

**Row code.** To tell people apart without trusting the server, the sender assigns each grant a short code:

- **The code is one canonical value: three characters** from `ACDEFHJKMNPRTUVWXY3479` (22 symbols, no look-alikes), 10 648 codes, written in upper case. Its index is `484·i₀ + 22·i₁ + i₂`, where `iₙ` is the character's position in that alphabet, and that index is what travels inside E2EE boxes (CONTRACT §4.3).
- **The colour is derived, not chosen:** `colours[index mod 8]` from `red, orange, yellow, green, teal, blue, purple, pink`. It's a display aid only. It's always shown with its name, so colour-blind owners can use it, and it's never typed, compared or sent.
- The sender picks the code uniformly at random from the codes not used by the stream's live grants and pending knocks (at most 550 at once, CONTRACT §1.3), and stores it with the grant. Reconnects with the same grant keep the code.
- It reaches the viewer **only inside the E2EE session**: in the knock's `PENDING` box or in the `ADMIT` bundle (§6.4). The server relays those boxes but can't read or forge them.
- The viewer's page shows "Your code: K7P · purple" (in the waiting screen, and on a tap in the player). The owner's row shows the same code from the sender's own state.
- The owner can ask the person, by phone or chat, "what code do you see?" A matching code proves that the row they are about to act on is that person's admission. This holds under trusted viewer code, which is the same ceiling as E2EE itself.

The code goes to the server inside `SenderState`, so the dashboard can show it (labelled "reported by the sender, via the server"). Knowing a code doesn't let the server change what a viewer's screen shows.

**Knock approval binds to the code.** `knock.approve` carries the `row_code` the approving surface displayed: the three-character string, nothing else. The sender compares it byte for byte with the knock's code and refuses a mismatch with `code_mismatch`. The plugin and the sender window pass the code of the row that was clicked. The CLI requires `--code`: it trims the argument, upper-cases ASCII letters and requires exactly three alphabet characters (anything else, a colour name included, is a usage error), then sends the canonical string (CONTRACT §12).

**No automation on relay metadata.** Agents and scripts must not approve, kick, ban or otherwise choose a person from IP, geo, ASN, User-Agent, device, path, timing or `reported` fields. Those fields are fine for display and for anomaly alerts to a human. This is a contract rule in `/llms.txt` (CONTRACT §13). The CLI's `knock approve` refuses to run without `--code`, which an agent can only get from the human.

**Ban** is a server action keyed by `connection_id`: the server bans that connection's IP, then asks the sender to kick the admissions it attributes to that connection. If the server lies about which admissions those are, the result is still only kicks.

**Provenance:** every field is shown as **sender** (lock icon), **server-observed** or **viewer-reported**. In the dashboard, the sender block is labelled "reported by the sender, via the server".

## 4. Commands and their authority

### 4.1 The rule

A command is **restrictive** if, applied by an adversary, it can only lower availability. It's **permissive** if it can raise anyone's access to the screen or audio, or reset the PIN guess budget. Restrictive commands are accepted from any channel. Permissive commands are accepted only from the local channel (the daemon's WebSocket) or from the sender window's own UI.

The sender page has exactly two command inputs: the local WebSocket from the daemon, and `control` messages on its publisher signaling WebSocket from the server. Each has its own argument schema (CONTRACT §8.5 and §11). The server checks the remote allowlist first as a courtesy, so remote callers get a 403 without a round trip; the sender's check is the one that counts.

### 4.2 Classification

| Command | Class | From the server? | Why |
|---|---|---|---|
| `stop` | restrictive | yes | The server can end the stream anyway. |
| `pause` | restrictive | yes | Stops audio and video. Sets the pause latch. |
| `resume` | **permissive** | no | Re-exposes a screen the owner hid on purpose. |
| `mode` text/motion | neutral | yes | Quality only. |
| `safe` `{on: true}` | restrictive | yes | Redacts sensitive data before encoding (FEATURES.md "Safe mode"). |
| `safe` `{on: false}` | **permissive** | no | Sends sensitive data unredacted. |
| `lock` `{locked: true}` | restrictive | yes | No new admissions. Sets the lock latch. |
| `lock` `{locked: false}` | **permissive** | no | Reopens admissions. |
| `kick` | restrictive | yes | Removes access; rekeys. |
| `kick` with `lock: true` | restrictive | yes | The remote substitute for kick + rotate. |
| `kick` with `rotate_pin: true` | **permissive** | no | Rotation resets the guess budget (PROTOCOL.md §6). A server that could rotate after every 19 failed guesses would turn the 20-guess cap into unlimited guessing for a colluding link holder. |
| `pin.rotate`, `pin.set` (any policy) | **permissive** | no | Budget reset, and possibly a weaker PIN. |
| `unfreeze` (with or without a new PIN) | **permissive** | no | Re-opens guessing after a freeze. |
| `knock.approve` | **permissive** | no | Admits someone. |
| `knock.deny` | restrictive | yes | |
| `ban` | restrictive | server-side, then a `kick` | |
| `annotations {viewers: false}`, `annotations.clear` | restrictive | yes | Stops viewer drawing; wipes the overlay (FEATURES.md "Track D2"). |
| `annotations {viewers: true}` | **permissive** | no | Puts one viewer's input on every other viewer's screen. |

The privacy curtain (`passstream curtain on|off`, plugin `curtain`/`curtainOff`, Escape in the sender window) is `pause`/`resume`: the same latch and classes.

### 4.3 Argument validation per channel, and kick + lock

Each channel has a closed argument schema per op. An unknown argument is `invalid_argument`, because ignoring it could silently drop something the caller relied on. An argument that only the local channel may send (`rotate_pin`, `pin`, `keep_pin`, `row_code`, a policy) arriving over `control` is refused with `local_owner_required`, and nothing is applied, not even the restrictive part of the command.

**Kick + lock ordering.** The sender sets the lock latch **synchronously, before awaiting anything**: it calls `host.lock(true)`, then awaits the latch write (§5.6) and `host.kick(...)` together, and replies only when both have settled. The lock stays set whatever the kick does: `unknown_admission`, a storage error with `kick_incomplete`, a timeout or a thrown exception. The result reports both parts: `{admission_ids, locked: true}` or an error with `locked: true` in its `details`.

### 4.4 Policy sources

- **Local launcher policy** has two forms:
  - an *explicit* choice for this share: `--pin none|4|6|passphrase`, or the plugin's per-share picker;
  - the *local default* from the plugin settings or `~/.config/passstream/config`.
- **Server defaults** (`GET /api/v1/settings`) are read by the daemon once, when it creates a stream.
- **At creation** the daemon computes the stream's policy:
  - explicit local choice → that choice, even when it's weaker than the server defaults (`pin none` included);
  - otherwise → the stricter of the local default and the server defaults (CONTRACT §4.1 order).
  If the result is a passphrase and the caller supplied none, `start` fails with `passphrase_required`. A server can cause that failure; that's denial of service, which it can always do.
- **After creation** nothing from the server changes the policy. No server message, settings change or reload makes the sender regenerate or weaken the PIN. The page ignores `init.policy` whenever a recovery record exists, except `knock`, which only restricts and is OR-merged with the record (§5.3).
- The stream's `declared` field on the server is informational. `effective` is what the sender reports.

### 4.5 What this costs

The dashboard and remote agents can't resume, unlock, approve knocks or change the PIN. The owner is sharing their own screen, so they're normally at the machine with the bar plugin and the sender window in front of them. Remote approval from a phone is the one real loss (§11 Q4).

### 4.6 No owner signing key in v1

The dashboard's code comes from the server, so any key it holds is a key a malicious server holds, and the plugin already has the local channel. The `control` message reserves an `auth` field for a v2 enrolled-device signature; v1 ignores it.

## 5. Local IPC

### 5.1 The daemon

`passstream host` is a subcommand of the one static binary.

- **Plugin and CLI:** the Unix socket `$XDG_RUNTIME_DIR/passstream/host.sock`, directory `0700`, socket `0600`, inherited from the socket unit. The daemon also checks that the `SO_PEERCRED` uid equals its own. JSON lines (CONTRACT §10). The plugin uses a bounded relay (including peer credentials and line limits) rather than an unbounded line parser. Connecting activates the daemon, so the plugin needs no start logic. The daemon exits 60 s after it was last needed; the sockets stay.
- **Sender page:** HTTP on `127.0.0.1:47823`. The port is fixed because the sender's origin storage is keyed by it. It serves `GET /sender`, `/assets/*`, the WebSocket `GET /local/v1/sender`, and the local owner capture API/page (CONTRACT §10.1). The capture API requires a short-lived credential issued over the owner Unix socket; it has no CORS or cookie auth.
- **Single instance:** `flock` on `$XDG_RUNTIME_DIR/passstream/host.lock`.
- **Key:** reads `~/.config/passstream/key` with the PassPage guards: open once with `O_NOFOLLOW|O_NONBLOCK`, regular file, owned by the user, mode `600` or `400`, ≤ 4 KiB, one printable token. Status reports `missing | unsafe | invalid | rejected` distinctly. Recommended scope: `streams`.
- **Server URL:** from the config or `--server`, validated like the PassPage `baseUrl` (one http(s) origin; https unless loopback; no userinfo, query, fragment, controls or globs). The signaling URL is derived from it, never taken from a server response.
- **Treats the server as hostile input:** bounded response sizes, schema-validated JSON, strings clamped and stripped of control, bidi and zero-width characters before they reach the plugin.

### 5.2 How the sender page authenticates to the daemon

The loopback listener is reachable by every local user and by every website the user visits. The checks on each WebSocket upgrade:

1. **Peer UID by the full tuple.** The daemon looks up the connection in `/proc/net/tcp{,6}`: a socket in state `ESTABLISHED` (`01`) whose local address is the client's ip:port and whose remote address is `127.0.0.1:47823`, the reverse of the accepted connection's tuple. Exactly one socket must match, and its uid must be the daemon's. Repeated procfs observations count once only if their nonzero inode, normalized local/remote tuple, state and UID all agree. Distinct inodes, zero-inode duplicates and contradictory observations are never coalesced, even when their UIDs agree. Zero matches, two or more, or a read error fail closed with HTTP 403. Linux only.
2. **Host** must be exactly `127.0.0.1:47823`. This blocks DNS rebinding.
3. **Origin** must be exactly `http://127.0.0.1:47823`. A missing `Origin` header, or `Origin: null`, is refused.
4. **Mutual authentication** (below) before anything else on the socket.

`GET /sender` and `/assets/*` need checks 1 and 2 only. Browsers send no `Origin` on a top-level navigation, and the page's code is public anyway.

**What this authenticates.** These checks prove the client runs as the same UID and is a page on the sender origin. They don't prove it's the dedicated browser profile: any same-UID process can open such a connection. That's the account boundary from §1.

**Implementation gate (§11 Q1).** Check 1 holds for the tested Chromium network service and remains on by default. If it is unavailable, `peer_check = launch_file` explicitly disables it; mutual authentication still applies. Launch keys always use the **launch file**, in both modes: the daemon writes `$XDG_RUNTIME_DIR/passstream/launch-<random>.html` (mode `0600`, in the `0700` directory) containing a redirect to `http://127.0.0.1:47823/sender#i=<launch_key_id>&k=<launch_key>`, and launches `--app=file://<that path>`. Other UIDs can't read the file, so they can't learn the key, and the daemon deletes the file when the launch key is retired or expires (§5.3). The gate must also confirm that `--app` follows that navigation. Without check 1, any local UID can open the WebSocket, so check 4 is the only thing between it and the attachment, which is why the sender WebSocket never relies on a bearer token. The separate `/owner` HTTP API does use a short-lived bearer token issued only over the owner Unix socket. In `peer_check=launch_file` mode the peer-UID check is disabled for HTTP too: that token is the only authentication barrier against other native local users, who can forge Host/Origin headers. Host, Origin and no-CORS checks still protect against websites; they do not replace the token. Never share it, put it in a URL, or send it to the remote server.

**Mutual authentication (check 4).** Every connection, first launch and reconnect alike, runs the same exchange (CONTRACT §11.2):

1. Page → daemon `hello {key_id, nonce_p}`. The key id is public; the key itself never travels after the page has it.
2. The daemon looks up the key by `key_id`, draws `nonce_d` for this connection and answers `challenge {nonce_d, proof_d}`, where `proof_d` is an HMAC under the key over a role byte, the key id and both nonces.
3. The page verifies `proof_d` in constant time. On failure it closes the socket and sends nothing more to that peer. On success it answers `auth {proof_p}`, the same HMAC with the page's role byte.
4. The daemon verifies `proof_p` against the `nonce_d` it drew for **this** connection, within 5 s, one attempt per connection. Only then is the connection authenticated: it may supersede an existing attachment, and it gets `init`.

The role byte stops reflection, the daemon's per-connection nonce stops replay, and the page's nonce stops a stranger from presenting a proof it collected earlier. The daemon answers `challenge` to anyone who knows a key id, so a stranger can collect `proof_d` values, but only for nonces that the real page will never use again, and it can't relay a live exchange: the page connects only to its own origin, which has one listener, and the daemon's other socket is `0600`. These proofs authenticate only the sender WebSocket; `/owner` and the local HTTP API use the separate Unix-issued bearer credential described above. After step 4 the WebSocket itself is the authenticated channel; loopback traffic can't be read or injected by another non-root UID.

### 5.3 Attachment, recovery and the recovery record

**Launch key.** For each Go-live the daemon mints a 32-byte `launch_key` and a public 16-byte `launch_key_id`. It always writes a `0600` launch file in its `0700` runtime directory containing a redirect to `http://127.0.0.1:47823/sender#i=<launch_key_id>&k=<launch_key>`, and launches `--app=file://<path>`. Neither key nor full link enters argv. The fragment never reaches an HTTP server, and the page only proves it holds the key (§5.2). The key expires after 60 s. The launch file is removed on handover, expiry or stream end.

**Handover from launch key to attachment key.** The launch key isn't consumed by the first message. It's retired only once the page provably holds its replacement, so a lost `init` can always be retried:

1. The first time a connection authenticates with the launch key, the daemon generates the stream's **attachment key** (32 bytes) and `attachment_key_id`, writes them to `host-state.json` (fsync), and sends `init` with `attachment {key_id, key}`.
2. The page writes the attachment key and id to `sessionStorage`, and only then sends `init_ack`. From then on it authenticates with the attachment key only.
3. The daemon **retires the launch key** (forgets it, deletes the launch file) on `init_ack`, or on the first connection that authenticates with the attachment key, whichever comes first.
4. If the socket drops before `init_ack`, the page reconnects with the attachment key if it stored it, else with the launch key. Until it's retired, the launch key authenticates again and gets an `init` with the **same** attachment key, so nothing is minted twice and nothing is stranded. (The launch key is in the daemon's memory only, so a daemon restart during the handover ends it: a page that already stored the attachment key attaches; otherwise the launch fails and the owner starts again.)
5. The page reads the fragment once, keeps the launch key in `sessionStorage` (so a reload during the handover can retry), removes the fragment with `history.replaceState`, and deletes the launch key from `sessionStorage` after `init_ack` is sent.

A launch key that is retired or expired gets `launch_consumed` or `launch_expired`. Those arrive before any proof, so anyone holding the port could send them: the page shows them and keeps retrying, and a page with no attachment key gives up after 60 s (CONTRACT §11.2). If no connection authenticates with the attachment key or acks within 60 s of minting, the launch fails with `browser_failed` and the daemon stops the browser.

**One active attachment.** A stream has at most one sender attachment: one authenticated WebSocket, one page instance holding a `SenderHost`. The rules:

- The attachment key lives as long as the stream. It's never rotated, so a reconnect can't race a rotation.
- **Replacement needs the page's proof.** A connection that completes the exchange in §5.2 **supersedes** the attached one: the old socket gets `{"type": "error", "code": "superseded"}` and closes. A connection that fails or abandons the exchange changes nothing. This is a reload of the same tab, or a tab duplicated with its `sessionStorage`. A superseded page stops its capture tracks, drops its `SenderHost` and shows "This window is no longer the sender". It never retries.
- The daemon runs the exchanges of one stream one at a time; the last to complete is the attachment.
- Inside the browser, the page also takes the Web Lock `ps:sender:<stream_id>` before it creates a `SenderHost`, so two tabs of the profile can't both run one.

**The recovery records.** Everything needed to rebuild the attachment after a crash, split by who must not see what:

| Where | Holds | Notes |
|---|---|---|
| Page origin storage, `ps:stream:<stream_id>` (`LockedLocalStorage`) | `v`, `stream_id`, `server_url`, `view_capability`, `identity_seed`, `link_secret`, `pin_state {version, policy, pin}`, `knock`, `pending_rotation` (§5.5), `latches` (§5.6), `created_at` | The secrets never leave the dedicated profile, except PIN and link to the daemon over the local WebSocket. |
| Page `sessionStorage` | `attachment_key_id`, `attachment_key`; the launch key during the handover | Per tab, so a new window of the profile can't attach. |
| E2EE library storage (same origin) | Guess budget, grants with `viewer_id` and row code, revocation fence with `pin_version`, epoch counter | PROTOCOL.md §5.6, §6, §7. |
| Daemon, `$XDG_STATE_HOME/passstream/host-state.json` (`0600`, written with fsync) | `v`, `stream_id`, `server_url`, `publish_token`, `knock`, `latches` (a mirror), `attachment {key_id, key, page_instance, attached_at}`, `browser` (§5.9) | No PIN, link or link secret. Deleted when the stream ends. |

`knock` is fixed at creation and written to both records then. On every `init` the page sets `knock = record.knock OR init.policy.knock` and persists it if it changed. `browser` and `page_instance` say which browser owns the attachment.

**Page load versus reconnect.** There are two different recovery paths, and the page tells them apart by its own memory, not by anything the daemon says:

- **Page load** (first launch, a reload, or a browser restart): a fresh JavaScript context with no `SenderHost`. It runs the full **startup order** below and creates a new `SenderHost` and a new `instance`.
- **Reconnect**: the same JavaScript context, with its `SenderHost` still running, whose local WebSocket dropped (a daemon restart or crash, or a socket error). Media and admissions carry on throughout. The page re-authenticates with the attachment key (backoff 100 ms, doubling to 1 s, for as long as the page lives) and runs the **reconnect steps** below. It never recreates the host, rejoins `/signal`, asks for capture again or uses `init.policy`.

**Startup order** (page load), in this order:

1. Authenticate (§5.2): with the attachment key from `sessionStorage`, else with the launch key from the fragment or `sessionStorage`, else show "Open PassStream from the bar or the CLI" and stop.
2. Load `ps:stream:<id>`, or create it on a first launch only, from `init.stream` and `init.policy`. If `init` came from an attachment-key exchange and the record is missing or corrupt, the page can't rebuild the link: it reports `ended {reason: "state_lost"}`, and the daemon ends the stream.
3. Merge: `latches = page.latches OR init.latches`, with `frozen` also ORed with the library's `inspect().frozen`, and `knock` as above. Persist the result.
4. Reconcile the PIN (§5.5).
5. `SenderHost.create({..., knock, locked: latches.locked || frozen gate || reconciling})`, so it starts locked when it must.
6. Join `/signal` as publisher and wire the E2EE relay.
7. Ask for capture (it needs a user gesture after a reload). Attach tracks only if `paused` is false; otherwise keep them unattached until a local `resume`.
8. Send `state`, then `secrets` (§5.4).

**Reconnect steps** (same context):

1. Authenticate with the attachment key (§5.2).
2. `init.stream.id` must equal the running host's stream id; otherwise the page reports `ended {reason: "state_lost"}`.
3. Merge latches the same way and **apply any newly set one to the running host** (`host.lock(true)`, detach tracks). A reconnect can set a latch, never clear one.
4. If the merged `knock` is true and the running host was created without it, latch `locked`, alert `state_mismatch` and keep running locked. A running host's knock mode can't change.
5. Send `state`, then `secrets`. A pending rotation keeps its own retry loop (§5.5).

**Daemon side.** The daemon can't see a page load; it sees only connections. At its own startup it reloads `host-state.json`, re-adopts the browser (§5.9) and puts the attachment in `detached`. Whichever path the page takes, the next connection that authenticates with the attachment key attaches. If none does within 10 s of the daemon's start (or of the socket drop, for a daemon that stayed up), or the browser exits, the daemon ends the stream with `window_closed`.

**Secrets after every reconnect.** The daemon keeps the PIN and link only in memory. After every authenticated connection (first launch, reload, reconnect, daemon restart), the page sends `secrets` again. The daemon reports `has_link: false` until it has them.

**Stop is locally enforceable.** The daemon launches the browser in its own transient unit:

```
systemd-run --user --unit=passstream-sender-<stream_id> --collect \
  --property=TimeoutStopSec=5 -- chromium --app=… --user-data-dir=$XDG_STATE_HOME/passstream/chromium …
```

It's a separate cgroup, so a daemon restart doesn't kill it, and stopping it kills every Chromium process. Without systemd, the daemon uses a process group instead (§5.9). `stop` from the plugin or CLI:

1. daemon → page `command stop` (the page stops its tracks and sends `leave`), waiting at most 2 s;
2. `DELETE /api/v1/streams/{id}` (best effort; a failure is reported but doesn't block step 3);
3. stop the browser: `systemctl --user stop passstream-sender-<stream_id>.service` (SIGTERM, then SIGKILL after 5 s), or the process-group kill in §5.9.

Step 3 runs even if the page never answered, so capture ends regardless of what the page does. Before a launch, the daemon stops any leftover sender browser, because Chromium hands a second launch on the same profile to the running process.

**Window closed:** the browser exits, or the local WebSocket drops and isn't re-authenticated within 10 s. The daemon ends the stream and emits `ended {reason: "window_closed"}`.

**Server-side stop** (dashboard `DELETE`): the server revokes publishing and sends `stream.ended`; the page reports `ended`; the daemon stops the browser.

**Daemon restart while live:** media is unaffected, because the page talks to the server directly. The page takes the reconnect path. The daemon reloads `host-state.json`, and the page re-sends `state` and `secrets`.

### 5.4 Launch sequence (Go live)

1. Plugin → daemon `start {knock: false}` (no `pin_policy`: use defaults) or CLI → daemon `start {pin_policy: {kind: "none"}}` (explicit).
2. The daemon runs the port check (§2.2), reads the server defaults, computes the creation policy (§4.4) and calls `POST /api/v1/streams` → `{stream, view_link_base, publish_token}`.
3. It writes `host-state.json` (with `knock`), mints the launch key and starts the browser (a transient unit, or §5.9) with a **dedicated profile** (directory `0700`: no extensions, no shared history, its own storage).
4. The page and the daemon authenticate with the launch key (§5.2). Daemon → page `init {stream, policy, latches, attachment, active_stream_ids}`. The page stores the attachment key and sends `init_ack` (§5.3).
5. The page runs the startup order in §5.3. On a first launch it generates the identity seed, link secret and PIN (§5.5) and writes the recovery record **before** it reports anything.
6. **The link becomes available** once the recovery record is durable, the `SenderHost` exists and the page has joined `/signal` as publisher. The page then sends `secrets {pin, link}` and the daemon sets `has_link: true` and emits `link_ready`. Viewers who open the link before the first frame see "Waiting for the host's screen".
7. On the first encoded frame the page reports `phase: "live"`. The daemon emits `live`. The plugin copies the link (wl-copy over stdin), toasts **"Live · link copied"**, and turns on do-not-disturb, the idle inhibitor and the border.

`passstream share` waits for `live` by default (up to `--timeout`, 120 s), because the picker needs a human. With `--no-wait` it returns at `link_ready`. On timeout it stops the stream and exits with `picker_timeout`.

### 5.5 PIN generation, rotation intent and reconciliation

**Generation.** The sender generates digit PINs with `crypto.getRandomValues`, rejection-sampled to a uniform `0…10^n−1` and zero-padded. A rotation regenerates until the value differs from the current PIN. A passphrase is never generated: it comes over the local channel.

**Explicit values** reach the daemon only over its socket. The CLI reads them from a no-echo prompt on the TTY, from stdin (`--pin-stdin`) or from a file descriptor (`--pin-fd N`). They're never in argv (CONTRACT §12).

**The problem.** The page stores the PIN in `ps:stream:<id>`, and the library stores the revocation generation and the budget. A crash between the two writes would leave them disagreeing. For example, the old PIN could come back with a budget that was reset for the new one, which hands an attacker another 20 guesses at the old PIN.

**Rotation intent.** Every PIN change (`pin.rotate`, `pin.set`, `unfreeze` without `keep_pin`, `kick` with `rotate_pin`) goes through one procedure:

1. **Validate, writing nothing.** Refuse the request at once, with no intent persisted, if:
   - another intent is pending → `rotation_pending` (except in `state_mismatch`, below);
   - the policy or value is malformed: a value supplied for a generated policy, a passphrase that's missing (`passphrase_required`) or outside 8–256 bytes after NFC, digits that don't match the policy → `invalid_argument`;
   - the new value equals the current PIN after NFC, `none` → `none` included → `invalid_argument`. (A generated value never does: generation retries until it differs.) This is the same check the library makes (PROTOCOL.md §5.8), made first, so a refusal never reaches the retry loop.
2. Compute the new value: generated, or supplied locally.
3. Persist `pending_rotation {version: pin_state.version + 1, policy, pin, op, admission_id?}` in `ps:stream:<id>`, and await the write.
4. Set the `reconciling` gate, which keeps the host locked.
5. Call the library with the version: `host.rotatePin(pin, {version})`, or `kickAndRotatePin(id, pin, {version})`, or `unlock({newPin, version})`. The library writes `pin_version = version` into its revocation fence in the same write-ahead write as the generation bump. It marks the rotation complete only after the grant cleanup and the budget reset (§6.4).
6. On success, persist `pin_state = {version, policy, pin}`, clear `pending_rotation`, clear `reconciling` (the lock latch still applies), send `secrets` and `state`, and reply `ok`.
7. On failure, by error:
   - **`storage`**: **retryable, by the page alone.** Keep `pending_rotation` and `reconciling`, and retry **with the same value and version** (100 ms backoff, doubling to 2 s) until it succeeds or the stream ends. The library accepts the same new PIN once more to finish a rotation it started. The command replies `storage` with `details: {pending: true}`: the change is accepted and will finish by itself, so the caller must not resend it. `state.access.rotation_pending` is `true`, and the plugin shows "PIN change pending (storage)".
   - **Any other error on the first call** (`invalid_argument`, `unknown_admission`, …): **terminal.** The library checks its arguments before its write-ahead write, so nothing changed. Clear `pending_rotation` (persisted) and `reconciling`, and reply with that error. The caller may send a corrected request.
   - **Any other error on a retry**, when a write may already have landed: the page can't tell what changed, so it fails closed to `state_mismatch` (below).

**Clearing and replacing an intent.** A pending intent is cleared on success, on a terminal first-call refusal, or when the stream ends and its record is deleted. It's never replaced by an ordinary request: while it's pending, every PIN change gets `rotation_pending`, which is **retryable**: wait until `access.rotation_pending` is `false`, then send the request again (the CLI does this with `--wait`). If storage never recovers, the stream stays locked and the way out is `stop`. The one replacement is the fail-closed state: in `state_mismatch`, a local `pin.rotate`, `pin.set` or `unfreeze` without `keep_pin` overwrites `pending_rotation` with version `max(page version, library version) + 1` and runs from step 3.

**Reconciliation at startup.** Before `SenderHost.create`, the page reads the library's persisted state with `SenderHost.inspect(storage, streamId)` → `{pin_version, rotation_complete, frozen}`:

| Page record | Library | Action |
|---|---|---|
| No pending; `pin_state.version = v` | `pin_version = v`, complete | Agree. Create with `pin_state.pin`. |
| Pending `v+1` | `pin_version = v`, complete | The bump never landed. Create with the old PIN, `reconciling` set, then finish the rotation with the same pending value (step 5). |
| Pending `v+1` | `pin_version = v+1`, not complete | The bump landed and the cleanup didn't. Create with the pending PIN, `reconciling` set, and call the same rotation again to complete it. |
| Pending `v+1` | `pin_version = v+1`, complete | Only the page's final write is missing. Do step 6. |
| Anything else (corrupt, or versions out of order), or a non-`storage` error while finishing a rotation | | **Fail closed:** latch `locked` and `frozen`, keep `reconciling`, and alert `state_mismatch`. Only a fresh local rotation (a new version above both) clears it. |

`SenderHost.create({pinVersion})` also refuses every admission with a signed `locked` while its PIN version and the fence disagree or the rotation is incomplete, so a page bug can't skip this table. **Admissions open only when the PIN, the policy (same version), the revocation generation and the budget all agree.**

**Display.** `pin.reveal` returns the current PIN, plus the pending one with `pending: true` while a rotation is incomplete.

### 5.6 Privacy latches

Three latches are persisted in `ps:stream:<id>.latches` and mirrored in the daemon's `host-state.json`:

| Latch | Set by | Cleared by | Effect |
|---|---|---|---|
| `locked` | `lock`, kick + lock, `state_mismatch` | local `lock {locked: false}` | `host.lock(true)`: no new PIN admissions. Grant re-admissions of current viewers still work. |
| `paused` | `pause` (any channel) | local `resume` | No audio and no video leave the page (below). |
| `frozen` | the library's budget freezing; `state_mismatch` | local `unfreeze` | The page holds `host.lock(true)` while it's set, even if the library's budget record was reset or lost. |

**Write order. A latch is persisted before the command that set it is acknowledged.** Setting a latch:

1. **Daemon-originated** (plugin, CLI, shell): the daemon writes the latch into `host-state.json` and fsyncs it **before** it forwards the command. The page then applies it in memory, persists it to its record, and replies.
2. **Page-originated** (a server `control`, the sender window's own button, `state_mismatch`): the page applies it in memory synchronously (`host.lock(true)`, tracks detached), awaits the write to its record, sends `state` to the daemon and, if attached, awaits the daemon's `state_ack` for that `seq` (the daemon fsyncs its mirror first) for at most 1 s. Only then does it reply.
3. **Library-originated** (`frozen`): the library persists the freeze in its budget record before it emits the event, and startup ORs in `inspect().frozen`.

**The exact window** in which a crash can lose a latch is therefore: for (1) and (3), none; for (2), from the in-memory apply until the page's record write resolves (one awaited storage write, typically milliseconds), and, if a browser-process or OS crash rather than a page crash, until the daemon's fsync, or until Chromium flushes local storage to disk when the daemon is detached. A latch lost in that window was never acknowledged, so no caller was told it took effect, and the in-memory apply already stopped the frames and admissions. If the record write fails, the latch stays applied in memory, the page retries the write with backoff, and the command replies `storage` with `details: {applied: true}`.

**The daemon's mirror** sets a latch from any `state` that has it set, and clears one only on the `ok` result of a clearing command it forwarded, or on the page's `latch_cleared` after a clear from the sender window's own UI, never because a `state` lacks it (a `state` sent before the command applied would otherwise undo the write-ahead). While the page is detached, `pause` and `lock {locked: true}` from the plugin or CLI still succeed: they're in the mirror and reach the page through `init.latches` at the next attach.

A failed daemon mirror write stays dirty. A subsequent restrictive latch command retries the write even if memory already has the latch, and returns `internal` while it fails; `state_ack` is withheld. A new authenticated exchange also writes before `init` or supersede and, on failure, drops the socket without an authoritative error. The daemon reports only disk-saved mirror latches and retries writes after 1 s, 2 s, … up to 5 s. Three consecutive failures raise `revocation_durability {not_durable: true}` and the status flag; success clears the daemon's warning. See CONTRACT §10.

Clearing goes the other way: persist the cleared value in the page record, then apply it, then report it. If the page reloads before the mirror is cleared, the OR-merge sets the latch again; that's the safe direction, and the owner clears it again.

**Pause covers audio and video, independently of encryption.** On pause, the page sets `enabled = false` on the capture tracks; video is replaced on the wire by the sender-drawn "Paused" picture, and the disabled audio track sends only encoded silence (as built in phase 2c: detaching audio with `replaceTrack(null)` left the SFU deaf to it after resume, see PROTOCOL §19). No capture content leaves. Only a local `resume` enables them again. Encryption suspend and recovery (`onSuspend`, `onEpoch`, `replaceEncryptor`) control the worker's keys only. They never call `replaceTrack` and never consult or change `paused`. A frame leaves only if `!paused`, the track is attached and an epoch is active.

**Recovery never loosens.** Each of these re-reads the latches and applies them before anything is sent:
- a page reload (startup order, §5.3);
- a peer-connection replacement (the new connection's senders start with `null` tracks, and tracks are attached only if `!paused`);
- worker recovery;
- a WebSocket reconnect;
- a fresh `init` (latches are OR-merged; `init` can't clear one).

There's no "reset to defaults" path that clears a latch.

### 5.7 Secrets at the edges

- **argv:** no PIN, passphrase, link, key or token. The launch key travels only in the guarded launch file and is never sent by the page. The CLI takes PINs by prompt, `--pin-stdin` or `--pin-fd`. `--pin 4|6|none|passphrase` names a policy, never a value.
- **Toast:** "Live · link copied". Never the PIN: notifications can be captured by the share itself and are kept in the notification history.
- **Masked by default.** The PIN shows as `••••` in the plugin panel, the sender window and the CLI.
  - Reveal shows it for **10 s**, then masks it again.
  - The sender window is the window most likely to be in the share. It asks Hyprland to keep its reveal out of capture with a `noscreenshare` window rule for class `passstream-sender`, an implementation gate (§11 Q7). Where that isn't available, the reveal button says "Visible in your share".
  - The plugin panel uses the matching layer rule when the shell supports it, and the same warning otherwise.
  - The CLI prints the PIN only with `pin --reveal`. On a TTY it erases the line after 10 s; scrollback and terminal logs may keep it, and the help says so. With `--json` it prints the PIN in plain text, because that output is meant for a program.
- **Clipboard:** "Copy link" and "Copy PIN" are separate actions; the link alone is auto-copied on Go-live.
  - Copies offer the `x-kde-passwordManagerHint: secret` type, which some clipboard managers honour by not keeping a history entry.
  - Copies use `wl-copy --sensitive`, never `--paste-once`, and clear after 30 s only while the copy still owns the selection (CONTRACT §12, Appendix A #20). A newer clipboard selection must survive. The CLI helper holds the selection with `wl-copy --foreground`; it ends on replacement or deadline.
  - Documented limit: a clipboard manager (cliphist, for example) may already have stored the value, and clearing the clipboard doesn't remove history entries.
- **Profile cleanup is logical, not secure erasure.** When the stream ends, the page deletes `ps:stream:<id>` and the library's keys for it. On every `init` it also deletes records for streams not in `active_stream_ids`. Chromium's storage (LevelDB) and the SSD can keep old copies until they're overwritten. The docs say "removed from the profile", never "erased".
- **Logs:** no secret or credential is ever logged by the daemon, the page, the plugin, the CLI or the server (CONTRACT §2).

### 5.8 How the plugin learns status

The plugin keeps one socket connection open, sends `subscribe`, and gets `status` events (a full `HostStatus`, at most 4 per second), plus `alert`, `link_ready`, `live` and `ended`. On reconnect it calls `status` again, so there's no polling. `omarchy-shell passstream start|pause|resume|stop|lock|status` routes through the plugin's `IpcHandler` to the daemon. `status` never includes the PIN or the link.

### 5.9 Supervision without systemd (`self_bound`)

With socket activation, systemd keeps the listeners, starts the daemon on demand and runs the browser in a transient unit. Without a systemd user manager, the same jobs are done like this, and `host status` reports `port_mode: "self_bound"`, `supervision: "process_group"`.

**Starting the daemon.** The plugin and the CLI connect to the Unix socket. If nothing listens, they spawn `passstream host --detach`, which double-forks, calls `setsid`, takes the `host.lock` `flock` (a second instance exits at once), binds both listeners itself and then reports ready on the socket. The client retries the connection for up to 5 s. The plugin repeats this whenever its socket closes, so a crashed daemon comes back as soon as the plugin notices; the page, which can't start processes, keeps reconnecting meanwhile (§5.3). The idle exit (60 s) applies only while no stream exists, and the port check (§2.2 check 2) runs again before the next launch.

**Launching the browser.** The daemon starts Chromium directly, in a **new session and process group** (`setsid`, so the process group id is the browser's pid), with stdin, stdout and stderr on `/dev/null` and the same arguments as the unit. It records `browser {pid, start_time, pgid}` in `host-state.json`, where `start_time` is field 22 of `/proc/<pid>/stat`, and opens a `pidfd` (`pidfd_open`, Linux ≥ 5.3) on it. The browser survives a daemon restart, because nothing ties it to the daemon's lifetime.

**Watching it.** The daemon polls the pidfd. When it becomes readable the browser has exited: window closed (§5.3). After a daemon restart, the daemon re-adopts the browser from `host-state.json`: it opens a pidfd on `pid` and accepts it only if `/proc/<pid>/stat` still shows the recorded `start_time` and the process belongs to the daemon's uid. Otherwise the browser is gone, and the stream ends with `window_closed`.

**Killing it** (step 3 of `stop`, a failed launch, or a leftover browser before a launch):

1. `kill(-pgid, SIGTERM)`, and wait on the pidfd for up to 5 s;
2. `kill(-pgid, SIGKILL)`;
3. **sweep:** every process of the daemon's uid whose process group (field 5 of `/proc/<pid>/stat`) is `pgid` and whose `start_time` is at or after the browser's gets `SIGKILL`, and so does every process of that uid whose command line contains the exact argument `--user-data-dir=<verified profile>` (never argv[0]). Repeat until none is left, for up to 2 s.

The process-group match covers what Chromium spawns, provided it keeps its children in the launching process group; the profile match covers any that leave it. That's an implementation gate (§11 Q8). The test bound stays the same: all Chromium processes gone within 7 s of `stop`.

## 6. Admission transport

The E2EE handshakes and rekeys (PROTOCOL.md §4–§7) travel through the server. This section is what lets the server, the sender page and the viewer page be built separately. The wire is in CONTRACT §8.2–§8.4.

### 6.1 The relay envelope

- **Carrier:** the signaling WebSocket on both sides, not a data channel. It's ordered, as the library requires, and it exists before ICE completes, so admission runs in parallel with media setup.
- **Envelope:** viewer → server `{"type": "e2ee", "b": "<b64url bytes>"}`; server → publisher `{"type": "e2ee", "peer": "c_…", "b": …}`; publisher → server `{"type": "e2ee", "to": "c_…", "b": …}`; server → viewer `{"type": "e2ee", "b": …}`. The server tags each viewer's messages with that viewer's `connection_id`, which becomes the library's `peerId`.
- **Bounds:** at most 4096 bytes decoded per message (PROTOCOL.md §4). Per viewer: 16 messages per second towards the publisher (burst 32), excess dropped and counted; a queue of 64 messages towards the viewer, and overflow closes that viewer (`peer_closed`, reason `overflow`). The publisher's socket is never blocked by one viewer.

### 6.2 Peer lifecycle

- `e2ee.peer_closed {peer, reason}` (server → publisher) when a viewer's signaling socket closes, times out, is kicked or banned, or overflows. The page calls `host.peerClosed(peer)`.
- `e2ee.close {to}` (publisher → server) asks the server to close that viewer, for example after `kicked` lists its peers.
- **Publisher reconnect:** after a publisher `join`, the server sends `e2ee.peers {peers: [...]}` with the current viewer connections. The page calls `peerClosed` for every peer it knows that isn't listed.
- The server sends viewers `e2ee.sender_back {instance}` when a publisher joins. A viewer whose last-seen `instance` differs (the sender reloaded, so its sessions are gone) re-admits with its grant, after a random delay of 0–2 s. A viewer with the same `instance` keeps its session. It re-admits only if it sees frames of a newer epoch it has no key for, for more than 2 s.

### 6.3 Knock (pending approval)

Knock mode is chosen when the stream is created and never changes (§6.4 item 1). When it's on, a handshake whose key confirmation (`Tb`) verifies doesn't commit. It enters **pending approval**:

1. The guess reservation settles as success (the PIN was right; knock is an extra gate).
2. The library mints a `knock_id` (16 random bytes) **bound permanently to this one handshake object**, and a row code. It sends the viewer `PENDING` (type 0x08), a box under a key derived from this handshake's ISK, which carries the row code's index and the timeout. The server sees an opaque message.
3. The knock appears in `host.knocks()` and in `SenderState.knocks` with its row code. The viewer shows "Waiting for the host · your code K7P · purple".
4. **Approve** (local only, with a matching `row_code`): the library runs the normal `commit()` with all its commit-time checks (lock latch, PIN generation and version, `reconciling`, revocation, and that the handshake is still current on its peer), and only then sends `ADMIT`. If any check fails, the approval fails with that check's error, and the knock ends with a signed `REJECT`.
5. **Deny, timeout (120 s), peer close, a new HELLO on that peer, PIN rotation, or stop:** the knock ends with a signed `REJECT` reason 8 `denied` (where the peer is still there). Its `knock_id` is invalid forever.
6. Approving an ended or unknown `knock_id` fails with `unknown_knock`. A knock id is never reused or rebound to another handshake. **No key material leaves the sender before approval and the commit-time checks.**

At most 50 knocks are pending. Beyond that, a verified handshake gets `REJECT denied` with `retryAfterMs = 30000`. Knock mode applies to PIN admissions (and to link-secret admissions on a stream without a PIN). Grant re-admissions don't knock: every grant on a knock stream came from an approved knock.

### 6.4 e2ee library additions

These are the changes the library needs. They're in scope for the e2ee worktree and are pre-release format changes to PROTOCOL.md.

1. **Pending approval.**
   - `SenderHost.create({knock, knockTimeoutMs = 120000, maxKnocks = 50})`. Knock mode is fixed for the stream's life in v1.
   - `host.knocks(): KnockInfo[]`, with `{knockId, peerId, rowCode, at, expiresAt}`.
   - `host.approve(knockId, {rowCode})` runs `commit()`; it throws `unknown_knock`, `code_mismatch` or the commit error.
   - `host.deny(knockId)`.
   - Events `knock` and `knockEnded {knockId, reason}`.
   - Wire: message `PENDING` 0x08, S→V, with one field: `box` (22 bytes: 6 bytes of plaintext and the 16-byte GCM tag) = AES-256-GCM(`pending_key`, nonce `0^12`, aad `"PassStream v1 pending"`, `u16(code_index) || u32(timeout_ms)`), where `pending_key = HKDF-SHA-512(ISK, salt=sid, info="PassStream v1 pending", 32)`. The whole message is 25 bytes (version, type, a 1-byte length, the box).
   - REJECT reason 8 `denied`.
   - The viewer state machine gains `pending` between `RESPONSE` sent and `ADMIT`/`REJECT`.
2. **Grant-scoped `viewerId`:** 16 random bytes created with each grant and stored with it. `ViewerInfo.viewerId` is the same for every admission made with that grant.
3. **Row code:**
   - Assigned per grant (and per knock, carried into the grant it produces), unique among the stream's live grants and knocks, and stored with the grant.
   - The admit bundle gains `u16(code_index)`: 95 + 2 = 97 bytes of plaintext, so the ADMIT box is 113 bytes. The index is in 0–10 647 (§3.3), and the viewer rejects anything larger as `malformed`. There's no colour on the wire.
   - Exposed as `ViewerInfo.rowCode`, `KnockInfo.rowCode` and `ViewerAdmission.result.rowCode` / `.pending.rowCode`, each as the canonical string (`"K7P"`), plus a pure helper `rowCodeColour(code)` for display.
4. **PIN version in the fence:**
   - `rotatePin(pin, {version})`, `kickAndRotatePin(id, pin, {version})` and `unlock({newPin, version})` write `pin_version` with the generation bump, and set `rotation_complete` only after the grant cleanup and the budget reset.
   - `static SenderHost.inspect(storage, streamId)` → `{pinVersion, rotationComplete, frozen}`.
   - `create({pinVersion})` refuses all admissions with a signed `locked` while they disagree or the rotation is incomplete.
5. **`create({locked})`**, so a host can start locked before its first `receive`.
6. Remove nothing else. The earlier idea of `rotatePin(pin, {keepBudget})` is dropped: kick + lock is the remote substitute.

## 7. Dashboard

Served by the server and behind a session login: the login form sends the key once as `Authorization: Bearer` to `POST /api/v1/session`, forgets it, and gets an HttpOnly session cookie (CONTRACT §7).

**Shows:**

- Streams with phase, sender online, the watching count, PIN state ("PIN · 4 digits", "No PIN" in warning colour, never the value), latches, knock mode, rotation pending, E2EE status (beta), mode and expiry.
- The joined viewer table with provenance badges and row codes, sender health, history and the audit log (CONTRACT §9.4–§9.5).

**Can do:** kick, kick + lock, ban, pause, lock, deny a knock, stop, and set server-side settings (retention, max viewers, expiry, defaults for *new* streams, keys).

**Can't do:**

- Resume, unlock, unfreeze, approve a knock, or rotate or change the PIN. These are shown disabled with "Do this on the host machine (bar plugin, `passstream` CLI, or the sender window)."
- Copy the share link. The server never has the link secret, so the dashboard shows "Link is on the host machine."

**Banner on live pages:** "This view is relayed by the server. The bar plugin shows the sender's own view. To check who someone is, ask them for their row code."

Changing the defaults affects only streams created afterwards (§4.4). Key management needs the owner key as a bearer token, not the session cookie.

## 8. Agents

### 8.1 Local agents (on the host machine)

They use the CLI, which talks to the daemon socket with the same authority as the plugin (CONTRACT §12):

```
passstream share [--pin 4|6|none|passphrase] [--pin-stdin|--pin-fd N] [--knock] [--no-wait] [--json]
passstream status|viewers|link [--json]
passstream pin [--reveal] | pin rotate | pin none | pin set 4|6|passphrase [--pin-stdin|--pin-fd N]   (each change: [--wait])
passstream kick <admission_id> [--lock] [--rotate-pin] [--action-id ID]
passstream ban <connection_id> [--action-id ID]
passstream lock | unlock | unfreeze [--keep-pin] | pause | resume | stop
passstream knock approve <knock_id> --code K7P | knock deny <knock_id>
passstream copy link | copy pin
```

**"Turn the PIN off"** is `passstream pin none` (or `share --pin none`): CLI → Unix socket (owner uid) → daemon → page `pin.set {policy: {kind: "none"}}` → a rotation intent (§5.5) → `host.rotatePin(null, {version})`. The plugin and the sender window then show "No PIN" in warning colour and toast "PIN turned off (CLI)". Owning the account is the authentication.

### 8.2 Remote agents (HTTP API, scoped key)

They have the dashboard's powers: create a stream (when the agent *is* the sender, for example a headless browser opening `host_url`), list streams, read viewers, history and audit, follow the event stream, and run restrictive commands. A request for a permissive op gets `403 local_owner_required`.

**Both kinds** follow the agent rules in CONTRACT §13: no decisions from relay metadata, and idempotency keys on commands.

The agent change feed (PLAN feature 4) is local only: see [FEATURES.md](FEATURES.md) and CONTRACT §10.1. Remote agents admit through the viewer page (PIN/CPace or knock); the server cannot supply plaintext frames. Pass publishing is an explicit local owner action.

## 9. Threats

| Attacker | Gains | Doesn't gain | Mitigations |
|---|---|---|---|
| **Malicious server** | Denial of service: stop, pause, kick, lock, deny knocks, drop media, refuse to create streams, force `passphrase_required` at creation. All metadata, including PIN *state*, failure counts, admission ids and row codes (PROTOCOL §10). It can lie consistently in the dashboard and API: invent or hide rows, and swap every relayed field between rows so the owner picks the wrong one. It can serve malicious JS to viewers and the dashboard. | The PIN, link secret, seed or keys. Admitting anyone, resuming, unlocking, unfreezing, changing the PIN or resetting the budget. Changing the policy after creation. Clearing a latch. Changing the Omarchy sender's code. Hiding or inventing admissions in the plugin and the sender window. Redirecting a command away from the selected admission. Forging the row code on a viewer's screen. | Sender served locally (§2). Monotonic rule with per-channel schemas (§4). Creation-only policy (§4.4). Persisted latches (§5.6). Joins keyed by `admission_id`, provenance, mismatch flags, row codes (§3). Secrets go to the daemon in a separate message with no server-bound serializer. CSP `connect-src` pinned to the configured server. |
| **Other local user** | Nothing, except during the residual window (§2.2), where it can bind the port before the user's systemd does (then launches are refused), and in `self_bound` mode, a user's own reload of the sender window while the daemon is down. | The socket (`0700` directory, `SO_PEERCRED`), the loopback WebSocket (full-tuple UID check where available), the port while systemd holds it, the launch and attachment keys (never sent; proved by HMAC), replacing the sender (needs the page's proof), replaying anything it saw while holding the port. | §2.2, §5.2, §5.3, §5.9. Lingering recommended. |
| **Website in the owner's browser** | Nothing. | The loopback owner API (Unix-issued credential, no CORS) and the sender WebSocket (Origin, missing or `null` Origin refused, Host, tokens). DNS rebinding (Host). | §5.2. A dedicated profile, so it can't share storage with the sender. |
| **Malicious native code, same UID** | Everything: profile storage (seed, PIN), the daemon's memory, the key file, the socket, ptrace. | — | **Out of scope**, as for every desktop app. Hygiene only: no secrets in argv, `0700` directories and `0600` files, nothing logged, `status` without the PIN. |
| **Shoulder surfing, screen capture, notification or clipboard history** | A PIN the owner revealed while it was visible or copied. | The PIN in toasts or `status`. | Masked by default, 10 s reveal, capture exclusion where possible, separate copy actions, a 30 s clipboard clear (§5.7). |
| **Stolen plugin key** (`~/.config/passstream/key`, `streams` scope) | Remote metadata and restrictive commands. It can create streams, which have no sender. With an `owner` key it can also mint and revoke keys. | The PIN, the link, permissive commands, watching. Local authority is the socket, not the file. | `streams` scope recommended. File guards (§5.1). Revocation, audit log, rate limits. |
| **Stolen dashboard session** | Dashboard powers until it expires (12 h, 1 h idle) or the owner logs out. | Minting keys, the PIN, the link, permissive commands. | HttpOnly, Secure, SameSite=Strict cookie. Origin check on writes. Session ids stored hashed. |
| **Stolen agent key** | Scope-limited metadata, restrictive commands and stream creation. | The PIN, the link, permissive commands, watching. | Scopes, expiry, rate limits, revocation, audit. |

**Honest note:** the dashboard, the API and the server's copy of the viewer table are advisory under a malicious server. The plugin and the sender window are the ground truth for who is admitted, and the row code is the ground truth for who a row is.

## 10. Work this implies

Each workstream builds against CONTRACT.md and can start on day one. Dependencies are marked.

1. **e2ee library** (e2ee worktree): §6.4, items 1–5, with tests. PROTOCOL.md gets the new message type, the REJECT reason, the bundle bytes and the fence fields.
2. **Server** (`internal/server`, `internal/hostview`):
   - the error envelope and key scopes;
   - stream fields, `/viewers`, `/history`, `/audit`, `/events`, and `/commands` with idempotency;
   - `/session`, `/keys`;
   - the E2EE relay with bounds and peer lifecycle (§6);
   - the publisher control channel and `label` in the viewer join;
   - connection telemetry;
   - `/signal` accepting the local sender's Origin.
3. **Daemon** (`cmd/passstream host`, `internal/host`):
   - `host install-units` (units) and `LISTEN_FDS`;
   - the port-owner and full-tuple checks;
   - the socket ops, the mutual authentication, the launch-key handover, attachment and supersede rules;
   - transient browser units, and `self_bound` supervision (§5.9);
   - `host-state.json` with the latch write-ahead and mirror, the join, and the API client.
4. **Web, sender local mode** (`web/src`, `/sender`): the startup order, the recovery record, reconciliation, latches, pause independent of encryption, the command router with per-channel schemas, the two serializers, masked PIN with reveal, and the minimal sender-window UI. The knock UI depends on item 1.
5. **Web, viewer:** the relay envelope, the `pending` screen and the row code display. The new link format (CONTRACT §3). Depends on item 1 for `PENDING`.
6. **Plugin** (`passstream-omarchy-plugin`): socket client, panel, masked PIN, separate copy actions with the clipboard clear, notifications, IPC, `port_not_owned` handling.
7. **Dashboard:** read views, history and audit, restrictive actions, row codes.
8. **Tests** (torture and unit):
   - A fake malicious server that forwards every permissive op and every local-only argument over `control`: the sender refuses them all and its state is unchanged.
   - Every server-bound frame from a live sender is scanned for the PIN, the link secret and the seed: never present.
   - Loopback: wrong Host; wrong, missing and `null` Origin; a retired and an expired (61 s) launch key; a wrong `proof_p`, a `proof_p` from another connection, a reflected `proof_d`; concurrent exchanges (the last to complete is attached, a failed one changes nothing); supersede; another uid (a netns test user); a listener owned by another uid → `port_not_owned`.
   - **Self-bound replay:** with check 1 disabled, a second uid holds 47823 while the daemon is down and records everything the page sends, then releases the port; after the daemon is back, that uid replays it and runs its own exchanges. The attachment never changes, and nothing it captured includes a key.
   - **Lost `init`:** drop the socket after the daemon sends `init`, before `init_ack`, and again after `init_ack` is sent but not received; reload the page during the handover. The page attaches every time, with one attachment key.
   - Kill the daemon while live: the port stays bound (`ss -ltnp`) and media is uninterrupted. The page reconnects (no new `SenderHost`, same `instance`), re-authenticates and re-sends secrets. Repeat in `self_bound` mode, where the plugin restarts the daemon and the daemon re-adopts the browser.
   - A rotation request with an unchanged PIN (also NFC-equivalent, and `none` → `none`) gets `invalid_argument` and writes no intent; a request during a pending rotation gets `rotation_pending` and succeeds when resent after it clears.
   - A remote `pause` is acknowledged only after the page record and the daemon mirror have it; kill the browser process right after the acknowledgement and restart: still paused.
   - Kill the page at every step of a rotation: the reconciliation table converges, and no admission happens before it does.
   - Pause, then force each recovery path (reload, PC replacement, worker crash, re-init): no audio or video RTP payload leaves until a local resume. The same for lock with admissions.
   - Kick + lock where the kick fails: still locked.
   - Knock: approve after timeout fails, approve with the wrong code fails, deny then approve fails, and no `ADMIT` before approval.
   - Join: a swapped `peer_id` flags a mismatch, and a kick still hits the chosen admission.
   - Window close → stream ended in ≤ 15 s. `stop` with a hung page → all Chromium processes gone in ≤ 7 s, with systemd and in `self_bound` mode.

## 11. Open questions

1. **Implementation gate: peer-UID lookup through Chromium's sandboxed network service.** Verify that `/proc/net/tcp` shows the user's uid for Chromium's loopback sockets on Omarchy. If not, use the launch-file fallback (§5.2) and confirm that `--app=file://…` follows the redirect.
2. **Browser choice:** plain `chromium` with a dedicated profile, versus the default browser through `omarchy-launch-webapp`. The latter shares the main profile and its extensions, so this design says dedicated. Confirm that Chromium is acceptable as a hard dependency.
3. **The user manager's environment:** `WAYLAND_DISPLAY`, `XDG_*` and `DBUS_SESSION_BUS_ADDRESS` must be present for the portal picker. Omarchy's uwsm normally exports them. Verify.
4. Is losing **remote approve/resume** acceptable for v1? (§4.5)
5. **Port 47823 collisions:** a fixed default plus a config option. Changing it orphans the profile's storage.
6. **Knock semantics:** this design treats knock as an extra gate *after* the PIN (or after the link secret when there's no PIN). Confirm, versus knock *instead of* a PIN.
7. **Capture exclusion:** does Omarchy's Hyprland honour `noscreenshare` for the sender window, and is there a layer equivalent for the Quattro panel? If not, the reveal shows "Visible in your share".
8. **Implementation gate: Chromium's process groups.** In `self_bound` mode (§5.9), verify that every Chromium process (zygote, sandboxed renderers, GPU, utility) either stays in the launch process group or names the profile directory in its command line, so the kill sweep finds it.

## 12. Review changes

### 12.1 Review round 1 changes

GPT-6.1-Sol reviewed the first draft. The principles (monotonic rule, no owner signing key) stood. These are the 13 fixes:

| # | Severity | Finding | Fix | Sections |
|---|---|---|---|---|
| 1 | High | The browser never authenticates the daemon; another UID could take 47823 after a crash. | systemd `--user` socket activation holds the port. The plugin, CLI and daemon check the listener's owning UID before any launch. A mutual daemon proof on reconnect. Residual window and fallback documented. | §0, §2, §2.2, §5.1, §9, CONTRACT §10–§11 |
| 2 | High | Stable ids don't stop misidentification; the server can swap names and telemetry consistently. | The guarantee is narrowed to "commands target the selected admission". A sender-assigned row code, delivered inside the E2EE session and shown on the viewer's screen. Approval binds to the code. No automation on relay metadata. | §0, §1, §3.3, §6.3, §6.4, CONTRACT §4.3, §13 |
| 3 | High | PIN persistence isn't coordinated with the library's rotation. | A versioned, persisted rotation intent, a startup reconciliation table, admissions blocked until all parts agree, and retries with the same pending value. | §5.5, §6.4 item 4, CONTRACT §5.4 |
| 4 | High | Restrictive state can disappear in recovery. | Persisted latches (lock, pause, freeze), restored before admission or transmission. Pause covers audio and video, independent of encryption. No recovery path loosens. | §0, §5.3, §5.6 |
| 5 | Medium | Kick + lock ordering, and argument validation. | The lock is set synchronously before the kick is awaited and survives failure. Closed per-channel argument schemas. | §4.3, CONTRACT §8.5 |
| 6 | Medium | Server policy strengthening vs the rotation rule. | Server defaults apply only at creation. No server-driven regeneration. An explicit local choice overrides the defaults. | §4.4, CONTRACT §9.1 |
| 7 | Medium | IPC authenticates the account, not the profile. | The same-UID boundary is stated. Full reversed-tuple match in `ESTABLISHED`, fail closed on ambiguity, missing/`null` Origin refused. Q1 stays a gate, with the 0600 launch-file fallback. | §1, §5.2, §9, §11 |
| 8 | Medium | Bootstrap and recovery are incomplete. | A complete recovery record, one attachment, single-use 60 s launch tokens consumed atomically, first-wins resumes, stop through a transient unit, secrets re-sent after every authenticated reconnect. | §5.3, §5.4, CONTRACT §5.2, §11 |
| 9 | Medium | Secret-handling contradictions. | No PINs in argv (prompt, stdin, `--pin-fd`). The toast doesn't show the PIN. Masked everywhere with a 10 s reveal and capture exclusion. Separate copy actions with a 30 s clear. Clipboard history and profile-deletion limits documented. | §5.7, CONTRACT §12 |
| 10 | Medium | The admission transport can't be built independently. | The relay envelope, bounds, peer close, publisher reconnect, knock wire behaviour and grant reconnect are specified, with the exact library additions. | §6, CONTRACT §8.2–§8.4 |
| 11 | Medium | Schemas are ambiguous. | Normative schemas and state machines. The join's rules. History and audit endpoints. Command idempotency. When `share` makes the link available. Unknown fields and enum values. | §5.4, CONTRACT §1, §4–§6, §9 |
| 12 | Medium | Phase-1 compatibility. | No phase-1 compatibility (pre-release). New link and fragment formats. Credential-specific transport rules replace the blanket rule. | Preamble, CONTRACT §1.4, §2, §3 |
| 13 | Low | Unneeded options. | `rotatePin(…keepBudget)` removed. v2 signing reduced to a reserved field and one sentence. | §4.6, §6.4 item 6 |

### 12.2 Review round 2 changes

GPT-6.1-Sol verified round 1 at e0f3c70. It marked round 1 items 5, 6, 7, 9, 12 and 13 resolved, accepted random row codes and creation-fixed knock mode, and found the HMAC challenge-response suitable. These are the 11 fixes for what remained:

| # | Finding | Fix | Sections |
|---|---|---|---|
| 1 | `hello` sent the bearer resume token before the daemon was verified. In `self_bound` mode with the launch-file fallback (no peer-UID check), a stranger could hold the port, capture the token, release the port and replay it to the real daemon to replace the sender. | No bearer token is sent by the sender WebSocket. Every sender connection runs a mutual HMAC challenge-response over a role byte, the key id and both nonces, bound to the daemon's per-connection nonce, **daemon first**. Only a completed exchange may supersede the attachment. The launch token and the resume token become a launch key and an attachment key, used only as HMAC keys. Errors before the daemon's proof are unauthenticated and never terminal. | §0, §2.1, §2.2, §5.2, §5.3, §9, §10, CONTRACT §2, §3, §5.2, §11 |
| 2 | The row code's string/colour binding was ambiguous. | One canonical value: three characters, with a 0–10 647 index for the wire. The colour is derived from the index, display-only, never input or compared. `--code` and `knock.approve` compare the canonical string byte for byte; the CLI alone normalises input. | §0, §3.3, §6.4, CONTRACT §4.3, §10, §12 |
| 3 | An unchanged supplied PIN failed inside the retry loop and was retried forever, while other requests got `rotation_pending`. | Validation (including the NFC unchanged-PIN check) happens before any intent is written. Library errors are classified: `storage` is retried by the page and reported as accepted; any other first-call error is terminal and clears the intent; any other retry error fails closed. A pending intent is never replaced except from `state_mismatch`. `rotation_pending` is retryable. | §5.5, CONTRACT §5.4, §12, §14 |
| 4 | "A crash at any point" overclaimed latch durability. | Latches are persisted before the command is acknowledged: daemon-originated latches are fsynced into the daemon's mirror before forwarding, library freezes are already durable, and page-originated ones are acknowledged after the page record and the daemon's `state_ack`. The remaining window is stated exactly. The mirror clears only on an explicit clear. | §0, §4.3, §5.6, CONTRACT §10, §11 |
| 5 | Consuming the launch token before `init` reached the page could strand it. | A handover: the launch key stays valid until `init_ack` or the first attachment-key exchange, and a retried launch-key exchange gets the same attachment key. | §5.3, §5.4, CONTRACT §5.2, §11 |
| 6 | `PENDING` was said to be 35 bytes; it's 6 bytes of plaintext plus a 16-byte tag. | `PENDING` box 22 bytes (message 25); with the colour off the wire, the admit bundle is 97 bytes (box 113). Every size was re-checked and a computed size budget added: `sender.state` raised to 256 KiB, daemon lines to 8 MiB, per-row connections capped at 3, `joining` rows at 100, string fields bounded in bytes. | §6.4, CONTRACT §1.1, §1.3, §4, §8.4 |
| 7 | `Join` lacked the inputs its outputs need. | `JoinInput`: receipt time, publisher liveness, stream ended, connections; `JoinMemory`. Removal causes come from a new `SenderState.ends`. A history `end` mapping for every cause, plus `revoked`. | §3.2, CONTRACT §4.4, §4.9, §6 |
| 8 | Knock mode wasn't in either recovery record, and resumed pages ignored `init.policy`. | `knock` is in both records, OR-merged on every `init`; a mismatch on a running host fails closed. The obsolete "knock turned on later" wording is gone. | §4.4, §5.3, §6.3, CONTRACT §11.2 |
| 9 | A live WebSocket reconnect and a page load were conflated. | Two paths, chosen by the page's own memory: a page load runs the full startup order; a reconnect only re-authenticates, sets latches, and re-sends state and secrets. The daemon's view of both is defined. | §5.3, CONTRACT §5.2, §11.2 |
| 10 | Launch and kill supervision without systemd was undefined. | `self_bound` supervision: clients start the daemon on demand; the browser in its own session and process group, tracked by pid, start time and pidfd, re-adopted after a daemon restart, killed by group with a sweep. A new implementation gate for Chromium's process groups. | §0, §2.2, §5.3, §5.9, §11 Q8, CONTRACT §10 |
| 11 | §2 said API keys travel only as bearer tokens, but §7 logged in with the key in a JSON body. | Bearer only: `POST /api/v1/session` takes the key in `Authorization: Bearer` with an empty body. | §7, CONTRACT §7 |


### D1 capture and publishing clarifications

Lock (and the admission freeze) gates new admissions, not capture. A locked live
stream continues video, the demanded snapshot feed and an explicit recording.
Pause/privacy curtain, media suspension and unsafe tracks gate capture. Idle
snapshot demand expires after 30 seconds and releases retained images. Publishing
is always a request: only an on-screen confirmation in the authenticated local
sender window may approve its exact preview and destination; CLI, plugin and
`/owner` cannot confirm. The confirmation expires after 30 seconds.
The button checks its trusted native click and passes a one-use, request-bound
capability to a private receiver. Production builds expose no sender/channel
global; test hooks require an explicit `--mode e2e` build. The daemon cannot
attest a DOM gesture: arbitrary sender-origin script execution is full
compromise, and privileged browser automation can generate trusted input.
Agents must not confirm their own requests through browser automation.
