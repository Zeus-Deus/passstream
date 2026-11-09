#!/usr/bin/env python3
"""Tabulate bad-Wi-Fi runs (audio_ab.sh pairs or single gate runs) as Markdown.

  python3 -I test/torture/report_audio_ab.py test/torture/results/<prefix>-*.json
"""
import json
import re
import sys


def row(path):
    d = json.load(open(path))
    r = next(x for x in d["results"] if x["name"] == "bad_wifi")
    m, f = r["metrics"], r.get("feedback") or {}
    v = f.get("viewer") or {}
    pub = f.get("publisher") or {}
    vid = [s for s in v.get("ssrcs", []) if s["kind"] == "video" and s["type"] == "inbound-rtp"]
    aud = [s for s in v.get("ssrcs", []) if s["kind"] == "audio" and s["type"] == "inbound-rtp"]
    sfu = f.get("sfu") or {}
    name = path.rsplit("/", 1)[-1].removesuffix(".json")
    pair = re.search(r"-(\d+)-(mline|none)$", name)
    return {
        "run": name,
        "pair": int(pair.group(1)) if pair else None,
        "arm": r.get("audio_mode"),
        "seed": r.get("netem_seed"),
        "binary": d["binary_sha256"][:8],
        "gaps": m["freezes_over_500ms"],
        "max_gap": m["max_gap_ms"],
        "g2g": f'{m["g2g_p50_ms"]}/{m["g2g_p95_ms"]}',
        "repl": m.get("replacements", r.get("replacements")),
        "mlines": f'{v.get("m_lines", {}).get("audio", "?")}/{pub.get("m_lines", {}).get("audio", "?")}',
        "v_pkts": sum(s["packets"] or 0 for s in vid),
        "v_lost": sum(s["lost"] or 0 for s in vid),
        "v_nack": sum(s["nack"] or 0 for s in vid),
        "v_pli": sum(s["pli"] or 0 for s in vid),
        "v_ssrcs": len(vid),
        "a_pkts": sum(s["packets"] or 0 for s in aud),
        "a_nack": sum(s["nack"] or 0 for s in aud),
        "a_ssrcs": len(aud),
        "sfu_replay": (sfu.get("replay") or [None])[0],
        "sfu_audio": sfu.get("audio"),
        "load": f'{r["load_start"].split()[0]} → {r["load_end"].split()[0]}',
        "pass": r["pass"],
    }


rows = [row(p) for p in sys.argv[1:]]
rows.sort(key=lambda x: (x["pair"] or 0, x["run"]))
cols = ["run", "seed", "gaps", "max_gap", "g2g", "repl", "mlines", "v_pkts", "v_lost", "v_nack", "v_pli", "v_ssrcs", "a_pkts", "a_nack", "sfu_replay", "load", "pass"]
print("| " + " | ".join(cols) + " |")
print("|" + "---|" * len(cols))
for x in rows:
    print("| " + " | ".join("PASS" if c == "pass" and x[c] else "**FAIL**" if c == "pass" else str(x[c]) for c in cols) + " |")
pairs = {}
for x in rows:
    if x["pair"] is not None:
        pairs.setdefault(x["pair"], {})[x["arm"]] = x
done = [p for p in pairs.values() if "mline" in p and "none" in p]
if done:
    diff = [p["mline"]["gaps"] - p["none"]["gaps"] for p in done]
    print()
    for arm in ("mline", "none"):
        g = [p[arm]["gaps"] for p in done]
        print(f"- {arm}: gaps {g}, total {sum(g)}, passes {sum(p[arm]['pass'] for p in done)}/{len(done)}, replacements {sum(p[arm]['repl'] or 0 for p in done)}")
    print(f"- paired gap difference (mline - none): {diff}; mline worse in {sum(d > 0 for d in diff)}, better in {sum(d < 0 for d in diff)}, tied in {sum(d == 0 for d in diff)} of {len(diff)} pairs")
