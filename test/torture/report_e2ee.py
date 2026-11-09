#!/usr/bin/env python3
"""Print the "E2EE end-to-end" REPORT.md section from committed JSON.

    python3 test/torture/report_e2ee.py <e2e-functional-run> <gates-run>

Compares the E2EE gates against the round-3 pin (r3-round3-final-full and
r3-round3-final-wifi2) with the same metric code as report_round3.py.
Missing values stay missing.
"""
import gzip
import json
import math
import sys
from pathlib import Path

RESULTS = Path(__file__).resolve().parents[2] / 'test/torture/results'


def read(name):
    for p in (RESULTS / f'{name}.json', RESULTS / f'{name}.json.gz'):
        if p.exists():
            b = p.read_bytes()
            return json.loads(gzip.decompress(b) if p.suffix == '.gz' else b)
    raise SystemExit(f'missing {name}')


def n(v):
    return '—' if v is None else f'{v:.1f}'


def pct(v, p):
    return sorted(v)[min(len(v) - 1, math.ceil(len(v) * p) - 1)] if v else None


def delta(a, b):
    return '—' if a is None or b is None else f'{b - a:+.1f}'


def metrics(r):
    m = r.get('metrics', {})
    ms = m if isinstance(m, list) else [m]
    lat = max((x['g2g_p95_ms'] for x in ms if x.get('g2g_p95_ms') is not None), default=None)
    gap = max((x['max_gap_ms'] for x in ms if x.get('max_gap_ms') is not None), default=None)
    return lat, gap


functional, gates = read(sys.argv[1]), read(sys.argv[2])
base = {r['name']: r for run in ('r3-round3-final-full', 'r3-round3-final-wifi2') for r in read(run)['results']}
after = {r['name']: r for r in gates['results']}
out = ['## E2EE end-to-end (phase 2c)', '',
       f"Functional run `{functional['run']}`: commit `{functional['commit'][:12]}`, binary `{functional['binary_sha256']}`, {functional.get('chromium', '')}. "
       f"Gates run `{gates['run']}`: commit `{gates.get('commit', '')[:12]}`, binary `{gates['binary_sha256']}`. Round-3 comparison: `r3-round3-final-full` and `r3-round3-final-wifi2` (binary `53ccead9…`).", '',
       'PIN and E2EE are on everywhere: the sender generates the PIN, viewers type it (the harness reads it from the sender window or `passstream pin --reveal`), admission runs over the server relay, and video is VP9 under SFrame. Headless system Chromium on one shared host; loads are recorded in the JSON.', '',
       '### Functional and security tests', '', '| Test | Result | Measured |', '|---|---|---|']
keep = ['ttff_ms', 'g2g_ms', 'frames_2s', 'tries_left', 'final', 'viewer_code', 'kicked_last_frame_after_ms', 'other_max_gap_ms', 'epoch', 'refusals',
        'new_viewer', 'switch_ms', 'screen_frames_while_paused', 'reload_to_frame_ms', 'viewer_recovered_ms', 'latches', 'frames_while_daemon_down_2s', 'reattach_ms',
        'recovery_ms', 'signaling_sockets', 'statuses', 'pin_digits', 'frames', 'keyframes', 'vp9_sync_codes', 'entropy_bits_per_byte', 'file_hits', 'sent_hits', 'requests_to_server', 'error']
for r in functional['results']:
    shown = []
    for k in keep:
        if k in r:
            v = r[k]
            if isinstance(v, float):
                v = round(v, 3)
            shown.append(f'{k} {json.dumps(v, separators=(",", ":"))}')
    if 'sfu_eye' in r:
        e = r['sfu_eye']
        shown.append(f"sfu_eye frames {e['frames']}, keyframes {e['keyframes']}, VP9 sync codes {e['vp9_sync_codes']}, entropy {e['entropy_bits_per_byte']:.3f} bit/B")
    if 'plaintext_control' in r:
        c = r['plaintext_control']
        shown.append(f"plaintext control: keyframes {c['keyframes']}, sync codes {c['vp9_sync_codes']}, entropy {c['entropy_bits_per_byte']:.3f} bit/B")
    out.append(f"| {r['name']} | {'PASS' if r.get('pass') else '**FAIL**'} | {'; '.join(shown).replace('|', '/')} |")

out += ['', '### Performance with E2EE on, against round 3', '',
        '| Scenario | Round 3 | E2EE | G2G p95 ms (r3 → e2ee, Δ) | Max fresh gap ms (r3 → e2ee, Δ) | Recovery ms (r3 → e2ee, Δ) |', '|---|---|---|---|---|---|']
names = [r['name'] for r in gates['results']]
for name in names:
    a, b = base.get(name), after[name]
    la, ga = metrics(a) if a else (None, None)
    lb, gb = metrics(b)
    ra, rb = (a or {}).get('recovery_ms'), b.get('recovery_ms')
    out.append(f"| {name} | {'—' if a is None else ('PASS' if a.get('pass') else 'FAIL')} | {'PASS' if b.get('pass') else '**FAIL**'} | {n(la)} → {n(lb)} ({delta(la, lb)}) | {n(ga)} → {n(gb)} ({delta(ga, gb)}) | {n(ra)} → {n(rb)} ({delta(ra, rb)}) |")
out.append('')
g0, g1 = base.get('good'), after.get('good')
if g1:
    out.append(f"- Good-network TTFF p50/p95: round 3 {n(g0 and g0['ttff_p50_ms'])}/{n(g0 and g0['ttff_p95_ms'])} ms → E2EE {n(g1['ttff_p50_ms'])}/{n(g1['ttff_p95_ms'])} ms (Δ {delta(g0 and g0['ttff_p50_ms'], g1['ttff_p50_ms'])}/{delta(g0 and g0['ttff_p95_ms'], g1['ttff_p95_ms'])}). E2EE TTFF is measured from page load and includes the harness typing the PIN and the CPace admission.")
for name in ['bad_wifi']:
    if name in after:
        m0, m1 = base[name]['metrics'], after[name]['metrics']
        out.append(f"- bad_wifi, {after[name].get('observation_s')} s: gaps >500 ms {m0['freezes_over_500ms']} → {m1['freezes_over_500ms']}; G2G p50/p95 {m0['g2g_p50_ms']}/{m0['g2g_p95_ms']} → {m1['g2g_p50_ms']}/{m1['g2g_p95_ms']} ms; layer {m0.get('layer')} → {m1.get('layer')}.")
for name in ['competing_2mbit_1v']:
    if name in after and 'tcp_link_share' in after[name]:
        out.append(f"- {name}: TCP link share {base[name]['tcp_link_share'] * 100:.1f}% → {after[name]['tcp_link_share'] * 100:.1f}%.")
for name in ['udp_blackhole', 'viewer_flap', 'server_restart']:
    def trials(d):
        return [r for k, r in d.items() if k == name or k.startswith(name + '_trial')]
    t0, t1 = trials(base), trials(after)
    if t1:
        r0 = [r['recovery_ms'] for r in t0 if r.get('recovery_ms') is not None]
        r1 = [r['recovery_ms'] for r in t1 if r.get('recovery_ms') is not None]
        out.append(f"- {name}: {len(t1)} trials, {sum(bool(r.get('pass')) for r in t1)} pass; recovery p50/p95 {n(pct(r0, .5))}/{n(pct(r0, .95))} → {n(pct(r1, .5))}/{n(pct(r1, .95))} ms.")
loads = [float(r[k].split()[0]) for r in gates['results'] for k in ('load_start', 'load_end') if r.get(k)]
if loads:
    out.append(f"- Gates run one-minute load {min(loads):.2f}–{max(loads):.2f} (scenario start/end samples).")
print('\n'.join(out))
