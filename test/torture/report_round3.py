#!/usr/bin/env python3
"""Regenerate round-3 evidence tables without filling missing measurements."""
import gzip
import json
import math
import os
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
RESULTS = ROOT / 'test/torture/results'

def read(p):
    return json.loads(gzip.decompress(p.read_bytes()) if p.suffix == '.gz' else p.read_bytes())

def n(v):
    return '—' if v is None else f'{v:.1f}'

def percent(v):
    return '—' if v is None else f'{v:.1f}%'

def pct(v, p):
    return sorted(v)[min(len(v)-1, math.ceil(len(v)*p)-1)] if v else None

prefix = os.environ.get('REPORT_RUN_PREFIX', 'r3-round3-final')
runs = sorted([read(p) for p in RESULTS.glob(prefix+'*.json*')], key=lambda r:r['created'])
if not runs:
    raise SystemExit(f'no measured runs matching {prefix}')
if len({r['binary_sha256'] for r in runs}) != 1:
    raise SystemExit('refusing to combine different pinned binaries')
pin = runs[0]['binary_sha256']
rows = [(run, r) for run in runs for r in run['results']]
lines = ['# PassStream phase 1 torture report', '',
    'Regenerated from committed round-3 JSON by `python3 test/torture/report_round3.py`. Earlier measurements remain in the result archives and report history.', '',
    'Phase 1 uses a 960×540 / 15 fps canvas VP9 publisher with transport encryption on localhost inside unprivileged network namespaces (loopback MTU 1500). No portal capture, physical phone, PIN or SFrame browser integration is covered. Server CPU excludes client processes. Shared-host load averages are measured rather than normalized. Final stages wait up to 180 s for one-minute load ≤3; each browser scenario additionally waits up to 30 s and records that wait. The bounds keep progress possible if unrelated work remains busy.', '',
    '## Gate definitions and limits', '',
    '- Viewer-only 256 kbit/s: 10 s convergence, then at least 30 s with p95 timestamp-valid glass-to-glass ≤1,000 ms, every fresh-frame gap ≤1,000 ms, and a fresh frame at the end.',
    '- Each viewer-only bandwidth step is evaluated separately. Three advancing, timestamp-valid frames within one second establish recovery; step-down recovery must take ≤3,000 ms. The remaining interval must satisfy the latency, freshness and continuity gate.',
    '- Competition: 512 kbit/s and 2 Mbit/s, one viewer plus one backpressured TCP POST; an additional two-viewer 512 kbit/s stress case is retained. TCP goodput must be ≥30% of the configured link and every viewer must satisfy the same 1 s latency/continuity gate after convergence (10 s for one viewer, 20 s for two).',
    '- Competition network models are reported separately: HTB with FQ-CoDel (5 ms target, 100 ms interval, 1,000-packet memory bound) and the original shared 1,000-packet netem FIFO with both directions impaired. The FIFO row retains its original 10 s convergence and 1 s latency/continuity gates. No queue-based exemption is applied. The extra two-viewer AQM row retains its documented 20 s convergence; earlier 10 s failures remain archived.',
    '- Bad Wi-Fi: viewer-only, both directions, Gilbert-Elliott 1% good→bad / 19% bad→good / 100% loss in bad, 40±10 ms delay per direction. At most one >500 ms decoded-frame gap in each 300 s run. Candidate filters are installed throughout replacement; selected-path changes and increasing impairment counters are recorded. The polling interval is 50 ms, so transient traffic before the next refresh is not claimed impaired. An explicit replacement scenario verifies continued impairment.',
    '- Recovery: five trials per event, <3 s to three advancing, timestamp-valid fresh frames; sustained fresh progress and no >1.5 s fresh-frame gap afterward. Stale decoded callbacks cannot satisfy recovery. Ordinary loss profiles require valid pixels and final freshness; 20% loss remains an ordinary profile with a recorded failure if final freshness is missed; 30% is a stress observation.',
    '- One wire bucket covers media, parity and replay: rate × time + 32 KiB burst credit + at most one packet of debt. Ordinary picture expiry is 100 ms; dependent pictures get at most six seconds of additional keyframe catch-up grace. Started pictures finish at the budget rate; queue admission retains the 2 MiB / 2,048-packet bounds.',
    '- Wire counters include RTP bytes plus a conservative 50-byte per-packet allowance for IP/UDP/SRTP and transport overhead, covering media, parity and NACK replay. Repair share is the last PeerConnection’s cumulative estimated wire-byte ratio at the final heartbeat, including its startup (counters reset on replacement); signaling/RTCP/heartbeat bytes are not in this counter.', '',
    '## Pinned runs', '', '| Run | Binary SHA-256 | Associated commit | Rows passing |', '|---|---|---|---:|']
for run in runs:
    lines.append(f"| {run['run']} | `{run['binary_sha256']}` | `{run['commit'][:12]}` | {sum(bool(r.get('pass')) for r in run['results'])}/{len(run['results'])} |")
for run in runs:
    loads=[float(r[k].split()[0]) for r in run['results'] for k in ['load_start','load_end'] if r.get(k)]
    if loads:lines += ['',f"{run['run']}: sampled scenario start/end one-minute load {min(loads):.2f}–{max(loads):.2f}; first/last 1/5/15-minute load {' / '.join(run['results'][0]['load_start'].split()[:3])} → {' / '.join(run['results'][-1]['load_end'].split()[:3])}."]
    good=next((r for r in run['results'] if r['name']=='good'),None)
    if good:lines += ['',f"Good-network TTFF p50/p95: {n(good['ttff_p50_ms'])}/{n(good['ttff_p95_ms'])} ms; p50 gate <1,000 ms: {'PASS' if good['pass'] else 'FAIL'}."]
before_path = RESULTS/'r3-final3-full.json.gz'
if before_path.exists():
    before = {r['name']:r for r in read(before_path)['results']}
    after = {r['name']:r for _,r in rows}
    lines += ['', '## Before / after the resumed fixes', '',
        'Before is the interrupted round-3 checkpoint `r3-final3-full` on c02192c. Failed intermediate runs remain archived; these columns do not combine their measurements with the final pin.', '',
        '| Gate | Before | Final pin |', '|---|---|---|']
    for name in ['bad_wifi','viewer_256k','competing_512kbit_1v','competing_2mbit_1v','competing_512kbit_2v','competing_fifo_512kbit_2v']:
        def describe(r):
            if r is None:return 'not measured'
            m=r.get('metrics',{});ms=m if isinstance(m,list) else [m]
            p95='/'.join(n(x.get('g2g_p95_ms')) for x in ms)
            suffix=f"; TCP {percent(r['tcp_link_share']*100)}" if 'tcp_link_share' in r else ''
            if name=='bad_wifi':suffix=f"; {m.get('freezes_over_500ms')} {'gap' if m.get('freezes_over_500ms')==1 else 'gaps'} >500 ms / {r.get('observation_s')} s"
            return f"{'PASS' if r.get('pass') else 'FAIL'}: p95 {p95} ms{suffix}"
        lines.append(f"| {name} | {describe(before.get(name))} | {describe(after.get(name))} |")
lines += ['', '## Gate table', '', '| Run / scenario | Result | G2G p95 ms | Max fresh gap >500 ms | Recovery ms | TCP link share | Repair wire share | Load start → end (1/5/15 min) |', '|---|---|---:|---:|---:|---:|---:|---|']
for run, r in rows:
    viewers = r.get('viewers', [])
    shares=[]
    for v in viewers:
        q=v.get('quality',{}).get('repair',{}) or {}
        if q.get('wireBytes'): shares.append(q.get('repairBytes',0)/q['wireBytes']*100)
    m=r.get('metrics',{})
    metrics=m if isinstance(m,list) else [m]
    latency=max((x['g2g_p95_ms'] for x in metrics if x.get('g2g_p95_ms') is not None),default=None)
    gap=max((x['max_gap_ms'] for x in metrics if x.get('max_gap_ms') is not None),default=None)
    load=lambda key:' / '.join(r.get(key,'').split()[:3])
    share=r.get('tcp_link_share')
    lines.append(f"| {run['run']} / {r['name']} | {'PASS' if r.get('pass') else '**FAIL**'} | {n(latency)} | {n(gap)} | {n(r.get('recovery_ms'))} | {percent(None if share is None else share*100)} | {percent(max(shares,default=None))} | {load('load_start')} → {load('load_end')} |")
    if r.get('intervals'):
        for i in r['intervals']:
            m=i['metrics']
            lines.append(f"| ↳ {i['rate']} at {i['from']} | {'PASS' if i['pass'] else '**FAIL**'} | {n(m.get('g2g_p95_ms'))} | {n(m.get('max_gap_ms'))} | {n(i.get('recovery_ms'))} | — | — | same scenario |")
lines += ['', '## Remaining failures', '']
failed = [(run, r) for run, r in rows if not r.get('pass')]
if not failed:
    lines.append('No rows failed on the pinned browser binary.')
for run, r in failed:
    if r.get('error'):
        detail = r['error'].splitlines()[0]
    else:
        m=r.get('metrics',{});ms=m if isinstance(m,list) else [m]
        indexed=list(enumerate(ms))
        failed_metrics=[(i,x) for i,x in indexed if x.get('max_gap_ms',0)>500 or x.get('last_frame_age_ms',0)>=1500 or not x.get('frames')]
        detail='; '.join(f"viewer {i+1}: {x.get('frames')} frames, p95 {n(x.get('g2g_p95_ms'))} ms, max gap {n(x.get('max_gap_ms'))} ms, final age {n(x.get('last_frame_age_ms'))} ms" for i,x in failed_metrics or indexed)
        if 'tcp_link_share' in r:detail+=f"; TCP {percent(r['tcp_link_share']*100)}"
        target=r.get('publisher',{}).get('quality',{}).get('stats',{}).get('targetBitrate')
        if target is not None:detail+=f"; final publisher target {target} bit/s"
    lines.append(f"- {run['run']} / {r['name']}: {detail}")
lines += ['', '## Wi-Fi and event aggregates', '']
for run in runs:
    for r in run['results']:
        if r['name']=='bad_wifi':
            m=r['metrics'];lines.append(f"- {run['run']}: {r['observation_s']} s, {m['freezes_over_500ms']} gaps >500 ms, max {m['max_gap_ms']} ms, G2G p50/p95 {m['g2g_p50_ms']}/{m['g2g_p95_ms']} ms, {m['layer']}.")
for name in ['udp_blackhole','network_flap','viewer_flap','publisher_flap','server_restart']:
    trials=[r for _,r in rows if r['name']==name or r['name'].startswith(name+'_trial')]
    if trials:
        times=[r['recovery_ms'] for r in trials if r.get('recovery_ms') is not None]
        lines.append(f"- {name}: {len(trials)} trials, {sum(bool(r.get('pass')) for r in trials)} pass; recovery p50/p95 {n(pct(times,.5))}/{n(pct(times,.95))} ms. Missing recovery: {len(trials)-len(times)}.")
lines += ['', '## FIFO queue evidence', '',
    '| Scenario | TCP goodput bit/s | TCP link share | Probe FIFO delay p50/p95 ms | Viewer G2G p95 ms |',
    '|---|---:|---:|---:|---:|']
for _, r in rows:
    if 'queue_probe' not in r: continue
    q=r['queue_probe']['metrics']
    metrics=r.get('metrics',[])
    latency=max((x['g2g_p95_ms'] for x in metrics if x.get('g2g_p95_ms') is not None),default=None)
    lines.append(f"| {r['name']} | {n(r.get('tcp_goodput_bps'))} | {percent(r.get('tcp_link_share',0)*100)} | {n(q['p50_ms'])}/{n(q['p95_ms'])} | {n(latency)} |")
lines += ['', 'The probe is a 64-byte timestamped UDP payload sent twice per second through the same FIFO and received after one qdisc traversal on the same clock (about 1.5 kbit/s IP rate). These are contemporaneous distributions, not a packet-matched decomposition of media latency. The TCP-only case uses the same TCP writer, rate, delay, FIFO limit and directional filters as the viewer case.', '',
    'TCP-only queue delay did not explain the failed diagnostics: viewer G2G reached 5–6 s while the contemporaneous probe stayed below 600 ms. The application trapped itself in keyframe expiry and a 64 kbit/s pacing budget. The final code uses a 32 KiB bounded wire burst so a recovery keyframe does not create repeated reference expiry, preserves serialization catch-up, reacts once to each changed delay observation, permits bounded probes for measured next-layer demand plus repair, and seeds RTT growth from pre-media ICE/handshake measurements. Each PC starts with a 256 kbit/s wire ceiling and withholds the highest temporal layer for two seconds of feedback; replacement cannot reset its sender to the optimistic 2 Mbit/s GCC estimate. A measured successful productive media rate may protect an admitted keyframe catch-up for at most six seconds after fresh decoded progress. RTT growth over 120 ms cuts the shared wire budget once per new receiver report, at most twice per second, including when decoded frames still advance. Parity admission uses the total wire budget because layer admission has already reserved repair; it does not subtract the reservation a second time. Repair uses a one-second rate window and replay spacing of max(30 ms, RTT), with 100 ms before RTT is known. The decoded-stall deadline uses pre-media RTT rather than the congested current RTT. Brief heartbeat loss is latched between stats observations, and playout increases by at most 100 ms per observation toward the 400 ms repair target, so filling that target does not itself create a long pause. The original FIFO gate is retained without a replacement proposal.', '']
lines += ['', '## Controlled CPU A/B', '',
    'Separate-process Go/Pion workload: 256 viewers, one 259.2 kbit/s 15 fps synthetic publisher, no encoding/decoding, a matched 10-second warm-up after all peers connect followed by a 20-second full-concurrency observation with the host production processor limit for both helper and server, Go 1.27.3, CGO_ENABLED=0 and matching -trimpath builds. Profiles use the same test-only Go build overlay exposing localhost pprof; production binaries have no profiling listener. Runs execute sequentially in one namespace in A/B/B/A order, including a reverse-order repeat. CPU is process ticks/wall time, 100% = one core. The ordinary capacity helper retains its three-second default warm-up and all original quality gates.', '',
    'An earlier comparison with a three-second warm-up stopped after its first B run: three of 256 receivers had maximum gaps 1,081.8–1,084.9 ms, despite mean delivered rate 14.86 fps. A was 40.67% CPU, B 37.92%, but this was an incomplete cohort and is excluded from the final four-run mean. Its JSON/profiles and driver failure are preserved in capacity/interrupted-final-cpu. The final driver records every observation and its provenance even if a quality gate fails, and completes the reverse-order comparison.', '',
    'Every observation must pass the unchanged per-receiver continuity/freshness gate and mean delivered rate must be ≥14.7 fps. The driver also records a separate quiet-window target of sampled one-minute load ≤3; any miss remains explicit beside the result and does not turn a quality failure into a pass. Entry waits for one-minute load ≤2 and five-minute load ≤3, with the existing 180 s bound. Host load is sampled every 250 ms. The earlier loaded, near-quiet, edge-load and quiet-quality cohorts remain in separately named capacity directories; none is folded into the final mean. One quiet older-source B observation had a 1,086.5 ms gap in one receiver. The final server requires RTT growth or stalled decoded video with active ingress to corroborate a GCC delay cut; an unconfirmed local gradient cannot collapse the wire budget.', '',
    'Four-processor profiling remains a failed stress observation: the final source had a continuity failure and did not meet its declared comparison criteria. Those partial results remain in capacity/four-processor-final-stress; the accidentally overlapping restart is separately preserved in capacity/cpu-protocol-overlap and excluded. The final comparison uses the host production default of 12 processors for both variants; browser and synthetic-capacity quality gates are unchanged.', '',
    '| Binary | Result | CPU % | RSS MiB | Mean delivered fps | Load start → end (1/5/15) | SHA-256 |', '|---|---|---:|---:|---:|---|---|']
cpu_prefix = 'r3-final-current' if list((RESULTS/'capacity').glob('r3-final-current-*.json')) else 'r3-final'
cpu_paths={label:RESULTS/'capacity'/f'{cpu_prefix}-{label}.json' for label in ['before','after','after-repeat','before-repeat']}
if not all(p.exists() for p in cpu_paths.values()):
    raise SystemExit('refusing an incomplete CPU A/B/B/A cohort')
cpu_runs={label:read(p) for label,p in cpu_paths.items()}
if any(cpu_runs[label]['source_commit']!=runs[0]['commit'] for label in ['after','after-repeat']):
    raise SystemExit('CPU after source differs from the pinned browser source')
if len({r['go_version'] for r in cpu_runs.values()})!=1 or len({r['gomaxprocs'] for r in cpu_runs.values()})!=1:
    raise SystemExit('CPU cohort toolchain or GOMAXPROCS differs')
if any(not r.get('quality_comparable',r.get('comparison_eligible')) for r in cpu_runs.values()):
    raise SystemExit('CPU cohort failed a quality or delivered-throughput criterion')
for p in sorted((RESULTS/'capacity').glob(cpu_prefix+'-*.json'),key=lambda p:read(p)['created']):
    r=read(p)
    fps=sum(m['frames'] for m in r['metrics'])/len(r['metrics'])/r['observation_s']
    lines.append(f"| {p.stem} | {'PASS' if r['pass'] else '**FAIL**'} | {n(r['server_cpu_pct'])} | {n(r['server_rss_peak_mib'])} | {fps:.2f} | {' / '.join(r['load_start'].split()[:3])} → {' / '.join(r['load_end'].split()[:3])} | `{r['binary_sha256']}` |")
lines += ['',f"CPU source commits: before `{cpu_runs['before']['source_commit']}`; after `{cpu_runs['after']['source_commit']}`. Both helper and server use GOMAXPROCS={cpu_runs['before']['gomaxprocs']}, the host production default. The profiling overlay makes these hashes differ from the production browser/capacity pin."]
quiet_misses=[label for label,r in cpu_runs.items() if not r.get('quiet_target_met',r.get('comparison_eligible'))]
if quiet_misses:
    peak=max(float(s.split()[0]) for r in cpu_runs.values() for s in r.get('load_samples',[])+[r['load_start'],r['load_end']])
    lines += ['',f'The ≤3 one-minute quiet target was missed in {len(quiet_misses)}/4 observations ({", ".join(quiet_misses)}); maximum sampled one-minute load was {peak:.2f}. All four pass the original per-receiver quality gates and the ≥14.7 fps throughput criterion. The CPU delta is an observed matched-configuration result on this shared host, with the load variation retained; it is not an isolated-machine measurement.']
before=[read(p)['server_cpu_pct'] for p in (RESULTS/'capacity').glob(cpu_prefix+'-before*.json')]
after=[read(p)['server_cpu_pct'] for p in (RESULTS/'capacity').glob(cpu_prefix+'-after*.json')]
if before and after:
    a,b=sum(before)/len(before),sum(after)/len(after)
    packet_cost={label:r['server_cpu_pct']/100*r['observation_s']/sum(m['packets'] for m in r['metrics'])*1_000_000 for label,r in cpu_runs.items()}
    packet_before=(packet_cost['before']+packet_cost['before-repeat'])/2
    packet_after=(packet_cost['after']+packet_cost['after-repeat'])/2
    lines += ['',f'Mean server CPU time per delivered RTP packet: {packet_before:.2f} → {packet_after:.2f} µs ({(packet_before-packet_after)/packet_before*100:.1f}% lower), including all server work in each observation. This normalization exposes any delivered-throughput difference beside the raw CPU comparison.']
    def cumulative(label, fn):
        p=RESULTS/'capacity'/f'{cpu_prefix}-{label}.json.top.txt'
        if not p.exists():return None
        for line in p.read_text().splitlines():
            parts=line.split()
            if len(parts)>=6 and parts[-1].endswith(fn):
                m=re.fullmatch(r'([0-9.]+)(ns|us|µs|ms|s)',parts[3])
                if not m:raise ValueError(f'unrecognized pprof duration: {parts[3]}')
                return float(m[1])*{'ns':1e-9,'us':1e-6,'µs':1e-6,'ms':1e-3,'s':1}[m[2]]
        return None
    schedule=[cumulative(x,'runtime.schedule') for x in ['before','after']]
    pacer=[cumulative(x,'(*Pacer).run') for x in ['before','after']]
    lines += ['',f'Mean CPU: {a:.2f}% → {b:.2f}% of one core ({(a-b)/a*100:.1f}% lower). First-profile cumulative CPU: runtime.schedule {n(schedule[0])} → {n(schedule[1])} s; Pacer.run {n(pacer[0])} → {n(pacer[1])} s over 20 s. These are observed relative results at the recorded shared-host loads; different GOMAXPROCS/load prevents equating them to earlier round-1 absolute CPU numbers. Earlier unmatched-trimpath pilot profiles remain in capacity/pilot and are excluded. Earlier source cohorts (root r3-final-before/after and capacity/pre-intrinsic-clock files) are excluded when the final r3-final-current cohort is present.']
lines += ['', '## Pinned synthetic capacity', '', '| Run / case | Result | CPU % | RSS MiB | Load start → end (1/5/15) |', '|---|---|---:|---:|---|']
for p in sorted((RESULTS/'capacity').glob('r3-capacity-final-*.json')):
    r=read(p)
    if r['binary_sha256'] != pin:
        raise SystemExit('capacity evidence does not match the pinned browser binary')
    lines.append(f"| {p.stem}: {r['viewers']} viewers / {r['streams']} streams, {r['observation_s']:.1f}s | {'PASS' if r['pass'] else '**FAIL**'} | {n(r['server_cpu_pct'])} | {n(r['server_rss_peak_mib'])} | {' / '.join(r['load_start'].split()[:3])} → {' / '.join(r['load_end'].split()[:3])} |")
lines += ['', 'The earlier loaded capacity failure remains in `capacity/r3-capacity-loaded-viewers512.json` (load 10.43 → 16.03); it is excluded from the final capacity cohort. Labelled load-selection samples for diagnostic and final chains are retained in `r3-load-selection.txt`.', '', '## Diagnostics and reproducibility', '',
    'Build the SPA with `mise exec -- npm --prefix web run build`, then the static server with `CGO_ENABLED=0 mise exec -- go build -trimpath -o build/passstream-r3-round3-final ./cmd/passstream`. The recorded browser runs used `TORTURE_SERVER_BIN=./build/passstream-r3-round3-final`, `TORTURE_SECONDS=12`, `TORTURE_WIFI_SECONDS=300`, `TORTURE_RECOVERY_TRIALS=5`, `TORTURE_STRESS_MAX=16`, `TORTURE_MAX_LOAD=3` and `TORTURE_LOAD_WAIT_MS=30000`, launched with `unshare -rn sh -ec \'ip link set lo up mtu 1500; mise exec -- node web/torture.mjs\'`. Use a fresh `TORTURE_RUN` for each reproduction. The second Wi-Fi observation additionally uses `TORTURE_FILTER=^bad_wifi$`. CPU reproduction: `CPU_AB_PREFIX=<fresh-prefix> python3 test/torture/cpu_ab.py`; existing CPU evidence is never overwritten.', '',
    'The round-2 original shared-bandwidth diagnostic was rerun before edits: viewer p95 8,598/8,366 ms; TCP 141,982 bit/s with viewers versus 391,482 bit/s in its TCP-only follow-up. Observed host loads bracketing that run were 9.82/7.82/7.04 and 4.48/6.25/6.54 (these are bracket samples, not automatically captured start/end values). The supplied earlier independent review measured p95 11.5–11.8 s and ~27 kbit/s TCP; stochastic queue/loss and shared-host results vary.', '',
    'All `r3-*` archives retain failed diagnostics, automatic per-scenario load samples, exact binary hashes, frame/sample timelines and impairment counters. CPU `.pprof` and top reports are under `results/capacity/`. Build/check commands and implementation decisions are recorded in REVIEW.md. Final tests never replace the pinned server binary during a run. No push or deployment occurred.', '']
(ROOT/'docs/REPORT.md').write_text('\n'.join(lines))
