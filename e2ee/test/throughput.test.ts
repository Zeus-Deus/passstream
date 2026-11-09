/**
 * Throughput of the full frame path (layout, SFrame header, AES-128-GCM via
 * WebCrypto, copies) for 1440p-sized encoded frames. Prints a table; the
 * numbers recorded in docs/PROTOCOL.md come from this test.
 */
import { cpus } from "node:os";
import { describe, expect, it } from "vitest";
import { randomBytes } from "../src/bytes.ts";
import { FrameDecryptor, FrameEncryptor } from "../src/frame-cryptor.ts";
import { ReceiverKeyRing, SenderKeyRing } from "../src/keyring.ts";
import { FakeFrame } from "./media.ts";
import { join, makeHost, viewer } from "./harness.ts";

const SIZES: Array<[string, number]> = [
  ["8 KiB (text-mode delta)", 8 << 10],
  ["64 KiB (busy delta)", 64 << 10],
  ["256 KiB (screen keyframe)", 256 << 10],
  ["1 MiB (large keyframe)", 1 << 20],
];

async function bench(codec: "video/VP9" | "video/VP8", size: number) {
  const epoch = { epoch: 1, secret: randomBytes(32) };
  const sr = new SenderKeyRing();
  sr.setEpoch(epoch);
  const rr = new ReceiverKeyRing({ replayWindow: 1 << 16 });
  rr.addEpoch(epoch);
  const enc = new FrameEncryptor(sr, { track: 0, kind: "video", codec });
  const dec = new FrameDecryptor(rr, { track: 0, kind: "video", codec });
  const src = randomBytes(size);
  if (codec === "video/VP8") src[0] = src[0]! & 0xfe; // keyframe: 10-byte clear prefix
  // Enough frames for ~0.5 s of work per size, at least 50.
  const n = Math.max(50, Math.min(3000, Math.floor((64 << 20) / size)));
  const frames = Array.from({ length: n }, () => new FakeFrame(src, codec));
  for (let i = 0; i < 20; i++) {
    const w = new FakeFrame(src, codec);
    await enc.encrypt(w);
  }
  let t0 = performance.now();
  for (const f of frames) if (!(await enc.encrypt(f))) throw new Error("encrypt failed");
  const encMs = performance.now() - t0;
  t0 = performance.now();
  for (const f of frames) if (!(await dec.decrypt(f))) throw new Error("decrypt failed");
  const decMs = performance.now() - t0;
  const mb = (n * src.length) / 1e6;
  return {
    frames: n,
    encUsPerFrame: (encMs * 1000) / n,
    decUsPerFrame: (decMs * 1000) / n,
    encMBps: mb / (encMs / 1000),
    decMBps: mb / (decMs / 1000),
  };
}

describe("throughput", () => {
  it("encrypt/decrypt 1440p-sized frames", async () => {
    const rows: string[] = [];
    rows.push(`node ${process.version}, ${cpus()[0]?.model ?? "unknown CPU"} (${cpus().length} threads)`);
    rows.push("| codec | frame | frames | enc µs/frame | dec µs/frame | enc MB/s | dec MB/s |");
    rows.push("|---|---|---|---|---|---|---|");
    for (const codec of ["video/VP9", "video/VP8"] as const) {
      for (const [label, size] of SIZES) {
        const r = await bench(codec, size);
        rows.push(
          `| ${codec.slice(6)} | ${label} | ${r.frames} | ${r.encUsPerFrame.toFixed(0)} | ${r.decUsPerFrame.toFixed(0)} | ${r.encMBps.toFixed(0)} | ${r.decMBps.toFixed(0)} |`,
        );
        expect(r.encMBps).toBeGreaterThan(1);
      }
    }
    console.log(`\n${rows.join("\n")}\n`);
  });

  it("admission handshake cost", async () => {
    const h = await makeHost();
    const n = 50;
    const t0 = performance.now();
    for (let i = 0; i < n; i++) await join(h, `p${i}`, viewer(h));
    const ms = (performance.now() - t0) / n;
    console.log(`\nfull PIN admission (both sides, in-process relay): ${ms.toFixed(2)} ms per handshake over ${n}\n`);
    expect(ms).toBeLessThan(1000);
  });
});
