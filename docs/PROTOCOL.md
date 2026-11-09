# PassStream E2EE protocol, v1

Status: **beta.** Implemented in [`e2ee/`](../e2ee). One adversarial review round is done and its findings are fixed (§15); per [PLAN.md](PLAN.md), the UI must not call this "E2EE" until an independent review signs it off. Use "beta" until then. Section 14 lists the open questions.

## 1. Guarantee and threat model

**The guarantee (from the plan):** with trusted client code, the server (SFU) can't watch, decrypt or forge your stream. It *can* drop or delay traffic and lie in its own telemetry. People you admit can record what they see. JavaScript served by a compromised server defeats the guarantee.

Every share link carries a 256-bit **link secret** in its fragment, which the browser never sends to the server, and the stream PIN (on by default) is a second secret on top of it. So the server can't even make PIN guesses: without the fragment secret every CPace run fails, whatever PIN it tries. Someone who holds the full link is limited by the PIN guess budget (§6).

| Adversary | Can | Cannot |
|---|---|---|
| **Relay / SFU / server** (sees every data-channel message and every RTP packet, can inject, drop, replay, reorder, delay, and mislabel which viewer sent what) | Deny service. Burn the PIN budget by running handshakes itself (§6): each one fails without the link secret, so it learns nothing. See the metadata in §10. Swap viewer-reported display labels (§5.8). | Learn the PIN, the link secret or any key from transcripts (CPace). Test PIN guesses at all, against the sender or the viewers, without the fragment's link secret. Make an admitted session unkickable (§5.8). Decrypt or forge media. Replay frames or rekeys. Roll an epoch back. |
| **Link holder without the PIN** | Make online guesses against the sender: at most 20 per stream before a freeze (≈0.2 % for 4 digits), plus server-side IP limits. Unlocking rotates the PIN by default (§6). | Anything more. |
| **Admitted viewer** | Record. With the SFU's help, inject frames other viewers accept (SFrame has no per-sender authentication, RFC 9605 §7.2; out of scope for v1, §14 Q11). | Decrypt epochs issued after being kicked. Re-admit after a kick with its grant, or with any copy of it on another connection (§5.8). |
| **Kicked viewer** | Keep decrypting until the sender switches epoch: at most 300 ms after the kick, plus in-flight frames. Re-admit with the PIN if it knows it and the owner used `kick()` alone, not `kickAndRotatePin()`. | Receive or derive the new epoch. |
| **Malicious JS from the server** | Everything. This is the honest ceiling for a web app. | — |

Long-term secrets live on the sender only: the Ed25519 seed, the PIN, grant secrets and the budget state. Compromising the sender's seed lets an attacker impersonate the sender to viewers (they could then guess PINs against viewers, or show fake media). It does not reveal past epoch keys. Those come from ephemeral CPace scalars and random epoch secrets.

## 2. Notation

- `||` is concatenation. `u8/u16/u32/u64` are big-endian unsigned integers. Strings in quotes are UTF-8 bytes.
- `lv_cat(a, b, …)` is CPace's length-value encoding: each argument gets a LEB128 length prefix (draft-irtf-cfrg-cpace-21 Appendix A.1). It is used for every transcript and signature input in this document, so all of them are prefix-free.
- `HKDF-SHA-512(ikm, salt, info, L)` is RFC 5869. `HMAC-SHA-512` is RFC 2104.
- `b64url` is RFC 4648 §5 without padding. Decoding is canonical-only.

## 3. Parties and long-term values

| Value | Where | Notes |
|---|---|---|
| Sender identity `(sk_S, pk_S)` | Ed25519 (RFC 8032), generated on the sender | `pk_S` goes in the link fragment. `sk_S` (the 32-byte seed) stays in sender-only storage for the link's lifetime. Verification is strict (no ZIP-215). |
| Link fragment | `#c=<capability>&k=<b64url(pk_S)>&s=<b64url(linkSecret)>`, exactly this canonical form (helpers: `buildFragment`/`parseFragment`) | The capability is the server's concern and opaque here. The browser never sends a fragment to the server. `parseFragment` rejects a missing, short, long, non-canonical, duplicated or reordered field. |
| Link secret (**mandatory**) | Exactly 32 CSPRNG bytes (`generateLinkSecret()`) in the fragment, never sent to the server | Mixed into the CPace password of every PIN-mode admission (§5.2). `SenderHost` and `ViewerAdmission` refuse to run without exactly 32 bytes. The server never has it, so it can't run a meaningful PIN guess. |
| `streamId` | Public, 1–128 bytes | Bound into the CPace CI and every signature. |
| `viewerSid` | 16 random bytes, chosen by the viewer and kept across reloads | Keys the per-session backoff. Grants bind to it. The viewer chooses it, so it carries no trust (§6). |
| PIN | Sender only | 4 digits by default. 6+ digits or a passphrase are allowed. Normalised with NFC, 1–256 bytes, no control characters. |

## 4. Wire format

All handshake and key messages travel as binary payloads over the ordered signaling WebSocket relay (CONTRACT §8.2). The server wraps them in opaque b64url `e2ee` envelopes; it does not parse the library protocol.

```
message = version:u8 (=0x01) || type:u8 || field*
field   = LEB128(len) || bytes        (minimal LEB128, at most 3 bytes)
```

Each type has a fixed field list with exact or bounded lengths. Parsers reject unknown versions or types, wrong lengths, non-minimal lengths, trailing bytes and messages over 4096 bytes. They throw only `E2EEError("malformed")`.

| Type | Name | Direction | Fields (bytes) |
|---|---|---|---|
| 0x01 | HELLO | V→S | streamId (1–128), viewerSid (16), nV (32), mode (1: 0 = PIN, 1 = grant), grantId (0 for PIN mode, 16 for grant mode) |
| 0x02 | CHALLENGE | S→V | nS (32), ADa (8), Ya (32), sig (64) |
| 0x03 | RESPONSE | V→S | Yb (32), ADb (1), Tb (64) |
| 0x04 | ADMIT | S→V | Ta (64), box (113) |
| 0x05 | REJECT | S→V | reason (1), retryAfterMs (u32), triesLeft (u8), sig (64) |
| 0x06 | REKEY | S→V | seq (u64), box (16–512) |
| 0x07 | REKEY_ACK | V→S | seq (u64), tag (16) |
| 0x08 | PENDING | S→V | box (22) |
| 0x09 | ANNOTATION | V→S | epoch (4), ctr (8), box (17–528) (§20) |
| 0x0A | ANNOTATIONS | S→V | epoch (4), ctr (8), box (22–2064) (§20) |

REJECT reasons: 1 `wrong_pin`, 2 `backoff`, 3 `frozen`, 4 `locked`, 5 `grant_invalid`, 6 `expired`, 7 `invalid`, 8 `denied`.

## 5. Admission

### 5.1 Flow

```
Viewer V (CPace responder B)                           Sender S (CPace initiator A)
HELLO {streamId, viewerSid, nV, mode, grantId}  ───────▶
                                                        PIN mode: reserve a guess and persist it (§6)
                                                        grant mode: look up the grant, else signed REJECT
                     ◀─────── CHALLENGE {nS, ADa, Ya, sig = Sign(sk_S, challenge_input)}
verify sig with the pinned pk_S
  (fails → abort: nothing PIN-dependent is computed or sent)
RESPONSE {Yb, ADb, Tb}  ───────▶
                                                        K = ya·Yb, verify Tb (constant time)
                                                        ok  → settle "success", then commit (§5.8):
                                                              re-check revocation, register, ADMIT
                                                        bad → settle "wrong_pin", signed REJECT
                     ◀─────── ADMIT {Ta, box = AES-256-GCM(box_key, 0^12, "PassStream v1 admit", bundle)}
verify Ta (constant time), open box → epoch key + grant
```

Both sides are explicit state machines (`SenderAdmission`, `ViewerAdmission`) that consume and produce opaque bytes. A message that arrives in the wrong state, fails to parse or fails verification raises a typed error. That ends the handshake. On the sender, `SenderHost` then calls `abort()`, which settles any reserved guess as `abandoned`.

The ordering is the point. The sender's signed CHALLENGE commits to `nV`, which the viewer picked fresh, and to `Ya` before the viewer touches the PIN. A relay can't produce a CHALLENGE the viewer accepts, so it can't get a PIN-dependent `Tb` for a share it controls. The viewer's `Tb` is keyed by an ISK that only the real sender can compute. Wrong-PIN REJECTs carry nothing keyed by the ISK. Their signature covers only public values (verified in `test/adversarial.test.ts`).

### 5.2 CPace instantiation

The suite is **CPACE-RISTR255-SHA512** from [draft-irtf-cfrg-cpace-21](https://datatracker.ietf.org/doc/draft-irtf-cfrg-cpace/21/), §8.3. It uses ristretto255 (RFC 9496) and SHA-512 with `s_in_bytes = 128`. The **initiator-responder** setting applies (§6.3, `transcript_ir`), with the sender as initiator A and the viewer as responder B.

| CPace input | Value |
|---|---|
| `PRS` (PIN mode) | `lv_cat("PassStream v1 PRS pin", linkSecret, NFC(pin) or "")`. `linkSecret` is always 32 bytes. The PIN part is empty only when the signed ADa says no PIN is required. |
| `PRS` (grant mode) | `lv_cat("PassStream v1 PRS grant", grantSecret)` |
| `CI` (never sent) | `lv_cat("PassStream v1 CPace CI", streamId, "initiator:sender", pk_S, "responder:viewer", viewerSid, u8(mode), grantId)`. Role-tagged identities, initiator first (§10.1.1). |
| `sid` | `nV \|\| nS` (64 bytes, fresh randomness from both parties, §10.9) |
| `ADa` | `u8(1) \|\| u32(epoch) \|\| u16(0x0004) \|\| u8(flags)`. Carries the protocol version, the active epoch at challenge time, the SFrame suite (downgrade protection) and flags (bit 0 = PIN required). |
| `ADb` | `u8(1)`, the protocol version |
| Scalars | 32 random bytes with the top 4 bits cleared (`group_size_bits = 252`), little-endian. Zero is rejected. |
| `scalar_mult_vfy` | Decode (fails → G.I). An identity result → G.I. Either side **aborts on G.I** (§7.2). |
| `ISK` | `SHA-512(lv_cat("CPaceRistretto255_ISK", sid, K) \|\| transcript_ir(Ya, ADa, Yb, ADb))` |

`K` is never exposed (§10.3).

### 5.3 Signatures (Ed25519, strict)

```
challenge_input = lv_cat("PassStream v1 challenge", streamId, pk_S, viewerSid, nV, u8(mode), grantId, nS, ADa, Ya)
reject_input    = lv_cat("PassStream v1 reject",    streamId, pk_S, viewerSid, nV, nS_or_empty, u8(reason), u32(retryAfterMs), u8(triesLeft))
```

`nS_or_empty` is empty when the REJECT answers a HELLO (backoff, frozen, locked, grant_invalid) and is `nS` after a CHALLENGE. The viewer verifies both. It doesn't believe a REJECT (such as "frozen" or "0 tries left") the sender didn't sign for this exact handshake.

### 5.4 Key confirmation and derived keys

Key confirmation follows draft §10.4:

```
mac_key = SHA-512("CPaceMac" || sid || ISK)
Ta      = HMAC-SHA-512(mac_key, lv_cat(Ya, ADa))      (sent by S in ADMIT)
Tb      = HMAC-SHA-512(mac_key, lv_cat(Yb, ADb))      (sent by V in RESPONSE)

box_key = HKDF-SHA-512(ISK, salt=sid, info="PassStream v1 admit box",   32)   AES-256-GCM, one message, nonce 0^12
s2v_key = HKDF-SHA-512(ISK, salt=sid, info="PassStream v1 channel s2v", 32)   AES-256-GCM, sender → viewer
v2s_key = HKDF-SHA-512(ISK, salt=sid, info="PassStream v1 channel v2s", 32)   AES-256-GCM, viewer → sender
```

Because the ISK covers CI, sid and both messages, these keys bind the stream, `pk_S`, the viewer session, the mode, the grant, both nonces, the epoch and the suite.

### 5.5 Admit bundle

The box plaintext is 97 bytes (113 bytes including the GCM tag):

```
u8(1) || u32(epoch) || u16(0x0004) || epoch_secret[32] || grantId[16] || grantSecret[32] || u64(grantExpiresAt, ms since Unix epoch) || u16(code_index)
```

The viewer rejects a bundle whose epoch is older than the epoch in the signed ADa. A rekey may land between CHALLENGE and ADMIT, but the epoch never steps back.

### 5.6 Session grants (re-admission without the PIN)

- On a successful PIN admission the sender issues `grantId` (16 bytes) and `grantSecret` (32 bytes), valid for 8 h by default and bound to `viewerSid`. The sender stores them in its storage.
- To re-admit, the viewer sends a HELLO with `mode = 1` and the `grantId`, then runs the same CPace flow with the grant PRS. Grant handshakes don't touch the PIN budget, because the secret is 256-bit, and they work while the stream is frozen or locked: these are current viewers, not new admissions.
- An unknown, expired, revoked or wrongly bound grant gets a signed `grant_invalid`.
- Kicking a viewer revokes its grant and removes every connection admitted with it (§5.8). Rotating the PIN revokes all grants. Grants are not rotated on use (§14 Q5).
- **Revocation is write-ahead and survives a restart.** A separate storage record, the revocation fence (`revocations:<streamId>`), holds a **grant generation** and **tombstones**. Every grant is stamped with the generation current when it was issued. A grant from an older generation, or with a tombstone, is refused by every lookup, including after a sender restart, whether or not the grant table was cleaned. PIN rotation (`rotatePin`, `unlock({ newPin })`, `kickAndRotatePin`) bumps the generation: one durable counter revokes every earlier grant. A kick writes a tombstone. `unlock({ keepPin: true })` does not bump it, because it revokes nothing (current viewers keep their grants, §6). At startup the host applies the fence to the grant table. That is cleanup only, because lookups enforce the fence anyway. A corrupt fence refuses every grant.
- A kicked viewer who knows the unchanged PIN can run a new PIN admission and get a new grant. Use `kickAndRotatePin()` (or require approval, a later feature) to keep them out.

### 5.7 Lock and no-PIN

- `host.lock(true)` refuses new PIN admissions with `locked`. Grant re-admissions still work. `lock()` takes effect at once: a PIN admission that verified before it, but had not committed, is refused at commit and its just-issued grant revoked.
- A `pin: null` stream is protected by the link secret alone. The ADa flag tells the viewer no PIN is needed, so it uses the empty PIN. A viewer that has no PIN but gets a signed "PIN required" flag stops with `rejected/pin_required` and sends nothing.

### 5.8 Admission records, commit and owner actions

`SenderHost` keeps two identities apart:

- **`peerId`** is the transport id the app routes by (the SFU's viewer id). The relay controls it. It routes messages and nothing else. A HELLO on a peer only replaces an unfinished handshake on that peer; it never touches an existing admission, because the relay can forge HELLOs.
- **`admissionId`** (16 random bytes, b64url) names one authenticated admission. The sender creates it at commit time. The record links the session to the grant it was admitted with and stays after the transport closes (until the grant expires or the PIN rotates), so it can always be kicked. Owner actions take `admissionId`, never a `peerId`. `host.viewers()` lists the records.

**Commit.** When `Tb` verifies, the handshake does not register itself. It calls `commit()`, which is serialised with `kick`, `kickAndRotatePin`, `rotatePin`, `unlock` and `peerClosed`. Inside the lock, immediately before the session is registered and the epoch key is released:

1. PIN mode: the PIN generation read together with the PIN at HELLO must still be current (a rotation bumps it), and the stream must not be locked. A grant is issued only now.
2. Grant mode: the grant is looked up again; revoked (in storage, or in memory by a kick whose storage write has not finished), expired or rebound → `grant_invalid`.
3. Synchronously with registration, after the last await: the handshake must not have been aborted (`claim()`) and must still be the current one on its peer; PIN mode re-checks the PIN generation and the lock; grant mode re-checks the in-memory revocation. Otherwise any just-issued grant is revoked and the viewer gets a signed `expired`, `locked` or `grant_invalid`.

`abort()` on a handshake that is committing marks it aborted, so the commit refuses it. After a successful `claim()` the admission has happened and `abort()` is a no-op.

**Kick.** `kick(admissionId)` affects every record admitted with the same grant (a grant copied to other connections or tabs goes too). The revocation is written ahead, then applied in memory, then cleaned up:

1. At the call, before queueing: the key manager's revocation cutoff starts (§7), so the 300 ms bound runs from the owner's click, not from when the action reaches the front of the lock.
2. In the lock, synchronously: the fence write (a tombstone for the grant, or for `kickAndRotatePin` a generation bump, §5.6) is handed to storage first. Then the records stop being routed and are marked `kickIncomplete`, the grant is refused by `commit()`, and the key manager drops their channel keys and queues one fast rekey for everyone else. Nothing in this step can fail halfway.
3. Pending handshakes that claim the grant are aborted and receive a signed `grant_invalid` for their exact transcript, including checks waiting for RESPONSE. The fence write and the rekey's fan-out complete.
4. Cleanup: remove the grant from the grant table (or, for `kickAndRotatePin`, the rest of the rotation).
5. Only then are the records removed and `kicked` emitted (admission ids, and the peers that were connected, so the app can close those transports).

If step 3 or 4 throws (a storage error), the kick throws, but the records stay in `viewers()` with `connected: false, kickIncomplete: true`, the grant stays refused in memory, the sessions never get another epoch, and calling `kick()` again finishes the job. If the rekey itself fails, encryption is suspended at once (§7).

**A revocation that is not durable.** If the fence write fails or times out, the revocation still applies in memory, and three more things happen until a write succeeds:

- Media stops at once: the key manager is held (`onSuspend`) and no epoch is delivered.
- Every admission is refused with a signed `locked`: new PIN viewers, grant re-admissions and the victim.
- `host.revocationNotDurable` is `true`. `revocationDurability { notDurable: true, error }` fires once, and `{ notDurable: false }` fires when the write lands.

The kick or rotation throws `E2EEError("storage")`. `tick()` retries the merged fence write with backoff (100 ms, doubling, at most 2 s apart). Once it succeeds, admissions resume, and encryption resumes on an epoch the encryptor never had.

**`kickAndRotatePin(admissionId, pin)`** does the kick and a PIN rotation as one locked action with a single fast rekey. This is the host UI's "kick + rotate PIN".

**PIN rotation** (`rotatePin`, `unlock({ newPin })`, `kickAndRotatePin`) refuses a PIN equal to the current one after NFC normalisation, with `invalid_argument`, inside the lock and before any change. An unchanged "rotation" would reset the guess budget and let a kicked viewer back in; `unlock({ keepPin: true })` is the explicit way to keep the PIN. A rotation first hands the grant-generation bump to storage (write-ahead), then switches the PIN and generation in memory, then cleans the grant table and resets the budget in storage. If any of that fails, the call throws and the same new PIN is accepted once more to finish it.

Rotation cancels unfinished grant checks with signed `grant_invalid`, and unfinished PIN checks with signed `expired`; a pending knock retains its existing signed denial semantics. `SenderAdmission.abort(reason, rejection?)` optionally returns this existing REJECT format, signed with empty `nS` before CHALLENGE or the challenge nonce afterwards. It still settles a reserved guess as abandoned (or lets an already committing check settle), and cannot undo a claimed admission. Every handshake reply checks current ownership, including approval success/errors and synchronous knock teardown. A new HELLO takes ownership before ending the previous knock, suppressing the old transcript's denial. Supersession never removes an already claimed admission record; replies and error cleanup cannot affect the newer handshake.

**Display labels** are viewer-reported and unauthenticated: the viewer or the relay can set or swap them. The API calls them `viewerReportedLabel` (`setViewerReportedLabel(admissionId, label)`, at most 64 characters, no control characters). The UI must show them as such; actions bind to the `admissionId` the owner picked, so a swapped label can't redirect a kick to an unpicked admission.

### 5.9 Secret lifetime in memory

A handshake drops its secrets as soon as it no longer needs them. The viewer drops the PIN and zeroes its copies of the link secret and grant secret once it has computed its RESPONSE, and keeps only the derived session keys until ADMIT. The sender zeroes its CPace scalar, the PRS, `K`, the ISK, the MAC key, the admit bundle and the grant secret when the handshake ends (admitted, rejected or aborted). Callers' buffers are copied, never zeroed.

JavaScript limits this: the engine may have copied a buffer, strings (the PIN) are immutable and can only be dropped for the GC, and non-extractable `CryptoKey`s are freed by the GC. Zeroing shortens the window in which a heap snapshot or a later bug can read a secret; it is not a guarantee. The sender necessarily keeps the current PIN, the link secret and the identity seed for the stream's lifetime.

## 6. Guess budget (sender side)

Implemented in `GuessBudget` over an injected `AtomicStorage`. `update(key, fn)` must apply `fn` atomically and persist before resolving. `MemoryStorage` and `LockedLocalStorage` (localStorage plus the Web Locks API for cross-tab atomicity) are provided. `LockedLocalStorage` refuses to run without Web Locks. Corrupt stored state loads as **frozen** (fail closed).

- **Reserve before revealing.** Every PIN handshake reserves a guess, persisted, before the sender computes or sends its CPace share. Only a successful key confirmation releases it. Wrong PIN, invalid point, abort or closed channel, a timeout (30 s), and a reservation found stale after a crash or reload all settle as **failures**. Parallel handshakes can't overshoot: in-flight reservations count against the limits.
- **5 failures per viewer session** put that session in backoff: 60 s, doubling on each further block of 5 (60, 120, 240 s, …). A success clears that session's history.
- **Stream-wide pacing, independent of the session id.** Once the stream has 5 failures within 10 minutes, every further PIN attempt from *any* session starts at least `min(60 s, 2^(n−5) s)` after the previous attempt, where `n` counts the failures in the window plus attempts in flight (1, 2, 4 … 32, then 60 s). In-flight attempts count, so parallel handshakes can't start together when a wait ends. A paced attempt gets the existing signed `REJECT backoff` with `retryAfterMs`, and a failure's `REJECT wrong_pin` carries the wait, so the wire and CPace are unchanged. The pacing is persisted with the budget and survives a sender reload. Grant re-admissions spend no guesses and aren't paced. A legitimate viewer typing the right PIN during pacing waits at most 60 s. The pacing lapses after 10 quiet minutes, and the owner's unlock (the freeze's unlock path, including PIN rotation) clears it.
- **20 failures per stream freeze** new PIN admissions until the owner unlocks. `unlock({ newPin })` is the default and rotates the PIN, because unlocking with the old PIN hands an attacker another 20 guesses at it. `unlock({ keepPin: true })` is the explicit override. `unlock()` without either is refused. Rotating resets the budget, since guesses against the old PIN say nothing about the new one. Admitted sessions and grant re-admissions are unaffected.
- **Events** for the UI: `failure` (with outcome `wrong_pin | invalid | abandoned | timeout`), `backoff`, `frozen`, `unlocked`, `success`. `AlertBatcher` coalesces them, for example "7 failed PIN tries from 2 sources", and flushes freezes immediately.

Honest limits:

- `viewerSid` is chosen by the viewer, so an attacker sidesteps per-session backoff by rotating it, but not the stream-wide pacing: hammering with fresh ids gets 10 guesses in the first minute, then about one a minute. The per-stream freeze is the hard bound: 20 guesses per unlock.
- The pacing is shared, so an attacker who keeps guessing makes legitimate new viewers wait too (at most 60 s per attempt) until the freeze, which then needs the owner. That is the same denial of service the freeze already allows. Admitted viewers are unaffected.
- The relay can forge handshakes and freeze the stream on purpose. That is denial of service, which the relay can do anyway, and without the link secret those handshakes test nothing. Server-side IP limits are the second layer, per the plan.

## 7. Epochs, rekey, kick

- **Epoch** = `u32` number plus a 32-byte random `epoch_secret`. Numbers come from an atomic persisted counter per stream (`epoch:<streamId>`), so a sender reload or a second tab never reuses one.
- **Rekey triggers:** kick, PIN rotation, encryptor replacement, every 10 minutes (`tick()`), and on demand.
- **Procedure** (`SenderKeyManager`, serialised):
  1. Kicked sessions are removed first and the sender drops their channel keys. Allocate epoch `e+1` and a fresh secret.
  2. Send each remaining session a REKEY, individually:
     ```
     REKEY.box = AES-256-GCM(s2v_key, nonce = 0^4 || u64(seq), aad = lv_cat("PassStream v1 rekey", u64(seq)), u32(epoch) || u16(0x0004) || epoch_secret)
     REKEY_ACK.tag = AES-256-GCM(v2s_key, nonce = 0^4 || u64(seq), aad = lv_cat("PassStream v1 rekey ack", u64(seq), u32(epoch)), "")
     ```
     `seq` starts at 1 and strictly increases per session. The viewer rejects a non-increasing `seq` (replay) and an epoch that doesn't advance.
  3. **Activate** `e+1` (sender worker switches) when every session has acked or the deadline has passed. The deadline is `fastAckTimeoutMs` (**300 ms**) for kicks and encryptor replacement, `ackTimeoutMs` (1.5 s) otherwise. The `activated` event lists the sessions that timed out. Then fire `onKeyframeNeeded`, and the worker calls `generateKeyFrame()` on each video encoder. `startTimers()` ticks the key manager every 50 ms so the deadline is met closely.
- **Revocation cutoff (fail closed).** `kick()` removes the sessions and arms a cutoff synchronously, before anything queued: by `now + 300 ms`, the active epoch must be newer than every epoch allocated so far. `SenderHost` arms it already when the owner action is called (`beginRevocation()`), so a busy host lock can't delay it either. `tick()` enforces deadlines synchronously before it queues anything, so a long REKEY fan-out or a backlog of kicks cannot hold the switch: an overdue pending epoch is activated even mid-fan-out (sessions not yet reached get the REKEY right after). If no fresh epoch can be active by the cutoff (the epoch counter's storage failed, or the kick is still queued behind other work), the sender **stops encrypting**: `onSuspend` fires, the app posts `sender-suspend` (`postSenderSuspend`), and the worker drops every frame until the next `onEpoch`. A kick whose rekey throws suspends immediately. Encryption resumes only with a newer epoch; a key is never re-delivered.
- **Never activate an epoch a revoked session may hold.** Every epoch allocated before a kick armed its cutoff counts as stale. A stale pending epoch is discarded, even when its own deadline passes or every remaining viewer acked it, and a fresh epoch takes its place. So the encryptor never gets a key the kicked viewer received, not even briefly.
- **Bounded recovery.** While encryption is stopped, every `tick()` checks whether a recovery attempt is due: deliver the newest epoch if the encryptor never had it and no revocation is open, or else allocate a fresh one. Attempts back off from 100 ms, doubling, to at most 2 s apart, until one succeeds. This does not depend on any other event, so abandoning a revocation mid-recovery can't strand the sender. Every storage operation times out after `storageTimeoutMs` (1 s, `TimedStorage`). The timeout is measured on the injected clock at each `tick()`, with a real timer as a backstop. So a hung write fails, the queue moves on, and recovery proceeds while the original call is still unresolved. A late write is harmless: an epoch number is skipped, or a revocation revokes more.
- A rekey started while another is pending **supersedes** it. The superseded epoch is never activated, so a session kicked mid-rekey that received it learns nothing useful. The new pending epoch keeps the **earlier absolute deadline** (`min(old deadline, now + timeout)`), so a stream of rekeys can't postpone the switch.
- A viewer admitted while a rekey is pending gets the pending epoch too. That REKEY may overtake the ADMIT, so the viewer buffers up to 8 REKEYs until admission completes.
- **Receivers** (`ReceiverKeyRing`, in the worker) start a **5 s grace** for older epochs when the first frame of a newer epoch authenticates, then delete them. If no new-epoch frame arrives, older epochs are deleted **30 s** after the newer key was installed. At most 3 epochs are held.
- **Keyframe hooks:** the sender worker calls `generateKeyFrame()` on each epoch switch. A receiver that saw frames of an epoch before its key calls `sendKeyFrameRequest()` once the key arrives, at most 1/s.

### 7.1 Encryption-key lifecycle and worker recovery

Each (epoch, track) starts its SFrame CTR at 0. Installing an epoch in a second encryptor (a recreated worker, a fresh `SenderKeyRing`) would therefore reuse (key, nonce) pairs, which breaks AES-GCM (RFC 9605 §9.1). The rule is: **an epoch key reaches an encryptor exactly once.**

- `onEpoch` is the only way a sender epoch key leaves the library. It is called once per epoch. `SenderKeyManager` exposes only `activeEpoch` (the number), never the active secret.
- Within one worker, `SenderKeyRing` refuses to install an epoch that isn't newer than the current one and refuses a second encryptor per track.
- **Recovery:** if the transform worker dies or must be replaced, terminate it, create the new worker and transforms, then call `host.replaceEncryptor()`. It starts a rekey with the 300 ms deadline. Viewers get the fresh epoch first, then `onEpoch` delivers it to the new worker. Until then the new worker has no key and drops every frame (fail closed). Never post an earlier `EpochKey` to the new worker.
- A sender reload creates a new host, which allocates a fresh epoch number from the persisted counter, so it is covered the same way.

## 8. Media: SFrame profile (RFC 9605)

- **Cipher suite** `0x0004 AES_128_GCM_SHA256_128` (Nk = 16, Nn = 12, Nt = 16). The key schedule, nonce and AAD follow §4.4 exactly: `sframe_secret = HKDF-Extract("", base_key)`, labels `"SFrame 1.0 Secret key " || KID:u64 || suite:u16` and `"SFrame 1.0 Secret salt " || …`, `nonce = salt XOR CTR`, `aad = header || metadata`.
- **KID** = `epoch × 16 + track` (track 0–15; convention: native video 0, audio 1, independent 320×180 thumbnail 2). Each (epoch, track) gets its own base key and its own CTR from 0:
  ```
  base_key = HKDF-SHA-256(ikm = epoch_secret, salt = "", info = "PassStream v1 SFrame base_key" || u64(KID) || u16(0x0004), 32)
  ```
- **No nonce reuse:**
  - Keys are marked encrypt-only or decrypt-only (§4.4.1).
  - The CTR is consumed before the AEAD call.
  - Encryption stops at CTR 2³²−1 (`counter_exhausted`); a 10-minute epoch at 60 fps uses about 36 000.
  - The sender key ring refuses to move an epoch backwards and refuses a second encryptor for the same track in one worker.
  - An epoch key reaches an encryptor once; a replaced worker gets a fresh epoch (§7.1).
  - Epoch numbers persist across reloads.
- **Replay window**: 1024 counters per KID, checked before decryption, checked again right after the AEAD verifies and committed synchronously with that second check, before any plaintext is returned. Forged frames can't advance it, and two concurrent decryptions of one frame can't both pass.
- **Track binding**: a receiver transform drops frames outside its explicit track allowlist, for example audio spliced into the video stream. The switched video receiver accepts native 0 and thumbnail 2; audio accepts only 1.
- **Header encoding** uses the minimal form only; non-minimal headers are rejected, so each (KID, CTR) has exactly one AAD.

### 8.1 Frame layout and what stays clear

```
protected_frame = clear_prefix || SFrame(header || AEAD(rest, aad = header || clear_prefix))
```

The clear prefix is the SFrame `metadata`, so it is **authenticated** (§4.4.3). The receiver derives the prefix length from clear bytes only, so it gets the same answer as the sender.

| Codec | Left in the clear | Why |
|---|---|---|
| VP8 (standalone helpers only; refused by app/SFU negotiation) | 10 bytes on keyframes, 3 on delta frames (keyframe bit = bit 0 of byte 0) | The RTP packetizer and depacketizer read the uncompressed data chunk (RFC 6386 §9.1): frame tag, plus start code and dimensions on keyframes. |
| VP9 | nothing | The payload descriptor is built from encoder metadata, not by parsing the bitstream. |
| H.264 | **refused** (`unsupported_codec`, frame dropped on both sides) | libwebrtc parses `first_mb_in_slice`, `slice_type` and `pps_id` from the slice header before the receive transform runs, and can rewrite SPS on the send path. No frame-level clear prefix is both sufficient and reliably preserved. |
| AV1 | **refused** (`unsupported_codec`, frame dropped) | The packetizer parses OBU headers and sizes and drops some OBU types, so a frame-level ciphertext breaks packetization. |
| Audio (Opus, …) | nothing | The receive transform runs before NetEq, so the decoder sees plaintext. |

**PassStream negotiates VP9 profile 0 only (non-flexible L1T3)**, exported as `E2EE_VIDEO_CODECS` for `setCodecPreferences`. An active video m-line without VP9 profile 0 is refused with `unsupported_codec`; VP8 cannot be negotiated because every SFU path assumes VP9 descriptors. The standalone crypto helpers retain VP8 for vectors, not app negotiation. The codec comes from `frame.getMetadata().mimeType`, falling back to the transform's `codec` option. If neither is available the video frame is dropped. Empty frames are dropped.

### 8.2 Worker glue and failing closed

- `e2ee/src/transform-worker.ts` is the module worker for `RTCRtpScriptTransform`. Options are `{ role: "encrypt" | "decrypt", kind: "video" | "audio", track, tracks?, codec? }`. `tracks` is a decrypt-only video allowlist; the viewer accepts `{0, 2}` on its switched video receiver, with a separate replay window per KID. Invalid or duplicate claims fail closed. Main thread → worker messages are `sender-epoch`, `sender-suspend`, `receiver-epoch`, `receiver-drop-epoch` and `keyframe`. `sender-suspend` drops the sender key; frames are refused until a newer `sender-epoch`.
- Every error **drops the frame**. A frame is never forwarded unencrypted (sender) or undecrypted (receiver). Invalid options produce a transform that discards everything and posts an error. Drops are reported to the main thread, throttled per reason.
- `detectE2EESupport()` / `assertE2EESupported()` check for a secure context, WebCrypto, Worker and `RTCRtpScriptTransform`. A missing piece is `E2EEError("not_supported")` with the reason; the UI shows "browser not supported" and never falls back to plaintext.
- `attachTransform()` verifies the browser kept the transform.
- `addEncryptedTrack()` creates the transceiver without a track, attaches the transform, and only then calls `replaceTrack()`. No frame can leave before encryption is in place.

## 9. Derivation and label registry

| Label | Used for |
|---|---|
| `"PassStream v1 PRS pin"`, `"PassStream v1 PRS grant"` | CPace PRS |
| `"PassStream v1 CPace CI"`, `"initiator:sender"`, `"responder:viewer"` | CPace CI |
| `"CPaceRistretto255"`, `"CPaceRistretto255_ISK"`, `"CPaceMac"` | CPace draft-21 (generator, ISK, mac_key) |
| `"PassStream v1 challenge"`, `"PassStream v1 reject"` | Ed25519 signature inputs |
| `"PassStream v1 admit box"`, `"PassStream v1 channel s2v"`, `"PassStream v1 channel v2s"` | HKDF-SHA-512 info, from ISK with salt = sid |
| `"PassStream v1 admit"` | ADMIT box AAD |
| `"PassStream v1 rekey"`, `"PassStream v1 rekey ack"` | REKEY / ACK AAD |
| `"PassStream v1 SFrame base_key"` | HKDF-SHA-256 info, per-KID base key |
| `"SFrame 1.0 Secret key "`, `"SFrame 1.0 Secret salt "` | RFC 9605 §4.4.2 |
| `"PassStream v1 annotation session"` | HKDF-SHA-512 info, from ISK with salt = sid (§20) |
| `"PassStream v1 annotation epoch"` | HKDF-SHA-256 info, from the epoch secret (§20) |
| `"PassStream v1 annotation v2s"`, `"PassStream v1 annotation s2v"`, `"PassStream v1 annotation"` | Annotation key info and AAD (§20) |

## 10. What the SFU can still see

- **Data channel:** every handshake message in the clear (it is only DTLS hop-by-hop). That reveals `streamId`, `viewerSid`, nonces, CPace shares, grant ids, epoch numbers (in ADa), REJECT reasons and tries left: how many PIN failures happen and when. It also sees REKEY sizes and timing, which reveal when kicks and rotations happen.
- **SFrame headers:** KID (epoch number and track) and CTR (frames sent per track).
- **Frame sizes and timing,** hence keyframes, motion and activity. RTP timestamps, sequence numbers and the marker bit.
- **Codec headers left clear:**
  - VP9: the payload descriptor (layer ids, resolution in the scalability structure).
- **RTP header extensions,** all readable and none authenticated by us:
  - The **Dependency Descriptor**: layer structure, frame dependencies, decode targets, resolution. It stays clear so the SFU can drop layers.
  - **audio-level**: voice activity. **Don't negotiate `ssrc-audio-level` under E2EE**; a single-publisher SFU doesn't need it.
- **Session metadata:** viewer count, join and leave times, IPs.

The SFU can lie about the DD or rewrite it. That breaks decoding (denial of service) but not confidentiality. Frames are authenticated end to end, but layer metadata is not (§14 Q10).

## 11. Tests

Run with `cd e2ee && mise exec -- npm test`. All **133 tests pass** (Node 26.13.0, vitest 5.1.0), and `tsc --noEmit` is clean.

| File | What it proves |
|---|---|
| `cpace-vectors.test.ts` | draft-irtf-cfrg-cpace-21 Appendix A string utilities and B.3 (ristretto255/SHA-512): generator string and generator, Ya/Yb, K, ISK (IR and symmetric), sid_output, valid `scalar_mult`, invalid points → G.I. Vectors are decoded from the draft's base64 JSON blocks. |
| `sframe-vectors.test.ts` | RFC 9605 Appendix C: all 289 header cases, C.2 AES-CTR+HMAC for suites 1–3, C.3 SFrame for suites 1–5 (labels, secret, key, salt, nonce, ciphertext). Parsed straight from the RFC text. |
| `admission.test.ts` | Success. Wrong PIN consumes budget. NFC. Abandoned (peer closed) and timed-out handshakes count, and a late RESPONSE is refused. A reservation left by a sender crash counts after reload. 5 failures → 60 s, then 120 s. Parallel handshakes can't overshoot. Freeze at 20 while admitted viewers and grants keep working, then unlock (rotating the PIN). Grants: re-admission, bound to viewerSid, revoked by kick and rotation, expiry, no budget use. Lock. No-PIN with the link secret alone. Signed "PIN required". |
| `adversarial.test.ts` | Relay MITM: a single-bit flip at **every byte** of every handshake message (492 cases, a fresh host each, and each asserts the targeted message was really produced before mutating it) never yields an admission and always raises typed errors. ADa can't be rewritten. Replays of CHALLENGE (to new and same viewer sessions), RESPONSE (a charged failed guess), ADMIT and REJECT are refused. Out-of-order messages are refused. Forged REJECT. Forged, garbage, wrong-stream and wrong-nonce challenge signatures give no RESPONSE. A passive offline attack over all 10 000 PINs (every key/MAC candidate computable without a secret scalar), even by someone who also holds the link secret, confirms none and can't open the key box. An active MITM holding the link secret gets one charged guess, and the REJECT is verifiable from public values. A relay without the link secret can't join a no-PIN stream. |
| `sframe-frames.test.ts` | Round trips for VP8 key/delta, VP9 and Opus, with exactly the documented prefix clear. H.264, AV1 and unknown codecs refused on both sides. No key → drop. Tamper detection (header, body, tag, clear prefix, truncation, extension). Replay and out-of-window rejection. Forged frames don't advance the window. Cross-track splicing dropped. One encryptor per track. Counter limit. Key directions. |
| `review-round1.test.ts` | One or more regression tests per review finding (§15): revocation at commit (kick and storage revocation during a grant handshake, rotation racing a PIN admission), grant-wide kick, forged HELLO and closed transports, encryptor replacement, concurrent replay, mandatory link secret and canonical fragments, secret wiping, 300 ms kick switch and absolute deadlines, admission-id owner actions, `kickAndRotatePin`, unlock policy, labels. |
| `review-round2.test.ts` | Regression tests for review round 2 (§16): a failed kick stays listed, keyless and retryable (rotation, grant revocation and epoch allocation failures), suspension and recovery; lock racing a commit; unchanged and NFC-equivalent PINs refused; deadlines enforced past a stalled fan-out, a backlog of kicks and a busy host lock; the 250-session load case in real time; `sender-suspend` in the worker. |
| `review-round3.test.ts` | Regression tests for review round 3 (§17): a restart (after a failed grant write, or a crash mid-rekey) never revives a kicked grant; a fence write that fails holds media, refuses every admission and retries with backoff; the grant generation refuses older grants after a restart for all three rotations; an in-flight encryption is dropped after suspend or rekey; a stale pending epoch is never activated; recovery within 2 s after an abandoned revocation, a hung allocation and persistent storage failure; a hung grant write can't block a kick. |
| `rekey.test.ts` | Kick: no REKEY to the kicked session, it can't decrypt the new epoch, the others can. Ack-timeout switch. 10-minute rekey. A kick supersedes a pending rekey. Late admission gets the pending epoch. REKEY replay. Grace and fallback retirement. **(KID, CTR) unique across tracks, epochs and a sender reload**; epoch rollback refused. |
| `fuzz.test.ts` | Seeded random and mutated inputs: 20 000 each for wire messages, SFrame headers, codec layouts, fragments, keys and base64; 2 000 for SenderHost and ViewerAdmission; 5 000 garbage frames into decryptors. Only `E2EEError` is ever thrown, and the host still admits an honest viewer afterwards. |
| `worker.test.ts` | The worker runtime with fake `RTCRtpScriptTransformer` streams: an encrypt → decrypt pipe, keyframe on epoch switch, fail-closed on missing key or bad options, second encryptor per track refused, keyframe request after a missing epoch arrives, malformed messages. Feature detection, a transform ignored by the browser, and `addEncryptedTrack` refusing before touching the connection. |
| `storage-alerts.test.ts` | Web Locks adapter serialises two "tabs". Corrupt state → frozen. Alert batching. |
| `throughput.test.ts` | §12. |

**Not tested here:** real browsers. The worker runs against fakes. The VP8 prefix, `generateKeyFrame` / `sendKeyFrameRequest` behaviour and worker recovery need the Phase 1 torture suite in Chromium, Safari and Firefox.

## 12. Measured performance

Measured on 2026-11-03 with `npx vitest run test/throughput.test.ts`: Node v26.13.0, AMD Ryzen 5 7600 (12 threads), Linux 7.2.14. This is the full frame path (layout, header, AES-128-GCM through WebCrypto, copies), single-threaded and sequential. One run is shown (after review round 1, which removed H.264).

| Codec | Frame | Frames | Encrypt µs/frame | Decrypt µs/frame | Encrypt MB/s | Decrypt MB/s |
|---|---|---|---|---|---|---|
| VP9 | 8 KiB (text-mode delta) | 3000 | 26 | 22 | 317 | 369 |
| VP9 | 64 KiB (busy delta) | 1024 | 46 | 41 | 1427 | 1614 |
| VP9 | 256 KiB (screen keyframe) | 256 | 134 | 114 | 1958 | 2300 |
| VP9 | 1 MiB (large keyframe) | 64 | 448 | 387 | 2343 | 2708 |
| VP8 | 8 KiB | 3000 | 20 | 21 | 402 | 382 |
| VP8 | 64 KiB | 1024 | 48 | 44 | 1352 | 1501 |
| VP8 | 256 KiB (keyframe) | 256 | 164 | 120 | 1596 | 2191 |
| VP8 | 1 MiB (keyframe) | 64 | 483 | 475 | 2169 | 2207 |

- 1440p text mode at 15 fps with 8–64 KiB delta frames costs under 1 ms of CPU per second.
- A full PIN admission (both sides, in-process relay) takes **4.9 ms**.
- Kick backlog (review round 2): 250 sessions and 150 kicks queued at once, `tick()` every 10 ms. The first switch came **300.6–302.3 ms** after the kicks in three full-suite runs (`review-round2.test.ts`); before the fix it was 399.8 ms.
- Browser numbers inside a worker still need measuring.

## 13. Integration notes for the web app

Sender:

```ts
assertE2EESupported();
const identity = SenderIdentity.generate();             // persist identity.exportSecretKey() sender-side
const linkSecret = generateLinkSecret();                // persist sender-side with the identity
const link = `${origin}/s/${streamId}#${buildFragment(capability, identity.publicKey, linkSecret)}`;
const worker = new Worker(new URL("@passstream/e2ee/transform-worker", import.meta.url), { type: "module" });
const host = await SenderHost.create({
  identity, streamId, pin, linkSecret,
  storage: new LockedLocalStorage({ kv: localStorage, locks: navigator.locks }),
  send: (peerId, bytes) => dataChannelSend(peerId, bytes),
  onEpoch: (k) => postSenderEpoch(worker, k),
  onSuspend: () => postSenderSuspend(worker),          // a kick missed its cutoff: drop frames until onEpoch
  onKeyframeNeeded: () => worker.postMessage({ type: "keyframe" }),
});
const stopTimers = host.startTimers();
host.budget.on("failure", (e) => alerts.failure(e.viewerSid));
host.on("kicked", (e) => e.peerIds.forEach(closeViewerTransport));
// Owner UI: rows from host.viewers(), actions by admissionId:
//   host.kick(id) · host.kickAndRotatePin(id, newPin) · host.unlock({ newPin }) · host.lock(true)
//   A kick that throws (storage) leaves the row with kickIncomplete: show it and offer kick(id) again.
host.on("revocationDurability", (e) => showBanner(e.notDurable));   // media stopped, admissions refused, retrying
// Worker crashed or replaced: terminate it, build the new one, then
//   await host.replaceEncryptor();                      // never re-post an old EpochKey
await addEncryptedTrack(pc, videoTrack, worker, { track: 0 });
await addEncryptedTrack(pc, audioTrack, worker, { track: 1 });
onDataChannelMessage((peerId, bytes) => host.receive(peerId, bytes));
```

Viewer:

```ts
assertE2EESupported();                                  // else: "browser not supported"
const { capability, senderKey, linkSecret } = parseFragment(location.hash);
const adm = new ViewerAdmission({ streamId, senderKey, viewerSid, linkSecret, pin });   // or { grant }
send(adm.start());
onMessage(async (bytes) => {
  if (adm.state !== "admitted") {
    for (const out of await adm.handle(bytes)) send(out);     // typed errors: rejected/bad_signature/…
    if (adm.result) for (const k of adm.result.epochs) postReceiverEpoch(worker, k);  // store result.grant in sessionStorage
  } else {
    const { epoch, ack } = await adm.result!.session.handle(bytes);
    postReceiverEpoch(worker, epoch);
    send(ack);
  }
});
pc.ontrack = (e) => attachTransform(e.receiver, worker, { role: "decrypt", kind: e.track.kind as "video" | "audio", track: e.track.kind === "audio" ? 1 : 0 });
```

- Negotiate `E2EE_VIDEO_CODECS` (VP9 profile 0 only) on the video transceivers.
- The SFU relays these binary messages per viewer. Each message is ≤ 4096 bytes and starts with version byte `0x01`. `peekType()` routes them.
- Messages must arrive in order on the signaling WebSocket. The viewer tolerates a REKEY that overtakes the ADMIT.

## 14. Open questions for the outside review

Questions settled in review round 1 are marked; §15 has the details.

1. **CPace instantiation.** Is it sound to put role-tagged identities, mode and grant id into CI, use `sid = nV || nS`, carry epoch/suite/flags in ADa, follow draft §10.4 for key confirmation (viewer confirms first), and derive the session keys from the ISK with HKDF salted by `sid`?
2. **Signed challenge.** Does `challenge_input` (which binds `nV`, `nS`, `Ya`, `ADa`, `pk_S` and the stream) fully stop a relay from running PIN guesses against viewers? Is any oracle left in the viewer's error behaviour or timing? The viewer computes CPace only after the signature verifies.
3. **Budget semantics.** Is charge-before-challenge with refund-on-success right? Is 20 the right cap for a 4-digit default? *Settled:* unlocking after a freeze rotates the PIN by default; keeping it needs `{ keepPin: true }`.
4. *Settled:* **the link secret is mandatory** for every stream (`s=` in the fragment, 32 bytes, checked on both ends).
5. **Grants.**
   - Is using the 256-bit grant secret as the CPace PRS (rather than a MAC) the right choice? It gives forward secrecy.
   - Is it acceptable that grants are not rotated on use? A kick now removes every connection holding the grant.
   - Grants are bound to `viewerSid`, stored in sender storage, and last 8 h.
6. **Epoch keys.**
   - Per-track base keys are derived by HKDF from one epoch secret.
   - The KID leaks the epoch count and the track.
   - The CTR cap is 2³²−1.
   - Rollback protection relies on a persisted counter, so storage loss could repeat epoch numbers (with fresh secrets: no nonce reuse, but KID confusion).
7. *Settled:* **kick latency** is min(all acks, 300 ms), with an absolute deadline across superseding rekeys.
8. **Receiver retention.** Old epochs get a 5 s grace and a 30 s fallback, with at most 3 held.
9. **Codec prefixes.** *Settled for H.264:* refused, like AV1. Still need validation in Chromium, Safari and Firefox: the VP8 10/3 split, and Opus fully encrypted (some deployments leave the TOC byte clear).
10. **Dependency Descriptor and layer metadata** are not authenticated. Binding `getMetadata()` temporal and spatial indices into the AAD is possible if both sides see the same values.
11. *Out of scope for v1:* **per-frame sender authentication.** An admitted viewer plus the SFU can inject frames other viewers accept (RFC 9605 §7.2). No signatures are added; the plan defers them.
12. *Settled:* **session labels.** Owner actions bind to the sender-assigned `admissionId`; labels are `viewerReportedLabel`, display only (§5.8). Sending the label inside the authenticated channel remains possible later.
13. **Side channels.**
    - WebCrypto AES-GCM failure timing.
    - noble's constant-time claims for ristretto255 and Ed25519 in a JS engine.
    - JS string NFC normalisation of the PIN.
14. **Sender key handling.** Where should the Ed25519 seed and the link secret live for the Chromium app-window sender and for the future native sender?
15. *Settled:* **HELLO from an admitted peer** no longer removes the admission (§5.8). The relay can still drop that viewer's traffic, which is denial of service it can always do.

## 15. Review round 1 fixes

An adversarial review (GPT-6.1-Sol) reported nine findings against 6ce81dd and reproduced each. Each attack was re-run against 6ce81dd before fixing it, and failed as reported (the tampering test was checked by running its new stage assertions with the old shared host). Tests are in `e2ee/test/review-round1.test.ts` unless noted.

| # | Finding | Fix | Regression test |
|---|---|---|---|
| 1 | **High.** Revocation checked before the handshake, not at admission: a grant handshake started before a kick finished after it and got the post-kick epoch; an old-PIN admission could complete across `rotatePin()` with a fresh grant, and `abort()` was ineffective once the handshake was marked done. | Admission goes through `commit()`, serialised with kick, rotate, unlock and close. Immediately before registration it re-checks the PIN generation (PIN mode), looks the grant up again (grant mode), and atomically `claim()`s the handshake, which fails if it was aborted. A new `committing` state keeps `abort()` effective until the claim. Grants are issued only inside the commit (§5.8). | "a grant handshake started before a kick cannot finish after it", "a grant revoked between CHALLENGE and RESPONSE is re-checked at commit", "an old-PIN admission racing rotatePin() is refused and gets no grant" |
| 2 | **High.** A cloned grant survived a kick through another connection. | Records link every connection and pending handshake to its grant. `kick()` aborts the handshakes, revokes the grant and removes every record holding it, then rekeys once. Documented that a kicked viewer who knows the PIN can return; `kickAndRotatePin()` is the one-action fix the host UI offers. | "cloned grant on other peers, and a pending grant handshake, are all cut off"; "a kicked viewer who knows the PIN can come back; kickAndRotatePin keeps them out" |
| 3 | **High.** A forged HELLO removed an admitted session before validation, so a later kick couldn't find its grant. | The authenticated admission record (`admissionId` → grant) is independent of transport state. HELLO never touches it; a closed transport only marks it disconnected. Owner actions take `admissionId`. | "a bare or well-formed HELLO on an admitted peer leaves the admission kickable", "a closed transport keeps the admission record, so a later kick still revokes the grant" |
| 4 | **High.** Recreating the worker could reuse AES-GCM nonces: the current epoch installed in a fresh ring restarts the CTR at 0. | An epoch key reaches an encryptor once, via `onEpoch`; the active secret is no longer readable (`activeEpoch` only). `host.replaceEncryptor()` activates a fresh epoch within 300 ms for a new worker (§7.1). | "the active secret is not readable, and replaceEncryptor() gives the new ring a fresh epoch", "an unacknowledged replacement still activates within 300 ms" |
| 5 | **Medium.** The H.264 clear prefix was insufficient and not reliably preserved. | H.264 is refused under E2EE (fail closed), like AV1. The original library also supported VP8 vectors; PassStream now negotiates VP9 profile 0 only (`E2EE_VIDEO_CODECS`). The H.264 escaping code is removed. | `sframe-frames.test.ts`: "refuses H.264 and AV1 (fail closed) on both sides, and frames without a known codec" |
| 6 | **Medium.** Replay acceptance raced across concurrent decryptions. | The window is checked again right after the AEAD and committed synchronously before plaintext is returned. | "two concurrent decryptions of the same frame: exactly one succeeds" |
| 7 | **Medium.** The 256-bit link secret wasn't enforced or carried by the link helpers. | Mandatory for every stream: exactly 32 bytes, validated by `SenderHost`, `ViewerAdmission` and `pinPrs`. `buildFragment`/`parseFragment` carry a canonical `s=` and reject anything else. Guarantee text updated (§1). | "buildFragment/parseFragment round-trip a canonical s= and refuse anything else", "both ends refuse to run without exactly 32 bytes", "a server that knows (or guesses) the PIN but not the fragment cannot admit itself" |
| 8 | **Medium.** The bit-flip tampering test went vacuous once the budget ran out. | A fresh host per case, and each case asserts the targeted message (CHALLENGE, RESPONSE, ADMIT) was really produced before flipping it, plus a final count that every byte was reached. | `adversarial.test.ts`: "every single-bit flip in any handshake message breaks the handshake" |
| 9 | **Low.** A completed admission kept the ISK and PIN-bearing options. | Handshakes zero or drop their secrets when they no longer need them; JavaScript's erasure limits are documented (§5.9). | "after admission: sender scalar and grant secret wiped, viewer holds nothing", "after a wrong PIN or an abort: nothing retained either" |

Binding decisions applied in the same round:

- **Kick:** the sender drops a kicked viewer's channel keys; the remaining sessions switch at min(all acks, 300 ms); superseding rekeys keep the earliest absolute deadline. Tests: "a kick switches within 300 ms even if a remaining viewer never acks", "superseding rekeys keep the earliest deadline instead of restarting it".
- **Unlock after a freeze** rotates the PIN by default (`unlock({ newPin })`); `unlock({ keepPin: true })` is the explicit override. Test: "unlock after a freeze rotates the PIN unless keepPin is explicit".
- **Display labels:** owner actions bind to `admissionId`; labels are `viewerReportedLabel`. Test: "owner actions take admission ids, never transport ids; labels are display-only".
- **Q11** (colluding-viewer forgery) is documented as out of scope; no signatures added.

## 16. Review round 2 fixes

The verification review (GPT-6.1-Sol) confirmed all nine round 1 findings fixed against 294ba5d and reported four new ones, with independent reproductions. Each was ported into `e2ee/test/review-round2.test.ts`, and all 12 tests there fail against 294ba5d and pass now.

| # | Finding | Fix | Regression test |
|---|---|---|---|
| 1 | **High.** A failed combined action left an invisible session that couldn't be kicked. A persistence failure during the budget reset in `kickAndRotatePin` came after the records were deleted and before the key manager was told: the victim vanished from `viewers()`, kept its channel and the active media key, and a retried kick said "unknown admission". | Kicks revoke in memory first and persist last (§5.8): routing, an in-memory grant refusal and the key manager's session removal and cutoff are one synchronous step; storage writes follow; records go only when everything succeeded. A failed kick throws and leaves the records listed as `kickIncomplete`, and `kick()` retries it. A rotation that failed to persist accepts the same new PIN once more to finish. If the rekey fails, encryption is suspended at once (`onSuspend`, §7) until a fresh epoch activates. | "a persistence failure in kickAndRotatePin leaves the victim listed, keyless and kickable", "a failed grant revocation keeps the grant refused in memory and the kick retryable", "a kick whose rekey cannot allocate an epoch stops encryption at once, then recovers" |
| 2 | **Medium.** `lock(true)` raced the admission commit: locking while the new grant was being persisted still produced an ADMIT. | The lock is re-checked synchronously right before `claim()` and registration; a refused commit revokes the grant it just issued and sends a signed `locked`. | "locking while the grant is being persisted refuses the admission and revokes the grant" |
| 3 | **Medium.** An unchanged PIN counted as a rotation: `unlock({ newPin: oldPin })` reset the budget without `keepPin` (also with an NFC-equivalent string), and `kickAndRotatePin(id, oldPin)` let the kicked viewer back. | Every rotation compares the NFC-normalised new PIN with the current one inside the lock, before any change, and refuses an equal one (`null` → `null` included) with `invalid_argument`. | "rotatePin and unlock refuse the same PIN, also when only NFC-equivalent", "kickAndRotatePin with the current PIN is refused before anything changes", "a no-PIN stream cannot 'rotate' to no PIN" |
| 4 | **Medium.** The absolute rekey deadline wasn't enforced independently: `tick()` queued behind the fan-out it should cut short. Stalled crypto kept the old epoch past a simulated 400 ms; 250 sessions with 150 queued kicks switched after 402–405 ms. | A revocation cutoff (§7) armed synchronously by `kick()`, and by `SenderHost` when the owner action is called. `tick()` enforces deadlines synchronously before queueing. If no fresh epoch can be active by the cutoff, the sender fails closed: `onSuspend` (new, required) stops encryption via the worker's `sender-suspend` until the next `onEpoch`, and a recovery rekey is queued. Sessions kicked mid-fan-out never get the key. | "a stalled rekey fan-out cannot hold the switch past the deadline", "a backlog of queued kicks switches at the first kick's deadline, never to a kicked session", "a kick stuck behind the host's own queue suspends encryption at its cutoff", "250 sessions and 150 queued kicks: first switch within the 300 ms deadline (real time)" (300.6–302.3 ms, §12), "SenderKeyRing.suspend drops frames until a newer epoch; the worker handles sender-suspend" |

API changes in this round:

- `SenderHostOptions.onSuspend()` and `SenderKeyManagerOptions.onSuspend()` are required. Wire them to `postSenderSuspend(worker)`.
- `ViewerInfo.kickIncomplete`; `SenderKeyManager.suspended`, `revocationCutoff` and `beginRevocation()`; `SenderKeyRing.suspend()`; worker message `sender-suspend`.
- `rotatePin`, `unlock({ newPin })` and `kickAndRotatePin` reject an unchanged PIN.

Interpretation of "fail closed after a kick": when `kick()` returns, the remaining viewers may still be on the old epoch for up to the documented 300 ms (they need the new key first). After the cutoff, no frame is encrypted with a key a kicked viewer holds; when `kick()` throws because the rekey failed, encryption stops immediately.

## 17. Review round 3 fixes

The round 3 verification (GPT-6.1-Sol) confirmed the four round 2 fixes against e9a0844 and reported four new findings, with reproductions. The reproductions are ported into `e2ee/test/review-round3.test.ts`. Eleven of its twelve tests fail against e9a0844 and all pass now. The twelfth, "a grant from the current generation still re-admits after a restart", is a control against over-revoking and passes on both. All of the reviewer's reproductions pass now except "recovery limit". That one asserted the old limitation (healing stays blocked while a hung storage call is unresolved), and it now fails as intended: the hung kick rejects with a storage timeout, and recovery runs without waiting for it.

| # | Finding | Fix | Regression test |
|---|---|---|---|
| 1 | **High.** A restart revived revoked viewers. Revocation was persisted only after the rekey completed, so after a failed grant write, or a crash between eviction and persistence, a restarted sender accepted the kicked viewer's stale grant without a PIN. | **Write-ahead revocation** (§5.6, §5.8). A revocation fence, stored apart from the grant table, holds a **grant generation** and **tombstones**. A kick hands a tombstone to storage before it evicts anything in memory. A PIN rotation (`rotatePin`, `unlock({ newPin })`, `kickAndRotatePin`) bumps the persisted generation, and grants from older generations are refused. Lookups enforce the fence, so an unapplied intent counts as applied, at startup too, where the host also cleans the table. If the fence write fails, the revocation still applies in memory, media stops at once (key manager hold), every admission including grant re-admissions is refused with `locked`, `revocationNotDurable` and the `revocationDurability` event tell the UI, and `tick()` retries with backoff (≤ 2 s apart) until the write lands. | "a kick whose grant-table write failed is still refused after a restart", "a crash while the kick's rekey is stalled leaves a durable tombstone", "if the tombstone cannot be written: revoked in memory, media stopped, every admission refused, retried with backoff", "PIN rotation bumps a durable grant generation: older grants stay refused after a restart" (all three rotation APIs) |
| 2 | **High.** Suspension let in-flight old-key frames through. An encryption paused inside WebCrypto that resumed after `suspend()` still returned success with an old-key frame. | `SenderKeyRing.version` changes on every `setEpoch()` and `suspend()`. `FrameEncryptor` rechecks it after every await and before it emits. If it changed, the frame is dropped with the new reason `key_changed`. | "a frame awaiting WebCrypto when the sender ring changes (suspend) is dropped", "… (rekey) is dropped" |
| 3 | **Medium.** An overdue pending epoch that had already been sent to the kicked viewer was still activated at its deadline (`onActivate`, then `onSuspend`), briefly reinstalling an unsafe key. | Never activate an epoch that a revoked session may hold (§7). An epoch allocated before the kick's cutoff was armed is stale. A stale pending epoch is discarded, whatever path would activate it (deadline, acks, removals), and a fresh epoch is allocated. Delivery to the encryptor is also refused while an open revocation is past its cutoff. | "an overdue pending epoch already sent to the kicked viewer is discarded, not activated" |
| 4 | **Medium.** Recovery was conditional. An unresolved storage operation blocked queued healing indefinitely. Abandoning a revocation during healing could leave the sender suspended with no cutoff and no pending epoch, and only the 10-minute rekey rescued it. | Storage operations time out (`TimedStorage`, 1 s by default, `storageTimeoutMs`), measured on the injected clock at each `tick()`, with a real timer as a backstop. A hung call fails and the queue moves on. A late write is harmless (§7). While encryption is stopped, `tick()` starts a recovery attempt whenever one is due, with backoff from 100 ms, doubling, at most 2 s apart. It does not depend on any other event, so "suspended" always leads to a recovery attempt within 2 s. Recovery delivers a never-delivered safe epoch or allocates a fresh one; a key is never re-delivered. | "abandoning a revocation while recovery is in flight still resumes within 2 s", "a hung epoch allocation times out, and recovery proceeds while it is still unresolved", "a persistently failing storage is retried with backoff, at most 2 s apart, and recovers once it heals", "a hung grant write during an admission times out instead of blocking the owner's kick" |

API changes in this round:

- `SenderHost.revocationNotDurable` and the `revocationDurability` event. Show a banner: media is stopped and admissions are refused until the write lands.
- `SenderKeyManager.hold()`. `SenderKeyManager.suspended` is now true whenever the encryptor has no key: a missed cutoff, a failed kick rekey, or a hold. The new rekey reason `recovery` is fast (300 ms).
- `GrantStore.tombstone()`, `bumpGeneration()` and `recover()`. `revoke()` and `revokeAll()` are now housekeeping.
- `TimedStorage`, `DEFAULT_STORAGE_TIMEOUT_MS`, `retryDelayMs()`. New `storageTimeoutMs` options on the host and the key manager. New error code `storage`.
- `SenderKeyRing.version`; drop reason `key_changed`.
- Kicks and rotations whose fence write failed throw `E2EEError("storage")`. The kick is retryable as before (`kickIncomplete`), and a rotation accepts the same new PIN again.
- Behaviour change for round 2's "a failed grant revocation …" test: after a failed grant-table write, `grants.lookup()` now already refuses the grant, through the tombstone.

Not covered: the fence makes revocation durable against a sender restart on the same storage. It can't help if that storage itself is lost or rolled back, which §14 Q6 already notes for the epoch counter.


## 18. Host-control additions

These are pre-release format changes implementing HOST-CONTROL §6.4 and CONTRACT §8.4. The admission transport is the ordered signaling WebSocket, with one relay connection id passed as `peerId`. `peerClosed(peer)` ends unfinished handshakes and marks admitted sessions disconnected; it does not revoke their grants. On publisher reconnect the application calls it for every known peer missing from `e2ee.peers`. The application handles `sender_back` instance changes and jittered grant re-admission (CONTRACT §8.3); those transport messages are not library binary messages.

### Pending approval

`SenderHost.create` accepts `knock` (default false), `knockTimeoutMs` (default 120000), `maxKnocks` (default 50, at most 50), and `locked` (default false, applied before the first receive). Knock mode is immutable on a running host. Persist it in the page recovery record and OR it with each authenticated init policy. Grant re-admissions bypass knock, including on locked or frozen streams; reconciliation, incomplete rotations and non-durable revocations block every admission.

After verifying `Tb`, a PIN/link-secret handshake settles its guess reservation as success, then enters pending approval without issuing a grant, registering a session or releasing an epoch. The sender mints a random 16-byte b64url `knockId`, bound to that exact handshake object, and reserves a row code. `knocks()` returns copies of `{knockId, peerId, rowCode, at, expiresAt}`. Event `knock` follows delivery of PENDING. At most one PENDING is encrypted per handshake:

```
pending_key = HKDF-SHA-512(ISK, salt=nV || nS, info="PassStream v1 pending", 32)
plaintext   = u16(code_index) || u32(timeout_ms)                         // 6 bytes
box         = AES-256-GCM(pending_key, 0^12, "PassStream v1 pending", plaintext)
message     = 0x01 || 0x08 || 0x16 || box                               // 25 bytes
```

The 22-byte box uses a key distinct from the ADMIT and rekey keys. A valid PENDING moves `ViewerAdmission.state` from `response_sent` to `pending`; `.pending` exposes `{rowCode, timeoutMs, expiresAt}`. `ViewerAdmissionOptions.now` injects the clock. Call the viewer's `tick()` while waiting: it returns `ViewerTickStatus` (`{state, reason?}`), never throws, and fails closed at `timeout_ms + 5000`, even if the sender disappears. The tick that expires the wait returns `{state: "failed", reason: "knock_timeout"}` and wipes handshake secrets. `handle()` still rejects late messages. Receiving a duplicate PENDING fails the handshake. Signed REJECT and ADMIT are accepted from pending, with the usual sender authentication and key confirmation. The ADMIT code must match the PENDING code.

`approve(knockId, {rowCode})` requires three canonical alphabet characters and byte-for-byte equality. Invalid strings get `invalid_argument`; a different canonical code gets `code_mismatch`, leaving the knock pending. An ended or unknown id gets `unknown_knock`. A valid approval consumes the id once and runs the existing serialized admission commit. It rechecks the current handshake, abort, lock, freeze, PIN generation, version/reconciliation gate and revocation immediately before registration. Commit refusals map to CONTRACT §14: an expired handshake throws `unknown_knock`; lock, freeze and other admission-gate refusals throw `conflict`. Both carry `details.reason` and send the corresponding signed REJECT. The id is consumed immediately, but its code stays reserved until the approval finishes, including refused commits. Storage failures send a signed denied rejection and fail the approval; they never leave a live pending id.

`deny(knockId)`, timeout, peer close, replacement HELLO, PIN rotation and `close()` terminate knocks, send signed `REJECT denied` where a peer is available, and permanently invalidate their ids. `knockEnded` reports `{knockId, reason}`, where reason is `denied`, `timeout`, `peer_closed`, `replaced`, `pin_rotated`, `stopped`, `commit_failed` or `queue_full`; malformed or out-of-order traffic on a pending handshake ends it with `protocol_error` and signed `REJECT invalid`. A verified handshake beyond the queue limit is denied with a 30000 ms retry delay; its unused random id is reported only by the end event. Successful approval produces `admitted`, not a knock-ended event. `close()` is terminal and holds encryption suspended.

### Grant identity and row codes

Each issued grant now persists a random 16-byte `viewerId` and its canonical `rowCode`. Every `ViewerInfo` for that grant carries the same b64url `viewerId` and code across connections and sender reloads. Owner actions still target `admissionId`, never `viewerId` or relay metadata. Kicking any admission tombstones the whole grant, so that viewer id cannot return with that grant. A new PIN admission creates a new grant and viewer id.

The alphabet is `ACDEFHJKMNPRTUVWXY3479`. `rowCodeIndex` and `rowCodeFromIndex` map its three-character strings to `484*i0 + 22*i1 + i2`, in 0–10647. `rowCodeColour(code)` returns `COLOURS[index % 8]`, with colours `red, orange, yellow, green, teal, blue, purple, pink`; colour is never encoded or compared. The sender rejection-samples uniformly among unused codes. Pending reservations and persisted live grants participate in uniqueness; connected records also reserve their code after a PIN rotation, so two rows still watching never share one. This is deliberately stricter than uniqueness among live grants alone.

The ADMIT plaintext appends `u16(code_index)` after its previous 95 bytes. Plaintext is exactly 97 bytes, its box exactly 113, and its whole framed message exactly 181. Indices above 10647 in either encrypted message are `malformed`. `ViewerAdmission.result.rowCode` is canonical text; neither colour nor viewer id is added to the wire. Pre-release grant records missing the new identity/code fields are refused rather than assigned an unauthenticated replacement identity.

### Versioned rotation fence

`rotatePin(pin, {version})`, `kickAndRotatePin(id, pin, {version})` and `unlock({newPin, version})` persist `pin_version` and `rotation_complete: false` in the **same atomic fence update** as the grant-generation bump. Cleanup removes revoked grants, resets the budget, then marks that same version complete. Repeating the pending value and version finishes the operation without bumping the generation twice. A different request while it is pending gets `rotation_pending`. An unchanged idle PIN after NFC, including null → null, and invalid versions get terminal `invalid_argument` before any write. Legacy calls omitting a rotation version allocate the next version; new integrations always pass it.

`SenderHost.inspect(storage, streamId)` reads `{pinVersion, rotationComplete, frozen}` without changing reservations or storage. Missing fence state is version 0, complete; a corrupt fence reports version -1, incomplete. `create({pinVersion})` defaults to version **0**, never silently adopts a stored newer version, and refuses all admissions with signed `locked` if its version disagrees or the fence is incomplete. `setReconciling(true)` supplies the independent page-intent gate. `enterStateMismatch()` preserves the gate and lock and permits only a fresh higher-version rotation to repair the fence. The page still owns and persists the matching policy and PIN; a fence version alone cannot validate a caller-supplied PIN.

Storage rejections and timeouts are normalized to `E2EEError("storage")`. The original write-ahead revocation retry and encryption hold remain in force. A complete fence alone never authorizes the page to use an old PIN.

### Page recovery and latches

`HostRecovery` is an optional exported coordinator for the page-owned portion of HOST-CONTROL §5.3–5.6, keeping it testable without changing the web application. It preserves unrelated fields in `ps:stream:<streamId>` and owns `pin_state`, `pin_mismatch`, `pending_rotation`, `knock` and `latches`. Corrupt page data is persisted as `pin_state: null, pin_mismatch: true`, never as a fabricated no-PIN policy. The marker survives every reload and only a successful fresh rotation repairs it. Pass the same origin `AtomicStorage`, the stream id, initial PIN/policy only on first launch, authenticated `init` latches/knock, and the remaining host options. `.host`, `.record`, `.state`, `.reconciling` and `.knockMismatch` expose the running result. `onStateChange(status)` and `on("stateChanged", listener)` report `{state, reconciling, knockMismatch}` after the corresponding gates are applied; the options callback also receives the initial state. Event subscriptions return an unsubscribe function. Call **its** `tick()` for retries and host maintenance.

Creation reads the page record and library inspection, OR-merges all latches (including the durable budget freeze) and knock, persists them, and creates the host with the resulting gates before any admission. Its reconciliation follows this table:

| Page | Fence | Recovery |
|---|---|---|
| No intent, page version v | v, complete | Use page PIN/policy. |
| Intent v+1 | v, complete | Start with old PIN, gated; apply pending value/version. |
| Intent v+1 | v+1, incomplete | Start with pending PIN, gated; finish cleanup. |
| Intent v+1 | v+1, complete | Persist page PIN/policy and clear intent; do not reset budget again. |
| Any disagreement/corruption | Any | Latch locked/frozen, keep reconciling, report mismatch. |

`rotate({policy, value?, op?, admissionId?})` validates before writing. Digit policies (length 4–12) take no value: secure rejection sampling generates a different zero-padded PIN. `none` uses null. Passphrases are supplied locally, NFC-normalized and 8–256 bytes; omission is `passphrase_required`. Policy/value mismatch and unchanged values are terminal `invalid_argument`. A valid request persists its intent before calling the versioned library operation. `op` is `rotate` (default), `unlock` or `kick`. After a crash, a pending kick with no in-memory admission completes via generation rotation, which revokes its grant as well as all other old grants.

States follow CONTRACT §5.4: idle → intent_persisted → applying → committing → idle; storage moves to retrying, retaining the exact value/version with 100 ms exponential backoff capped at 2 s. The initial failure replies `storage` with `details.pending: true` (accepted; callers must not resend). After any library error, inspect the fence again before classifying it. A fence at the pending version retains the intent: retry incomplete library work or finalize completed work, including checking in-memory completion. Only an untouched fence permits a terminal first-call refusal to clear the intent; a non-storage refusal on retry with an untouched fence enters mismatch. A fence that disagrees with both the inspected baseline and the intent also enters mismatch. Ordinary new requests while pending get retryable `rotation_pending`. A fresh local request in mismatch uses `max(page version, library version) + 1`; the intent carries an internal `repair` marker so a crash during a repair that skips versions can still reconcile safely. Lock/freeze latches remain until explicitly cleared. Page writes carry internal generation/revision guards: timed-out older writes cannot replace newer state or a reloaded writer's record.

`setLatch(name, true)` applies synchronously before its awaited write. Write failures leave it applied and return `storage {applied: true}`; `tick()` retries. Clearing persists before application and is allowed only while idle. On failure, restore only the field being cleared and re-apply current latches, preserving any concurrent pause, freeze or mismatch. `setLatch("frozen", false)` is invalid: callers must use `unfreeze`. `mergeInit` OR-merges latches and knock; a reconnect cannot clear anything. If init adds knock to an already-running non-knock host, it latches locked, calls `onMismatch` and sets the separate `knockMismatch` flag. It does not enter PIN mismatch, freeze or reconciliation: current viewers' grant re-admissions continue, including after an in-flight rotation finishes. Unlocking and new rotations require a reload to create the host with the new immutable knock policy. `unfreeze({policy, value?})` rotates then clears frozen durably; `unfreeze({keepPin: true})` is the explicit no-rotation override and cannot bypass reconciliation. Lock and pause are preserved.

The coordinator's `onLatches` hook applies track policy. The application must detach **all audio and video RTP tracks** and disable capture on pause, and attach only on local resume when the persisted pause latch is clear. Key recovery never changes pause. Daemon mirror fsync/acks, local command authority, relay envelopes and capture are application responsibilities; this library does not authenticate a local command channel or send daemon acknowledgements.

### Verification and compatibility decisions

The host-control regression suites cover all knock endings, stale/mismatched approvals, approval-versus-peer-close and lock/freeze/reconciliation races, exact encrypted sizes and invalid indices, stable identities and kicked-grant refusal, all startup reconciliation rows, each persisted crash point for rotate/unlock/kick, accepted storage failures and terminal failures, retry-to-mismatch and repair, late writes, and latch restoration/clearing. Row-code tests exercise 10,000 persisted grant admissions in bounded live batches and a deterministic collision interleaving between a queued knock and an approval commit with 10,646 live grants. Existing CPace/SFrame vectors and review-round regressions remain in the normal Vitest run.

Initial implementation verification: `mise exec -- npm --prefix e2ee run typecheck` and `mise exec -- npm --prefix e2ee test` passed: **197 tests in 15 files** (the original 133 plus 64 additions). No browser, app integration or torture workload was run in this checkout.

Safer resolutions of underspecified edges: use the normative colour formula (`K7P` is purple, index 3838 mod 8 = 6); retain codes of connected admissions after their grant is revoked; refuse pre-release grants missing identity fields; preserve latches after repairing mismatch; require reload when a running host's immutable knock policy disagrees with init; consume an approval id immediately and reject a commit aborted by peer close. One earlier review regression now expects `locked` during incomplete rotation, then verifies `grant_invalid` after cleanup, matching the new mandatory all-admission gate. The test restart helper now passes the inspected version explicitly.


## Host-control additions: review round 1 fixes

- Failed latch clears restore only the cleared field, then re-apply all current latches. Concurrent pause, freeze and PIN-mismatch latches survive in memory and storage.
- Approvals consume knock ids once and keep a separate code reservation through commit success or refusal. A deterministic port of the review's queued-knock reproduction asserts uniqueness near code-space exhaustion and release after refusal; a separate test holds an approval commit in flight.
- Corrupt page records persist an explicit mismatch marker and null PIN state. Repeated reloads cannot fabricate no-PIN access, and crash recovery during a fresh repair rotation is covered.
- Library errors trigger another fence inspection. An applied rotation is retried or finalized with the same PIN/version; it cannot clear the intent and show the old PIN. Tests cover incomplete and complete fences, including completion written before an error prevents in-memory completion.
- Generic frozen clears are rejected; `unfreeze` owns rotation or the explicit keep-PIN override. Every latch clear requires idle state. A knock policy discrepancy locks only new admissions and alerts through `onMismatch`, retaining a distinct flag and current viewers' grant reconnects.
- Knock preparation returns a typed success/refusal with its signed rejection reason: blocked/locked → locked, stale PIN handshake → expired, queue full → denied with 30 s retry. Pending protocol errors end as protocol_error with signed invalid, rather than commit_failed.
- App APIs now use CONTRACT §14 approval error codes, a rotation options object, recovery state notifications and non-throwing viewer ticks.
- The fence assertion now inspects the actual advanced fence; the allocation test races real knock/approve/commit operations with over 10,000 live grant codes; the restart test creates a new host from copied storage. The review's latch and corrupt-record reproductions are assertion-based regressions. Colour examples and a documentation consistency test use K7P · purple.

Verification: `mise exec -- npm --prefix e2ee run typecheck` passed. Three consecutive runs of `nice -n 10 mise exec -- npm --prefix e2ee test -- --maxWorkers=1` each passed **217 tests in 15 files** (651 passing executions total; no observed flakes). Observed suite durations: 12.20 s, 12.00 s and 12.64 s. `git diff --check` passed. Browser/app integration and the independently running torture workloads are outside this fix round.

## 19. App integration (phase 2c)

How the web app uses this library. The code is `web/src/sender/` (one sender, mounted at `/sender` behind the local daemon and at `/host` as the server-served browser sender) and `web/src/viewer/` (the viewer at `/v`). Tests: `test/e2e/e2ee.mjs` (real browsers) and the torture suite.

**Sender.**

- `assertE2EESupported()` runs first: the sender fails closed too.
- Storage is IndexedDB (`web/src/sender/idb-storage.ts`): one readwrite transaction per `update`, strict durability, so it is atomic across tabs of the origin. It holds the page record `ps:stream:<id>` (identity seed, link secret, view capability, `pin_state`, `pending_rotation`, `knock`, `latches`) and the library's budget, grants, fence and epoch counter. A first launch writes the whole record in one update, so `HostRecovery.create` finds an agreeing record and nothing is reported before it is durable.
- The page holds the Web Lock `ps:sender:<stream_id>` for its lifetime; a second tab refuses to start.
- **Video and audio.** Native video is SFrame track 0, audio track 1, and the independent 320×180 encoding track 2 (KID = epoch×16+2, counter starts at 0), all on one worker and one `SenderKeyRing`, so every epoch, kick cutoff, suspend and encryptor replacement covers both. Each transceiver is created without a track, the encrypt transform attached, then the track set, all before negotiation. Video codec preferences are VP9 profile 0 only; a browser without it fails closed with an explicit unsupported-codec error. Audio is the capture's tab or system audio when `getDisplayMedia` provides it (`audio: true, systemAudio: "include"`), Opus only, fully encrypted (no clear prefix). A capture without audio (the usual Linux screen share) negotiates no audio m-line on the publisher PC; if a later recapture brings audio, its transceiver is added (transform first) and the PC renegotiated, and a transform that can't attach replaces the PC. A recapture while paused disables the audio track before attaching it. Audio RED is refused: Chrome splits RED after the receive transform, so the decryptor would get a RED container. A second audio track in the capture is dropped.
- **The SFU's audio path is opaque** (`internal/sfu/audio.go`): it negotiates Opus, never parses an audio payload, and only rewrites RTP sequence numbers and a timestamp offset across publisher changes. Per viewer, a 50-packet queue (oldest dropped first) feeds a pacer lane served ahead of video from the same wire budget; audio has its own wire counter and cannot establish video received proof or trial capacity; packets older than 200 ms are dropped, and video layer admission subtracts the measured audio rate. Viewers add `a=rtcp-fb:<opus> nack` to their offer (Chrome supports audio NACK but doesn't offer it) and the SFU answers from a 128-packet cache, each packet at most twice, at least 20 ms apart and never once it is older than 200 ms. Audio goes ahead of video only up to 1.5× the publisher's measured audio rate (64 kbit/s before it is measured) and never above half the viewer's budget; beyond that it waits behind video and ages out, so an audio flood or NACK storm can't starve video. RTCP is filtered by SSRC on both senders: video adaptation reads only the video report block, NACKs and PLIs.
- **A new publisher PC needs a new worker.** A replaced PC creates new RTCRtpSenders, and `SenderKeyRing` refuses a second encryptor per track (§7.1). When a track is attached a second time, the page terminates the worker, attaches a fresh one (native video, thumbnail, then audio) and calls `host.replaceEncryptor()`, so the new transforms get a fresh epoch within 300 ms and drop frames until then.
- **The kick cutoff is enforced in the worker too.** `SenderKeyManager` calls `onCutoff({ within, stale })` synchronously when a revocation begins (`beginRevocation`, with `stale` = every epoch allocated so far) and when `kick()` removes the sessions. The page posts it (`postSenderCutoff`) with an absolute monotonic deadline (`performance.timeOrigin + performance.now()`). The worker takes the earlier of that and receipt + `within`, checks it before and after every encryption, and past it without an epoch newer than `stale` suspends the ring by itself, drops frames (`cutoff`), and refuses a re-post of a stale epoch. A stalled UI thread (no `tick()`, no acks) can no longer stretch the 300 ms: measured in `kick_cutoff_cryptographic_ui_stall`, an 850 ms stall, no frame or audio captured after the cutoff decrypts at the kicked viewer.
- **Pause** (HOST-CONTROL §5.6 and PLAN "Viewer UX" together): the capture's video track is disabled synchronously and replaced on the wire by a sender-drawn "Paused" canvas, encrypted like any frame; the capture's audio track is disabled, so Chrome sends encoded silence (about 9 kbit/s with SFrame). Audio is not detached with `replaceTrack(null)`: when Chrome restarted a stopped audio sender (same SSRC), the SFU received none of its packets again (measured; likely an SRTP index jump), so audio would never come back after resume. Neither carries capture content. The capture is enabled again only by a local resume with the latch cleared. **Stop** sends an "Ended" canvas (and no audio) for about 800 ms before leaving. Both pictures carry a 64-cell marker row (a 32-bit magic, a kind and its complement) under the clock row, so the viewer can show the paused or ended state; the marker travels inside E2EE.
- **Command results** from the daemon and the server are cached by id together with a SHA-256 fingerprint of (op, canonical args) (`web/src/sender/command-cache.ts`). A known id with another op or other arguments is refused with `conflict`, never answered from the cache; an execution still running is shared and is never evicted (only settled results are, oldest first, beyond 256 ids; with 256 commands all still running a new id gets `rate_limited`, so a replayed id can't run twice); `storage`, `internal` and the retryable `rotation_pending` are dropped once settled so a retry runs again. The daemon's ids carry a random incarnation prefix (`d<incarnation>.<n>`), so a restarted daemon never reuses one.
- **Abandoned records.** A stream's IndexedDB state is deleted when it ends. A browser-sender tab closed without ending leaves it behind, so every sender start also deletes other streams' records unseen for 7 days (`ps:seen:<id>`, refreshed every 10 minutes while a sender runs).

**Viewer.**

- Without `RTCRtpScriptTransform` (or WebCrypto, or WebRTC) the viewer shows "browser not supported" and opens no socket.
- It joins signaling first and creates the PC only once admission starts (a PIN was typed, a grant exists, or the stream has no PIN). Frames stay encrypted until the keys arrive; non-admitted viewers don't download the stream.
- `viewerSid` and the grant live in `sessionStorage` (`ps:viewer:<stream_id>`): a reload re-admits with the grant, a new tab is a new session.
- **Every signaling connection is a new relay peer**, so the viewer re-admits with its grant after each new `joined` (its own reconnect, a PC replacement after a stall, a server restart). The decrypt worker and its keys survive PC replacement. It also re-admits after `sender_back` with a new instance (0–2 s jitter) and after more than 2 s of `unknown_key` drops.
- The typed PIN is kept only until the signed CHALLENGE is answered, so a reconnect before that resumes the same attempt; afterwards only the library's derived keys remain.
- HELLO and RESPONSE each have a 5 s progress deadline. An unanswered PIN HELLO retains its typed PIN and Waiting for host while the sender is known offline; sender_back starts a fresh check and phase clock. Once CHALLENGE has been answered, the PIN is gone and RESPONSE timeout remains bounded even if the sender leaves. Other timeouts drop the unfinished admission and clear busy; a PIN check returns to its input with an error, without replaying the PIN. Grant/no-PIN checks use at most five automatic retries, then show a local `admission_timeout` card with a manual reload instruction. Unauthenticated loss does not delete the stored grant. A signed `grant_invalid` still forgets it and either removes an admitted viewer or returns a fresh page to the PIN screen. The signed PENDING approval deadline and admitted-viewer missing-key recovery are separate. `ViewerAdmission.abort()` wipes unfinished check inputs/keys and fences late asynchronous results.
- REJECT handling: `wrong_pin` shows tries left; `backoff` shows the lockout; `frozen` and `denied` end on a card; `locked` is retried for an admitted viewer (reconciliation also answers `locked`) and is final for a new one; `grant_invalid` on an admitted page means kicked or rotated: the worker is terminated (the picture goes away) and the card says so. A reload then asks for the PIN.
- A stream that froze at 20 failures holds its host locked, so even the right PIN is answered with a signed `locked`.
- **Disposal.** Every terminal state (kicked, refused for good, stream ended, signaling refused) goes through one `dispose()`: the worker is told to drop its epochs and terminated, the channel keys are dropped (`ViewerSession.dispose()`, later rekeys refused), timers stop, and a revoked grant is removed from `sessionStorage`. Epoch secrets are wiped on the main thread right after they are posted, and the grant secret right after it is stored. A replaced PC's finished pipes leave the worker's lists.
- **Audio.** The viewer adds a recvonly Opus transceiver with the decrypt transform (track 1) before negotiation; audio shares the video's MediaStream, so the player's unmute plays it. The worker reports decrypted frames per track (at most every 200 ms); "Tap to unmute" shows only while audio decrypted in the last 1.5 s and the stream isn't paused.

### A0 merge: signaling continuity

Viewer signaling pings may carry `freshFrames` and `at` (client sample time in Unix milliseconds). They call the same `ReportDecoded` counter as SCTP without renewing transport liveness; publisher samples are ignored. Pongs include `refreshMs = RefreshTransitMS()`, a queue advisory bounded to 6000 ms (zero when idle or publishing), which does not extend the existing serialization envelope. Replacing a PC inherits that admitted viewer's applied path and native-key budget. Rekey calls the worker keyframe hook on **both** independent video encoders.

## 20. Annotations (laser pointer and strokes)

Design and app format: [FEATURES.md](FEATURES.md) "Track D2". Code: `e2ee/src/annotation.ts`; tests: `e2ee/test/annotation.test.ts`.

```
session_annot  = HKDF-SHA-512(ISK, salt=sid, info="PassStream v1 annotation session", 32)        // imported as a non-extractable HKDF key
epoch_annot(e) = HKDF-SHA-256(epoch_secret_e, salt="", info="PassStream v1 annotation epoch" || u32(e), 32)
key(dir, e)    = HKDF-SHA-256(ikm=session_annot, salt=epoch_annot(e), info="PassStream v1 annotation " || dir || u32(e))   dir = "v2s" | "s2v"; AES-256-GCM
nonce          = u32(e) || u64(ctr)
aad            = lv_cat("PassStream v1 annotation", dir, u32(e), u64(ctr))
ANNOTATION  (V→S) box = AES-256-GCM(key("v2s", e), nonce, aad, body)                       body 1–512 bytes, no origin field
ANNOTATIONS (S→V) box = AES-256-GCM(key("s2v", e), nonce, aad, count:u8 || (origin:u16 || len:u16 || body)*)   1–64 records, ≤ 2048 bytes; origin = row-code index or 0xFFFF (host)
```

- **Row authentication.** Only the viewer and the sender hold `session_annot`. The sender attributes an upstream message to the admission whose key opened it and stamps that admission's row code downstream. Each downstream batch is sealed separately per recipient. A viewer can't forge another row's or the host's annotations, and a relay that relabels peers only causes decryption failures.
- **Epoch binding.**
  - The sender keeps `epoch_annot` for the 4 newest *allocated* epochs (`SenderKeyManager`). It can authenticate upstream messages under those keys subject to the monotonic accepted-epoch rule below, and seals downstream under the active epoch.
  - The viewer derives `epoch_annot` for each epoch it receives (admit bundle, REKEY), keeps the newest 4, and seals under its newest.
- **Authenticated ordering.** Each annotation receive direction keeps the highest successfully authenticated accepted epoch. Reject any lower epoch, including delayed first deliveries under retained keys. Within the highest epoch, `ctr` must exceed the last accepted counter. Check epoch and counter before key/decrypt awaits and synchronously again after AEAD; commit them only after successful authentication. An invalid future-epoch box cannot poison valid current/future input, and an older decrypt held across newer accepted output cannot return plaintext. Disposal fences pending cryptographic output. These rules apply in both directions, independently of SFrame media old-key buffering.
- **Counters.** `ctr` starts at 1 per (session, epoch, direction). Seals are chained per session, so messages leave in counter order. Sending epochs cannot decrease; counters cannot wrap or be reset by key-cache pruning. New CPace/grant admissions derive fresh pairwise keys and start independent counters.
- **Kick.** `SenderKeyManager.kick()` drops the session's annotation keys synchronously with its channel keys. `SenderHost.receive` refuses an annotation from a peer without a connected admission, and `sendAnnotations` returns false for one. The fast rekey then moves everyone else to a new epoch.
- **Separation from SFrame.** Different labels, a different hash for the epoch step, and keys that never touch media. SFrame keys come from `"PassStream v1 SFrame base_key"` per KID. (KID, CTR) and annotation (key, nonce) pairs can't coincide, because the keys differ.
- **API.**
  - `ViewerSession.sealAnnotation(body)`, `.openAnnotations(bytes)` and `.annotationEpoch`.
  - `SenderHost.sendAnnotations(admissionId, records, current?)` (optional synchronous lifetime guard rechecked after sealing) and the event `annotation {admissionId, viewerId, rowCode, epoch, body}`.
  - Codec: `encodeAnnotationRecords` / `decodeAnnotationRecords`. Rate limits and the drawing policy are app policy (`web/src/annotations/`).
- **What the relay sees.** Message sizes and timing: when someone draws, and roughly how much.

### 20.1 App causal ordering

All integers are big-endian. Primitive bodies are listed in FEATURES §3. Only host ink uses bare downstream laser/stroke primitives; viewer ink is host-stamped INK. The encrypted app formats (including the kind byte) are:

```
INPUT (6): session:u64 || op:u32 || clearThrough:u32 || gen:u32 || barrier:u32 || namespace:u64 || primitive
RECEIPT (7): session:u64 || through:u32 || barrier:u32
BOOTSTRAP (8): namespace:u64 || source:u64 || gen:u32 || barrier:u32
INK (9): source:u64 || op:u32 || laser-or-stroke
SOURCES (10): n:u8 || n * (source:u64 || origin:u16 || through:u32)
```

- Every batch leads with host-origin BOOTSTRAP (25-byte body / 29-byte record), authenticated by the current admission's pairwise channel. It supplies the random nonzero router-lifetime namespace, recipient source and current barriers before any permissive policy. Input is suspended during re-admission. A successful authenticated admission permits one namespace adoption; a changed namespace resets prior global/origin/source ordering and live-only ink, while a same-host PC replacement retains barriers and accepted ink. Once adopted, a delayed different-namespace batch is refused. The controller fences pending opens/seals against channel identity after awaits; stopped host routers invalidate their pending seals through the optional `sendAnnotations` guard. Upstream namespace must exactly match, so a delayed prior-router input cannot exploit reset counters.
- `session` is a random nonzero viewer-input incarnation, bound once per authenticated admission. Changing it on that admission is refused. `op` is nonzero and increasing, except segments of one pen operation reuse it. `clearThrough <= op`; own-clear requires equality and primitive `gen=0`. INPUT can contain only laser/stroke/own-clear; no nesting or host-only primitives. Maximum INPUT is 294 bytes (33-byte header + 261-byte stroke).
- Row codes identify display rows, not connections. Concurrent admissions sharing a grant/row remain distinct and may all draw. The host assigns each admission a unique live `source = incarnation:u32 << 16 | slot:u16`, with incarnation 1..2^32−1 and slot 0..499. A fixed 500-slot table holds source, admission ownership, bound input session, highest observed operation and cutoff. New admissions fill unused slots first, then reuse retired slots with an incremented incarnation. A packet never changes ownership. Reuse purges old-source ink; it cannot evict a live admission. Old admission/channel output is fenced independently by crypto routing.
- Every viewer drawing record is INK (maximum 274 bytes), carrying host-authenticated source and input operation within the stamped display row. Storage identifies strokes by `(source, stroke ID)`, preventing concurrent 16-bit ID collisions. Fan-out filters the actual source admission only. Local echo and remote ink share the 32-stroke / 256-laser-point row bounds.
- `clearOwn` immediately clears this admission's local source and old pending input, and raises `clearThrough`. Every subsequent input repeats it. The host accepts only its exact namespace/global-generation/row-barrier stamps and nondecreasing operations, then purges the source cutoff before applying new ink. Other admissions' accepted ink survives. A lost standalone upstream clear repairs on the next input; no retry timer.
- Host-origin SOURCES pages retain source incarnation and cutoff independently of ink. Each entry is 14 bytes; n<=32 gives a <=450-byte body. A fair cursor reserves 16 slots each tick, appending needed ink sources up to 32, with another bounded source record if needed. Every source's current state and row clear generation precede its ink in the same batch. The fixed 500-slot viewer table rejects older incarnations, purges a reused slot's previous source and removes only operations at/below the source cutoff. The recipient-specific RECEIPT (17-byte body / 21-byte record) additionally confirms its matching input session; session=0 requires through=0. New local ink survives earlier confirmations.
- Host clear-all advances global `gen`; forced origin purge (overflow/kick) advances the row barrier and covers all its incarnations' observed operations. Observing a newer barrier erases all older optimistic input, including envelopes the host never saw. Held first deliveries with older stamps are refused. Fresh input uses observed barriers immediately. Ordinary own-clear advances neither host barrier, so subsequent drawing never waits for confirmation. Fixed row-generation and barrier tables each retain 10,649 u32 counters.
- Reserve bootstrap and receipt bytes/records plus fair generation/source repair pages before ink: body<=512, batch<=2048, records<=64, one paced batch per recipient per tick, 160 envelopes/s aggregate long-run with a tick burst. Admission/bootstrap announcements use this same pacing. Both repair cursors complete three wraps after a relevant change (at least two full visits) and repair runs for at least 30 seconds, while enabled or while ink remains. There is no separate acknowledgment/retry timer. Empty host-only streams eventually become silent; outages covering the entire finite repair window may require a new relevant event. Ink delivery itself remains live-only. Retired source ink can remain until slot reuse or a host purge; ordinary own-clear targets the current admission. Operation, incarnation and barrier exhaustion fails closed instead of wrapping.
