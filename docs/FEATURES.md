# Local agent frames and Pass publishing

## Design (track D1)

The capture is plaintext only in the sender. Feature 4 therefore has **no remote
server frame endpoint**. The local sender samples its capture only on demand (a frame request/waiter
within 30 seconds, or a pending snapshot publish), at most twice a second, compares exact RGBA pixels before PNG encoding, and sends changed PNGs
only over
the existing mutually authenticated daemon WebSocket. The daemon retains at most
four PNGs (4 MiB each), in memory. IDs include a random capture generation and a
monotonic counter. `frame` on the owner-UID Unix socket supports an `after` ID,
a bounded 30-second wait, and optional downscale. A loopback HTTP owner API uses
short-lived credentials issued only over that socket, exact Host and Origin
checks, and no CORS. Remote bearer keys and viewer credentials grant no access.
Remote agents open the viewer link and complete CPace/PIN or knock admission.

Every capture operation checks the video's pause latch, encryption suspension,
track readiness/enabled/muted state, and stream lifetime. Privacy transitions
invalidate pending encodes, erase retained frames and recordings, and wake frame
waiters with `paused`. Disconnect/reload starts closed until the current sender
reports safe capture again. No snapshot field enters SenderState or signaling.
The canvas and diff baseline are bounded to 3840×2160 pixels; larger sources are
refused rather than silently reducing native resolution. Optional downscale is
an explicit local output choice. PNGs exceeding 4 MiB are refused.

Recordings use MediaRecorder WebM in the sender, including capture audio when
present. Owner start/stop actions are local only. Defaults and hard ceilings are
120 seconds and 32 MiB; chunks are bounded and transferred over the authenticated
local channel, never signaling. A pause/privacy transition discards the whole
recording, including a completed one. There is no automatic recording or upload.

`publish snapshot passfile|passpage` and `publish recording passfile` only request
publishing. The local sender shows the configured destination host and immutable
preview (or recording duration/size), and explains that it creates a shareable
link. Only the owner's on-screen confirmation there uploads, within 30 seconds.
CLI/agent/plugin/owner-page callers can never confirm. Agents may request only
when the owner asked for it in the current task. The daemon reads the matching guarded key file at use time and
uploads once, with redirects disabled. It never persists keys, payloads or links,
and does not include service response bodies or transport errors in logs/errors.
PassFile receives `PUT /api/v1/files?filename=…` with raw PNG/WebM. PassPage receives
`POST /api/shares`, multipart field `archive`, containing a gzip tarball with
`index.html` and `snapshot.png`. Each action returns the resulting URL. No retry
can silently publish a second copy.

### Approval boundary (D1 review round 2)

The confirmation handler checks the trusted native click on its own button and
mints a one-use capability bound to the current channel and request ID. It hands
that capability directly to a private channel receiver; no public channel method
can approve by ID. The socket and raw send function are JavaScript private
fields, and artifact methods cannot carry approval messages. Production builds
expose no sender core/channel global or test secrets. Instrumentation requires
the explicit Vite `--mode e2e` build, automation and a `?test` page; production
mode cannot enable it through a URL or runtime environment variable.

This is a client-side gesture boundary, not a hardware human-presence proof.
The daemon authenticates the sender connection and request, but cannot attest a
DOM gesture. Arbitrary script execution in the sender origin is already full
compromise (including its capture, stored credentials and keys); privileged
browser automation can issue trusted input. Stronger owner-presence attestation
would require a separate trusted native interface. Agents must never use browser
automation to confirm their own publish requests.

Service bases are `passfile_server` (default `https://passfile.space`) and
`passpage_server` (default `https://passpage.space`) in the local PassStream config.
Keys are `$XDG_CONFIG_HOME/passfile/key` and `passpage/key` (normally `~/.config`),
read with the existing owner/mode/symlink guards. HTTPS is required except for
literal loopback HTTP endpoints used by development and fake-service tests.

The server dashboard cannot possess local authority. Its owner view opens the
local `/owner` page for capture actions; that page
requires a five-minute credential from `passstream capture-auth` on the owner
Unix socket. The dashboard never sees that credential, snapshots or uploads.
Local sender controls use its existing authenticated channel. Plugin clients
can use the new Unix ops without any plugin changes.

Verification uses only temporary XDG state and local fake services. Browser runs
use the shared media-run lock. The real-stack suite changes a sender canvas,
checks change IDs and privacy invalidation, and scans server requests/logs plus
captured SFU ciphertext for snapshot needles.

## Owner commands

```
passstream frame --json
passstream frame --after CHANGE_ID --wait 30 --max-width 1280 --json
passstream frame --out snapshot.png --json
passstream record start --duration 120 --max-bytes 33554432 --json
passstream record stop --json
passstream publish snapshot passpage --json
passstream publish snapshot passfile --json
passstream publish recording passfile --json
passstream publish status REQUEST_ID --wait 30 --json
```

The frame JSON contains base64 PNG bytes, unless `--out` wrote a new private
file. A paused or unchanged result contains no bytes. `--out -` writes PNG to
stdout and cannot combine with `--json`. Requests return `pending` and a request ID. Read `publish status ID --json` for
confirmed/refused/timeout and the final URL/error.
Do not automatically retry an uncertain upload: inspect the selected service.

The local HTTP equivalents and complete schemas are in CONTRACT §10.1. Obtain
a five-minute credential with `passstream capture-auth --json`; send it only to
the returned loopback origin. The server dashboard opens `/owner` on that origin
(default port 47823); the credential is pasted into the local page, never the
dashboard. No action runs automatically from a link. Plugin clients use the same
Unix ops (`frame`, `record.start`, `record.stop`, `record.status`, `publish`).

Idle feed expiry clears PNGs, decoded pixels, derived images and the sender
canvas/diff baseline. Oversized sources/PNGs send `too_large`, so an agent never
gets a stale prior picture as `unchanged`. Lock only gates admission, not capture.
PNG validation runs outside the daemon lock. Scales serialize and cache at most
four (frame ID, width) results, using direct pixel-buffer access.

Upload results must match the configured service origin. Upload deadlines scale
from 45 s plus 1 s per 128 KiB (301 s at 32 MiB); the old 32 MiB/45 s budget
required 5.97 Mbit/s before overhead. Token auth is distinct from the sender
WebSocket's HMAC: `/owner` sends its short-lived bearer over loopback. With
`peer_check=launch_file`, this token alone authenticates native local callers.

# PassStream features

Design notes for the ranked features in [PLAN.md](PLAN.md) "Features". Each track keeps its own section.

## Track D2: live quality HUD, privacy curtain, laser pointer and annotations

Status: design (2026-11-08). Code: `web/src/hud/`, `web/src/annotations/`, `e2ee/src/annotation.ts`, plus small hook-ups in the viewer, the sender page and the CLI.

### 1. Live quality HUD

**What it shows.**

- **Viewer:** RTT, loss, bitrate, path (UDP, TCP, TURN/UDP, TURN/TCP or TURN/TLS, from the selected candidate pair), fps, rendition (native or low) with its temporal layer, freezes (gaps over 500 ms between rendered frames), glass-to-glass delay when the source carries the clock row, and E2EE state (newest epoch held, and decrypt failures by reason).
- **Sender:** uplink, encode time per frame, encoder busy share (`Δ totalEncodeTime / Δ wall time`; a browser has no process CPU), `qualityLimitationReason`, fps, capture resolution, and audio (none, live or muted by the curtain, with its bitrate).

**Sources.** Nothing new polls the network on the viewer. The HUD reads state that already exists:

| Value | Source |
|---|---|
| RTT, loss, bitrate, path, fps | `Session.quality` (the session's own `getStats`, every 500 ms) |
| Freezes, G2G | `Session.frameTimeline` (every rendered frame, with the decoded clock row), read incrementally from a cursor |
| Rendition, temporal layer | `MediaLayerSource`, a typed interface. The default implementation parses the layer the SFU reports on the heartbeat (`quality.layer`, e.g. `L1T3`, `180p/L1T2`). The controller rewrite (track A1) can supply its own implementation without touching the HUD. |
| Epoch, decrypt failures | The viewer controller: the epochs it handed to the worker, and the worker's drop reports |
| Sender values | `senderHealth()` (already reported in `SenderState.health`) plus the capture's audio track; audio bitrate from the audio sender's own `getStats`, once a second and only while the sender HUD is open |

**Cost.** A `HudSampler` recomputes the model at most every 500 ms (2 Hz) and otherwise returns the cached one, so re-renders triggered by other state don't redo the work. All mapping functions are pure and synchronous over small inputs; the frame-timeline scan is incremental. No HUD code runs in a frame callback.

**Layout.** The viewer's existing info button and HUD panel are extended, not duplicated. The panel has a compact view (RTT, loss, bitrate, fps, path) and a "More" toggle for rendition, layer, freezes, G2G and E2EE. It stays inside `min(19rem, 100%)`, so it fits 320 px. The sender page gets the same panel, collapsed by default.

**Seam.** `web/src/hud/stats.ts` exports `MediaLayerSource { layer(): { rendition: "native" | "low" | null; temporal: number | null; temporalMax: number | null } }`. No change to `web/src/session.ts` or `internal/sfu`.

### 2. Privacy curtain

The curtain **is the pause latch**. Pause already does what a curtain needs: it's restrictive and persisted, it covers audio and video, it's drawn into the video (an opaque "Paused" picture with a marker row that travels inside E2EE), and only a local resume clears it (HOST-CONTROL §5.6). Adding a second latch would add a second way to get the rules wrong.

- **Tighten from anywhere, loosen locally.** `pause` (curtain on) is accepted from the daemon, the sender window and the server. `resume` (curtain off) is local only: the server's command API answers `403 local_owner_required`, and the sender page refuses it on the remote channel.
- **What the curtain guarantees.** Capture and audio disable synchronously when the page accepts the pause command, so no new screen content is captured for sending afterward. The worker's no-enqueue boundary starts when it receives and arms the fence, rather than when the page posts the message. In order:
  1. `HostRecovery.setLatch("paused", true)` applies the latch synchronously, before its storage write. The page disables the capture track and audio track in the same call. Chrome stops delivering capture frames; a disabled track yields black frames and encoded silence.
  2. The page posts a **fence** to the encrypt worker (`postSenderFence`). On receipt, the worker drops every video and audio frame until the fence opens, whenever it was captured. It also rejects encryption already awaiting WebCrypto when the fence armed, even if the fence opens before that encryption finishes. Pre-command frames can still complete encryption and leave between the page post and worker receipt.
  3. The outgoing tracks switch to the "Paused" canvas. When `replaceTrack` resolves, the page opens the fence (`postSenderFenceOpen`; 1 s at most). The canvas's first frame is a keyframe, because the resolution changes, and it passes. A video track whose first frame after the fence isn't a keyframe waits for one, re-requesting it every 250 ms.
  - An earlier version used a fixed 150 ms window. That dropped the canvas's own keyframe, and the worker's keyframe requests weren't answered, so viewers waited 1.0–1.6 s for a stall-triggered PLI (measured).
  - With the open-on-swap fence, the switch takes 5–10 ms after the command and the paused picture is back to about 130 ms at the viewer.
- **What it can't do.** Pre-command frames can finish encryption until the worker handles the fence message. Output that left before that receipt cannot be recalled, including frames already in the packetizer, on the wire, in the SFU's pacer queue (up to about 100 ms), or in a viewer's jitter buffer. They still display. Measured on loopback, the last such frame shows up to 66 ms after the command. On a delayed path it is later by about the extra one-way delay (REPORT.md "Track D2").
- **Entry points.**
  - Sender page: `Escape` turns the curtain on (it never turns it off, so a double press can't uncover). A large "Lift curtain" button resumes in one tap. While curtained, the host's preview is blurred and dimmed.
  - CLI: `passstream curtain on|off` (aliases for `pause` and `resume`).
  - Omarchy plugin: `omarchy-shell passstream curtain` (on) and `curtainOff`, over IPC only. No default keybind, because Omarchy owns many Super combinations. The plugin README documents an example bind.
- **Viewers** see the existing opaque paused cover once the first "Paused" picture decodes. Until then they may see the in-flight pre-curtain frames described above. The cover hides the stage from then on.
- **Blur, but no leak.** No blurred copy of the screen is ever sent: a blur still shows layout and large text, and the last frame is often the thing the host wanted hidden. "Blur" applies only to the host's own preview.
- **Measurement.** The e2e test records the sender's `Date.now()` when the command reaches the page. The test source stamps every frame with its capture time, and the viewer decodes it from either rendition. Real and curtain frames are told apart by brightness. Reported:
  - Real frames captured after the command that reached a viewer (must be 0).
  - Frames whose encryption starts and succeeds between worker fence receipt and fence opening, from the worker trace (must be 0). Page-command/post time and worker-receipt time are recorded separately. This trace check does not cover encryption already awaiting WebCrypto at receipt; deterministic held-encryption regressions cover that boundary.
  - The last pre-curtain frame shown.
  - Command to the first "Paused" picture.
  - Tone samples at the viewer during the curtain (must be 0).
  - These are checked on loopback, on a delayed path (a second Chromium with libwebrtc's fake-network receive delay) and on the low rendition.

### 3. Laser pointer and annotations

**Model.** The overlay is drawn by each viewer (and the host's preview) on top of the picture. Coordinates are normalised to the video frame (u16, 0–65535), so zoom, fit/fill and screen size don't matter. Nothing touches the capture.

- **Tools:** a laser (ephemeral: a dot and a short trail that fades within 1.2 s) and a pen (persistent strokes until cleared).
- **Attribution:** each origin draws in its row colour with its row code next to the dot or stroke end. The host draws in white, labelled "Host".

**Transport.** Annotation messages ride the existing ordered E2EE relay on signaling (CONTRACT §8.2) as two new opaque library message types. The server already relays `e2ee` envelopes without parsing them, so no server change is needed. Nothing goes over the SFU data channel. All traffic goes through the host: viewer → host, then host → each other viewer.

- The host is the only party that can say who drew what.
- The host can enforce the policy, rate and size limits.
- A kicked viewer is cut off by the host at once.

**Crypto (e2ee/src/annotation.ts, PROTOCOL §20).**

```
session_annot  = HKDF-SHA-512(ISK, salt=sid, info="PassStream v1 annotation session", 32)    // per admitted session, imported non-extractable as an HKDF key
epoch_annot(e) = HKDF-SHA-256(epoch_secret_e, salt="", info="PassStream v1 annotation epoch" || u32(e), 32)
key(dir, e)    = HKDF-SHA-256(ikm=session_annot, salt=epoch_annot(e), info="PassStream v1 annotation " || dir || u32(e))   dir ∈ {"v2s", "s2v"}; AES-256-GCM
nonce          = u32(e) || u64(ctr)          ctr from 1, strictly increasing per (session, epoch, dir)
aad            = lv_cat("PassStream v1 annotation", dir, u32(e), u64(ctr))
```

- **Two secrets.** The key mixes the **pairwise** session secret (only that viewer and the host have it, from CPace) with the **current epoch** secret (every admitted viewer has it).
  - The pairwise part authenticates the row. A viewer can't impersonate another viewer or the host, because it doesn't have their pairwise secret. A relay that relabels which peer sent a message just makes it fail to decrypt.
  - The epoch part ties every message to the E2EE epoch. A rekey (every kick, PIN rotation and 10-minute tick) moves to fresh keys and fresh counters.
- **No collision with SFrame.** The keys come from their own labels and a different hash, and they are never used for media. SFrame keys come from `"PassStream v1 SFrame base_key"`. The nonce spaces can't meet because the keys differ.
- **Upstream (0x09 ANNOTATION, V→S).** The plaintext is one app body of at most 512 bytes. It has **no origin field**: the host stamps the origin from the authenticated admission record.
- **Downstream (0x0A ANNOTATIONS, S→V).** The plaintext is `count:u8 || (origin:u16 || len:u16 || body)*`, at most 2048 bytes. `origin` is a row-code index (0–10647) or `0xFFFF` for the host. It is sealed separately for each recipient under that recipient's pairwise key, so another viewer can't forge "the host says K7P drew this".
- **Replay and reordering.** Annotation output advances by `(authenticated epoch, counter)`: after accepting an epoch, the receiver refuses every lower epoch, even if its key is retained and the ciphertext is a first delivery. In the same epoch, `ctr` must exceed the last accepted counter. Both checks run before and after the decrypt await; only successful authentication commits the watermarks. Invalid future ciphertext cannot advance them. Each side keeps at most 4 epoch keys; this does not change SFrame's media playout buffer.
- **Kick.** The host removes the session's keys synchronously at the kick (PROTOCOL §5.8 step 2), so its next message fails with "no admitted session". The kicked viewer gets no further downstream messages. The fast rekey then changes the epoch for everyone else.
- **Server's view.** The server sees opaque envelopes, their sizes and their timing: when someone draws, and roughly how much.

**App body (`web/src/annotations/codec.ts`).** Binary, at most 512 bytes:

| Kind | Fields |
|---|---|
| 1 laser | `n:u8 (1–64)`, then `n × (x:u16, y:u16)` |
| 2 stroke | `id:u16`, `end:u8`, `n:u8 (0–64)`, points |
| 3 clear | `scope:u8` (0 everything, host only; 1 own strokes), `gen:u32` (the host's clear generation); downstream only |
| 4 policy | `viewers:u8` (0 off, 1 on); host only, downstream |
| 5 state | `flags:u8` (bit 0: partial), `gen:u32` (clear-all generation), `n:u8` (0–80), `n × (origin:u16, gen:u32)`; host only, downstream |
| 6 input | `session:u64`, `op:u32`, `clearThrough:u32`, `gen:u32`, `barrier:u32`, `namespace:u64`, then one laser, stroke or own-clear body; upstream only, ≤294 bytes |
| 7 receipt | `session:u64`, `through:u32`, `barrier:u32`; host only, recipient-specific, downstream, 17 bytes |
| 8 bootstrap | `namespace:u64`, `source:u64`, `gen:u32`, `barrier:u32`; host only, first record in every recipient batch, 25 bytes |
| 9 ink | `source:u64`, `op:u32`, then laser/stroke primitive; host-stamped downstream viewer ink, ≤274 bytes |
| 10 sources | `n:u8` (0–32), `n × (source:u64, origin:u16, through:u32)`; host-only incarnation/cutoff repair, ≤450 bytes |

**Convergence and causal ordering (fix rounds 3–5).** The relay may drop a whole batch silently, so a clear can't be fire-and-forget.
- **Generations:** every host clear carries a generation, one counter for clear-all and one per origin. A recipient applies a clear only if it is newer than the last it applied. A stale, delayed or duplicate clear cannot erase ink delivered after its generation.
- **Retained restrictive state:** host and viewer each keep a fixed 10,649-entry generation table (42,596 bytes), covering all `22³` row codes plus the host. Tombstones survive independently of current ink, admission churn and the 500-concurrent-admission limit; they are retained for the router's lifetime. No per-clear acknowledgment journal or per-recipient retry map is needed.
- **Fair pages and new ink:** host-only partial state records carry up to 80 retained nonzero generations each, including origins with no ink. A rotating cursor advances on every sent page; new clears never reset it. Every ink origin's current generation also precedes its ink in the same batch, even if a full repair page requires a second state record. Repair records reserve space before ink and share the existing batch and aggregate envelope budgets. Absence from a partial page never implies a clear; remote origins use these generations; the original drawer uses the causal receipt and host barriers below.
- **Local clear boundary:** every viewer input has a random nonzero 64-bit incarnation for that annotation router, a nonzero increasing u32 operation, and its latest local `clearThrough`. A pen stroke keeps the same operation across segments; laser batches carry their newest point's operation. `clearOwn` cancels old pending input, clears locally at once, and raises the watermark. Every subsequent input repeats it, so losing the standalone clear request cannot leave earlier host ink when later input arrives. The host applies a newer watermark before its following ink, without guessing host generations. Multiple rapid clears coalesce to the newest boundary.
- **Admission ownership:** row codes are display identities shared by legitimate concurrent grant admissions. The host binds one input incarnation to each authenticated admission; packets cannot replace it. Up to 500 fixed slots hold admission ownership, input session, highest observed operation and purge cutoff. The host assigns each slot a nonzero u32 incarnation, encoded as `source = incarnation << 16 | slot` (slot 0–499). New admissions use unused slots first, then retired slots with a higher incarnation. Reuse purges the retired incarnation's ink; no historical admission journal grows. A source plus stroke ID identifies ink, so concurrent peers' colliding IDs cannot merge, overwrite or erase each other. Fan-out excludes the actual source admission, never everyone sharing its row.
- **Causal repair:** ordinary own-clear prunes only that admission's source. Host-only SOURCES pages repeat the incarnation and cutoff to all recipients; every ink source's current state leads its ink in the same batch. Each batch also carries the original drawer's matching-input-session receipt, so local confirmation preserves later optimistic ink. The viewer has a fixed 500-slot latest-incarnation/cutoff table and bounded metadata on its existing 32 strokes and 256 trail points per row. A reused slot erases its old incarnation and refuses delayed old-source ink. Host clear/overflow/kick purges cover every live incarnation of the affected row; no unconditional drawer exemption exists.
- **Router bootstrap and reconnect:** every downstream batch starts with authenticated BOOTSTRAP, before policy can enable drawing. It binds the random nonzero host-router namespace, recipient source and current host barriers. Successful re-admission suspends pending input and permits namespace adoption on that authenticated channel only. A changed namespace resets generation/barrier/source tables and the local live-only overlay; a same-host replacement retains its barriers and existing ink. Input carries the namespace, so delayed prior-router input cannot use the new router's zero counters. The controller rechecks channel identity after open/seal awaits; stopped routers also fence pending downstream seals. Once adopted, a namespace cannot change until another authenticated admission completes.
- **Host barriers:** clear-all advances the global `gen`; a forced per-origin purge (overflow or kick) advances that origin's separate `barrier`. The host accepts input only with the exact current stamps. On observing a newer host barrier, the drawer erases all prior optimistic ink and pending input, including input silently dropped upstream that the host never saw. An authentic input held across the host clear is refused on first delivery. Thus a host clear wins over concurrent drawing that has not observed it; drawing after observing it uses the new stamps immediately. Ordinary `clearOwn` never advances these barriers and never holds subsequent pen input for confirmation. Receipts include the origin barrier even when no input was ever observed (`session=0`, `through=0`). Both sides retain one extra fixed 10,649-u32 barrier table (42,596 bytes).
- **Batch budget:** one 29-byte bootstrap record, one 21-byte receipt record and two record slots are reserved before shared packing. Receipts ride normal paced drawing/repair batches: no extra envelope, acknowledgment timer, retransmission loop or idle traffic. Input bodies cannot nest envelopes or carry host-only primitives. Operations, host generations and barriers never wrap; exhaustion refuses further operations.
- **Repair lifecycle:** after a policy or clear change, repair pages run at the normal paced fan-out cadence until both generation and source cursors complete three wraps (at least two full visits after the change). Periodic policy/state repair also continues every 5 s while viewers may draw, while the host has ink, and for at least 30 s after a control change. Generation pages visit the entire row namespace in at most 134 pages; source pages visit all 500 slots in at most 32 pages (16 slots reserved per tick, up to 32 with ink); large histories can extend the repair window beyond 30 s. A lost single batch followed by restored delivery converges on a later page containing its generation, even with more than 80 or 500 historical origins.
- **Limits:** dropped ink segments are not retransmitted. After the finite repair window finishes, an empty host-only stream becomes silent; an outage lasting beyond that window needs another relevant event to restart repair. An unused host-only stream sends nothing. Retired ink may remain until its slot is reused or the host clears it. Ordinary own-clear covers the current admission, while host row purges cover all incarnations. Generations cannot wrap: exhaustion is rejected rather than forgetting an earlier clear.

**Default policy: host-only.** The host can always point and draw from the sender window. Viewer drawing is **off** until the host turns it on.

- **Why.** Viewer drawing puts one viewer's input on every other viewer's screen. With a link that may be forwarded, that's an abuse surface (graffiti, fake "click here" arrows) the host didn't ask for. The host's own laser carries no such risk and is the common case (a presenter pointing).
- **Rules.** Turning viewer drawing on is a **loosening** action: local only (daemon, CLI, sender window), and the server gets `403 local_owner_required`. Turning it off and clearing are **tightening** actions, allowed from anywhere, the server included.
- **Not persisted.** A sender reload falls back to host-only, the restrictive state, which is consistent with "restrictive state survives everything".
- **Commands.** `annotations {viewers: bool}` and `annotations.clear {}`. CLI: `passstream annotations on|off|clear`.

**Bounds.**

| Limit | Value | Why |
|---|---|---|
| Viewer send rate | token bucket, 8 msg/s, burst 8 | The relay allows 16/s per viewer, shared with rekey acks and handshakes |
| Host accept rate | 10 msg/s, burst 20 per admission; excess dropped and counted | |
| Body size | ≤ 512 bytes (enforced by the wire schema) | |
| Points | ≤ 64 per message; ≤ 1024 per stroke | |
| Strokes kept | ≤ 32 per origin, oldest dropped | |
| Host fan-out | one batch per recipient every 125 ms (≤ 8 msg/s), ≤ 2048 bytes; lasers coalesce to the newest position per origin | The relay closes a viewer whose 64-message queue overflows |

Policy violations (viewer drawing while off, a viewer sending policy or clear-everything) are dropped and counted, never relayed.

**Mobile.** Drawing needs an explicit draw mode (the pen button), so a one-finger pan or a pinch is never mistaken for a stroke. In draw mode one finger draws, and a second finger ends the stroke and pinches as usual. The overlay lives inside the zoom layer, so it zooms with the picture. The toolbar keeps 44 px targets at 320 px.

**Not done.** A viewer who joins late doesn't get strokes drawn before it joined; strokes are live-only. The host doesn't keep a replayable canvas.

### D2 results (2026-11-08)

Built as designed above, with three changes found while building:

- The HUD's "More" toggle sits in its fixed header, next to close. Only the rows scroll on a short phone.
- RTT counts as known whenever a candidate pair is selected, so loopback shows "<1 ms" instead of "—".
- The host's laser dot has a heavier dark ring so it shows on light screens.

**Tests.**

| Suite | Count | Result |
|---|---|---|
| `e2ee/test/annotation.test.ts` | 14 | pass |
| `web/src/hud/stats.test.ts` | 9 | pass |
| `web/src/annotations/annotations.test.ts` | 17 | pass |
| `web/src/sender/curtain.test.ts` | 5 | pass |
| `internal/server/annotations_test.go`, `internal/cli` `TestCurtainAndAnnotationsCommands` | 2 | pass |
| `test/e2e/features-ui.mjs` (real stack) | 5 | pass |

The unit suites cover:

- **Annotation crypto:** encrypt/decrypt, epoch rotation, row authentication against relabelling and forging, replay and reordering, tampering, size bounds and SFrame domain separation.
- **Curtain latch:** tighten from anywhere, loosen locally; applied before its write, lifted only after its write, kept down on a storage failure.
- **Rate limits:** viewer 8/s, host 10/s burst 20, fan-out batches and queue bound.

Measured numbers are in [REPORT.md](REPORT.md) "Track D2".

**Seams for the media controller (track A1).**

- `MediaLayerSource` in `web/src/hud/stats.ts`. The default reads `Session.quality.layer`; a new controller can pass its own source to `hudSampler()` in `web/src/viewer/index.tsx`.
- No change to `web/src/session.ts` or `internal/sfu`. The HUD reads `Session.quality`, `Session.frameTimeline` and `Session.pc` (all already public).
- Freeze counting uses rendered-frame gaps over 500 ms, not Chrome's `freezeCount`.

**Caveats.**

- Annotations are live-only: a viewer who joins late, or re-admits after a sender reload, sees only ink drawn after that.
- The policy isn't persisted. A sender reload falls back to host-only, deliberately.
- Annotation messages share the viewer's relay budget (16/s) with rekey acks. The viewer sends at most 8/s.
- The curtain's "blur" is on the host preview only. Viewers get the opaque paused picture.
