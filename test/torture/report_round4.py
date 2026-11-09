#!/usr/bin/env python3
"""Render only measured round-4 evidence, with an exact single-binary check."""
import datetime
import gzip
import json
import math
import os
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
RESULTS = ROOT / "test/torture/results"
PREFIX = os.environ.get("REPORT_RUN_PREFIX", "r4-final-low-budget")

def read(path):
    raw = path.read_bytes()
    return json.loads(gzip.decompress(raw) if path.suffix == ".gz" else raw)

def num(value):
    return "—" if value is None else f"{value:.1f}"

def pct(values, fraction):
    ordered = sorted(v for v in values if v is not None)
    return ordered[min(len(ordered)-1, math.ceil(len(ordered)*fraction)-1)] if ordered else None

def measures(row):
    values = row.get("metrics", {})
    return values if isinstance(values, list) else [values]

def maximum(row, key):
    return max((m[key] for m in measures(row) if m.get(key) is not None), default=None)

def gap(value):
    # The harness retains gaps above 500ms; zero means none crossed that limit.
    return "≤500" if value == 0 else num(value)

def wifi_regression_failed(row):
    if row.get("name") != "bad_wifi" or row.get("observation_s", 0) < 300:
        return False
    metrics = row.get("metrics", {})
    return metrics.get("freezes_over_500ms", math.inf) != 0 or metrics.get("g2g_p95_ms", math.inf) > 305

paths = [p for p in RESULTS.glob(PREFIX + "*.json*") if p.suffix == ".gz" or not Path(str(p) + ".gz").exists()]
runs = sorted((read(p) for p in paths), key=lambda r: r["created"])
if not runs:
    raise SystemExit(f"no measured {PREFIX} runs")
pins = {r["binary_sha256"] for r in runs}
if len(pins) != 1:
    raise SystemExit("refusing to combine different server binaries")
pin = pins.pop()
rows = [(run, row) for run in runs for row in run["results"]]
load_rejected = [(run, row) for run in runs for row in run.get("load_rejected", [])]
# Recompute the new competition floor from retained whole-frame counts. Original
# pass booleans stay untouched in the raw archives; all other gates are identical.
frame_boundary_corrections = []
for run, row in rows:
    if "competing_" not in row["name"] or not row.get("fairness"):
        continue
    fair = True
    for m, f in zip(measures(row), row["fairness"]):
        if not f["applicable"]:
            continue
        seconds = f.get("observation_s") or (m["frames"] / f["decoded_fps"] if f["decoded_fps"] > 0 else 0)
        required = math.floor(f["offered_fps"] * seconds * .9 + .5)
        fair &= seconds > 0 and m["frames"] >= required and f["received_bps"] >= f["offered_bps"] * .9 and f.get("full_layer_share", 0) >= .9
    quality = all(m.get("pixel_samples", 0) > 0 and m.get("g2g_p95_ms", math.inf) <= 1000 and m.get("max_gap_ms", math.inf) <= 1000 and m.get("last_frame_age_ms", math.inf) <= 1000 for m in measures(row))
    score = bool(fair and quality and row.get("impairment", {}).get("verified") and row.get("source_workload", {"pass": True}).get("pass") and row["tcp_link_share"] >= .3 and all(v.get("quality", {}).get("repair", {}).get("initialRTTMs", 0) > 0 for v in row.get("viewers", [])))
    if score != row["pass"]:
        frame_boundary_corrections.append((run, row, row["pass"], score))
        row["pass"] = score

# The requested round-2 Wi-Fi regression target is stricter than its old raw
# gate. Preserve that original archive while scoring the final contract here.
for _, row in rows:
    if wifi_regression_failed(row):
        row["raw_pass"] = row["pass"]
        row["pass"] = False

by_name = {}
for run, row in rows:
    by_name.setdefault(row["name"], []).append((run, row))

baseline = read(RESULTS / "r3-round3-final-full.json.gz")
old = {r["name"]: r for r in baseline["results"]}
missing = sorted(set(old) - set(by_name))
new_required = {"non_pixel_clock", "screen_source", "screen_cold_start", "viewer_no_heartbeat"}
for source in ["", "screen_"]:
    new_required.update(source + name for name in [
        "viewer_256k", "bandwidth_flaps", "competing_512kbit_1v", "competing_2mbit_1v",
        "competing_512kbit_2v", "competing_fifo_512kbit_1v", "competing_fifo_512kbit_2v",
        "competing_fifo_2mbit_1v", "competing_fifo_2mbit_2v",
    ])
missing_new = sorted(new_required-set(by_name))
gate_rows = [(run, row) for run, row in rows if not row.get("diagnostic")]
failed = [(run, row) for run, row in gate_rows if not row.get("pass")]
lines = ["# PassStream phase 1 torture report", "",
    f"Generated {datetime.datetime.now(datetime.timezone.utc).isoformat()} from `{PREFIX}*.json[.gz]` by `test/torture/report_round4.py`.", "",
    f"Pinned server SHA-256: `{pin}`. Source commit(s): {', '.join('`'+r+'`' for r in sorted({r['commit'] for r in runs}))}.", "",
    f"Measured browser rows: {len(rows)}; gate observations {len(gate_rows)-len(failed)} pass, {len(failed)} fail; {len(rows)-len(gate_rows)} diagnostic observations. Missing round-3 rows: {', '.join(missing) or 'none'}. Missing required new rows: {', '.join(missing_new) or 'none'}.", "",
    f"All browser runs use this exact static binary, nice -n 10, unprivileged user/network namespaces, localhost and a 1500-byte MTU. Each scenario waits for one-minute load ≤{max(r['load_limit'] for r in runs):g} on this 12-CPU host; the predeclared peak limit of {max(r['observed_load_limit'] for r in runs):g} rejects loaded attempts. Declared entry limits across retained runs: {", ".join(str(v) for v in sorted({r["load_limit"] for r in runs}))}. The entry check precedes browser launch; the table's first sample is taken after launch. Only load violations trigger automatic retries. Media failures at valid load remain failures. The table preserves start/end/peak load. No physical phone, portal picker, PIN or browser SFrame integration is covered.", "",
    'The ordinary source remains 960×540 at 15 fps. The detailed source is a 2560×1440 dense editor and animated terminal capped at 2 Mbit/s, with measured keyframes ≥150 KB. Its bandwidth rows wait for four consecutive 500ms samples at ≥12 encoded fps and 1.5–2.5 Mbit/s, including a new qualification after viewers join. Offered bitrate remains scored during impairment. Detailed cold startup is a separate publisher-only diagnostic, not a gate. Ordinary-source gates and setup are unchanged.', "",
    'Both sources include a separate 320×180 rendition for constrained viewers: 80 kbit/s for sources at least 1920 px wide and 100 kbit/s otherwise. Growing ≥600ms residence temporarily retains only its temporal base; stable transit returns to the cadence funded by the same measured wire budget. A small native stream changes to it only after its budget cannot carry the full stream and ≥600ms added receiver RTT persist for 3s. Its return needs 500ms sustained clear delay or a completed loss-free full-stream proof at moderate residence with an affordable refresh. The actual arriving key is checked from ≥120ms added delay. This preserves the full-stream fairness floor on capable links while avoiding native-rendition oscillation on a resident FIFO. This explicitly lowers resolution to deliver a small independent refresh. Small native returns use a received window of at least 500ms tied to the exact bounded trial. Later low-rate traffic cannot erase the completed measurement or qualify a different trial. All trial packets, including padding, contribute fresh loss evidence; more than 2% trial loss blocks admission. Admission waits for the feedback tail. Trials offer 120% of native demand. Small native returns and temporal-only switches require the measured media demand; the numerical wire budget separately reserves headroom and repair. Large-key returns require measured steady throughput and enough received capacity for a 300ms refresh; offered trials include 200ms refresh headroom. Actual limited-path trials grow by at most 25% above sustained delivery. Resident ordinary padding trials keep at most max(20KiB, a half-second of native demand), within their existing bound; large-screen trials on a resident FIFO have a 6KiB outstanding cap; clear-path trials start at 36KiB and expand to 64/128/256KiB only after ≥2/4/6Mbit/s of received proof. Stable delivery proof survives encoder pauses but fresh loss or RTT growth revokes it. The actual incoming refresh is checked again against both the budget and received proof before replacing a live low decoder, since it can be twice the previous measured key. Native refreshes without current full-capacity proof retain the base temporal layer while paced debt drains, then resume temporal admission at valid switching points. A clear, loss-free received proof funding the actual large key admits those switching points with the refresh, avoiding an unnecessary base-only catch-up hold. Their recent measured capacity survives through queued dependent pictures for at most one second; new delay or loss revokes it. Clean large-source proof retries after 400ms; constrained paths retain the 2s spacing. Native refresh trains retain 100ms of feedback slack beyond current RTT. An active large native key and its queued catch-up additionally cap outstanding bytes at max(48KiB, physical bandwidth-delay product +16KiB), so a recently proved wide upgrade cannot leave a whole refresh ahead of the next narrow-path fallback. Ordinary/low-rendition traffic and steady native flight keep their existing propagation allowance. A current ≥75% delivery collapse with growing local backlog cannot use past full-rate decoding to suppress early feedback delay. Ordinary returns into ≥600ms of added receiver RTT also need their remaining serialized key and one-way transit to fit the 750ms refresh envelope. Their padding pauses while that key cannot arrive fresh at the native wire budget; fresh low video continues until the queue clears. Partial native temporal layers can also test capacity with bounded padding, avoiding a fresh encoding/keyframe merely to recover fps. Temporal-only upgrades use VP9 switching points; join and spatial refreshes remain coalesced.', "",
    'Decoder freshness comes from advancing requestVideoFrameCallback presentation/RTP metadata and recent receive time, independently of content. The same counter/sample timestamp also travels on authenticated WebSocket pings, so delayed SCTP setup or unreliable heartbeat delivery cannot be the only feedback path. Both transports share a monotonic serialized counter; zero and publisher reports do not establish viewer decoding, and signaling does not refresh RTCP transport liveness. Zero counts do not establish decoder reporting. The non-pixel-clock source verifies fresh full-layer feedback under measured loss with the repair protection active. Pixel timestamps instrument latency only. Join PLIs coalesce per room and encoding for 500ms. Liveness-gap PLIs bypass obsolete holdoff; normal holdoff is serialization plus two RTTs. Short outages retain pre-outage budgets and replacement PCs inherit their own viewer history. The first moderate excess-RTT sample seeds history when fresh decoding and received delivery cover the existing ceiling; subsequent sustained growth still cuts, while unknown, queued and stopped paths retain the original response. ICE/DTLS connection attempts are bounded at 45s, then newly connected media gets its existing 15s SCTP startup grace; fresh decoded progress prevents a blind startup replacement. Browser recovery also uses actual inbound video packet deltas when SCTP source counters are delayed; first/unchanged counters, audio and consent cannot establish video activity.', "",
    'All media, parity, retransmission and padding share a wire bucket with credit max(4×1500 bytes, rate×100ms). Successful probes and stable windows reset backoff; clean paths probe by 25% per 100ms and source keyframe backlog cannot fail a network-capacity probe. Layer requirements round upward and admission uses the same calculation, so a retained minimum cannot sit one bit/s below its own layer threshold. Selected-layer decoder productivity uses quarter/half/full source fps, with smoothed source counters and recent receiver windows for sparse ordinary screens. Repaired loss cannot indefinitely defer a productive layer’s recovery probe. Sustained heavy loss, poor selected-layer fps and independently increased RTT can bound a join burst before video stops entirely. Small, recently full-decoded streams retain their proven bounded reservation through source-rate and receiver-rate dips; loss, stale decoding, severe residence and large-stream delivery collapse revoke it. Nearby queue loss requires corroborated fast RTT before preferring NACK over parity. Fresh ICE evidence establishes a post-loss response baseline, then needs two new responses below 20ms. Slower checks on the same pair stop renewing the ten-second hold; they cannot erase independent receiver proof. Changed candidate pairs or reset response counters revoke the preference. Known pre-media propagation of at least 30ms cannot qualify. A shared native key that cannot serialize within 750ms requests the small independent refresh. Expiry behind repair compacts the queue without allocating a prefix. A paced non-flexible L1T3 middle-layer expiry keeps independent bases and suppresses dependent enhancements until U on retained TID0; U on the lost TID1 cannot repair itself; unknown dependencies and actual base loss still require a keyframe. Full-layer timestamp evidence excludes that recovery interval.', "",
    'Publishers negotiate REMB with absolute send time; their receiver controller responds to sustained transit-delay growth and measured wire delivery, avoiding the publisher collapse caused by raw-loss TWCC despite successful repairs. Viewer feedback remains TWCC. Wi-Fi keeps parity and short bounded repair retries where capacity permits. Positive original-media loss retains its extra-copy eligibility for two seconds across clean or missing feedback; send-time RTT, queue and 3.3× wire-headroom checks still apply. Ordinary delta repair’s extra copy waits max(100ms, min(160ms, 1.5×RTT)). An independently identified small keyframe sends its existing first parity copy immediately and the second at 80ms; severe keyframes retain immediate/80ms/RTT-scaled/180ms timing. This changes timing, not copy counts. All copies share the same wire bucket. Independently severe >25% media loss may add immediate and 180ms copies only with a complete ≥5.3× native wire reservation, for four parity copies total. This covers compounded relay loss; RTT, queue, key-size and two-second evidence limits remain, and all media/repair bytes stay paced. Delayed parity queues independently of partial media assembly, and excess parity is suppressed at the shared queue bound instead of evicting video. A productive small native stream may probe its next temporal layer despite raw-loss trial failure only with parity enabled, no queue-loss classification, a fresh low-residual RR, 50–150ms RTT, clear local queue/excess delay, ≥3.3× native wire budget and fresh received capacity ≥110% of native media demand. It requests no key and raises no wire budget. Nearby AQM, resident FIFO, large keys, stale feedback, low renditions and stalled decoders retain their proof gates. Known pre-media propagation cannot be reclassified as a nearby queue by compressed TWCC. Fresh receiver-report residual loss and actual receiver RTT growth keep repair available when loss delays TWCC; stale reports and initially nearby competing queues retain the early feedback signal. Repair-heavy paths use their rate pacer. Other constrained native refreshes and capacity padding retain an outstanding-byte bound as RTT grows, with at most one second of feedback transit. Small steady deltas retain their wire bucket and 100ms queue deadline without waiting for old feedback; large native trains retain their flight bound. Upstream repair sends the first of eleven NACK attempts immediately, preserving 20ms slots and the 300ms deadline. Server and browser decoded progress also keep an already working PC alive if SCTP heartbeat setup lags.', "",
    'Every bandwidth interval requires p95 G2G ≤1,000ms, maximum fresh gap ≤1,000ms and final fresh age ≤1,000ms. Each bandwidth-flap step also requires recovery ≤3s from captures made after the step. Competition uses 10s convergence for AQM and FIFO, TCP ≥30%, and a full-stream fairness floor when aggregate offered media is ≤50% of the link: ≥90% media bytes, ≥90% decoded frames and ≥90% native full-layer samples. The added FIFO rows include one viewer and 2 Mbit/s. Good-link targets are TTFF p50 ≤150ms and decoded full layer ≤2s; viewer/network flap p95 ≤500ms. Viewer Wi-Fi retains its original ≤1 gap >500ms per 300s gate; the round-2 regression target additionally requires zero such gaps and p95 G2G ≤305ms; both-leg max gap is ≤550ms. The report applies the requested zero-gap Wi-Fi regression target even if the older raw gate passes; raw archives stay unchanged. Zero in the retained max-gap metric means no gap crossed 500ms and is displayed as ≤500.', "",
    'The harness disables TSO/GSO/GRO only inside its network namespace and records those settings. This corrects loopback TCP packets that had exceeded the configured 1500-byte MTU, without changing sources, rate limits, convergence or pass thresholds. Length-only capture evidence and the original scores remain archived.', "",
    "## Before / after", "", "| Item | Round 3 / review | Round 4 pin |", "|---|---|---|"]

def last(name):
    return by_name.get(name, [(None, {})])[-1][1]

good = last("good")
full = [v for v in good.get("full_layer_ms", []) if v is not None]
nonpixel = last("non_pixel_clock")
nonpixel_protection = [s for v in nonpixel.get('viewers', []) for s in v.get('samples', []) if s.get('layer') == 'L1T3' and (repair := s.get('repair', {})).get('decoderReported') and repair.get('freshAgeMs', float('inf')) < 1000 and repair.get('wireBudget', 0) > 2*repair.get('targetBitrate', float('inf')) and (repair.get('estimate') or {}).get('averageLoss', 0) > .01]
screen = last("screen_source")
# Heartbeat samples retain the latest native refresh size, not every packet.
# Say sampled explicitly; the initial source-join key is not the largest one.
screen_keys = [value for _, row in rows for peer in [row.get("publisher", {}), *row.get("viewers", [])] for sample in [peer.get("quality", {}), *peer.get("samples", [])] for key in ["sourceKeyframeBytes", "keyframeBytes"] if (value := sample.get("repair", {}).get(key)) is not None and row["name"].startswith("screen_")]
largest_screen_key = max(screen_keys, default=None)
lines += [
    f"| Content-independent freshness | Real capture reports zero; protection unavailable | {'PASS' if nonpixel.get('pass') and nonpixel_protection else 'FAIL / not measured'}: non-pixel-clock source; {maximum(nonpixel,'frames')} decoded frames; {len(nonpixel_protection)} samples with fresh L1T3 decoding, measured loss and a wire budget >2× GCC's target |",
    "| Probe / join behavior | Supplied review simulation: 16.5 s clean ramp; 74.9 s with keyframe hits; backoff latched at 30 s | Measured unit simulation: 1.1 s clean and with 25 ms local backlog, backoff 2 s; focused PLI tests and browser fanout below |",
    f"| TTFF and full layer | 509/709 ms TTFF p50/p95; ~6.5 s full layer | {num(good.get('ttff_p50_ms'))}/{num(good.get('ttff_p95_ms'))} ms; decoded full-layer max {num(max(full,default=None))} ms |",
    f"| Keyframe / burst cost | ~28 KB canvas; 32 KiB credit at 256 kbit/s (~1,024 ms) | Screen join keyframe {num(screen.get('keyframe_bytes'))} bytes; largest sampled native key {num(largest_screen_key)} bytes, offered {num(screen.get('offered_bps'))} bit/s; low key {num(screen.get('thumbnail_keyframe_bytes'))} bytes, native full {num(screen.get('native_full_layer_ms'))} ms; 6,000-byte credit at 256 kbit/s (187.5 ms) |",
    "| Competition | Missing FIFO one-viewer/2 Mbit/s rows; two-viewer AQM convergence 20 s | All rows below use 10 s; media-byte/fps fairness floor is scored |",
]
for event, before in [("viewer_flap",1674),("network_flap",1455)]:
    trials = [r for _,r in rows if r["name"]==event or r["name"].startswith(event+"_trial")]
    times = [r.get("recovery_ms") for r in trials]
    lines.append(f"| {event} p50/p95 | p95 {before} ms | {num(pct(times,.5))}/{num(pct(times,.95))} ms, {sum(bool(r.get('pass')) for r in trials)}/{len(trials)} row passes |")
for run,row in rows:
    if row["name"]=="bad_wifi":
        m=row["metrics"]
        lines.append(f"| Wi-Fi ({run['run']}) | 1 gap / 300 s; maximum 1,050–1,067 ms; p95 486–488 ms | {m['freezes_over_500ms']} gaps / {row['observation_s']} s; maximum {gap(m['max_gap_ms'])} ms; p95 {num(m['g2g_p95_ms'])} ms |")
both=last("bad_wifi_both_legs")
lines += [f"| Both-leg Wi-Fi | Maximum 2,952 ms | Maximum {gap(maximum(both,'max_gap_ms'))} ms |",
    "| Expiry allocation | Allocates a prefix on every expiry behind repair | Focused regression verifies zero allocations |", "",
    "## Gate table", "", "| Run / scenario | Result | G2G p95 ms | Max fresh gap ms | Recovery ms | TCP share | Load start → end (1/5/15 min) |",
    "|---|---|---:|---:|---:|---:|---|"]
for run,row in rows:
    load = lambda key: " / ".join(row.get(key, "").split()[:3])
    share=row.get("tcp_link_share")
    lines.append(f"| {run['run']} / {row['name']}{' (forced TURN/UDP)' if row.get('forced_relay_udp') else ''} | {'OBSERVATION' if row.get('diagnostic') else 'PASS' if row.get('pass') else '**FAIL**'} | {num(maximum(row,'g2g_p95_ms'))} | {gap(maximum(row,'max_gap_ms'))} | {num(row.get('recovery_ms'))} | {'—' if share is None else f'{share*100:.1f}%'} | {load('load_start')} → {load('load_end')}; peak {num(row.get('load_peak_1m'))} |")
    for interval in row.get("intervals", []):
        m=interval["metrics"]
        lines.append(f"| ↳ {interval['rate']} | {'PASS' if interval['pass'] else '**FAIL**'} | {num(m.get('g2g_p95_ms'))} | {gap(m.get('max_gap_ms'))} | {num(interval.get('recovery_ms'))} | — | same scenario |")
lines += ["", "## Competition fairness", "",
    "The media floor applies only when aggregate offered media is at most half the link. Each applicable viewer must receive at least 90% of offered media bytes and decode 90% of offered frames, with the required count rounded to the nearest whole frame (at most 0.5 frame boundary tolerance). TCP still needs at least 30% in every competition row. Original raw pass booleans are retained; any deterministic boundary correction is listed below.", "",
    "| Scenario / viewer | Floor applies | Offered kbit/s | Received kbit/s | Received / offered | Decoded / offered fps | Full-layer samples (≥90% when applicable) |", "|---|---|---:|---:|---:|---:|---:|"]
for _, row in rows:
    for i, f in enumerate(row.get("fairness", [])):
        offered=f.get("offered_bps", 0)
        ratio=f.get("received_bps", 0)/offered if offered else None
        lines.append(f"| {row['name']} / {i+1} | {'yes' if f.get('applicable') else 'no'} | {num(offered/1000)} | {num(f.get('received_bps',0)/1000)} | {'—' if ratio is None else f'{ratio*100:.1f}%'} | {num(f.get('decoded_fps'))} / {num(f.get('offered_fps'))} | {f.get('full_layer_share',0)*100:.1f}% |")
for run, row, original, score in frame_boundary_corrections:
    lines.append(f"- Whole-frame boundary correction: {run['run']} / {row['name']}, original {'PASS' if original else 'FAIL'} → {'PASS' if score else 'FAIL'}. Retained frame counts: {', '.join(str(m['frames']) for m in measures(row))}. Original unrounded fps: {', '.join(str(f['decoded_fps']) for f in row['fairness'])}. The original archive is unchanged.")

lines += ["", "## Severe-loss route limits", "",
    "The existing loss30 gate checks fresh playback at the endpoint. A pass does not establish continuous playback throughout severe loss; startup, gaps and PC replacements remain visible here.", "",
    "| Run / route | TTFF ms | Fresh frames | Max fresh gap ms | Final fresh age ms | PC replacements | Endpoint gate |",
    "|---|---:|---:|---:|---:|---:|---|"]
for run, row in rows:
    if row["name"] == "loss_30":
        viewers = row.get("viewers", [])
        lines.append(f"| {run['run']} / {'forced TURN/UDP' if row.get('forced_relay_udp') else 'selected route'} | {num(max((v.get('ttff', 0) for v in viewers), default=None))} | {maximum(row, 'frames')} | {gap(maximum(row, 'max_gap_ms'))} | {num(maximum(row, 'last_frame_age_ms'))} | {sum(v.get('replacements', 0) for v in viewers)} | {'PASS' if row.get('pass') else '**FAIL**'} |")

lines += ["", "## Detailed-source workload and cold startup", "",
    "Publisher encoder/rate-control startup can lower encoded fps before media reaches SFU forwarding. The cold rows below measure that first 10-second window independently, with no viewers; their gaps are not attributed to the SFU. Time to steady fps is the first of four consecutive 500ms samples at ≥12 fps. `screen-baseline` keeps a fixed 2 Mbit/s cap. `screen` temporarily raises that cap to 4 Mbit/s for 2.5s after an encoded keyframe, then returns to 2 Mbit/s; it changes neither resolution nor scalability mode and does not request additional keys. `screen-startup` tests a fixed initial 3 Mbit/s cap. The table's parameter value is sampled at the end, not the initial cap. The bandwidth workload remains qualified at 1.5–2.5 Mbit/s. Cold startup quality is reported separately and is not a gate.", "",
    "| Run / cold profile | Final maxBitrate | Time to steady fps ms | Min fps, first 10s | Min positive fps | Steady offered Mbit/s | Primary ingress gaps >500ms, first 10s | Viewer fresh gaps >500ms, first 10s |",
    "|---|---:|---:|---:|---:|---:|---|---|"]
for run, row in rows:
    if row["name"] == "screen_cold_start":
        encoding=row.get("parameters",{}).get("encodings",[{}])[0]
        ingress=", ".join(num(g["duration_ms"]) for g in row.get("publisher_gaps_first10s", [])) or "none"
        viewer=", ".join(num(g["duration_ms"]) for g in row.get("viewer_fresh_gaps_first10s", [])) or ("none" if "viewer_fresh_gaps_first10s" in row else "not observed (publisher only)")
        offered=row.get("steady_bitrate_p50_bps")
        lines.append(f"| {run['run']} / {row.get('source')} | {encoding.get('maxBitrate')} | {num(row.get('time_to_steady_fps_ms'))} | {num(row.get('min_fps_first10s'))} | {num(row.get('min_positive_fps_first10s'))} | {num(offered/1e6 if offered is not None else None)} | {ingress} | {viewer} |")
lines += ["", "| Qualified bandwidth observation | Qualification ms | Offered Mbit/s | Encoded fps | Workload gate |", "|---|---:|---:|---:|---|"]
for run, row in rows:
    workloads=[(row['name'],row.get('source_workload'))]+[(row['name']+" / "+i['rate'],i.get('source_workload')) for i in row.get('intervals',[])]
    for name, workload in workloads:
        if workload:
            lines.append(f"| {name} | {num(workload.get('qualification',{}).get('elapsed_ms'))} | {num(workload.get('offered_bps',0)/1e6)} | {num(workload.get('encoded_fps'))} | {'PASS' if workload.get('pass') else '**FAIL**'} |")
lines += ["", "The detailed-source join row separately records the publisher after its requested keyframe. These post-join samples are diagnostic and do not replace pre-impairment workload qualification.", "",
    "| Run / source join | Post-join offered Mbit/s | Post-join encoded fps |", "|---|---:|---:|"]
for run, row in rows:
    encoder = row.get("post_join_encoder")
    if encoder:
        offered = encoder.get("offered_bps")
        lines.append(f"| {run['run']} / {row['name']} | {num(offered/1e6 if offered is not None else None)} | {num(encoder.get('encoded_fps'))} |")

lines += ["", "## Host-load validity", ""]
packet_model_valid = all(len(run.get('offloads', [])) == 3 and all(flag.get('after') == 0 for flag in run['offloads']) for run in runs)
load_valid = all(run.get("nice") == 10 and run.get("low_load_required") and 0 < run.get("load_limit", 0) <= 1 and 0 < run.get("observed_load_limit", 0) <= 3 for run in runs) and all(not row.get("load_invalid") and float(row.get("load_start", "inf").split()[0]) <= min(3, run["observed_load_limit"]) and row.get("load_peak_1m", float("inf")) <= run["observed_load_limit"] for run, row in rows)
lines.append(f"1500-byte TCP packet model: {'PASS' if packet_model_valid else 'FAIL / missing offload metadata'}.\n")
lines.append(f"Predeclared final-run load/nice contract: {'PASS' if load_valid else 'FAIL / missing metadata'}. Rejected host-load attempts: {len(load_rejected)}. They are not presented as passing media observations.")
if load_rejected:
    lines += ["", "| Run / scenario / attempt | Original quality result | Peak load | G2G p95 ms | Max fresh gap ms |", "|---|---|---:|---:|---:|"]
    for run, row in load_rejected:
        lines.append(f"| {run['run']} / {row['name']} / {row.get('attempt')} | {'PASS' if row.get('pass') else 'FAIL'} | {num(row.get('load_peak_1m'))} | {num(maximum(row, 'g2g_p95_ms'))} | {num(maximum(row, 'max_gap_ms'))} |")
lines += ["", "## Remaining failures and missing evidence", ""]
if not failed and not missing and not missing_new:
    lines.append("No browser row failed and every required scenario is present on this pin.")
for run,row in failed:
    detail=row.get("error") or json.dumps({k:row[k] for k in ["metrics","fairness","recovery_ms","full_layer_ms"] if k in row},separators=(",",":"))
    lines.append(f"- {run['run']} / {row['name']}: {detail}")
if missing or missing_new:
    lines.append(f"- Missing evidence: {', '.join(missing+missing_new)}")
wifi_runs = [r for _,r in rows if r["name"] == "bad_wifi" and r.get("observation_s", 0) >= 300]
wifi_ok = len(wifi_runs) >= 2 and all(r.get('pass') and r.get('metrics', {}).get('freezes_over_500ms') == 0 and r.get('metrics', {}).get('g2g_p95_ms', math.inf) <= 305 for r in wifi_runs)
lines.append(f"- Two consecutive 300 s viewer Wi-Fi observations, zero >500 ms gaps and p95 ≤305 ms in every retained observation: {'PASS' if wifi_ok else 'FAIL / missing'} ({len(wifi_runs)} retained).")
for event in ["viewer_flap","network_flap"]:
    times=[r.get("recovery_ms") for _,r in rows if r["name"]==event or r["name"].startswith(event+"_trial")]
    p95=pct(times,.95)
    lines.append(f"- Regression target {event} p95 ≤500 ms: {'PASS' if len(times)>=5 and p95 is not None and p95<=500 else 'FAIL / missing'} ({num(p95)} ms).")
lines += ["", "## Synthetic capacity", "", "| Case | Result | CPU % of one core | Peak RSS MiB | Load start → end |", "|---|---|---:|---:|---|"]
capacity_cases = []
for path in sorted((RESULTS/"capacity").glob(PREFIX+"*.json")):
    r=read(path)
    capacity_cases.append(r)
    if r["binary_sha256"]!=pin:raise SystemExit("capacity evidence differs from browser pin")
    peak_load = max(float(s.split()[0]) for s in [r['load_start'], r['load_end'], *r.get('load_samples', []), *r.get('setup_load_samples', [])])
    r['report_load_valid'] = 0 < r.get('load_limit', 0) <= 1 and 0 < r.get('observed_load_limit', 0) <= 3 and peak_load <= r['observed_load_limit'] and r.get('nice') == 10 and float(r.get('entry_load', 'inf').split()[0]) <= r['load_limit']
    lines.append(f"| {r['viewers']} viewers / {r['streams']} streams | {'LOAD REJECTED' if r.get('load_invalid') else 'PASS' if r['pass'] and r['report_load_valid'] else '**FAIL**'} | {num(r.get('server_cpu_pct'))} | {num(r.get('server_rss_peak_mib'))} | {' / '.join(r['load_start'].split()[:3])} → {' / '.join(r['load_end'].split()[:3])}; peak {num(peak_load)} |")
for viewers, streams in [(512, 1), (512, 512)]:
    present = [r for r in capacity_cases if r["viewers"]==viewers and r["streams"]==streams]
    if not present:
        lines.append(f"| {viewers} viewers / {streams} streams | **MISSING** | — | — | — |")
lines += ["", "Capacity uses the existing separate-process synthetic transport workload; its small keyframes do not establish the realistic-screen gates. The historical CPU A/B is retained in the round-3 archives and report history; it is not presented as a new round-4 CPU improvement.", "",
    "## Retired pin failures", "",
    "Earlier candidate failures remain in their archives and are not combined with the selected final cohort. The table retains their measured quality failures at valid load; completed rows from interrupted runs retain their scores; incomplete scenario checkpoints are unscored and load-rejected attempts remain separate. Later fixes address the receiver-proof, large-key flight-window, sparse packet-train and refresh-headroom failures shown here. Pin 21 passed both 300s Wi-Fi observations (one 549/534 ms gap, p95 283/287 ms), then failed loss_30 on TURN/UDP: a blind 15 s SCTP-startup timeout replaced the PC seven milliseconds after a fresh frame. The client now respects current decoded progress and a dedicated closed-heartbeat row crosses that deadline. Pin 22 passed both 300s Wi-Fi observations with zero >500ms gaps (p95 282/273ms), but its AQM512/1v row had a 5,224ms gap and two replacements. Repair traffic sustained the managed queue loss. Pin 20 additionally exposed the full-RTT replay-spacing regression: one 567ms Wi-Fi gap with stable budget, full layer, zero pacer drops and 290ms p95 latency.", "",

    "| Archived run / scenario | Server SHA-256 | Failure measurement | Peak load | Result |", "|---|---|---|---:|---|"]
for archive in ["r4-final-stable-cadence-critical", "r4-final-refresh-layers-critical-rest", "r4-final-native-playback-priority", "pre-r4-final-attempt1", "pre-r4-final2-attempt", "pre-r4-final3-attempt", "pre-r4-final4-attempt", "pre-r4-final5-attempt", "pre-r4-final6-attempt", "pre-r4-final7-attempt", "pre-r4-final8-attempt", "pre-r4-final9-attempt", "pre-r4-final10-attempt", "pre-r4-final11-attempt", "pre-r4-final12-attempt", "pre-r4-final13-attempt", "pre-r4-final14-attempt", "pre-r4-final15-attempt", "pre-r4-final16-attempt", "pre-r4-final17-attempt", "pre-r4-final18-attempt", "pre-r4-final19-attempt", "pre-r4-final20-attempt", "pre-r4-final21-attempt", "pre-r4-final22-attempt", "pre-r4-final28-attempt", "pre-r4-final29-attempt", "pre-r4-final30-attempt", "pre-r4-final31-interrupted", "pre-r4-final32-attempt", "pre-r4-final33-attempt", "r4-retired-final1", "r4-retired-verified1", "r4-final-key-watch-full", "r4-final-upstream-full", "r4-smaller-demand-aqm", "r4-final-current-loss-bandwidth", "r4-queue-bound-critical", "r4-current-decoder-critical", "r4-feedback-bdp-fifo", "r4-final-flight-bandwidth", "r4-verified-rtt-bandwidth", "r4-screen-transitions-critical", "r4-final-native-window-bandwidth", "r4-proven-refresh-critical", "r4-final-received-critical", "r4-final-quiet-critical", "r4-final-delivery-critical", "r4-final-base-critical", "r4-final-middle-critical", "r4-final-refresh300-critical", "r4-final-refresh-headroom-interrupted", "r4-final-refresh-drain-critical", "r4-final-current-capacity-critical", "r4-final-temporal-recovery-critical", "r4-final-base-clear-critical", "r4-final-base-clear-critical-rest", "r4-final-actual-key-critical", "r4-final-actual-key-remaining-interrupted", "r4-final-propagation-critical", "r4-final-known-demand-critical", "r4-final-refresh-transit-critical", "r4-final-wire-trial-critical", "r4-final-header-window-critical", "r4-final-repair-notice-critical", "r4-final-temporal-proof-critical", "r4-final-temporal-proof-critical-rest", "r4-final-steady-flight-critical", "r4-final-live-shortfall-critical", "r4-final-feedback-hold-critical", "r4-final-feedback-hold-critical-rest-resume1", "r4-final-residence-confirm-critical", "r4-final-residence-confirm-critical-rest", "r4-final-metadata-recovery-critical", "r4-final-metadata-recovery-critical-rest", "r4-final-repair-feedback-critical", "r4-final-repair-feedback-critical-rest", "r4-final-repair-feedback-remaining", "r4-final-extreme-stale-critical", "r4-final-confirm-restoration-critical", "r4-final-low-residence-critical", "r4-final-low-temporal-critical", "r4-final-low-temporal-critical-rest", "r4-final-clear-probes-critical", "r4-final-clear-probes-critical-rest", "r4-final-clear-probes-remaining", "r4-final-clear-probes-wifi1", "r4-final-repair-headroom-critical", "r4-final-timely-parity-critical", "r4-final-loss-memory-critical-rest", "r4-final-video-deltas-critical", "r4-final-video-deltas-wifi1", "r4-final-parity-interleave-critical", "r4-final-repaired-temporal-critical-rest", "r4-final-native-residence-critical", "r4-final-residence-hold-critical", "r4-final-residence-proof-critical", "r4-final-residence-entry-critical-rest", "r4-final-signal-freshness-critical", "r4-final-compounded-parity-critical", "r4-final-compounded-parity-loss-native", "r4-final-compounded-parity-wifi1", "r4-final-compounded-parity-critical-rest", "r4-final-compounded-parity-remaining", "r4-final-fresh-ice-critical", "r4-final-fresh-ice-hold-critical", "r4-final-fresh-ice-hold-loss-forced", "r4-final-stable-loss-critical", "r4-final-connected-start-critical", "r4-final-connected-start-loss-native", "r4-final-connected-start-wifi1", "r4-final-first-rtt-critical", "r4-final-key-parity-critical", "r4-final-key-parity-loss-forced", "r4-final-key-parity-loss-native", "r4-final-key-parity-critical-rest", "r4-final-native-cap-critical", "r4-final-native48-critical", "r4-final-native48-priority", "r4-final-native48-wifi1", "r4-final-native48-loss-forced", "r4-final-funded-refresh-critical", "r4-final-funded-refresh-priority", "r4-final-live-refresh-critical", "r4-final-live-refresh-loss-forced", "r4-final-live-refresh-priority", "r4-final-live-refresh-wifi1", "r4-final-live-refresh-loss-native", "r4-final-live-refresh-critical-rest"]:
    if archive.startswith(PREFIX):
        continue
    paths = list(RESULTS.glob(archive+".json*"))
    if paths:
        run = read(paths[0])
        for row in run["results"]:
            if row.get("load_invalid") or row.get("load_peak_1m", math.inf) > 3:
                continue
            if not row.get("pass") or wifi_regression_failed(row):
                detail = f"native full-layer {num(row['native_full_layer_ms'])} ms" if "native_full_layer_ms" in row else f"final fresh age {num(maximum(row, 'last_frame_age_ms'))} ms; maximum gap {num(maximum(row, 'max_gap_ms'))} ms"
                if row['name'] == 'bad_wifi' and row.get('metrics'):
                    detail = f"{row['metrics']['freezes_over_500ms']} gaps / {row['observation_s']}s; maximum {gap(row['metrics']['max_gap_ms'])} ms; p95 {num(row['metrics'].get('g2g_p95_ms'))} ms; end {row['metrics']['layer']}"
                if row.get('intervals'):
                    detail = '; '.join(f"{i['rate']} recovery {num(i.get('recovery_ms'))} ms; p95 {num(i['metrics'].get('g2g_p95_ms'))} ms; fresh gap {num(i['metrics'].get('max_gap_ms'))} ms" for i in row['intervals'] if not i.get('pass'))
                if row['name'] == 'stale_video_recovery':
                    detail = f"{row.get('replacements_during_stale')} stale-period replacements; recovery {num(row.get('recovery_ms'))} ms; required a replacement during the 5s capture-clock offset"
                if row.get('error'):
                    detail = row['error']
                elif row.get('fairness') and not row.get('pass'):
                    detail += '; decoded fps ' + ', '.join(num(f['decoded_fps']) + '/' + num(f['offered_fps']) for f in row['fairness'])
                    detail += '; full-layer samples ' + ', '.join(f"{f.get('full_layer_share', 0)*100:.1f}%" for f in row['fairness'])
                lines.append(f"| {archive} / {row['name']} | `{run['binary_sha256']}` | {detail} | {num(row.get('load_peak_1m'))} | **FAIL** |")
lines += ["",
    "## Reproduction", "",
    f"Build with `nice -n 10 mise exec -- npm --prefix web run build`, then `nice -n 10 env CGO_ENABLED=0 mise exec -- go build -buildvcs=false -trimpath -o build/passstream-{PREFIX} ./cmd/passstream`. Browser runs use `TORTURE_SERVER_BIN=./build/passstream-{PREFIX}`, `TORTURE_SECONDS=12`, `TORTURE_WIFI_SECONDS=300`, `TORTURE_RECOVERY_TRIALS=5`, `TORTURE_STRESS_MAX=16`, and `TORTURE_MAX_LOAD=1 TORTURE_MAX_OBSERVED_LOAD=3 TORTURE_REQUIRE_LOW_LOAD=1 TORTURE_LOAD_ATTEMPTS=5` with `TORTURE_LOAD_WAIT_MS=Infinity` and recorded per-scenario waits/retries. Launch with `nice -n 10 unshare -rn sh -ec 'ip link set lo up mtu 1500; mise exec -- node web/torture.mjs'`. Capacity uses `nice -n 10 mise exec -- go build -o build/capacity ./test/torture/capacity`, then `nice -n 10 unshare -rn sh -ec 'ip link set lo up mtu 1500; python3 test/torture/run_capacity_round4.py ./build/passstream-{PREFIX} {PREFIX}'`. Use a fresh run ID. Current-pin raw results and verification logs are committed. Earlier candidate diagnostic archives remain local; their historical table is context, while only the selected final prefix determines this verdict.", "",
    f"For a fresh complete cohort, give every browser run a distinct `TORTURE_RUN` beginning with one shared prefix. Run the full matrix without a filter, then repeat `TORTURE_FILTER='^bad_wifi$'` for the second 300 s observation. Run `TORTURE_FILTER='^loss_30$' TORTURE_RELAY_UDP=1` separately for the forced route. The full matrix includes the default detailed cold diagnostic; repeat `TORTURE_FILTER='^screen_cold_start$'` with `TORTURE_COLD_SOURCE=screen-baseline` and then `screen-startup` for the two comparisons. Keep the same server binary throughout, use that prefix for capacity, and render with `REPORT_RUN_PREFIX=<prefix> nice -n 10 python3 test/torture/report_round4.py`. The report exits nonzero unless all required evidence is present and passes.", ""]
verification_path = RESULTS / "verification" / (PREFIX + ".json")
verification = read(verification_path) if verification_path.exists() else {}
verification_ok = bool(verification.get("pass") and verification.get("binary_sha256") == pin and verification.get("rebuilt_sha256") == pin and len(verification.get("checks", [])) == 7 and all(c.get("exit_code") == 0 for c in verification["checks"]))
lines += ["## Code verification", "", f"Same-binary checks and reproducible static rebuild: {'PASS' if verification_ok else 'FAIL / missing'}. Evidence: `test/torture/results/verification/{PREFIX}.json`.", "", "| Check | Result |", "|---|---|"]
for check in verification.get("checks", []):
    lines.append(f"| `{' '.join(check['command'])}` | {'PASS' if check['exit_code'] == 0 else 'FAIL'} |")
lines.append("")
capacity_ok = not any(not r['pass'] and not r.get('load_invalid') for r in capacity_cases) and all(any(r['viewers'] == viewers and r['streams'] == streams and r['pass'] and r['report_load_valid'] for r in capacity_cases) for viewers, streams in [(512, 1), (512, 512)])
recovery_ok = all(len(trials := [r for _, r in rows if r['name'] == event or r['name'].startswith(event + '_trial')]) >= 5 and (p95 := pct([r.get('recovery_ms') for r in trials], .95)) is not None and p95 <= 500 for event in ['viewer_flap', 'network_flap'])
cold_ok = {'screen', 'screen-baseline', 'screen-startup'} <= {r.get('source') for _, r in rows if r['name'] == 'screen_cold_start' and r.get('window_ms') == 10000 and 'publisher_gaps_first10s' in r and r.get('min_fps_first10s') is not None}
green = not failed and not missing and not missing_new and load_valid and packet_model_valid and capacity_ok and wifi_ok and recovery_ok and cold_ok and bool(nonpixel_protection) and verification_ok
lines[6:6] = [f"**Round 4 verdict: {'PASS' if green else 'INCOMPLETE / FAIL'}.** Required browser rows, two five-minute Wi-Fi observations, recovery trials, cold diagnostics, load validity and both capacity cases are checked together.", '']
(ROOT/"docs/REPORT.md").write_text("\n".join(lines))
print(f"{PREFIX}: {'PASS' if green else 'INCOMPLETE / FAIL'}; {len(gate_rows)-len(failed)}/{len(gate_rows)} browser gates; {len(capacity_cases)} capacity observations")
if not green:
    raise SystemExit(1)
