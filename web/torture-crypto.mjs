// E2EE diagnostics for the torture suite: summaries of the transform
// workers' per-frame traces (e2ee/src/worker-runtime.ts TraceRow) and a
// forensic view of every >500 ms decoded-frame gap.
//
// TraceRow: [mono start ms, role 0 enc / 1 dec, track, residence ms | null,
//            crypto ms, size in, size out, keyframe, true | drop reason]
import { pct } from "./torture-metrics.mjs";

const dist = (a) => (a.length ? { n: a.length, p50: pct(a, 0.5), p95: pct(a, 0.95), p99: pct(a, 0.99), max: Math.max(...a) } : { n: 0 });
const round = (o) => JSON.parse(JSON.stringify(o, (_, v) => (typeof v === "number" ? Math.round(v * 1000) / 1000 : v)));

/**
 * Chrome's VP9 packetizer keeps RTP packets under ~1200 bytes; with the RTP
 * header, extensions and the VP9 descriptor about 1,150 bytes of frame data
 * fit in one packet. The count is an estimate used only to compare the same
 * frames before and after SFrame.
 */
export const PAYLOAD_PER_PACKET = 1150;
const packets = (bytes) => Math.max(1, Math.ceil(bytes / PAYLOAD_PER_PACKET));

export function cryptoSummary(rows, role, track = 0) {
  const r = rows.filter((x) => x[1] === role && x[2] === track);
  const ok = r.filter((x) => x[8] === true);
  const reasons = {};
  for (const x of r) if (x[8] !== true) reasons[x[8]] = (reasons[x[8]] ?? 0) + 1;
  const sizesIn = ok.map((x) => x[5]), sizesOut = ok.map((x) => x[6]);
  const overhead = ok.map((x) => (role === 0 ? x[6] - x[5] : x[5] - x[6]));
  const plain = role === 0 ? sizesIn : sizesOut, cipher = role === 0 ? sizesOut : sizesIn;
  let extraPackets = 0, crossed = 0;
  for (let i = 0; i < plain.length; i++) {
    const d = packets(cipher[i]) - packets(plain[i]);
    extraPackets += d;
    if (d > 0) crossed++;
  }
  return round({
    frames: r.length,
    failed: r.length - ok.length,
    failed_by_reason: reasons,
    failed_keyframes: r.filter((x) => x[8] !== true && x[7]).length,
    keyframes: ok.filter((x) => x[7]).length,
    crypto_ms: dist(ok.map((x) => x[4])),
    residence_ms: dist(r.map((x) => x[3]).filter((x) => x !== null)),
    plaintext_bytes: dist(plain),
    ciphertext_bytes: dist(cipher),
    sframe_overhead_bytes: dist(overhead),
    est_packets_plain: plain.reduce((n, b) => n + packets(b), 0),
    est_packets_sframe: cipher.reduce((n, b) => n + packets(b), 0),
    frames_crossing_a_packet_boundary: crossed,
    extra_packets: extraPackets,
  });
}

/** Every >500 ms gap between decoded frames, with what the crypto, transport and repair did around it. */
export function gapForensics(viewer, rows, timeOrigin, from, to) {
  const frames = viewer.timeline.filter((f) => f.at >= from && f.at <= to);
  const gaps = [];
  for (let i = 1; i < frames.length; i++) if (frames[i].at - frames[i - 1].at > 500) gaps.push([frames[i - 1].at, frames[i].at]);
  // Trace rows are monotonic ms since the worker's time origin base (performance.timeOrigin + now()).
  const wall = (x) => x[0] - (timeOrigin ?? 0);
  return gaps.map(([a, b]) => {
    const near = rows.filter((x) => x[1] === 1 && x[2] === 0 && wall(x) >= a - 1000 && wall(x) <= b);
    const before = viewer.samples.filter((s) => s.timestamp <= a).at(-1);
    const after = viewer.samples.find((s) => s.timestamp >= b);
    const delta = (k) => (before?.stats && after?.stats && typeof after.stats[k] === "number" ? after.stats[k] - (before.stats[k] ?? 0) : null);
    const rep = (k) => (before?.repair && after?.repair ? JSON.stringify(after.repair[k]) + " ← " + JSON.stringify(before.repair[k]) : null);
    const events = viewer.events.filter((e) => e.at >= a - 1000 && e.at <= b + 500).map((e) => `${e.at - a}:${e.kind}`);
    return round({
      at: a,
      gap_ms: b - a,
      decrypts_in_window: near.length,
      decrypt_failures: near.filter((x) => x[8] !== true).map((x) => `${Math.round(wall(x) - a)}:${x[8]}${x[7] ? ":key" : ""}`),
      max_decrypt_ms: near.length ? Math.max(...near.map((x) => x[4])) : null,
      max_residence_ms: Math.max(0, ...near.map((x) => x[3] ?? 0)),
      last_decrypt_before_gap_ms: near.filter((x) => wall(x) <= a).length ? a - Math.max(...near.filter((x) => wall(x) <= a).map(wall)) : null,
      first_decrypt_in_gap_after_ms: near.filter((x) => wall(x) > a).length ? Math.min(...near.filter((x) => wall(x) > a).map(wall)) - a : null,
      decrypted_during_gap: near.filter((x) => wall(x) > a + 50 && wall(x) < b - 50).length,
      keyframe_decrypted_in_gap: near.some((x) => x[7] && wall(x) > a),
      stats_delta: { packetsLost: delta("packetsLost"), nackCount: delta("nackCount"), pliCount: delta("pliCount"), framesDropped: delta("framesDropped"), fecPacketsReceived: delta("fecPacketsReceived"), framesDecoded: delta("framesDecoded") },
      repair: { replay: rep("replay"), suppressedRepair: rep("suppressedRepair"), pacerDrops: rep("pacerDrops"), fecPackets: rep("fecPackets") },
      events,
    });
  });
}
