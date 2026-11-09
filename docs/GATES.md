# PassStream media gates (checkpoint C0, revision 3)

## Ownership and evidence

**Who owns what.** The reviewer and the orchestrator own the gate predicates; the user approves changes at a checkpoint. Implementers (A1, A3) must not edit:
- this file;
- the pass expressions in `web/torture.mjs`;
- `web/torture-metrics.mjs`;
- `internal/sfusim/gates.go`.

**Floor.** No gate may be weaker than PLAN. Thresholds come from PLAN or the product, never from the best observed run.

**Tiers**
- **Tier A** runs the deterministic simulator on every controller change.
- **Tier B** runs browser netem only at a freeze: at most once a day, the whole manifest, unchanged.

**Regression rule** (AGENTS.md "Build process"): controller-policy fixes require a simulator regression; wire, browser, security and negotiation fixes require a deterministic regression at their owning layer.

## Running

| Kind | Command | Lock |
|---|---|---|
| Unit tests: `go test` (including `-race`), `go vet`, vitest, and the Tier A suite | `nice -n 10 mise exec -- go test ./internal/sfusim` | **No lock** |
| Tier A smoke (≤30 s) | `nice -n 10 mise exec -- go test -short ./internal/sfusim` | No lock |
| Functional browser runs (e2e, Playwright, `web/vp9-structure.mjs`) | `scripts/media-lock <cmd>` | Exclusive lock, no load gate |
| Tier B and calibration measurement runs | `scripts/media-lock --gate <cmd>` | Waits for 1-minute load ≤3 *before* locking, rechecks after, and releases and retries if load rose |

Never wait for load while holding the lock. `scripts/media-lock` has the same semantics as `/tmp/passstream-briefs/media-run`.

## Definitions (both tiers)

- **Native:** a 500 ms sample whose decoded picture is the native rendition at **any** TID ("fewer fps, not blurry text").
- **Full native:** native and forwarding T2. Each row below says whether it requires **native** or **full**.
  - Full is required only where the capacity available to the viewer is ≥ 1.25 × full native demand. For the detailed source that is ≥ 2.83 Mbit/s.
- **Gap:** time between consecutive rendered frames, including the window edges. A **freeze** is a gap >500 ms.
- **Fresh frame:** G2G ≤1 s, with an advancing picture.
- **Padding share:** padding bytes ÷ video wire bytes in the window.
- **Unique original media:** media bytes counted once per sequence number at the receiver. Duplicates, RTX copies beyond the first, FEC and padding are excluded.
- **Reference option o\*** (competition rows): the most preferred option whose hold total (audio + need) fits the viewer's fair share link/(n+1).
  - rate(o\*) and fps(o\*) are its media rate and source fps × {T2: 1, T1: ½, T0: ¼}.
  - Native is required whenever native T0 fits the share.
- **Audio continuity:** gaps >200 ms between playable Opus frames, counting a frame as playable if it or its RED copy arrived.

## Tier A: simulator properties

**Setup**
- Each property runs **100 seeds**. A run is a pure function of (scenario, seed, controller).
- **Per-seed** properties must pass all 100 seeds for A1 (or all five in the short smoke). Pooled properties retain the exact pooled predicates below. This strengthens r2’s 95/100 (isolation 99/100), as required by the implementation brief.
- **Pooled** properties are computed over the union of all seeds.
- Failing seeds are printed.
- **Enforcement:** properties are measured, but not enforced, while `ctrl.Stub` is true.

**Budget:** suite ≤5 min; `-short` smoke ≤30 s. On 2026-11-08 (r2) the suite took 8.8 s on 12 cores and the smoke run 0.4 s.

**Source model** (REPORT "Simulator calibration")
- **Structures:**
  - Detailed: 2560 × 1440, 15 fps, TL0/TL1/TL2 at 1.13/0.35/0.66 Mbit/s, **flexible**.
  - Production: 14 fps, 2.24/0.70/1.25 Mbit/s, non-flexible.
  - Clock: 15 fps, 42/61/99 kbit/s, non-flexible.
  - Low: about 100 kbit/s.
- **Native keys:** 251 KB with probability 0.9, otherwise uniform 252–560 KB.
- **Post-key encoder pause:** 650–840 ms × (key bits ÷ (encoder rate × 1 s)), counted from key ingress completion.
- **Audio** (where marked): Opus at 50 packets/s, 120 B each including RED.

| ID | Scenario | Predicate (exact) | Native / full | Pool | Change vs PLAN | Change vs round-4/5 |
|---|---|---|---|---|---|---|
| A1 | `good_join`: 50 Mbit/s, 20 ms one way, isolated join at 2 s, budget available; audio | TTFF ≤1 s; full native ≤2 s after join | full | 100/100 | PLAN TTFF p50 <1 s → ≤1 s per seed | R4 p50 ≤150 ms was browser-only |
| A2 | `bad_wifi`: both directions, in-order jitter 40 ± 10 ms, GE p=1% r=19% bad-state loss 100%; 300 s; detailed source; audio | Freezes ≤1.0 per 300 s; seeds with a gap >2 s ≤5; G2G p95 ≤400 ms; full-native share ≥90%; audio gaps reported | full | pooled | = PLAN rate, plus gap, latency and native caps | Replaces R4 zero-freeze, p95 ≤305 |
| A3 | `isolation`: 2 viewers at 50 Mbit/s plus 1 at 256 kbit/s (40 ms, FIFO 1000); 60 s; control = same seed without the slow viewer | Healthy viewers: 0 freezes; full-native share 100%; G2G p95 ≤ control + 50 ms | full | 100/100 | AGENTS non-negotiable | New |
| A4 | `b1_{3,5,10}m_{prejoin,established}`: FIFO 1000, 40 ms; prejoin from join, established at 10 s; audio | From T0: full native ≤20 s. Over [T0+20, T0+50]: full-native share ≥95%; no non-native episode >2 s; fresh frames ≥90% of source frames; padding share ≤10% | full (3 Mbit/s ≥ 2.83) | 100/100 | PLAN "fewer fps, not blurry text" | R5: hold 100% → 95%; padding ≤50% → ≤10% |
| A4k | `b1_3m_560k_{prejoin,established}`: as A4 at 3 Mbit/s, every native key 560 KB | As A4 | full | 100/100 | — | New (C0 review R-3) |
| A5 | `b1_flap`: 5 Mbit/s → 256 kbit/s → 5 Mbit/s, 51 s each; audio | 5M intervals: full native ≤20 s, padding ≤10% over the last 30 s. 256k interval after 3 s: max fresh gap ≤1 s, G2G p95 ≤1 s | full at 5M | 100/100 | PLAN flaps | Padding 50 → 10% |
| A6 | `viewer_256k`: 256 kbit/s, 40 ms, FIFO 1000; score 15–45 s | Max fresh gap ≤1 s; G2G p95 ≤ one-way + 400 ms | — | 100/100 | — | Stronger than R4's 1 s |
| A7 | `rtt_step`: 5 Mbit/s, one way 40 → 115 ms at 30 s | Over [30, 90] s: full-native share ≥90%; max gap ≤1 s; ≤1 rendition downswitch | full | 100/100 | PLAN 50–800 ms RTT | New |
| A8 | `outage`: 2.5 s of 100% loss both ways at 20 s | Recovery (first of 3 fresh frames within 1 s, captured after restore) ≤1 s; post-recovery max gap <1.5 s | — | 100/100 | PLAN p95 <3 s → ≤1 s | R4 3 s → 1 s |
| A9 | `spam`: 3 viewers at 50 Mbit/s; viewer 0 sends decoder stalls every 10 ms during 10–40 s; control without spam | During 10–40 s: native keys ≤ control; viewer 1–2 freezes ≤ control; viewer 1–2 full-native share ≥90% | full | 100/100 | PLAN PLI limits | Replaces "≤4 keys and 0 gaps" |
| A10 | `fifo_{512k,2m}_{1v,2v}`: shared drop-tail FIFO of 1000 packets, both directions, 40 ms, one Cubic flow; score 15–45 s; audio. Control: TCP-only on the same link | Per viewer: unique original media ≥90% of rate(o\*); fresh fps ≥90% of fps(o\*); native share ≥90% if o\* is native; max fresh gap ≤1 s; G2G p95 ≤ control probe p95 + 500 ms. TCP ≥30% | per o\* | 100/100 | PLAN competing upload | Replaces 90% offered-byte fairness |
| A11 | `fq_{512k,2m}_{1v,2v}`: HTB at the rate plus FQ-CoDel (5/100 ms, limit 1000), forward leg only; clock source; one Cubic flow; audio | TCP ≥30%; max fresh gap ≤1 s; G2G p95 ≤1 s. When n × offered ≤ link/2: unique media ≥90% offered, fresh frames ≥90% of source, full-native share ≥90% | full when the floor applies | 100/100 | — | = R4 |
| A12 | `loss_{5,20}`: Bernoulli both ways, in-order 20 ± 5 ms; clock source | Max gap ≤1 s; G2G p95 ≤ RTT/2 + 400 ms | — | 100/100 | PLAN 1–30% loss | Stronger than R4 |
| A13 | `loss_prejoin_{5,20}`: detailed source, 50 Mbit/s, Bernoulli loss both ways from join, in-order 20 ± 5 ms; audio | Full native ≤20 s after join; full-native share ≥90% over [21, 51] s | full | 100/100 | PLAN bad networks | New (R-2) |
| A14 | `join_storm`: 2 healthy viewers from 1 s; 20 joiners at 10 s + k × 0.5 s, each on its own 50 Mbit/s link | Native keys completing in [10, 30] s ≤ 8; healthy freezes in [10, 30] s ≤ native keys in that window; healthy full-native share ≥90%; each joiner TTFF ≤4 s and native by 40 s | full (healthy) | 100/100 | AGENTS isolation | New (R-5) |
| A15 | `pair_change`: path A 50 Mbit/s at 20 ms switches at 20 s to path B, 256 kbit/s at 40 ms with FIFO 1000; old reverse path has 400 ms extra delay, so old feedback arrives after the switch | Low forwarded by 23 s and for the rest of the run; max fresh gap over [25, 50] ≤1 s; G2G p95 over [25, 50] ≤440 ms | — | 100/100 | PLAN handover | New (R-10) |
| A16 | `collapse`: viewer at 10 Mbit/s collapses to 64 kbit/s (FIFO 1000) for 10 s at 20 s; healthy co-viewer at 50 Mbit/s | Viewer queue ≤1 MiB and ≤64 frames at every 10 ms sample; co-viewer 0 freezes; collapsed viewer has fresh frames within 3 s of restore and full native by 50 s | full after restore | 100/100 | AGENTS isolation | New (R-6) |
| A17 | `tl1_loss`: clock (non-flexible) source at 50 Mbit/s; every 10th TL1 picture is incomplete at SFU ingress (publisher-leg loss) | Complete forwarded pictures the receiver could not decode (missing reference) = 0; fresh frames ≥90% of source frames | — | 100/100 | — | New (R-7) |
| — | `fifo_tcp_only_{512k,2m}`: A10's links with TCP only | Reports probe p50/p95 and TCP share; A10's control | — | — | — | Diagnostic |

## Tier B: browser freeze manifest

**Row validity**
- Every row runs with E2EE on, on one frozen binary and one harness hash, under `scripts/media-lock --gate`.
- A row whose required metric is missing or null **fails**.
- Every listed row must be present. A missing row fails the cohort.

**Mandatory dimensions** (orchestrator decision 4)

| Dimension | Applies to | Requirement |
|---|---|---|
| E2EE assertion | every browser row | Viewer and sender transform counters show frames processed on native (track 0), low (track 2, when used) and audio (track 1, when present). Zero counts fail. |
| Audio | every Wi-Fi, competition and B1 row | The publisher adds a 440 Hz Opus tone. Audio continuity (gaps >200 ms) is reported per row and pooled per profile. |
| Legibility | every native row | Tesseract OCR (`eng`) of known source text ≥14 px in a 1 s-spaced sample of decoded native frames. Character accuracy ≥95% pooled over the row's native samples. |
| Resources | `capacity_synthetic_*` | SFU CPU % per viewer and peak RSS ≤1.2 × round 4. Round 4: 512 viewers / 1 stream 55.9% of one core and 1,889.3 MiB; 512 viewers / 512 streams 115.6% and 2,525.0 MiB. |
| Capacity scope | capacity rows | `capacity_synthetic_{512v1s,512v512s}`: synthetic SFU routing, as in round 4, labelled synthetic. `capacity_browser_4v`: 4 real Chromium viewers decoding with E2EE (no freezes >500 ms, full native, OCR as above). |

| Row | Gate (exact) | Native / full | Reps / pooling | Min coverage | Change vs PLAN | Change vs round-4/5 |
|---|---|---|---|---|---|---|
| `good` | TTFF p95 <1 s (nearest rank over 10 isolated joins; p50 reported); full native ≤2 s on each isolated join | full | 10 joins pooled | 10 joins | stronger (p95) | R4 p50 ≤150 ms → PLAN level |
| `join_storm` (new) | 10 viewers joining 0.5 s apart beside 2 healthy viewers: native keys ≤ ⌈5 s / 3 s⌉ + 1; healthy freezes ≤ native keys; each joiner TTFF ≤4 s and native ≤30 s | full (healthy) | 3, pooled | 3 runs | AGENTS isolation | New |
| `bad_wifi`, `bad_wifi_prejoin`, 300 s each. **Spec changed** to `delay 40ms 10ms distribution normal rate 1000mbit loss gemodel 1% 19% 100% 0%` (in-order) | Pooled over **both** histories, 3 runs each (30 min): ≤6 freezes; no gap >2 s; G2G p95 ≤400 ms; full-native share ≥90%; OCR; audio reported | full | 3 per history, pooled | 2 × 3 × 300 s | = PLAN rate, plus caps | Replaces zero-freeze, 550 and 305 |
| `bad_wifi_reorder` (old reordering spec) | Same metrics, reported only | full | 1 | — | — | Diagnostic |
| `bad_wifi_both_legs`, `loss_{1,5,10,20,30}`, `rtt_{50,100,200,400,800}`, `reorder` | Per profile, pooled frames: max fresh gap ≤1 s; G2G p95 ≤ RTT/2 + 400 ms; final fresh age ≤1 s | native | 3 per profile, pooled | 3 × 60 s | PLAN profiles | Both-legs 550 → 1,000 ms; stronger on loss/RTT rows |
| `loss_prejoin_{5,20}` (new) | Detailed source, unconstrained: native ≤20 s; then full-native share ≥90% over 30 s; OCR | full | 3 pooled | 3 runs | PLAN | New |
| Recovery: `udp_blackhole`, `server_restart`, `publisher_flap` | Recovery p95 <3 s (nearest rank, 20 trials pooled); every trial recovers; post-recovery max gap <1.5 s; sustained | — | 20 trials | 20 | = PLAN | R4: every one of 5 <3 s → p95 over 20 |
| `viewer_flap`, `network_flap` | Recovery p95 ≤1 s (nearest rank, 20 trials); post gap <1.5 s. Median >300 ms is a regression alarm | — | 20 trials | 20 | stronger than PLAN | R4 every trial ≤500 ms → p95 ≤1 s |
| `pair_change` (new): `wifi_replacement` plus a forced candidate-pair switch onto a 256 kbit/s path | No native on the new path within 10 s; max fresh gap ≤1 s after 5 s | — | 3 | 3 | PLAN handover | New |
| B1 native: `screen_native_{3,5,10}mbit_{prejoin,established}`, `production_native_10mbit_{prejoin,established}` | Native ≤20 s; then over 30 s: full-native share ≥95%, no non-native episode >2 s, `nativeCadencePass`, padding share ≤10%, `bandwidthPass`, OCR | full | 5 per row; pooled samples per row | 5 × 51 s | PLAN | R5: share 100% → 95%; padding 50% → 10% |
| B1 flap `screen_native_flap` | Each 5M interval: native ≤20 s, padding ≤10% per 30 s; 256k interval `bandwidthPass` | full at 5M | 5, pooled | 5 | — | As R5, padding 10% |
| Isolation: `terrible_viewer`, `isolation_256k` (new) | Healthy viewers: 0 gaps >500 ms; full-native share 100%; G2G p95 ≤ control run + 50 ms | full | 5, pooled; any failing run fails | 5 | AGENTS | New row added |
| AQM competition `[screen_]competing_{512kbit,2mbit}_{1v,2v}`, `_prejoin_rtt160` | Fairness floor ≥90% unique media, frames and full layer when n × offered ≤ link/2; TCP ≥30%; `bandwidthPass` | full where the floor applies | 5, pooled per row | 5 | — | Same predicate, repetitions added |
| FIFO competition `[screen_]competing_fifo_*` | Per viewer: unique original media ≥90% of rate(o\*); fresh fps ≥90% of fps(o\*); native share ≥90% when o\* is native; max fresh gap ≤1 s; G2G p95 ≤ **matched** TCP-only probe p95 + 500 ms; TCP ≥30% | per o\* | 5, pooled | 5 | competing upload | Replaces 90% fairness |
| `fifo_tcp_only_{512kbit,2mbit}` plus a prejoin rtt160 variant per FIFO row | Matched references: probe p50/p95 and TCP share | — | 5 | 5 | — | Was one 512k probe |
| `screen_keyframe_spam` | Native keys during spam ≤ no-spam control (pooled over 5 pairs); healthy-viewer freezes ≤ control; `nativePass` | full | 5 pairs | 5 | PLAN PLI limits | Replaces "≤4 keys and 0 gaps" |
| `non_pixel_clock` | **Behavioural:** canvas without pixel timestamps under the in-order Wi-Fi profile: fresh decoded frames continuous (max gap ≤1 s) and decoded fps ≥ 0.8 × offered. The internal protection-override predicate is removed. | — | 3 | 3 | — | Internal predicate replaced |
| `cpu_throttle`, `server_cpu_throttle` | Unchanged output predicates, plus publisher encode time per frame and SFU CPU reported | — | 1 | — | — | Reporting added |
| `capacity_synthetic_{512v1s,512v512s}`, `capacity_browser_4v` | See "Resources" and "Capacity scope" | — / full | 1 / 3 | — | — | Scope split |
| Stress, cold start, `screen_source`, signaling, heartbeat, `wifi_replacement`, `stale_video_recovery`, `bulk_upload` | Unchanged predicates | — | 1–3 as before | — | — | Unchanged |
| Forced TURN `loss_30` | Unchanged endpoint gate; reported separately | — | 3 | — | — | Unchanged |

### Cohort acceptance (orchestrator decision 5)

**Target.** Acceptance targets **empirical PLAN profile metrics**, not population reliability:
- raw observations (frames, gaps, trials, samples) are pooled per profile;
- the PLAN percentile or rate predicate is evaluated on the pool.

**Reporting.** Every row reports its per-run results next to the pooled value, with uncertainty:
- Wilson 90% intervals for proportions;
- exact Poisson 90% intervals for freeze counts;
- bootstrap 90% intervals (2,000 resamples, fixed seed) for percentiles.

A row whose interval straddles its threshold is labelled **marginal**. It passes or fails on the point estimate, and the label is reviewed.

**Predeclared handling**
- **Load-invalid runs** (the harness's load check) are not media results. They are rerun at most twice, and the reruns recorded. Still invalid → the row is incomplete.
- **Incomplete runs** (harness error, missing or null metric) count as **failures**.
- **Media failures are never rerun.**
- **No subsets.** A cohort is every manifest row on one binary in one sitting. A partial or "critical" subset never selects a pin.
- **One extension.** The orchestrator may schedule one additional complete cohort on the same binary, declared before seeing marginal rows' details. Its observations are then pooled with the first; never a choice of the better cohort.

**Error and power note.**
- A per-row rule of "4 of 5 runs pass" at a true per-run rate of 0.95 accepts with probability 0.9774. Across 95 independent rows, a clean cohort has probability 0.9774^95 = **11.4%**, and a 70%-reliable row still passes its screen 52.8% of the time. That is why rows are pooled metrics, not run counts.
- **Wi-Fi power:** pooling both histories over 3 runs each gives 30 min, so the gate is ≤6 freezes. With Poisson counts:

  | True freeze rate | Mean freezes in 30 min | Probability of acceptance |
  |---|---|---|
  | 0.6 per 5 min | 3.6 | 0.926727 |
  | 1.0 per 5 min (PLAN boundary) | 6 | 0.606303 |
  | 1.5 per 5 min | 9 | 0.206781 |

  The test discriminates at roughly 1.5× PLAN, not at PLAN itself. Tighter discrimination needs longer observation; that is the orchestrator's call before freeze.
- **Recovery rank:** rank 19 of 20 is the empirical p95. It is not a confidence bound on the population p95.

## When a gate is wrong

**Evidence that can change a gate:** a gate changes only at a checkpoint, and only with one of:
1. **Physical bound.** A calculation shows the predicate cannot be met by any implementation, for example key bits ÷ fair share > deadline.
2. **Product mismatch.** The predicate measures something other than the product requirement.

The fix makes the gate statistical or PLAN-level, never weaker than PLAN.

**What is not evidence:**
- **Same-pin variability**, where one unchanged pin passes and fails, shows noise. It motivates a statistical gate, not automatically a different threshold.
- **libwebrtc P2P reference failure** triggers investigation, not relaxation (procedure below).

**Evidence that the design is wrong:** the simulator reproduces a failure deterministically, and the fix needs a new state field, constant or special case outside the components. Stop and redesign that component at a checkpoint.

### libwebrtc reference control (investigation only)

**Harness.** `web/torture-p2p.mjs`, to be added by A3 and reviewed like a gate. It runs Chromium ↔ Chromium P2P, matched to the row on:
- flow count;
- source, codec, `scalabilityMode`, `contentHint` and caps;
- join and impairment history;
- the exact netem and queue configuration on the same leg(s).

Score only comparable metrics, with the same `torture-metrics.mjs` functions, in A/B order with the PassStream row.

**Interpretation.** P2P adapts the encoder to the bottleneck and differs in repair and encryption overhead, so a failing P2P run is not an upper bound for an SFU. A row failed by both is investigated: check the physical bound and the product mismatch. The gate is relaxed only if one is shown.

### Evidence behind replaced gates

All numbers are from the approach review, which mined 3,905 archived observations, unless stated otherwise.

**`bad_wifi` zero-freeze and p95 ≤305**
- **Origin:** e248ca9 set p95 ≤305 from round 2's measured 304/305 ms.
- **Same build, 0 or 1 freezes:**
  - `61f9283`: 0 and 1;
  - `53ccead`: 0, 0, 1, 1;
  - `334030d4`: 0 and 1.
- **Pass rates over 10 runs:** at λ = 0.3 per 5 min, the zero rule passes 5% and PLAN's rule 69%.

**`bad_wifi` netem spec**
- **Measurement:** `test/torture/netem_order.py` on kernel 7.2.14. The old spec reorders 82% of packets in keyframe-sized bursts (median displacement 75, maximum 181).
- **Fix:** `rate 1000mbit` gives 0% reordering, with steady-packet delay unchanged (p50 40.1 vs 40.8 ms). Whole bursts arrive later (p50 59.7 ms).
- **Results:** `test/torture/results/netem-order.json`.
- **Loss model:** `test/torture/netem_gemodel.py` measured gemodel loss at 5.48% in runs of mean 5.76 packets (`netem-gemodel.json`). sfusim reproduces 5.0% and 5.54.

**Both-legs max gap 550 → 1,000 ms:** 550 was a best-observed tightening; the same-build repeat pass rate was 0.60.

**FIFO 90% fairness**
- Pass rates: 49% (`competing_fifo_512kbit_1v`) and 31% (`screen_competing_fifo_512kbit_2v`). The unchanged pin `9d5783f7` scored 1/5 and 3/5.
- Delay-based congestion control against Cubic in a shared FIFO is the known starvation regime.
- Physical bound on the detailed rows: native T0 needs 1.13 Mbit/s before overhead, against a fair share of 0.17–1 Mbit/s. The deliverable option is low at about 100 kbit/s, which the replacement's o\* encodes.

**Spam "≤4 keys and 0 gaps"**
- Every native key under a 2 Mbit/s cap is followed by 840 and 738 ms ingress gaps (round-4 screen-baseline cold start, `r4-final-low-budget-cold-baseline.json`). Allowing keys therefore implies healthy-viewer gaps.
- The replacement compares against a control.

**`good` TTFF p50 ≤150 ms:** 77% pass rate; PLAN says p50 <1 s.

**Flap recovery on every trial ≤500 ms:** per-trial pass rate 0.90–0.91, so 5/5 passes 59% of the time.

**`native_share===1`:** fails on one half-second sample (for example 61/62).

## Noise controls

- Run A/B/A/B against the previous freeze on noise-prone rows (`screen_source`, both-legs Wi-Fi, flaps).
- At most 2 fix attempts per failing row per round, then escalate.


## C0 r3 (orchestrator decisions)

**Substantive changes authorised on 2026-11-08; Opus reviews them at C2.** The F-1/F-2 discovery changes in CONTROLLER.md are controller policy, not weaker acceptance predicates.

- Tier A adds `native_1500k_{prejoin,return}`: detailed source, 1.5 Mbit/s, audio, native pixels at any TID within 20 s of join/return; native share ≥90% for the following 30 s; fresh frames ≥90% of native T0 source cadence; max fresh gap ≤1 s; retain the 3/5/10 Mbit/s full-native rows. Both new properties run every seed.
- Deterministic controller regressions cover equal-spacing probes with 0/1/6 interior losses and seeded 5%/20% Bernoulli, loss overdrive caps, clean failed-cluster evidence, generation fences, key admission/start/last-departure/abort and assembled-key deadlines. A13 remains mandatory at 5% and 20%.
- The Wi-Fi cohort is exactly 3 ×300 s per history, six runs/30 min pooled, ≤6 freezes. Poisson acceptance at true 0.6/1.0/1.5 freezes per five minutes is 0.926727/0.606303/0.206781.
- Security continuity: deterministic epoch-switch/missing-key recovery regressions, unchanged 300 ms revocation cutoff, unchanged FREEZE_MS=500 on unaffected viewers, and full `test/e2e/e2ee.mjs` passing three consecutive runs.
- A1 implements the in-order jitter netem `rate 1000mbit` and the Tier B definitions above exactly. Its short gated browser smoke is diagnostic evidence; it does not select a freeze pin or replace A3.
