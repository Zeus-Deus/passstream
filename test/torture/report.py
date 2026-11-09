#!/usr/bin/env python3
"""Generate the report only from measured run JSON, preserving missing values."""
import datetime
import gzip
import json
import math
import os
from pathlib import Path

if os.environ.get("REPORT_RUN_PREFIX", "r4-final").startswith("r4-"):
    import runpy
    runpy.run_path(str(Path(__file__).with_name('report_round4.py')), run_name='__main__')
    raise SystemExit

# Keep the standard torture entrypoint on the current measured round. An
# explicit round-1/2 prefix still uses the historical generator. Round-3
# cohorts enforce matching browser, capacity and CPU sources.
if os.environ.get("REPORT_RUN_PREFIX", "r3-round3-final").startswith("r3-"):
    import runpy
    runpy.run_path(str(Path(__file__).with_name('report_round3.py')), run_name='__main__')
    raise SystemExit

ROOT = Path(__file__).resolve().parents[2]
paths = list((ROOT / 'test/torture/results').glob('*.json')) + list((ROOT / 'test/torture/results').glob('*.json.gz'))
RUNS = [json.loads(gzip.decompress(p.read_bytes()) if p.suffix == '.gz' else p.read_bytes()) for p in paths]
ALL_RUNS = list(RUNS)
if prefix := os.environ.get("REPORT_RUN_PREFIX", "round2-final"):
    RUNS = [run for run in RUNS if run["run"].startswith(prefix)]
preferred = os.environ.get("REPORT_PREFER_RUN")
RUNS.sort(key=lambda run: (run["run"] == preferred, run["created"]))
LATEST = {}
for run in RUNS:
    for result in run['results']:
        LATEST[result['name']] = (run, result)


def number(value):
    return '—' if value is None else f'{value:.1f}'


def percentile(values, fraction):
    values = sorted(values)
    return values[min(len(values)-1, math.ceil(len(values)*fraction)-1)] if values else None


def metrics(result):
    value = result.get('metrics', {})
    return value if isinstance(value, dict) else {}


lines = [
    '# PassStream phase 1 torture report', '',
    f'Generated {datetime.datetime.now(datetime.timezone.utc).isoformat()} by '
    '`test/torture/report.py` from `test/torture/results/*.json[.gz]`.', '',
    '## Scope and interpretation', '',
    f'Report selection: prefix `{os.environ.get("REPORT_RUN_PREFIX", "round2-final")}`, preferred recovery run `{preferred or "latest"}`. Binary hashes identify the measured build; working-tree revision alone does not. Earlier rounds remain in `test/torture/results/` and in git history of this file.', '',
    'All tests use localhost inside an unprivileged `unshare -rn` namespace with a '
    '1500-byte loopback MTU. Go drives Node Playwright and system Chromium. '
    'Server and browsers share this host. Phase 1 is transport-encrypted only; '
    'PIN and SFrame are absent. No public deployment, portal picker or physical phone was tested.', '',
    'Netem applies in each direction on loopback: 40 ms one-way adds about 80 ms RTT. '
    'Bad Wi-Fi uses a Gilbert-Elliott chain with 1% good→bad, 19% bad→good, '
    '100% loss in bad and zero in good: stationary loss 5%, mean burst five packets. '
    'These are configured model parameters, not claims that each short sample had exactly 5% loss.', '',
    'Glass-to-glass comes from 48 timestamp bits and 16 frame-counter bits decoded '
    'from VP9 pixels, using the shared host wall clock. Only valid timestamps enter '
    'latency percentiles. Frame gaps >500 ms are counted independently of Chromium '
    'freezeCount. Current runs include leading/trailing gaps; early bring-up runs '
    'did not include leading gaps. TTFF begins at Session creation, excluding navigation. '
    'A low latency percentile does not imply an absence of freezes.', '',
    'Review-round runs apply the bad-Wi-Fi gate to the viewer leg only, with a 300-second default. Earlier runs impaired both legs and used 30 seconds. The allowed '
    'freeze count is scaled proportionally, so one freeze fails a 30-second window. '
    'A passing short window would not establish five-minute reliability. Ordinary '
    'loss/RTT profiles require valid decoded pixels and a frame within the final '
    '1.5 seconds; they do not impose the bad-Wi-Fi freeze gate. Good TTFF uses five joins. '
    'Recovery events use five independent trials (network flap also has a separate ten-trial run); p95 is the '
    'largest of five, a small sample. A missing recovery is a failed trial, never zero. Recovery needs three fresh frames within one second, at least five fresh frames in the final five seconds, a fresh frame within 1.5 seconds of the end, and, from round 2, no frame gap over 1.5 seconds after the first recovery (round-1 rows could pass while hiding a later ~5 s freeze).', '',
    'The UDP blackhole drops client UDP while exempting the internal TURN relay '
    'address `127.0.0.2`. Otherwise this one-namespace setup would also kill the '
    'TURN→SFU server leg, unlike a client-side UDP outage. Path is checked from the '
    'selected ICE candidate pair; fresh decoded pixels are required for recovery. '
    'Viewer/publisher flaps drop both directions of that peer’s selected UDP port '
    'and disable its HTTP/WebSocket network for 2.5 seconds. Recovery is timed '
    'after restoration; UDP-blackhole recovery is timed from fault injection.', '',
    'RSS is sampled from `/proc/PID/status` every 250 ms. CPU is process utime+stime '
    'divided by wall time (100 ticks/s); 100% is one core. Browser CPU is excluded. '
    'Per-viewer divisions include the shared server baseline. Server restarts may '
    'invalidate CPU deltas. Server CPU throttling uses SIGSTOP/SIGCONT at an '
    '80/20 ms duty cycle, not a cgroup quota. Viewer throttling uses Chromium’s '
    '6× main-thread emulation.', '',
    'Stress levels run sequentially until a gate fails or the configured cap is '
    'reached. A cap is a lower bound, never a measured maximum. All viewers must '
    'decode and stay fresh, with no >500 ms gap; the run JSON records the join gate. '
    'Stress CPU uses only the full-concurrency observation window when available; older runs use the whole case. These limits include browser encoding/decoding contention and are not isolated SFU capacity.', '',
    '## Runs', '',
    '| Run | Revision associated with working tree | Binary SHA-256 | Toolchain |',
    '|---|---|---|---|',
]
for run in RUNS:
    lines.append(f"| {run['run']} | {run['commit'][:12]} | "
                 f"`{run['binary_sha256'][:16]}` | {run['go']}; {run['chromium']} |")
if RUNS:
    run = RUNS[-1]
    lines += ['', f"Host: {run['kernel']}; {run['cpu']}; {run['memory']}."]
lines += ['', 'Bring-up probes are retained, including failed namespace/TCP probes. '
          'Longer diagnostic runs were superseded when implementation fixes changed '
          'the binary; their manifests and per-scenario files remain available. '
          'The verified run uses a private immutable binary throughout.', '',
          '## Latest result per scenario', '',
          'All rows below come from runs of one pinned binary (see Runs). `round2-final-full` is the complete suite; `round2-final-wifi2` is the second consecutive five-minute viewer-leg Wi-Fi run; `round2-final-netflap10` repeats network flap ten times. `round2-full` is an earlier complete run on the previous commit, listed with the diagnostic runs. A later run of the same scenario name supersedes the earlier row; the per-run gate table lists both Wi-Fi runs.', '',
          '| Scenario | Result | TTFF ms | G2G p50/p95 ms | >500ms gaps | Recovery ms | Peak RSS MiB | CPU % | Run |',
          '|---|---|---:|---:|---:|---:|---:|---:|---|']
for name, (run, result) in LATEST.items():
    m = metrics(result)
    lines.append(f"| {name} | {'PASS' if result.get('pass') else '**FAIL**'} | "
                 f"{number(result.get('ttff_p50_ms', m.get('ttff_ms')))} | "
                 f"{number(m.get('g2g_p50_ms'))} / {number(m.get('g2g_p95_ms'))} | "
                 f"{m.get('freezes_over_500ms', '—')} | {number(result.get('recovery_ms'))} | "
                 f"{number(result.get('server_rss_peak_mib'))} | "
                 f"{number(result.get('server_cpu_pct'))} | {run['run']} |")
lines += ['', '## Profile telemetry', '',
          '| Scenario | RTT p50 ms | Loss p50 % | Bitrate p50 kbit/s | Last fps | Path | Selected layer | RTC freezes / seconds |',
          '|---|---:|---:|---:|---:|---|---|---:|']
for name, (run, result) in LATEST.items():
    m = metrics(result)
    if not m:
        continue
    q = result.get('viewers', [{}])[0].get('quality', {})
    bitrate = m.get('bitrate_p50_bps')
    lines.append(f"| {name} | {number(m.get('rtt_p50_ms'))} | {number(m.get('loss_p50_pct'))} | "
                 f"{number(bitrate/1000 if bitrate is not None else None)} | {number(q.get('fps'))} | "
                 f"{m.get('path','—')} | {m.get('layer','—')} | "
                 f"{m.get('freezeCount','—')} / {number(m.get('totalFreezesDuration'))} |")
lines += ['', '## Good network and recovery percentiles', '']
if 'good' in LATEST:
    run, result = LATEST['good']
    delays = [f['delay'] for v in result.get('viewers', [])
              for f in v['timeline'] if f['delay'] is not None]
    lines += [f"Good: TTFF p50 {number(result.get('ttff_p50_ms'))} ms, "
              f"p95 {number(result.get('ttff_p95_ms'))} ms, {len(result.get('viewers', []))} joins. "
              f"Decoded glass-to-glass p50/p95 {number(percentile(delays, .5))} / "
              f"{number(percentile(delays, .95))} ms.", '']
for prefix in ['udp_blackhole', 'server_restart', 'viewer_flap', 'publisher_flap', 'network_flap']:
    trials = [r for name, (_, r) in LATEST.items()
              if name == prefix or name.startswith(prefix+'_trial')]
    if trials:
        recovered = [r['recovery_ms'] for r in trials if r.get('recovery_ms') is not None]
        post = [r['post_recovery_max_gap_ms'] for r in trials if r.get('post_recovery_max_gap_ms') is not None]
        lines.append(f"- {prefix}: {len(recovered)}/{len(trials)} measured recoveries; "
                     f"p50/p95 {number(percentile(recovered, .5))} / "
                     f"{number(percentile(recovered, .95))} ms among recovered trials; "
                     f"worst gap after recovery {number(max(post) if post else None)} ms; "
                     f"{'PASS' if all(r.get('pass') for r in trials) else 'FAIL'}.")
for run in RUNS:
    trials = [r for r in run['results'] if r['name'] == 'network_flap' or r['name'].startswith('network_flap_trial')]
    if len(trials) > 5:
        recovered = [r['recovery_ms'] for r in trials if r.get('recovery_ms') is not None]
        post = [r['post_recovery_max_gap_ms'] for r in trials if r.get('post_recovery_max_gap_ms') is not None]
        lines.append(f"- network_flap, {len(trials)} trials in {run['run']}: {len(recovered)} recovered; "
                     f"p50/p95 {number(percentile(recovered, .5))} / {number(percentile(recovered, .95))} ms, "
                     f"max {number(max(recovered) if recovered else None)} ms; worst gap after recovery "
                     f"{number(max(post) if post else None)} ms; {sum(bool(r.get('pass')) for r in trials)}/{len(trials)} pass.")
wifi = [(run, r) for run in RUNS for r in run['results'] if r['name'] == 'bad_wifi']
if wifi:
    lines += ['', 'Viewer-leg bad Wi-Fi, each five-minute run on the pinned binary:', '']
    for run, r in wifi:
        m = metrics(r)
        lines.append(f"- {run['run']}: {m.get('freezes_over_500ms')} gap(s) >500 ms in {r.get('observation_s')} s "
                     f"(max {m.get('max_gap_ms')} ms), G2G p50/p95 {number(m.get('g2g_p50_ms'))} / {number(m.get('g2g_p95_ms'))} ms, "
                     f"layer {m.get('layer')}; {'PASS' if r.get('pass') else 'FAIL'}.")
lines += ['', '## Stress boundary', '']
for name, (run, result) in LATEST.items():
    if not name.startswith('stress_'):
        continue
    rows = result.get('metrics', [])
    rows = rows if isinstance(rows, list) else []
    count = result.get('viewer_count')
    cpu = result.get('server_cpu_observation_pct', result.get('server_cpu_pct'))
    lines.append(f"- {name}: {'PASS' if result.get('pass') else 'FAIL'}, "
                 f"{count if count is not None else '—'} viewers; peak RSS {number(result.get('server_rss_peak_mib'))} MiB "
                 f"({number(result['server_rss_peak_mib']/count if count else None)} MiB/viewer), "
                 f"CPU {number(cpu)}% ({number(cpu/count if cpu is not None and count else None)}%/viewer). "
                 f"TTFF p50 {number(percentile([m['ttff_ms'] for m in rows if m.get('ttff_ms')], .5))} ms; "
                 f"{sum(m.get('freezes_over_500ms', 0) for m in rows)} gaps >500 ms. Run {run['run']}.")
lines += ['', '## Failures and limits', '']
failed = [(name, run, result) for name, (run, result) in LATEST.items() if not result.get('pass')]
for name, run, result in failed:
    reason=result.get('error', 'quality/recovery gate failed; inspect the metrics and raw timeline')
    if name.startswith('stress_') and isinstance(result.get('metrics'), list):
        rows=result['metrics']
        reason=(f"TTFF p50 {number(percentile([m['ttff_ms'] for m in rows if m.get('ttff_ms')],.5))} ms; "
                f"{sum(m.get('freezes_over_500ms',0) for m in rows)} gaps >500 ms; "
                f"{sum(not m.get('frames') for m in rows)} viewers without frames; "
                f"{sum(m.get('last_frame_age_ms') is None or m['last_frame_age_ms']>=1500 for m in rows)} stale at the end")
    lines.append(f"- **{name}** ({run['run']}): {reason}")
if not failed:
    lines.append('No latest scenario failed its implemented gate. Coverage remains limited to the listed observations.')
lines += ['', 'Round 2 changes (see docs/REVIEW.md, Phase 1 review round 2). Viewer layers start at the full stream and step down only on sustained congestion evidence: pacer queue sojourn, delay overuse confirmed by RR RTT growth, RTT inflation, or post-repair loss over enough packets; they probe back with doubling backoff. GCC targets no longer choose layers; they only floor the pacer, which drains each burst within 40 ms instead of throttling to an application-limited estimate. Downstream replay serves six NACKs within 1 s but never for packets older than the latest keyframe. The viewer raises its playout target only after two freezes within 30 s during NACK repair. Upstream repair makes six NACKs within 300 ms; keyframe requests wait for an in-flight keyframe. The browser watchdog replaces the PC instead of restarting ICE: at once when signaling answered pings throughout a 1 s media stall, otherwise 1 s after signaling returns from a total outage.', '',
          'Limits. Loss of 20–30% on both legs is bounded by Chrome’s publisher: its TWCC loss-based estimate can fall to its 30 kbit/s floor and encoding nearly stops, so whether a run plays to the end depends on when that happens (loss_20 failed this way in round2-final-full and passed in round2-full; loss_30 the reverse); the SFU cannot hide pre-repair loss from TWCC without corrupting delay feedback. A REMB-controlled publisher leg would be a separate design change. The bad-Wi-Fi latency reflects the raised playout target. Seamless warm-pair failover is replaced by PC replacement; a replacement renegotiates DTLS and, once E2EE lands, must re-admit with the session grant. E2EE integration, TURN TLS/443, physical mobile, NAT rebinding and tailnet remain unverified. The host was shared with unrelated builds; load averages are recorded in docs/PROGRESS.md.', '',
          '## Round 2 diagnostic evidence', '',
          'Diagnostic runs (`r2-*`, and `round2-full`) used intermediate binaries while isolating causes; they support the before/after analysis in docs/REVIEW.md and are not the gate. Recovery rows show recovery and the worst gap after recovery.', '',
          '| Run | Scenario | Result | >500ms gaps | Max gap ms | G2G p50/p95 ms | Recovery ms | Post-recovery gap ms | Binary |',
          '|---|---|---|---:|---:|---:|---:|---:|---|']
for run in sorted((r for r in ALL_RUNS if r['run'].startswith(('r2-', 'round2-full'))), key=lambda r: r['created']):
    for r in run['results']:
        m = metrics(r)
        if not m:
            continue
        lines.append(f"| {run['run']} | {r['name']} | {'PASS' if r.get('pass') else '**FAIL**'} | {m.get('freezes_over_500ms', '—')} | "
                     f"{m.get('max_gap_ms', '—')} | {number(m.get('g2g_p50_ms'))} / {number(m.get('g2g_p95_ms'))} | "
                     f"{number(r.get('recovery_ms'))} | {number(r.get('post_recovery_max_gap_ms'))} | `{run['binary_sha256'][:16]}` |")
lines += ['',
          '## Synthetic capacity', '',
          '`round1-current-*` is the round-1 cap on the frame-assembly build; `round2-capacity-*` reruns the sweep on the round-2 pinned binary. Invalid early probes remain under results/capacity and are excluded.', '',
          'Separate Go/Pion clients send synthetic VP9 RTP (259.2 kbit/s at 15 fps per publisher), without encoding/decoding. Server CPU/RSS are sampled from a separate process during full concurrency. Passing requires every receiver to observe >10 fps, no >500 ms frame gap and a last frame younger than 500 ms. This measures transport forwarding only; it does not establish decoded visual quality. A configured cap is a lower bound, not a discovered maximum.', '',
          '| Run | Viewers / streams | Result | Server CPU % | RSS MiB | CPU % / viewer | MiB / viewer | Binary |',
          '|---|---|---|---:|---:|---:|---:|---|']
capacity_prefixes = os.environ.get('REPORT_CAPACITY_PREFIX', 'round1-current-,round2-capacity-').split(',')
for path in sorted((ROOT / 'test/torture/results/capacity').glob('*.json')):
    if not any(path.name.startswith(prefix) for prefix in capacity_prefixes):
        continue
    c = json.loads(path.read_text())
    lines.append(f"| {path.stem} | {c['viewers']} / {c['streams']} | {'PASS' if c['pass'] else '**FAIL**'} | {number(c['server_cpu_pct'])} | {number(c['server_rss_peak_mib'])} | {c['cpu_per_viewer']:.3f} | {c['rss_mib_per_viewer']:.3f} | `{c.get('binary_sha256','unrecorded')[:16]}` |")
server_smokes = sorted((r for r in ALL_RUNS if r['run'].startswith('host-control-smoke-final')), key=lambda r: r['created'])
if server_smokes:
    run = server_smokes[-1]
    lines += ['', '## Phase 2b server API transport smoke', '',
              f"Run `{run['run']}`, code revision `{run['commit']}`, binary SHA-256 `{run['binary_sha256']}`.", '',
              'Two short scenarios, one recovery trial. The transport fixture uses canonical /v#c=…&k=…&s=… links, protocol-1 joins, and the existing phase-1 recovery/media implementation. It does not test sender/viewer E2EE integration, physical mobile devices, or public deployment.', '',
              '| Scenario | Result | Measurement |', '|---|---|---|']
    for r in run['results']:
        measurement = (f"Five joins: TTFF median {number(r.get('ttff_p50_ms'))} ms, p95 {number(r.get('ttff_p95_ms'))} ms"
                       if r['name'] == 'good' else
                       f"One trial: recovery {number(r.get('recovery_ms'))} ms; maximum later gap {number(r.get('post_recovery_max_gap_ms'))} ms; sustained recovery {r.get('sustained_recovery')}")
        lines.append(f"| {r['name']} | {'PASS' if r.get('pass') else '**FAIL**'} | {measurement} |")
    lines += ['', 'Restart browser console errors are connection-refused errors while the intentionally killed server is unavailable. Good-network playback has no browser errors. Earlier fixture setup failures remain in the archived evidence; they do not establish a media regression.', '']
lines += ['',           '## Artifacts', '',
          'Run JSON contains getStats samples, pixel/frame timelines, event times, state '
          'transitions, ICE restarts/replacements, browser errors, and resource samples. '
          '`test/torture/results/` is the committed evidence. Per-scenario files, logs and '
          'screenshots under `test/torture/out/` are ignored. `archive.py` stores exact JSON bytes in lossless gzip archives; uncompressed originals stay in `out/raw-results/`. Missing values are never filled.', '']
(ROOT / 'docs/REPORT.md').write_text('\n'.join(lines))
print(f'Rendered {len(LATEST)} scenarios ({len(failed)} failing) from {len(RUNS)} runs')
