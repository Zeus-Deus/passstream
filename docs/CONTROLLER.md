# PassStream media controller (checkpoint C0, revision 3)

This is the specification A1 implements in `internal/sfu/ctrl`, and the model `internal/sfusim` drives. It replaces the round-4/5 `wireController`.

- Sources: the approach review (§3), GPT-6.1-Sol's C0 review (REJECT, 14 items) and the orchestrator's decisions of 2026-11-08.
- Gates live in [GATES.md](GATES.md); responses to the review are at the end of this document.
- Changing a rule or constant needs a checkpoint review.

## Boundaries

- **`internal/sfu/ctrl`** holds the components and the forward loop, as plain structs driven by an explicit `now`.
  - Components: estimator, prober, allocator, pacer, repair policy, keys and dependency continuity.
  - No Pion types, clocks, goroutines or locks.
  - Interfaces are in `components.go`, data types in `types.go`, the loop in `viewer.go`.
  - All media **policy** lives here. Moving policy outside this directory to dodge a budget is prohibited.
- **`internal/sfu`** owns the wire: RTP/RTCP, the packet cache, SRTP, key assembly, writer isolation and locking.
  - It calls `ctrl.Viewer` and `ctrl.Continuity`, and decides nothing on rate, layers, keys or repair itself.
- **`internal/sfusim`** drives the same `ctrl` code on a virtual clock.

### SFrame and E2EE constraints (normative integration requirements)

- **What the SFU may read:** RTP headers, header extensions (TWCC sequence, abs-send-time) and the clear VP9 payload descriptor (RFC 9628). It never parses an Opus TOC or the encrypted VP9 payload.
- **Tracks and encodings:**
  - native (full resolution) is SFrame track 0;
  - Opus is SFrame track 1, opaque;
  - low (about 320 px, ≤100 kbit/s) is SFrame track 2.
- **Receiver side:**
  - the receiver accepts SFrame tracks {0, 2} on its one video receiver, with separate counters and replay windows per track;
  - an acknowledged SFrame epoch switch preserves both codec reference chains and sends no PLI; a receiver that actually missed encrypted frames recovers on low under the room budget. The 300 ms revocation cutoff is unchanged.
- **Codec negotiation fails closed.** A viewer that cannot decode the published codec or lacks `RTCRtpScriptTransform` gets "not supported", never plaintext.
- **One encoding per viewer.** A viewer receives exactly one video encoding at a time. Switching encodings starts at a keyframe of the target encoding.

### Supported VP9 dependency structures (measured)

`web/vp9-structure.mjs` with `test/torture/vp9probe` recorded every forwarded descriptor for 20 s per source. The run used Chromium 156.0.8078.79 and the main binary; results are in `test/torture/results/vp9-structure.json`.

| Source | Mode | Pattern | U bit | References |
|---|---|---|---|---|
| `screen-production` (contentHint text, 4 Mbit/s) | **non-flexible** (0/267 pictures with F) | TID 0,2,1,2 | set on **every** picture | implicit |
| `clock` | non-flexible | 0,2,1,2 | every picture | implicit |
| `screen` (detailed, 2 Mbit/s) | **flexible** (239/239) | 0,2,1,2 | every picture | explicit P_DIFF: TL0 −4, TL1 −2, TL2 −1 |

Both renditions of a source share its mode.

**Rules for each structure:**
1. **Flexible with P_DIFF.** Dependencies are exact. A picture is forwarded only if every reference was delivered (the existing `pictureHistory`).
2. **Non-flexible L1T3, pattern 0,2,1,2, one spatial layer.** U carries no information, because it is always set.
   - **TL1 loss:** forward only TL0 until the next retained TL0 or keyframe. U on a TL1 picture does not repair that layer.
   - **TL2 loss:** TL2 is dropped freely. This rests on libvpx's fixed L1T3 mode never referencing TL2, the current `dependency.go` claim. A1 adds a Chromium decode regression: drop every TL2 picture for 30 s, and every remaining picture must decode.
   - **TL0 loss:** nothing more of that rendition until a keyframe.
3. **Anything else is rejected explicitly.** That covers spatial layers on a rendition, TID > 2, flexible mode without references, and a non-flexible pattern other than 0,2,1,2.
   - The rendition is marked `Unsupported` and refused, including whole-rendition forwarding. Other supported rendition or `Untenable` is used. Flexible intra keys are valid without inter-picture references.

`ctrl.Continuity` implements these rules, and both the SFU and sfusim call it.

## Signals

| Signal | Feeds | Never feeds |
|---|---|---|
| TWCC (send time, arrival, size, lost), current path generation only | estimator, prober | — |
| RTCP RR (RTT, fraction lost) | estimator (RTT; loss fallback) | — |
| Selected ICE pair change, PC replacement | generation fence (all components) | — |
| Ingress per-rendition, per-TID wire rates; assembled key sizes | allocator costs, key admission | — |
| Ingress audio wire rate (Opus + RED + headers; own counter) | `Costs.AudioWire`, added to every admission total | video costs, padding budget |
| Pacer drops; key aborts; SFU-detected broken base (`OnBroken`) | continuity, keys (`CauseBroken`) | rate |
| Viewer decoder signals (stall heartbeats, PLI) | keys (`CauseRecovery`) | **R, probing** |

**Decoder signals and recovery**
- Decoder reports never change R and never start or score a probe.
- A recovery request that the key policy **accepts** may change allocation: the viewer is forced to low until `RecoveryQuiet` passes. That is the only path from a decoder signal to allocation.

## Constants

All tunables are in `ctrl/constants.go`. Each has exactly one meaning and is never reused under another name.

- **Budget: 48.** Responses §R-15 explains the r2 budget of 40; the C2 checkpoint (2026-11-08) approved eight A1 additions as separate meanings (see "A1 implementation notes"). Any growth needs a sfusim scenario and a checkpoint.
- `TestConstantBudget` counts every untyped and typed constant except the enumerations (`Rendition`, `Class`, `Kind`, `Cause`, `KeyStatus`) and `Stub`.

| Group | Name | Value | Single use |
|---|---|---|---|
| Estimator | RateMin | 128 kbit/s | estimate floor |
| | RateInit | 300 kbit/s | estimate before evidence; app-limited floor |
| | IncreasePerSecond | 1.08 | normal-state growth |
| | Backoff | 0.85 | congestion decrease factor |
| | OveruseSlope | 12.5 ms/s | fixed delay-slope threshold |
| | OveruseTime | 100 ms | overuse persistence |
| | TrendWindow | 1 s | slope regression window |
| | AckedWindow | 500 ms | acked-rate window |
| | AppLimitedLookback | 2 s | lookback for the app-limited cap |
| | AppLimitedFactor | 1.5 | app-limited cap multiplier |
| | CongestiveLoss | 10% | loss level that, with delay evidence, is congestion |
| | LossWindow | 1 s | loss measurement window; one congestive cut per window |
| | PathWindow | 10 s | path minimum window |
| | ProbeEvidenceTTL | 30 s | validity of a successful probe as evidence |
| Prober | ProbeLength | 100 ms | maximum cluster duration |
| | ProbeMinPackets | 5 | minimum acked packets for a measurement |
| | ProbeTimeout | 1 s | result finalisation after cluster end |
| | ProbeBackoffMin | 2 s | first failure backoff |
| | ProbeBackoffMax | 30 s | backoff ceiling |
| | PaddingShare | 10% | padding ÷ video wire bytes |
| | PaddingWindow | 30 s | padding accounting window |
| | BootstrapPadding | 48 KB | once-per-generation padding credit, reported |
| Allocator | UpMargin | 1.15 | upgrade margin on steady video need |
| | DownTime | 500 ms | downswitch persistence |
| | TemporalUpTime | 1 s | temporal-upgrade persistence |
| | RenditionDwell | 2 s | minimum between applied rendition changes |
| | KeyEnvelope | 2 s | key delivery envelope |
| Pacer | PacerBurst | 10 ms | burst depth at R |
| | BurstFloor | 1,260 B | minimum burst (one MTU-size RTP packet) |
| | FrameAge | 100 ms | delta-frame queue age limit |
| | QueueBytes | 1 MiB | hard per-viewer byte bound |
| | QueueFrames | 64 | hard per-viewer frame bound |
| Repair | NACKMaxSends | 2 | retransmissions per sequence |
| | FrameDeadline | 1 s | useful deadline after ingress (freshness bound) |
| | FECLossOn | 1% | video FEC threshold |
| | FECRatio | 0.25 | base parity ratio |
| | FECBurstRun | 3 packets | mean loss run at which FEC doubles and interleaves |
| Keys | KeyInterval | 3 s | room-wide gap between viewer-driven keys, per rendition |
| | KeyRetry | 1 s | re-send an unanswered PLI |
| | RecoveryQuiet | 2 s | recovery request debounce and upgrade quiet time |

Configuration, not constants: maximum rate (default 50 Mbit/s) and `Config.ProbeMaxOverdrive` (default 1.5). These defaults live in constants.go; the overdrive is a separate configuration value, not an alias of AppLimitedFactor. C2 reviews both configuration knobs as well as the 40 constants.

## Definitions used by several components

- **Generation.** Every `Sent` carries the path generation. Feedback, probe results and evidence from an older generation are discarded.
- **Video need.** need(o) = ingress wire rate of option o (cumulative over its TIDs, EWMA over `TrendWindow`, headers included) + FEC on protected bytes only (TL0 and key share × FEC ratio) + expected RTX (loss × rate).
- **Key wire bits.** keyWire(r) = assembled key wire bytes × 8 × (1 + FEC ratio). If no key of r has been assembled yet, the size is **unknown**.
- **Admission totals:**

  ```
  hold(o)  = audioWire + need(o)
  admit(o) = audioWire + max(UpMargin × need(o), keyWire(r) / KeyEnvelope)
  ```

  - Keeping an applied option needs hold(o) ≤ R.
  - Moving to a better option needs admit(o) ≤ min(R, Proven).
  - Proven is the current generation's evidence: the larger of its maximum `AckedWindow` acked rate over `AppLimitedLookback` and the delivered unique rate of its last delay-clean probe (including a target failure) within `ProbeEvidenceTTL`.
  - Probe success uses the **same** admit(o).

## Components

### Estimator (`bwe.go`, about 250 lines)

**Delay**
- excess = (arrival − send) − path minimum. The minimum is windowed over `PathWindow`, as in the existing `pathMinimum`.
- m (ms/s) is the least-squares slope of excess against local time over `TrendWindow` (the existing `delayTrend`). It is valid only with at least 3 samples spanning more than half the window.

**Overuse**
- Overuse is m > `OveruseSlope` continuously for `OveruseTime`.
- The threshold is **fixed**. Adaptation returns only with reviewed traces showing it cannot erase the signal (responses §R-1).

**Decrease**
- On entering overuse, and again each time a hold expires while overuse persists: R = max(RateMin, min(R, Backoff × acked)), then hold.
  - acked is the acked-byte rate of all classes over `AckedWindow`, by arrival time.
  - The hold lasts one RTT, or `AckedWindow` while RTT is unknown.
- `min(R, …)` means a congestion response never raises R.

**Underuse:** m < −`OveruseSlope` → hold.

**Increase:** in the normal state and outside a hold, R ×= IncreasePerSecond^Δt. Loss alone never blocks growth.

**Loss**
- Measured from TWCC over `LossWindow`, excluding padding.
- **Congestive** loss is loss > `CongestiveLoss` plus delay evidence: overuse within the last `TrendWindow`, or current excess ≥ `OveruseTime`.
  - It cuts R = max(RateMin, min(R, Backoff × acked)), at most once per `LossWindow`.
- **Non-congestive** loss (no delay evidence) never changes R. It provisions repair.

**App-limited cap:** R ≤ max(AppLimitedFactor × max acked over `AppLimitedLookback`, last delay-clean current-generation probe result, RateInit).

**Delay-clean probe result, including a target failure:** R = max(R, delivered unique rate); Proven includes that rate. Overuse-contaminated results do not create admission evidence.

**Generation change:** clear delay samples, path minimum, acked history and Proven. Keep R.

**Clamp:** R ∈ [RateMin, Config.MaxRate].

### Prober (`probe.go`, about 120 lines)

**Starts when all of these hold:**
- the allocator reports a candidate with admit(c) > Proven;
- the estimator is not in overuse and has had no congestive loss within `LossWindow`;
- no key is queued for the viewer;
- the backoff has elapsed;
- the padding budget can afford at least `ProbeMinPackets` packets.

Non-congestive loss does not block probing.

**Cluster**
- Offered rate = min(admit(c) / (1 − L̂), Config.ProbeMaxOverdrive × admit(c)); L̂ is the current-generation non-congestive media-loss estimate used by repair. Record when the required overdrive exceeds the cap; that option cannot be proved by this cluster.
- Duration = min(`ProbeLength`, available padding bytes × 8 ÷ (rate − media rate)).
- Every media **and** padding packet sent during the cluster carries its id and generation.

**Result**
- Measured D = delivered UNIQUE bytes by dispersion of acked cluster packets: bytes after the first ÷ (last arrival − first arrival), bounded by the send side of the same packets and the planned rate (C2-2 below). Duplicate TWCC reports never count twice. It needs ≥ `ProbeMinPackets` acked.
- Lost cluster packets are **not** an automatic failure.
- Success iff the cluster is complete (saturated or delivered, C2-2), D ≥ admit(c), the required overdrive does not exceed the cap, and no delay overuse occurs during the cluster or its feedback tail. Finalise after the final feedback/tail; a clean target failure still contributes D to current-generation evidence.
- The result is finalised when every cluster packet has been reported, or by `Tick` at `ProbeTimeout` after the cluster ends, even without further feedback.

**Backoff:** failure doubles it from `ProbeBackoffMin` up to `ProbeBackoffMax`; success resets it.

**Padding budget** (packet-level)
- A padding packet is sent only if, after it, padding over the trailing `PaddingWindow` ≤ PaddingShare × video wire bytes over that window + unused bootstrap.
- Bootstrap is `BootstrapPadding` once per generation, spent first and reported as `bootstrapPaddingBytes`. It is not projected traffic.

**Generation change:** cancel the cluster and its padding, and discard old-generation results.

### Allocator (`allocate.go`, about 150 lines)

**Options, in preference order:** native T2, T1, T0, then low T2, T1, T0. Fewer frames before fewer pixels.

**Outputs:** a **desired** option, the probe candidate, and `Untenable`. The forwarder reports the **applied** option (`OnApplied`) once the target key or switching point has been forwarded. Dwell and down rules use the applied option.

**Rules**
- **Down:** hold(applied) > R continuously for `DownTime` → the next option that holds.
  - Within a rendition the change is immediate. Native → low needs a low key.
- **Recovering:** an accepted recovery or broken request within `RecoveryQuiet` forces low.
- **Temporal up:** admit(next TID) ≤ min(R, Proven) continuously for `TemporalUpTime`. It is applied at a switching point, as the continuity rules allow.
- **Low → native:** all of these:
  - admit(candidate) ≤ min(R, Proven);
  - at least `RenditionDwell` since the last applied rendition change; initial low acquisition is initialisation, not a change and does not delay first native admission;
  - no recovery within `RecoveryQuiet`;
  - Keys returns `Accepted` or `Pending`.

  If the native key size is unknown, the request is still allowed; the assembled-key recheck decides.
- **Untenable:** hold(low T0) > R for `DownTime`. This is the slide-mode trigger (PLAN feature 3).
- **Candidate** = the next rung only, cheapest-better-first: low → native T0 → native T1 → native T2. Within low, climb T0 → T1 → T2 before the rendition ladder. A failed probe still supplies delay-clean delivered unique rate D; every admission uses admit(o) ≤ min(R, Proven), including D. No alternate admission rule.

### Key admission at assembly (in the forward loop)

1. The SFU assembles every key, of either rendition, completely before forwarding its first packet. Assembly completion is the single delivery origin: the key, parity, RTX and abort deadline is completion + KeyEnvelope, in both production and simulator. Assembly itself has separate bounded storage; it does not spend the post-assembly delivery budget.
2. For each viewer that would receive it, it checks (`KeyFits`) audioWire + keyWire(actual) / KeyEnvelope ≤ R. The steady part, hold(target option) ≤ R, is the allocator's.
3. If the key fits, it is forwarded.
4. If not, it is **not** forwarded to that viewer:
   - a native viewer moves to low, with a low key request;
   - a low viewer invalidates the reference chain and waits for a deliverable low key; it never forwards deltas referencing the refused key, and reports `Untenable` if low cannot hold;
   - the size is recorded for the next admission;
   - the viewer is never left stalled waiting for a key it cannot receive.

This applies to join, refresh, recovery and upgrade keys. Store-and-forward adds the key's ingress time (about 0.37 s for 251 KB at the publisher's 2.5× pacing) to key delivery. It overlaps the encoder's post-key pause.

### Pacer (`pacer.go`, 1,170 → about 400 lines)

**Rate envelope**
- Outside a cluster, departures satisfy bytes ≤ R × elapsed / 8 + burst, with burst = max(`BurstFloor`, R × `PacerBurst` / 8).
- Inside a cluster, the cluster rate applies for its duration.
- There is no 1.5× drain rate.

**Priority:** audio > RTX (audio first) > key > TL0 > TL1 > TL2 > FEC > padding.
- FEC for a group is scheduled immediately after its group's last packet, in the group's class. Key parity is therefore interleaved with the key and shares its deadline.

**Hard bounds**
- Queued bytes ≤ `QueueBytes` and queued frames ≤ `QueueFrames`. This includes traffic waiting behind a key.
- **Whole-frame admission:** a frame that would exceed either bound is not admitted. It is dropped whole and reported to continuity.

**Age:** a delta frame whose first packet waited more than `FrameAge` is dropped whole. For frames behind a key of the same rendition, the clock starts when the key completes, but they still count toward the hard bounds.

**Supersession:** a new key of a rendition drops all queued frames of that rendition.

**Key abort:** a key not fully sent by its assembly-completion + `KeyEnvelope` deadline is aborted:
- its remaining packets and dependants are dropped;
- the event is reported as broken, and the viewer moves to low.

A rendition that is no longer forwarded is purged at once. Writer isolation is kept: a slow socket never blocks the publisher or other viewers.

### Repair policy (`repair.go`; `fec.go` encoder kept)

**Resend a NACKed packet only if all of these hold:**
- sends < `NACKMaxSends`;
- the predicted arrival is before the frame's useful deadline (first ingress + `FrameDeadline`; for keys, assembly completion + `KeyEnvelope`). Predicted arrival = now + RTX queue delay + RTT/2;
- its frame is still forwarded, not superseded or aborted, in the current generation.

**FEC**

| Inputs: loss, mean loss run, RTT, NACK feasibility | Action |
|---|---|
| loss < `FECLossOn` | none |
| otherwise, NACK feasible and mean run < `FECBurstRun` | ratio `FECRatio` on TL0 and keys, interleave depth 1 |
| otherwise | ratio 2 × `FECRatio`, interleave depth = ⌈mean run⌉ |

- NACK feasibility means RTT + current RTX queue delay < `FrameDeadline` / 2.
- The mean loss run comes from TWCC loss sequences over `LossWindow`.
- FEC is charged only to protected bytes in need(). Expected RTX is included.

**Audio:** RED is negotiated, and audioWire includes it. The SFU adds no audio FEC.

### Keys (`keys.go`, about 120 lines; one per room)

`Request(viewer, rendition, cause)` returns one of four statuses.

| Status | Meaning |
|---|---|
| `Accepted` | a PLI goes out now |
| `Pending` | the request shares the next budgeted key |
| `Redirected` | recovery is served on low |
| `Ignored` | a recovery request within `RecoveryQuiet` of the viewer's previous one |

**One budget per rendition**
- Every viewer-driven key request consumes the same room-wide budget: at most one PLI per `KeyInterval`. Causes include join, first native upgrade, reconnect, upgrade, refresh and recovery.
- Excess requests stay pending and share the next key.

**Satisfaction:** a request is satisfied only by a key captured after the request, whether requested or encoder-initiated. A key that completed earlier never satisfies a later join.

**Retry:** a PLI unanswered for `KeyRetry` is re-sent for the same pending set. That does not consume budget.

**Join:** the joiner starts on low for prompt playback, then upgrades through the budget like everyone else.

**Recovery and broken:** always low (`Redirected`). Debounced per viewer by `RecoveryQuiet`.

**Stalls caused by a key:** a viewer's recovery request is `Ignored` from the moment an admitted key starts being forwarded to it until `KeyEnvelope` after that key's last packet. That covers both the key's delivery and the encoder's post-key repayment.
- Found in sfusim A4k: at 3 Mbit/s a 560 KB key takes ≈1.5 s to deliver, and the encoder then pauses ≈1.6 s.
- The viewer's 1 s no-frame request (`web/src/session.ts`) would otherwise turn every large key into a low-rendition recovery loop.
- Genuine loss inside the key is repaired by NACK and FEC, or surfaces as a key abort (`CauseBroken`), which is not ignored.

**Security rekeys** are sender-initiated, with protocol deadlines, outside this budget. Their keys are observed (`OnKey`): they satisfy pending requests and reset the budget clock.

**Spam:** a spamming viewer gets at most one low request per `RecoveryQuiet` and never qualifies for a native upgrade while spamming. It causes zero extra native keys.

### Forward loop (`ctrl.Viewer`, about 120 lines; `sfu.go` about 150)

```
OnSent(s)              → est, prober (every packet, with kind, class, generation, cluster)
OnFeedback(now, twcc)  → est; prober result → est.OnProbe (current generation only)
OnReport(now, rr)      → est
OnPathChange(now)      → generation++; fence est, prober, allocator, pacer purge of padding
OnDecoderStall(now)    → keys.Request(applied, CauseRecovery); no rate effect
OnBroken(now, r)       → keys.Request(r, CauseBroken)
OnApplied(now, o)      → allocator applied option
KeyFits(now, r, bytes) → assembled-key admission (above)
Step(now, costs)       → prober.Tick; R; repair table; allocator; keys; pacer rate; prober.Maybe
                         → Decision{Desired, Key status, Rate, Cluster, FEC, Interleave, Untenable}
Handoff() RateState    → R and evidence-free state for a replacement PC (new generation)
```

`internal/sfu` calls `Step` after each TWCC report and at least every `OveruseTime`.

## Size budget

- **3,000 non-test lines for `internal/sfu`, including `ctrl`, is a C2 review target, not a hard gate** (responses §R-16). Main is 5,131 today.
- `TestSizeBudget` reports the count.
- Constants: hard budget of 40.

## Deletion list

| Goes | Where | Replaced by |
|---|---|---|
| `wireController`: proof, reservation, decodedWire, originalRate, standing excess, `repairReserve`, `admittedLayer`, `nativeWireRequired` | `estimate.go` | Estimator plus allocator. **Keep** `delayTrend` and `pathMinimum`. |
| `capacityTrials`, `paddingBudget` ring, `trialGrowthFactor` | `probe.go` | Prober |
| `stableFlight*`, `flightLimited`, `flightCapacity`, `nativeTrialRate`, `nativeRefreshRate`, `nativeUpgradeFits`, `nativeReservationFits` | `transport.go` | admit(), `KeyFits`. **Keep** `deliveryWindow` as the acked-rate helper. |
| `adapt.go`, `ice_proximity.go`, `parity_policy.go`, the 3.3×/5.3× rules | — | Repair table and estimator |
| `decoder_repair.go` as a rate input | — | Keys (`CauseRecovery`) |
| `media_loss.go`, `path.go` | fold in | Estimator |
| `spatial.go` (`renditionAdmission`, `renditionFallback`) | fold in | Allocator |
| `dependency.go`, `layer.go` | move into `ctrl.Continuity` | shared with sfusim |
| Publisher budget logic | `publisher.go` | a plain REMB ceiling at `Config.MaxRate` |
| Per-class budgets, flight, BDP and catch-up logic in the pacer | `pacer.go` | Pacer above |
| Tests: `round4_test.go`, most of `controller_trace_test.go`, `trial_schedule`, `steady_flight`, `feedback_hold`, `severe_parity`, `key_parity`, `native_cap`, `budget`, `repair_reserve`, `parity_*`, `adapt`, `ice_proximity`, `media_loss`, `spatial` | `internal/sfu` | `ctrl` unit tests plus sfusim Tier A |
| `keyframe-budget.ts` (parked branch only) | `web/src` | Not merged |

**Kept:** `reorder.go`, `repair.go` (cache and ownership), `fec.go` (encoder), `heartbeat.go`, `source.go`, writer isolation, the HTTP header and connection caps (d3ca9fc), and the native-key budget idea (M7, now in Keys).

**Reused from `parked/r5-wip` (d58a918):**
- `frameClock`;
- removal of the 100 KB key-size threshold (M2);
- the B1 metrics in `web/torture-metrics.mjs`, with GATES.md thresholds;
- `audit_round5.py`.

## C0 r2 responses

The 14 review items and the orchestrator decisions, in order. **Accepted** means the rule above was changed to the reviewer's replacement, with the stated refinements.

- **R-1 (adaptive threshold) — Accepted.** With feedback every 100 ms, Δt × KUp = 100 × 0.01 = 1, so γ jumps to |m| in one update and m > γ never holds. The threshold is now fixed at 12.5 ms/s, and a persisting overuse re-applies `min(R, Backoff × acked)` after each hold. Tier A `rtt_step` and the FIFO rows cover it.
- **R-2 (loss vetoes) — Accepted.** Loss without delay evidence never changes R or blocks probing. New Tier A rows `loss_prejoin_{5,20}` require native at full cadence within 20 s on 50 Mbit/s with persistent loss.
- **R-3 (key envelope) — Accepted (orchestrator decision 1).** `KeyEnvelope` is 2 s, includes audio, key FEC and headers, and is rechecked on the assembled key, which is never forwarded if it does not fit.
  - Arithmetic: at 3 Mbit/s with 64 kbit/s audio, a 560 KB key needs 560,000 × 8 × 1.05 / 2 s + 64k ≈ 2.42 Mbit/s against 3 Mbit/s; it fits. At 1 s it would need 4.77 Mbit/s.
  - Tier A `b1_3m_560k` exercises it.
- **R-4 (one admission target; bootstrap) — Accepted.** Probe success and allocation share admit(o). A cluster shrinks to the padding it can afford. The bootstrap credit is explicit (48 KB per generation) and reported.
  - Check: a 560 KB key at a 2 s envelope needs admit ≈ 2.35 Mbit/s. From low at 100 kbit/s, a 5-packet cluster at 2.35 Mbit/s costs 6.3 KB of padding, within one bootstrap. The 30 s rolling share at low (37.5 KB) sustains one ≈ 100 ms cluster every 30 s.
- **R-5 (join exemptions) — Accepted.** One budget per rendition for every viewer-driven key; completed keys never satisfy later requests; joins start on low.
  - Native-within-2 s on `good` is scoped to isolated joins with budget available.
  - New `join_storm` rows in Tier A and Tier B.
- **R-6 (pacer envelope and bounds) — Accepted.** Departures follow R with an MTU-floored burst. Hard 1 MiB / 64-frame bounds count key-blocked traffic, plus whole-frame admission, supersession and key abort. New Tier A `collapse` row.
- **R-7 (TL1 rule) — Accepted, with measured structures.** The rule above follows the reviewer's wording. Measurement shows the detailed source is flexible (exact references), so the conservative rule applies only to the non-flexible production and clock streams. Unsupported structures are rejected. New Tier A `tl1_loss` row on the non-flexible source.
- **R-8 (repair deadlines, bursts, kinds) — Accepted.**
  - Resends are deadline-checked, FEC uses burst length and NACK feasibility, and key parity is interleaved.
  - `Kind` (audio/video) is separate from `Class`, so audio RTX is not video.
  - The FECRTT constant is replaced by deadline feasibility.
- **R-9 (interfaces) — Accepted.**
  - Allocation returns desired options and the forwarder acknowledges applied ones.
  - Keys return a status; `OnBroken`, `Prober.Tick`, `Config.MaxRate`, `Untenable`, `KeyFits` and `Handoff` were added.
  - The SFrame requirements are normative above.
- **R-10 (generation fence) — Accepted.**
  - Every packet and result carries a generation.
  - Upgrades need current-generation Proven, while R is preserved.
  - Tier A `pair_change` switches the real path, with old-path feedback arriving after the switch.
- **R-11 (FIFO floor) — Accepted.** Throughput counts unique original media. The reference option is the most preferred one whose hold total fits the share, with native required whenever native T0 fits. The TCP-only reference matches each row. GATES.md has the formula.
- **R-12 (manifest dimensions) — Accepted (orchestrator decision 4).** GATES.md adds the freeze manifest, audio, OCR, resources, capacity scope, E2EE assertions, and a behavioural `non_pixel_clock`.
- **R-13 (cohort statistics) — Accepted (orchestrator decision 5).** Pooled per-profile empirical metrics, per-run results with uncertainty, predeclared handling, and the error/power note. Tier A's former "every seed" wording is corrected to the ≥95/100 rule it enforces.
- **R-14 (P2P) — Accepted.** Reference failure triggers investigation only. Relaxation needs a physical bound, or a demonstrated mismatch between the predicate and the product requirement.
- **R-15 (constants) — Accepted, with a different number.** Typed constant blocks now count; only the named enumerations are exempt. Removing aliases makes the honest count 40:
  - KUp, KDown, LossLow, NeedMargin, FECRTT and Headroom are removed;
  - one constant each is added for every formerly shared time scale, plus the bootstrap, MTU floor, hard queue bounds, key envelope, retry, recovery debounce, deadline and burst run.

  Keeping 25 would mean re-aliasing unrelated time scales, which the review forbids. The budget is 40; any growth needs a sfusim scenario.
- **R-16 (size) — Accepted as a C2 target, per orchestrator decision 7.** Moving policy outside `internal/sfu/ctrl` is prohibited.
- **Orchestrator decision 2 (native).** "Native" means native pixels at any TID. Full T2 cadence is required only where capacity ≥ 1.25 × full native demand. GATES.md declares this per row.
- **Orchestrator decision 6 (process).** AGENTS.md takes the reviewer's wording.


## C0 r3 (orchestrator decisions)

**Substantive changes, authorised by the orchestrator on 2026-11-08; Opus reviews them at C2.** This section and the updated rules above supersede r2 and the rejected review's stop point. A1 proceeds without another C0 round.

- **F-1:** loss-compensated offered probes, bounded overdrive (configuration default 1.5), unique delivered-byte measurement, and a delay-clean feedback tail. Lost bytes are never credited. Repair still charges L̂. Equal-spacing 0/1/6-loss and 5%/20% Bernoulli regressions plus A13 are mandatory. A cap-limited probe reports that limit and follows the same ladder.
- **F-2:** next-rung discovery through native T0/T1/T2; clean failed probes contribute D as evidence under the shared admit(). Add 1.5 Mbit/s prejoin/return native-at-reduced-cadence properties; retain every full-native 3/5/10 Mbit/s property.
- **F-3:** assembly-completion is the key delivery, repair and abort origin. A refused low key invalidates its references; it cannot leave deltas flowing against a nonexistent decoder chain.
- **F-4:** unsupported dependencies fail closed, including whole forwarding; flexible intra keys remain valid.
- **F-5:** the forwarder/pacer reports per-viewer admission, first departure, last departure and abort to Keys. Keys owns suppression, not wire code. Local completion is not receiver receipt; explicit broken events bypass suppression. Initial low acquisition does not start rendition dwell.
- **F-6:** Wi-Fi is 3 ×300 s per history, six runs/30 minutes pooled, ≤6 freezes. GATES records its Poisson acceptance probabilities. For A1 all per-seed properties must pass every seed; pooled properties retain their pooled predicates.
- **Security continuity:** an acknowledged epoch change changes cryptographic state, not codec state. Remove unconditional worker epoch key requests and the duplicate activation request. Missing-key receivers request low recovery under the room budget; no native recovery is broadcast. Keep the existing write-ahead fence and 300 ms confidentiality cutoff. Deterministic regressions and three consecutive full e2ee.mjs runs must preserve FREEZE_MS=500 for unaffected viewers. This direction is confirmed by measured results, not assumed complete.
- **Process:** maximum two fix attempts per failing property/row, then escalate with evidence; no per-row policy, no gate relaxation, three working days. Browser work remains behind Tier A and media-run; A3's full freeze follows C2.

## A1 implementation notes (Opus, 2026-11-08; pending C2)

These rules were made concrete or changed while building A1 against Tier A. Each has a regression in `internal/sfu/ctrl/a1_test.go` or `internal/sfusim/a1_regression_test.go`, and the evidence is in the A1 ledger in REPORT.md. C2 must approve or reject each; none weakens a gate predicate.

**Prober**
- **Headroom.** Offered rate = min(ProbeHeadroom × admit / (1 − L̂), ProbeMaxOverdrive × admit). ProbeHeadroom is 1.1. An offer at exactly the target measures target − ε by dispersion: observed 1,693,462 bit/s against a 1,693,463 target, a failure with no loss at all.
- **Capacity ceiling.** Only a saturated complete cluster (C2-2) measured the bottleneck, even when its own overshoot raised delay. The ceiling is D/(1 − L̂). Before C2-2 every target failure that was not loss-capped installed it.
  - For ProbeEvidenceTTL it bounds R and later offers, at ProbeMaxOverdrive × ceiling.
  - It is not admission evidence unless the probe was clean.
  - A success below the ceiling does not clear it.
- **Ratchet.** A ceiling-limited cluster is sized to ProbeMinPackets packets of padding. A clean one that measures at least ProbeHeadroom × ceiling counts as progress, so no back-off is applied.
- **When to probe.** A probe starts when target > min(R, Proven), not only when target > Proven. After a cut, one clean cluster restores R instead of waiting for 8 %/s regrowth (A7).
- **ProbeBackoffMax is 15 s.** Worst-case return detection is 15 s plus the probe and the key, which stays within the 20 s return gates (A5, A18).
- **Switch rung.** While a viewer is on low and native T2 is admitted, discovery seeks hold(T2) + keyWire/KeyFast, with KeyFast = 400 ms.
  - The evidence is clamped to admit(T2) once the viewer is native (`Estimator.Clamp`).
  - Viewers already on native are not probed for it. The calibrated post-key encoder pause (≈1.1 s) means faster refresh-key delivery splits one freeze into two, so A14 fell to 1/100 when it applied to all viewers.

**Estimator**
- **Overshoot exemption.** A probe's own overshoot does not cut R while the last cluster packet is within ProbeTimeout and the excess is below ProbeLength. Larger excess is real congestion and is cut at once (A6: capacity collapsing under a probe).
- **Hold.** The hold lasts min(RTT, AckedWindow): a sender overfilling a queue inflates its own RTT.
- **AppLimitedFactor is 1.1.** Growth past the delivered rate bursts 37 KB TL0 pictures into the bottleneck and trips overuse; probes discover above it (A4 3 Mbit/s).

**Allocator**
- The candidate is the next rung above the best option the evidence admits. Its floor starts at the applied option but evidence may lower it, so after a loss shock the applied option is re-proved (A8).
- A low → native switch lands on the most-preferred native option admitted, since a key is a switching point for every TID.
- A requested switch persists until its key is admitted or refused, while hold(native T0) ≤ R.

**Keys** (the orchestrator's admission decision, completed)
- **Size estimate.** p75 of the last 8 native keys within 60 s.
- **Provisional estimate.** With fewer than 8 keys the estimate is provisional. Discovery first seeks the full admit; once a probe has measured the path below it, the steady part admits. Without this, one 394 KB startup key pinned A18 seed 16 for the 60 s age-out.
- **Recheck and back-off.** The actual assembled key is always rechecked (`KeyFits` takes the actual wire bytes). A refusal, or a native key aborted after admission, backs the viewer off for 3 → 6 → 12 → 24 → 30 s.
- A viewer's pending request is dropped when its key is admitted to that viewer, and the viewer does not re-request while its own key is being delivered.
- **Recovery debounce** runs against the last accepted recovery, so a stalled viewer is served once per RecoveryQuiet. Upgrade eligibility uses the last recovery request of any status, so a spammer never qualifies.

**Pacer:** FrameAge sheds only disposable layers (TID > 0). Base pictures are kept until their useful deadline and the hard bounds. At 86 % utilisation (A18) the frames produced during a key cannot drain within 100 ms, and dropping them broke the chain into a recovery loop.

**Ingress (`ctrl.Ingress`, shared by the wire and the simulator)**
- It uses MeterInterval buckets in an EWMA over TrendWindow, with bias correction.
- A bucket with no delta packets for a rendition is skipped: the post-key encoder pause is not lower steady demand.
- *Superseded by C2 item 6b:* costs over complete temporal cycles (`CostCycles`), audio by a continuous-time EWMA.

**Constants:** 48, the budget approved at C2 (GPT: "APPROVE an explicit budget of 48"; the approval covers these parameters, not every rule that uses them). The eight additions:
- the five key-admission constants from the orchestrator decision;
- ProbeHeadroom;
- MeterInterval (replaced by CostCycles in C2 item 6b, pending review);
- KeyFast.

Changed values: BootstrapPadding 96 KB, ProbeBackoffMax 15 s, AppLimitedFactor 1.1, NACKMaxSends 4.

## C2 correctness fixes (C2-1, C2-2, C2-4, C2-5; pending review)

No constant changed; these close correctness gaps the C2 review found. C2-2 changes the probe measurement and ceiling rules (below).

- **Interfaces.** `Pacer.SetRelease(fn)`: the pacer hands every discarded packet to `fn` inside the call that discarded it, so the wire frees payloads and reports lost pictures to continuity under the viewer lock without waiting for `Step`. `Dropped()` now returns only chain-breaking drops (at most one per rendition) and `KeyEvents()` coalesces to the latest key's phases plus its latest abort. `Continuity.Unsupported(r)` exposes the latched unsupported state; the forwarder refuses `r` at once. `ctrl.FECGroup(ratio, interleave)` bounds interleave depth by the 16-packet ULPFEC mask.
- **Memory.** Padding accounting is a fixed ring of `PaddingWindow` slots charged at send time. Estimator histories are capped at the 8,192-entry send ring. Receiver timing is discarded when the receiver clock stops while sends continue, falls more than `PathWindow` behind, or implies a transit more than `PathWindow` from the path minimum. Open FEC groups are discarded at their deadline or when a newer key or rendition supersedes them.
- **Descriptors.** The assembler validates every packet's clear VP9 descriptor against the first (picture ID, P/F/L, layer indices, TL0PICIDX, P_DIFF) and the B/E frame boundaries. Non-flexible continuity checks TL0PICIDX progression against the 0,2,1,2 pattern.
- **Keys.** Every recovery request updates the last-request time before any suppression, so a suppressed spammer stays ineligible for native.
- **Probe evidence (C2-2).** A sub-target offer could prove a higher target: burst-credited sends with compressed arrivals measured 2.4 Mbit/s from a 60 kbit/s offer.
  - **Measurement.** Over the cluster's acked packets, D is the receive-dispersion rate (unique bytes after the first arrival ÷ receive span), bounded by the send side of the same packets and by the planned rate. The send side is their bytes after the first, less the burst credit they used, over their send span. The burst credit is the most the cumulative bytes ran ahead of a pacer that starts empty at the first packet and sends at the planned rate: what the pacer could emit back-to-back from its starting allowance cannot shrink the span. The send bound allows one more acked packet than that, since dispersion resolves one packet on either side; without it the lower of two noisy estimates biased D low on short low-rate clusters (A18 return). A compressed or delayed delivery tail therefore cannot raise D above what was sustainably offered.
  - **Classification** (`ProbeResult.Class`, in this order). *timed-out*: no cluster packet reported by `ProbeTimeout`. *missing-feedback*: some cluster packets never reported by `ProbeTimeout`. *undersized*: fewer than `ProbeMinPackets` acked, every sent byte within one pacer burst allowance (max(`BurstFloor`, rate × `PacerBurst`)), or a zero send or receive span; D = 0. *send-limited*: over all sent cluster packets, burst credit excluded, the sender fell more than one largest packet behind the planned rate; a cluster the padding budget ended early at the full rate is not send-limited. *saturated*: during the receive span the sustained send rate offered more than one largest acked packet beyond what arrived. *dirty*: delay overuse without saturation. *delivered*: otherwise. `Clean` still records overuse for every class, so a saturated cluster can be dirty.
  - **Use.** Success needs saturated or delivered, clean, not loss-capped, and D ≥ target. Only saturated installs the D/(1 − L̂) ceiling, clean or not: the probe's own overshoot raises delay when it saturates a bottleneck (making a dirty saturated cluster unable to bound dropped A15 to 39/100 and A6 to 92/100 in a measured variant). Any other result with D above a live ceiling raises it to D/(1 − L̂), never lowers or newly installs one (the ceiling ratchet). A clean D > 0 of any class remains delivery evidence. Timed-out and undersized carry no D.
  - **Known limits.** Saturation is delay-deficit evidence. Arrival jitter of a few milliseconds can exceed the one-packet margin (A13 20 % loss with 5 ms jitter), and an AQM bottleneck that drops rather than queues shows as loss, not as a deficit (A11 fq). Missing feedback on a lossy reverse path discards target proofs and bounds whose D is still measured over corresponding spans (A12 20 %, A15, A6); this follows the rule as specified.

## C2 item 6a: keys and timing (pending review)

These rules change the sections above where they conflict. No constant changed; the budget stays 48. Evidence and Tier A deltas are in REPORT.md "C2 item 6a", and regressions are in `ctrl/item6_test.go`, `internal/sfu/item6_test.go` and `internal/sfusim/item6_regression_test.go`.

**Pacer**
- **Deadline behind a key.** A picture held behind a key of its rendition does not expire while blocked. When the key completes, the picture's deadline moves later by the time it spent blocked, as its FrameAge clock already did. `Pop` returns the extended deadline, and repair uses it.
  - Why: the sender's key boost (`web/src/keyboost.ts`) removed the post-key encoder pause that this section's "It overlaps the encoder's post-key pause" relied on. Base pictures captured during a slow key then missed FrameDeadline and broke the new chain (A4k, A18).
- **Dependants.** A dropped picture (shed, expired, aborted or refused at the hard bounds, but not superseded) takes the queued pictures that reference it:
  - for a key or TL0, everything of its rendition up to the next key;
  - otherwise, the higher-TID pictures up to the next one of its TID or lower.

  Each is released and reported to continuity (A17).

**Forward loop**
- **Switch intent.** `OnDecoderStall` and `OnBroken` return true when the recovery was accepted. The forwarder then runs `Step` at once, so it already wants low when the requested low key assembles, often within ≈20 ms (A8).
- **Media barrier.** Behind the media barrier the forwarder reports missed pictures to continuity only. `OnBroken` waits until the viewer is reachable again, so no key is requested for, spent on or satisfied by an unreachable viewer (viewer_flap diagnosis).
- **Closed PC.** A closed PC's forwarder leaves before `Keys.Leave`, so late RTCP of the old PC does not re-enter Keys.

**Keys**
- **Older admissions.** `OnDelivery` does not let a key admitted before a viewer's request serve that request.
- **Replacement exception.** `Replace(now, viewer, session)` is the first request of a loop built by `NewViewerFrom`, and it is low only. It is served at once by the room's budget or by one exception PLI. The exception requires all of these:
  - the session has not been served at once within KeyInterval;
  - the room sent no exception PLI within KeyRetry;
  - no low PLI is outstanding.

  Concurrent replacements share the exception key. Initial admissions and native requests never get an exception, and `KeyFits` rechecks the assembled key.
- **Session identity.** The session is `RateState.Session`, which `Handoff` carries: the first PC's viewer ID, not the new Peer's.
- **Diagnostics.** `Times(r)` reports the latest PLI emission and key assembly, and `Viewer.KeyTimes(r)` adds the viewer's request and delivery times.

**Allocator**
- **Climbing discovery.** A viewer on low that already admits native T0 does not request the native key while discovery is still climbing cleanly. Climbing (`AllocInput.Discovering`) means:
  - a cluster is in flight, or the last result succeeded and `Prober.Ready`;
  - there is no ceiling, overuse or congestive loss;
  - a rung or the switch rung lies above min(R, Proven).

  The key is then delivered at the rate discovery reached (A1 good_join: a key requested at ≈1.5 Mbit/s took ≈1 s).
- **Unchanged.** A requested switch is never withdrawn. Initial low acquisition stays immediate and is not a rendition change. The switch lands on the best admitted TID.

**Interfaces.** These were added:
- `Keys.Replace` and `Keys.Times`;
- `Prober.Ready`;
- `AllocInput.Discovering`;
- `RateState.Session` and `Viewer.Session`;
- the bool results of `OnDecoderStall` and `OnBroken`.

## C2 item 6b: estimator, admission, repair (pending review)

These rules change the sections above where they conflict. Evidence, Tier A deltas and escalations are in REPORT.md "C2 item 6b". Regressions are in `ctrl/item6b_test.go`, `internal/sfu/item6b_test.go` and `internal/sfusim/item6b_regression_test.go`.

**Constants.** `CostCycles = 16` replaces `MeterInterval`; the count stays 48. It needs checkpoint approval as a new meaning.

**Ingress meter (`meter.go`)**
- A cycle opens at the first packet of a base picture (TL0 or key) and closes when the next base picture begins. `Ingress.Video` takes a picture identity (the RTP timestamp in production) for this.
- A rendition's rates are the bytes of its newest `CostCycles` complete cycles over their span. A cycle containing a key is recorded only while no steady cycle is; nothing decays while the source is silent.
- Why: 100 ms buckets sampled the 3.75 base pictures/s at an arbitrary phase (T0 swung ±20%). A 1 s estimate rests on ≈4 base pictures whose sizes vary ≈25% each. Measured T2 cost sd: 6.5% before, 3.8% with 16 cycles (≈4.3 s at 15 fps L1T3).
- Audio: a continuous-time EWMA over TrendWindow with bias correction.
- `Costs.Flush`: one parity packet per protected picture (mean TL0 packet + 15 B). `Need` adds (1 − fec) × Flush when fec > 0, because the forwarder flushes each protected picture's partial group (below).

**Allocator: holding.** A cost-only excess (R did not fall) must outlast TrendWindow before it revokes a held option; a cut in R is answered within DownTime. One outlier TL0 picture lifted T0's cost 27% for ≈0.6 s (A18 seed 1).

**Estimator: drain episode (C2 §5 A5)**
- *Entry:* all of these:
  - excess transit ≥ OveruseTime, so the bottleneck has stood busy and arrivals over the newest OveruseTime measure its service rate S;
  - no probe packet in that window;
  - S < Backoff × acked and S < Backoff × the send rate of the same packets;
  - this sender's unreported bytes take ≥ FrameDeadline to drain at S.
- *On entry:* R = max(RateMin, min(R, Backoff × S)); acked history and probe evidence are cancelled; S/(1 − L̂) is installed as the ceiling.
- *Clear:* when a packet sent after entry is reported (the old work was served). It *settles* when the excess then falls below OveruseTime (behind a standing queue, once the path minimum re-bases within PathWindow).
- *`Estimate.Draining`* holds from entry until AckedWindow after settling. While the episode is open, `Service` and `Debt` (unreported bytes) are reported. R does not grow.
- *Forward loop:*
  - draining blocks upgrades, probe targets and padding (the cluster is cancelled);
  - the allocator drops the applied option without a down timer;
  - `Decision.Stop` makes the forwarder stop and purge a forwarded rendition other than the desired one until the desired rendition's key;
  - the pacer (and the allocator's view of R) runs at min(R, max(floor, S − 8 × Debt / FrameDeadline)); the floor is AudioWire, or hold(low T0) while low is decoded;
  - Step requests a switch key only once that rate admits the estimated key; `KeyFits` still uses R.
- *Replacement PC:* `RateState` carries `Congested` (the old PC was draining or congested) and `Ceiling` (its live ceiling). A congested replacement starts in an inherited episode that ends when its own delay trend is flat. The handed ceiling (min(ceiling, rate) when congested) bounds R and offers.
- *Diagnostics:* `catchupMs` also covers the debt (Debt × 8 / Service, ≤ KeyEnvelope), so the viewer app defers PC replacement while the queue ahead drains (`/llms.txt` updated).

**Estimator and allocator: sustainable admission (C2 §5 A10)**
- `Estimate.CongestedAt` is the latest congestion cut or drain episode.
- Within AppLimitedLookback of it, `Estimate.Sustained` is the least acked window over the lookback, plus only probe results measured since. Otherwise Sustained equals Proven.
- A native key from low is admitted on min(R, Sustained); temporal upgrades keep Proven.
- No switch to native within RenditionDwell of CongestedAt. A clean cold join is not held.
- A requested switch persists only while Sustained carries native T0's steady need.
- After recent congestion, probe offers are capped at ProbeMaxOverdrive × max(Proven, R).

**Prober (C2 §5 A12/A13)**
- A cluster whose first and last packets were reported is measured on its reported packets, at once, instead of being classified missing-feedback.
- Undersized, missing-feedback and timed-out clusters are measurement failures: they retry at the current back-off without doubling it.
- An incomplete result (not saturated or delivered) cannot lower live probe evidence.

**Repair (C2 §5 A12/A13)**
- *FEC ratio:* doubles (one parity per two) when a one-per-four group (four media plus parity) would lose two or more packets more often than a bare packet is lost; at independent loss that is above ≈14%. The burst and infeasibility rows are unchanged.
- *Wire budget:* while NACK is feasible, protection that does not fit beside the applied option (hold > R) steps down, 0.5 → 0.25 → 0.
- *Early critical retransmission:* a key or TL0 packet that TWCC reports lost is resent at once through `Repair.Resend`, in production (`forward.go` `resendVideo`) and the simulator.
- *Deduplication:* `RepairQuery` carries Since (age of the packet's latest copy) and RTT. A second trigger within one RTT of the latest copy is refused.
- *Obsolete pictures:* packets of pictures above the forwarded TID are not repaired.
- *Flush:* the last packet of a protected picture flushes its partial parity groups, in production and the simulator. The simulator sizes parity as production does: the longest protected packet plus 15 B.

**Pacer.** A packet longer than the burst cap leaves once the tokens reach the cap, and the deficit is repaid. Parity slightly longer than BurstFloor otherwise stalled the pacer for good below ≈1 Mbit/s.

**Fast-key rung for native viewers (6a escalation A16)**
- A native viewer at native T2 also seeks hold(T2) + keyWire/KeyFast, when all of these hold:
  - evidence admits T2;
  - no loss and no congestion within ProbeEvidenceTTL;
  - no ceiling below the target;
  - no current KeyRate covering it.
- The cluster is a ceiling ratchet against max(Proven, R, KeyRate).
- When its result is in, R is clamped to admit(T2) and the delivered rate is kept as `Estimate.KeyRate` (valid for ProbeEvidenceTTL unless congestion or a ceiling follows). A key in flight is paced at max(rate, KeyRate).

**Interfaces added:** `Estimate.{Draining, Service, Debt, CongestedAt, Sustained, LostAt, KeyRate}`, `Decision.Stop`, `RateState.{Congested, Ceiling}`, `RepairQuery.{Since, RTT}`, `Costs.Flush`, and the picture argument of `Ingress.Video`.

## Key-funded discovery and complete-key application (takeover checkpoint, pending review)

- `Pacer.QueuedKey(r)` reports the queued original key's remaining wire bytes and actual total. No new history or tunable is introduced.
- `Prober.MaybeKey` can use an **already admitted native key** as its experiment. It requests/admit no key and creates no padding. `Cluster.MediaOnly` prevents padding when the media ends early. The actual queued bytes must exceed one burst plus the minimum sample; duration remains bounded by ProbeLength and available bytes. Offers ratchet at most ProbeMaxOverdrive above current transport evidence, respect measured ceilings/debt, and use the existing loss/congestion quiet period. A measurement-only backoff with no capacity failure does not forbid a newly funded experiment.
- Original allocator and discovery-before-switch policy stay intact. The actual full key size sets the fast-delivery target; current-generation unique feedback supplies all credited evidence. Decoder messages do not fund or score it.
- A replacement PC starts its low acquisition under RecoveryQuiet. An immediate native upgrade can otherwise interrupt the third fresh recovery frame (A8/A19); cold initial joins do not wait.
- The wire and simulator report an applied rendition only after the complete key departs. A mid-key temporal downgrade must not announce that new rendition early. At completion the same key applies the controller's **current desired TID**, since it resets every temporal reference chain. This fixes the reproduced native/T1 announcement and stale-TID completion.
- Owning-layer regressions cover unchanged ciphertext-frame forwarding, key-backed experiments, no invented padding, congestion/ceiling refusal, exact queued bytes, replacement acquisition, and the wire's partial-key/complete-key ordering. Simulator A1seed16 moved from full native3s to within2s. Full TierA is still red (REPORT); this is not a freeze/feature-completion claim.

### Discovery sample and ceiling correction (next checkpoint, pending review)

This supersedes the key-backed offer's current-evidence ratchet above: an already admitted key uses the burst plus the minimum packet sample, about15ms at fast-key rates, instead of a100ms experiment. Existing loss/congestion quiet and actual-capacity-ceiling guards remain; the offer stays within ProbeMaxOverdrive of its target. A newly assembled native key is observed before the writer/periodic tick and can supersede a cheaper ordinary experiment. Cancelled-cluster feedback cannot score the new ID. Low keys do not fund this native-only experiment.

A ceiling-limited regular offer is bounded by ProbeMaxOverdrive × max(Ceiling,min(Proven,R)): delivery already demonstrated at the current paced rate can contradict an old lower ceiling, while one compressed burst aboveR cannot escalate the experiment. If the fixed high-rate padding ratchet fits wholly inside a burst, it grows to burst plus ProbeMinPackets×BurstFloor **only when real available credit affords it**. Low-rate padding cost and all accounting bounds stay unchanged. No constant or gate changes.

## Explicit switch service and original freshness (2026-11-09)

This authorized component checkpoint replaces the historical `AllocInput.Discovering` switch inference and qualifies the earlier blocked-picture age notes. Native eligibility remains the allocator's steady/key evidence decision. Before requesting an optional native switch, Viewer performs one bounded preparation round using the existing prober, credit/backoff and ProbeTimeout. Its outcomes are measuring, service demonstrated, capacity constrained, and unmeasured/budget-limited. Failure/idleness is never a capacity proof. A finite unmeasured fallback uses the best live native option; it can still incur a slow interruption. The objective uses that option's Hold plus measured key wire bits/KeyFast, without requiring sustainable T2 to acquire T0. A prepared actual key rechecks live Hold and its actual size/service; it does not repay a fresh upward margin solely because costs fluctuate during assembly. Lost eligibility/path evidence cancels the unstarted candidate.

The existing bounded Queue owns the candidate and observed descendants. Low remains valid until actual first-key-packet commitment, after any started old original completes; no low/native original interleaving is introduced. RTP timestamp/descriptor rewrite moves to departure. Applied rendition/TID is acknowledged only at complete key departure, using the current desired TID. An unstarted cancellation clears only its own rendition+frame delivery fact, without requesting CauseBroken; active and completed-tail suppression and the hard2s key abort are unchanged.

PictureService forecasts the actual Pop/NextAt scheduler, actual packet sizes/admitted work, audio/RTX priority, tokens, cluster tail and base rate. Generation and physical writer time are fenced; unknown writes/source tails/descendants remain unknown. Packet FreshUntil preserves the original RTP-clock/ingress age mapping separately from the extended reference lease returned in Deadline. Disposable enhancement dependency closures expire on original age even behind a key. A necessary late base can remain finite reference/recovery work for a known fresh descendant; its actual age is still late. A known chain that cannot reach any fresh output before breaking its existing lease is abandoned once through CauseBroken and normal room budgets. No standalone removal of deadline extension, new timer/rung/threshold or source calibration is introduced.

Owning evidence is in `test/torture/results/controller-switch-service/`. Typed SwitchOutcome is registered with the existing enum exclusions in the constant-budget test; the budget and48 tunable values are unchanged. The controlled phase fixture still measures slow-key interruption and2103ms-old first delta as red characterization, independently of Default eligibility and eventual catch-up tests.
