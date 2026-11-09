#!/usr/bin/env python3
"""Run the existing capacity gates with the browser cohort's load contract.

Invoke under nice -n 10 in a network namespace with loopback MTU 1500.
Usage: run_capacity_round4.py SERVER_BINARY RUN_PREFIX
"""
import json
import os
from pathlib import Path
import subprocess
import sys
import time

binary, prefix = sys.argv[1:]
entry_limit = float(os.environ.get("TORTURE_MAX_LOAD", "1"))
observed_limit = float(os.environ.get("TORTURE_MAX_OBSERVED_LOAD", "3"))
if os.getpriority(os.PRIO_PROCESS, 0) != 10:
    raise SystemExit("run with nice -n 10")
out = Path("test/torture/results/capacity")
out.mkdir(parents=True, exist_ok=True)

def load():
    return Path("/proc/loadavg").read_text().strip()

for streams in [1, 512]:
    for attempt in range(1, 6):
        deadline = time.monotonic() + float(os.environ.get("TORTURE_LOAD_WAIT_MS", "600000")) / 1000
        while float(load().split()[0]) > entry_limit:
            if time.monotonic() >= deadline:
                raise SystemExit("low-load entry condition was not met")
            time.sleep(1)
        entry = load()
        path = out / f"{prefix}-streams-{streams}-attempt{attempt}.json"
        process = subprocess.Popen([
            "./build/capacity", "-server", binary, "-viewers", "512",
            "-streams", str(streams), "-seconds", "12", "-out", str(path),
        ])
        samples = [entry]
        while process.poll() is None:
            samples.append(load())
            time.sleep(.25)
        if not path.exists():
            raise SystemExit(f"capacity process failed: {process.returncode}")
        result = json.loads(path.read_text())
        result.update(nice=10, entry_load=entry, setup_load_samples=samples,
                      load_limit=entry_limit, observed_load_limit=observed_limit,
                      load_invalid=max(float(s.split()[0]) for s in samples) > observed_limit)
        path.write_text(json.dumps(result, indent=2) + "\n")
        print(path, "LOAD REJECTED" if result["load_invalid"] else
              "PASS" if result["pass"] else "FAIL", flush=True)
        if result["load_invalid"]:
            continue
        if not result["pass"] or process.returncode:
            raise SystemExit(1)
        break
    else:
        raise SystemExit("all capacity attempts exceeded the load limit")
