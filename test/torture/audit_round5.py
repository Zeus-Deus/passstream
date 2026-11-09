#!/usr/bin/env python3
"""Audit a frozen round-five cohort without selecting away media failures.

Usage: python3 test/torture/audit_round5.py RUN_PREFIX
Only the predeclared load exclusions are omitted. TURN loss30 stays separate.
"""
import gzip
import json
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[2]
RESULTS = ROOT / "test/torture/results"
prefix = sys.argv[1]


def read(path):
    data = path.read_bytes()
    return json.loads(gzip.decompress(data) if path.suffix == ".gz" else data)


paths = sorted(p for p in RESULTS.glob(prefix + "*.json*")
               if p.suffix == ".gz" or not Path(str(p) + ".gz").exists())
runs = [read(p) for p in paths]
if not runs:
    raise SystemExit("no cohort evidence")
pins = {run["binary_sha256"] for run in runs}
if len(pins) != 1:
    raise SystemExit("refusing to combine static binaries")
pin = pins.pop()
harnesses = {json.dumps(run.get("harness_sha256"), sort_keys=True) for run in runs}
issues = []
if len(harnesses) != 1 or "null" in harnesses:
    issues.append("missing or inconsistent harness hashes")

browser, turn, diagnostic, rejected = [], [], [], []
for run in runs:
    if run.get("nice") != 10 or not run.get("low_load_required"):
        issues.append(f"{run['run']}: missing nice/load entry contract")
    if not 0 < run.get("load_limit", 0) <= 3 or run.get("observed_load_limit") != 3:
        issues.append(f"{run['run']}: invalid declared load limits")
    for row in run.get("load_rejected", []):
        if not row.get("load_invalid") or row.get("load_peak_1m", 0) <= 3:
            issues.append(f"{run['run']}/{row['name']}: invalid load exclusion")
        rejected.append((run["run"], row))
    for row in run["results"]:
        if row.get("load_invalid") or row.get("load_peak_1m", 100) > 3:
            issues.append(f"{run['run']}/{row['name']}: counted invalid host load")
        item = {"run": run["run"], **row}
        if row.get("diagnostic"):
            diagnostic.append(item)
        elif row["name"] == "loss_30" and row.get("forced_relay_udp"):
            turn.append(item)
        else:
            browser.append(item)

by_name = {}
for row in browser:
    by_name.setdefault(row["name"], []).append(row)

# Keep the old browser matrix as well as every added round-four scenario.
required = {row["name"] for row in read(RESULTS / "r3-round3-final-full.json.gz")["results"]
            if not row.get("diagnostic")}
required.update({"non_pixel_clock", "screen_source", "viewer_no_heartbeat",
                 "production_cold_start", "bad_wifi_prejoin", "screen_keyframe_spam",
                 "screen_native_flap"})
for source in ["", "screen_"]:
    required.update(source + name for name in [
        "viewer_256k", "bandwidth_flaps", "competing_512kbit_1v", "competing_2mbit_1v",
        "competing_512kbit_2v", "competing_fifo_512kbit_1v", "competing_fifo_512kbit_2v",
        "competing_fifo_2mbit_1v", "competing_fifo_2mbit_2v",
        "competing_5mbit_1v_prejoin_rtt160", "competing_fifo_10mbit_2v_prejoin_rtt160",
        "competing_512kbit_1v_prejoin_rtt160", "competing_fifo_512kbit_2v_prejoin_rtt160",
    ])
native_rows = {f"screen_native_{rate}mbit_{history}" for rate in [3, 5, 10]
               for history in ["prejoin", "established"]}
production_rows = {f"production_native_10mbit_{history}" for history in ["prejoin", "established"]}
required.update(native_rows | production_rows)
missing = sorted(required - by_name.keys())
if missing:
    issues.append("missing required rows: " + ", ".join(missing))

repeated = native_rows | production_rows | {
    "bad_wifi", "bad_wifi_prejoin", "screen_competing_fifo_512kbit_2v",
    "competing_fifo_512kbit_1v", "bandwidth_flaps", "screen_bandwidth_flaps", "screen_native_flap",
    "screen_source", "screen_keyframe_spam",
}
repeats = {}
for name in sorted(repeated):
    rows = by_name.get(name, [])
    repeats[name] = {"passes": sum(row["pass"] for row in rows), "observations": len(rows)}
    if len(rows) < 5 or any(not row["pass"] for row in rows):
        issues.append(f"{name}: requires at least five unchanged passing observations")

for row in by_name.get("bad_wifi", []) + by_name.get("bad_wifi_prejoin", []):
    metrics = row.get("metrics", {})
    p95 = metrics.get("g2g_p95_ms")
    if row.get("observation_s", 0) < 300 or metrics.get("freezes_over_500ms") != 0 or not isinstance(p95, (int, float)) or p95 > 305:
        issues.append(f"{row['run']}/{row['name']}: strict 300s Wi-Fi gate failed")


def audit_native(row, viewer, window, hold=False, padding=False, resolution=True):
    """Recompute resolution and padding from timestamped samples, not pass flags."""
    label = f"{row['run']}/{row['name']}"
    start, end = window.get("from"), window.get("to")
    if not isinstance(start, (int, float)) or not isinstance(end, (int, float)):
        issues.append(label + ": missing exact score window")
        return
    samples = [s for s in viewer.get("samples", []) if start <= s["timestamp"] <= end]
    native = [s for s in samples if s.get("layer") == "L1T3" and
              s.get("width", 0) >= 2560 and s.get("height", 0) >= 1440]
    span = samples[-1]["timestamp"] - samples[0]["timestamp"] if len(samples) > 1 else 0
    if hold:
        deadline = window.get("start")
        admitted = any(s["timestamp"] >= deadline and s["timestamp"] <= deadline + 20000
                       and s.get("layer") == "L1T3" and s.get("width", 0) >= 2560
                       and s.get("height", 0) >= 1440 for s in viewer.get("samples", [])) if deadline is not None else False
        if not admitted or len(samples) < 60 or span < 30000 or len(native) != len(samples):
            issues.append(label + ": raw native admission/30s hold failed")
    elif resolution and (len(samples) < 10 or len(native) < .9 * len(samples)):
        issues.append(label + ": raw native resolution/layer floor failed")
    if resolution:
        rates = sorted(s["fps"] for s in row.get("publisher", {}).get("samples", [])
                       if start <= s["timestamp"] <= end and isinstance(s.get("fps"), (int, float)))
        offered = rates[max(0, (len(rates) + 1) // 2 - 1)] if rates else 0
        previous, fresh = None, 0
        for frame in viewer.get("timeline", []):
            number = frame.get("frame")
            advancing = number is not None and (previous is None or number > previous)
            if advancing:
                previous = number
            delay = frame.get("delay")
            if advancing and start <= frame["at"] <= end and isinstance(delay, (int, float)) and 0 <= delay <= 1000:
                fresh += 1
        # Match the existing whole-frame 90% floor, independently of harness flags.
        minimum = int(offered * (end - start) / 1000 * .9 + .5)
        if offered <= 0 or fresh < minimum:
            issues.append(label + ": raw native fresh cadence floor failed")
    if padding:
        counters = [s.get("repair", {}) for s in samples]
        valid = span >= 30000 and all(isinstance(c.get(k), (int, float))
                                     for c in counters for k in ("wireBytes", "paddingBytes"))
        reset = any(c[k] < previous[k] for previous, c in zip(counters, counters[1:])
                    for k in ("wireBytes", "paddingBytes")) if valid else True
        wire = counters[-1]["wireBytes"] - counters[0]["wireBytes"] if valid else 0
        pad = counters[-1]["paddingBytes"] - counters[0]["paddingBytes"] if valid else 0
        if not valid or reset or wire <= 0 or pad / wire > .5:
            issues.append(label + ": raw 30s padding ratio failed")


for row in browser:
    relevant = row["name"] in native_rows | production_rows | {"screen_native_flap", "screen_keyframe_spam", "screen_bandwidth_flaps"} or row["name"].startswith("screen_competing_")
    if relevant and not row.get("viewers"):
        issues.append(f"{row['run']}/{row['name']}: missing raw viewer evidence")
        continue
    if row["name"] in native_rows | production_rows:
        audit_native(row, row["viewers"][0], row, hold=True, padding=True)
    elif row["name"] == "screen_native_flap":
        for interval in row["intervals"]:
            audit_native(row, row["viewers"][0], interval,
                         hold=interval["rate"] != "256kbit", padding=True,
                         resolution=interval["rate"] != "256kbit")
    elif row["name"] == "screen_keyframe_spam":
        for viewer in row["viewers"][1:]:
            audit_native(row, viewer, row)
    elif row["name"].startswith("screen_competing_"):
        # Derive applicability independently; a mistaken harness boolean must
        # not let detailed video pass at 180p on an affordable native path.
        bps = int(row["rate"].removesuffix("mbit")) * 1_000_000 if row["rate"].endswith("mbit") else int(row["rate"].removesuffix("kbit")) * 1000
        offered = row.get("source_workload", {}).get("offered_bps", 0)
        publisher = row.get("publisher", {})
        key_bytes = [publisher.get("quality", {}).get("repair", {}).get("sourceKeyframeBytes", 0)]
        key_bytes.extend(sample.get("repair", {}).get("sourceKeyframeBytes", 0)
                         for sample in publisher.get("samples", [])
                         if row["from"] <= sample["timestamp"] <= row["to"])
        count = len(row["viewers"])
        affordable = offered > 0 and max(offered, 8 * max(key_bytes)) * count <= bps * .5
        for viewer, fairness in zip(row["viewers"], row["fairness"]):
            if affordable or fairness["offered_bps"] > 0 and fairness["offered_bps"] * count <= bps * .5:
                audit_native(row, viewer, row)
    elif row["name"] == "screen_bandwidth_flaps":
        for interval in row["intervals"]:
            if interval["rate"] != "256kbit":
                audit_native(row, row["viewers"][0],
                             {"from": interval["from"] + 20000, "to": interval["to"]})

failed = [f"{row['run']}/{row['name']}" for row in browser if not row["pass"]]
if failed:
    issues.append("browser quality failures: " + ", ".join(failed))
if not turn or any(not row["pass"] for row in turn):
    issues.append("missing or failing separate forced TURN loss30 endpoint observation")

capacity = []
for path in sorted((RESULTS / "capacity").glob(prefix + "*.json")):
    row = read(path)
    if not row.get("load_invalid"):
        capacity.append(row)
if {row.get("streams") for row in capacity if row.get("pass") and row.get("binary_sha256") == pin} != {1, 512}:
    issues.append("missing matched passing 512-peer capacity observations")
if any(not row.get("pass") or row.get("binary_sha256") != pin or row.get("viewers") != 512 or row.get("nice") != 10 for row in capacity):
    issues.append("failing, unmatched or invalid capacity observation")

verification_path = RESULTS / "verification" / (prefix + ".json")
verification = read(verification_path) if verification_path.exists() else {}
checks = verification.get("checks", [])
if verification.get("binary_sha256") != pin or verification.get("rebuilt_sha256") != pin or "statically linked" not in verification.get("file", "") or len(checks) < 6 or any(check.get("exit_code") != 0 for check in checks):
    issues.append("missing matched code/rebuild verification")

audit = {"binary_sha256": pin, "browser_passes": len(browser) - len(failed),
         "browser_observations": len(browser), "turn_endpoint_passes": sum(row["pass"] for row in turn),
         "turn_endpoint_observations": len(turn), "diagnostics": len(diagnostic),
         "load_rejected": len(rejected), "repeats": repeats, "missing": missing,
         "issues": issues, "pass": not issues}
destination = RESULTS / "verification" / (prefix + "-audit.json")
destination.parent.mkdir(parents=True, exist_ok=True)
destination.write_text(json.dumps(audit, indent=2) + "\n")
print(json.dumps(audit, indent=2))
raise SystemExit(bool(issues))
