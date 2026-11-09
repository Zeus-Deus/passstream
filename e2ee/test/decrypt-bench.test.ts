/**
 * Bad-Wi-Fi investigation (phase 2c review): is the receive-side SFrame hop
 * slow enough to matter? Per-frame decrypt cost of
 *   - legacy: the phase-2c path (awaited key lookup + object spread, AEAD,
 *     then a full copy into a fresh buffer),
 *   - current: FrameDecryptor with the synchronous hot key lookup and no
 *     plaintext copy when the codec has no clear prefix,
 *   - sync reference: Node's synchronous AES-128-GCM (no promise, no copy
 *     back), the floor for any "synchronous-style" design. Browsers have no
 *     synchronous native AES; a JS AES-GCM would be slower per byte.
 * Prints a table; docs/REPORT.md records it.
 */
import { createDecipheriv } from "node:crypto";
import { cpus } from "node:os";
import { describe, expect, it } from "vitest";
import { concat, randomBytes } from "../src/bytes.ts";
import { FrameDecryptor, FrameEncryptor } from "../src/frame-cryptor.ts";
import { ReceiverKeyRing, SenderKeyRing, deriveBaseKey, kidFor } from "../src/keyring.ts";
import { decodeHeader, deriveKeySalt, makeNonce, MEDIA_SUITE } from "../src/sframe.ts";
import { FakeFrame } from "./media.ts";

const SIZES: Array<[string, number]> = [
  ["600 B (text delta)", 600],
  ["4 KiB (busy delta)", 4 << 10],
  ["30 KiB (text keyframe)", 30 << 10],
  ["256 KiB (screen keyframe)", 256 << 10],
];

async function frames(size: number, n: number, epoch: { epoch: number; secret: Uint8Array<ArrayBuffer> }) {
  const sr = new SenderKeyRing();
  sr.setEpoch(epoch);
  const enc = new FrameEncryptor(sr, { track: 0, kind: "video", codec: "video/VP9" });
  const src = randomBytes(size);
  const out: Uint8Array[] = [];
  for (let i = 0; i < n; i++) {
    const f = new FakeFrame(src, "video/VP9");
    if (!(await enc.encrypt(f))) throw new Error("encrypt");
    out.push(f.bytes());
  }
  return out;
}

async function run(size: number) {
  const epoch = { epoch: 1, secret: randomBytes(32) };
  const n = Math.max(200, Math.min(4000, Math.floor((32 << 20) / size)));
  const all = await frames(size, n + 20, epoch);
  const warm = all.slice(0, 20), cts = all.slice(20);
  const time = async (f: (ct: Uint8Array) => Promise<unknown> | unknown) => {
    for (const ct of warm) await f(ct); // warm-up: keys derived, JIT
    const t0 = performance.now();
    for (const ct of cts) await f(ct);
    return ((performance.now() - t0) * 1000) / n;
  };

  // legacy: awaited lookup with spread, AEAD, then concat(prefix, pt)
  const legacyRing = new ReceiverKeyRing({ replayWindow: 1 << 16 });
  legacyRing.addEpoch(epoch);
  const legacy = await time(async (ct) => {
    const f = new FakeFrame(ct, "video/VP9"); // same frame object cost as below
    const data = new Uint8Array(f.data);
    const h = decodeHeader(data);
    const k = await legacyRing.key(h.kid);
    if (!k.window.check(h.ctr)) throw new Error("replay");
    const pt = await k.key.decrypt(data, new Uint8Array(0), h);
    k.window.commit(h.ctr);
    f.data = concat(new Uint8Array(0), pt).buffer;
  });

  // current FrameDecryptor (hot key lookup, no plaintext copy)
  const ring = new ReceiverKeyRing({ replayWindow: 1 << 16 });
  ring.addEpoch(epoch);
  const dec = new FrameDecryptor(ring, { track: 0, kind: "video", codec: "video/VP9" });
  const current = await time((ct) => dec.decrypt(new FakeFrame(ct, "video/VP9")));

  // synchronous native reference
  const kid = kidFor(1, 0);
  const { key, salt } = deriveKeySalt(MEDIA_SUITE, kid, deriveBaseKey(epoch.secret, kid));
  const sync = await time((ct) => {
    new FakeFrame(ct, "video/VP9"); // same frame object cost
    const h = decodeHeader(ct);
    const body = ct.subarray(h.bytes.length);
    const d = createDecipheriv("aes-128-gcm", key, makeNonce(salt, h.ctr));
    d.setAAD(h.bytes);
    d.setAuthTag(body.subarray(body.length - 16));
    return Buffer.concat([d.update(body.subarray(0, body.length - 16)), d.final()]);
  });
  return { n, legacy, current, sync, dropped: dec.stats.dropped };
}

describe("decrypt path cost", () => {
  it("legacy vs fewer copies vs synchronous native", async () => {
    const rows = [`node ${process.version}, ${cpus()[0]?.model ?? "unknown CPU"}`, "| frame | frames | legacy µs | current µs | sync native µs |", "|---|---|---|---|---|"];
    for (const [label, size] of SIZES) {
      const r = await run(size);
      rows.push(`| ${label} | ${r.n} | ${r.legacy.toFixed(1)} | ${r.current.toFixed(1)} | ${r.sync.toFixed(1)} |`);
      expect(r.dropped).toEqual({});
    }
    console.log(`\n${rows.join("\n")}\n`);
  });
});
