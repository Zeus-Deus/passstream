import { describe, expect, it } from "vitest";
import { randomBytes, toHex } from "../src/bytes.ts";
import { E2EE_VIDEO_CODECS, frameLayout } from "../src/codec.ts";
import { FrameDecryptor, FrameEncryptor } from "../src/frame-cryptor.ts";
import { kidFor, ReceiverKeyRing, SenderKeyRing } from "../src/keyring.ts";
import { decodeHeader, ReplayWindow, SFrameKey } from "../src/sframe.ts";
import { FakeFrame, vp8Frame } from "./media.ts";

function setup(track = 0, kind: "video" | "audio" = "video", codec?: string) {
  const epoch = { epoch: 7, secret: randomBytes(32) };
  const sring = new SenderKeyRing();
  sring.setEpoch(epoch);
  const rring = new ReceiverKeyRing();
  rring.addEpoch(epoch);
  const opts = { track, kind, ...(codec ? { codec } : {}) };
  return { epoch, sring, rring, enc: new FrameEncryptor(sring, opts), dec: new FrameDecryptor(rring, opts) };
}

const CASES: Array<[string, () => Uint8Array, string | undefined, "video" | "audio"]> = [
  ["VP8 key", () => vp8Frame(true), "video/VP8", "video"],
  ["VP8 delta", () => vp8Frame(false), "video/VP8", "video"],
  ["VP9", () => randomBytes(5000), "video/VP9", "video"],
  ["Opus", () => randomBytes(120), "audio/opus", "audio"],
];

describe("SFrame over encoded frames", () => {
  for (const [name, make, mime, kind] of CASES) {
    it(`round-trips ${name} and leaves only the documented prefix clear`, async () => {
      const { enc, dec } = setup(0, kind);
      const pt = make();
      const f = new FakeFrame(pt, mime);
      expect(await enc.encrypt(f)).toBe(true);
      const ct = f.bytes();
      const { clear } = frameLayout(mime === "audio/opus" ? "audio" : (mime!.split("/")[1]!.toLowerCase() as "vp8"), pt);
      expect(toHex(ct.subarray(0, clear))).toBe(toHex(pt.subarray(0, clear)));
      expect(toHex(ct.subarray(clear, clear + 64))).not.toBe(toHex(pt.subarray(clear, clear + 64)));
      expect(await dec.decrypt(f)).toBe(true);
      expect(toHex(f.bytes())).toBe(toHex(pt));
    });
  }

  it("VP8 clear prefix is 10 bytes on keyframes, 3 on delta frames", () => {
    expect(frameLayout("vp8", vp8Frame(true)).clear).toBe(10);
    expect(frameLayout("vp8", vp8Frame(false)).clear).toBe(3);
  });

  it("refuses H.264 and AV1 (fail closed) on both sides, and frames without a known codec", async () => {
    // Review round 1, finding 5: libwebrtc parses H.264 slice headers before
    // the receive transform and may rewrite SPS, so H.264 is not carried under E2EE.
    expect(E2EE_VIDEO_CODECS).toEqual(["video/VP9"]);
    const { enc, dec } = setup();
    for (const mime of ["video/H264", "video/AV1"]) {
      const pt = Uint8Array.from([0, 0, 0, 1, 0x65, 0x88, ...randomBytes(100)]);
      const f = new FakeFrame(pt, mime);
      expect(await enc.encrypt(f)).toBe(false);
      expect(toHex(f.bytes())).toBe(toHex(pt)); // untouched, and dropped by the caller
      expect(await dec.decrypt(new FakeFrame(randomBytes(100), mime))).toBe(false);
    }
    expect(enc.stats.dropped.unsupported_codec).toBe(2);
    expect(dec.stats.dropped.unsupported_codec).toBe(2);
    expect(() => frameLayout("h264", Uint8Array.of(0, 0, 1, 0x65, 0))).toThrow(expect.objectContaining({ code: "unsupported_codec" }));
    const g = new FakeFrame(randomBytes(100));
    expect(await enc.encrypt(g)).toBe(false);
    expect(enc.stats.dropped.no_codec).toBe(1);
    // The configured fallback codec is used when metadata lacks one.
    const { enc: enc2 } = setup(0, "video", "video/VP9");
    expect(await enc2.encrypt(new FakeFrame(randomBytes(100)))).toBe(true);
  });

  it("never emits plaintext without a key", async () => {
    const enc = new FrameEncryptor(new SenderKeyRing(), { track: 0, kind: "video" });
    const pt = randomBytes(300);
    const f = new FakeFrame(pt, "video/VP9");
    expect(await enc.encrypt(f)).toBe(false);
    expect(enc.stats.dropped.unknown_key).toBe(1);
  });

  it("detects tampering anywhere in the frame, including the clear prefix", async () => {
    for (const [name, make, mime, kind] of CASES) {
      const { enc, dec } = setup(0, kind);
      const f = new FakeFrame(make(), mime);
      await enc.encrypt(f);
      const ct = f.bytes();
      const positions = new Set([0, 1, 2, 5, 9, 10, 11, 20, Math.floor(ct.length / 2), ct.length - 17, ct.length - 1]);
      for (const i of positions) {
        if (i < 0 || i >= ct.length) continue;
        const t = Uint8Array.from(ct);
        t[i] = t[i]! ^ 0x01;
        const ff = new FakeFrame(t, mime);
        // Either the layout/escape/header parse fails or AEAD auth fails; never accepted.
        expect(await dec.decrypt(ff), `${name} byte ${i}`).toBe(false);
      }
      // Truncation and extension too.
      expect(await dec.decrypt(new FakeFrame(ct.subarray(0, ct.length - 1), mime))).toBe(false);
      expect(await dec.decrypt(new FakeFrame(Uint8Array.from([...ct, 0]), mime))).toBe(false);
    }
  });

  it("rejects replayed frames and frames older than the window", async () => {
    const { enc, dec } = setup();
    const frames: FakeFrame[] = [];
    for (let i = 0; i < 1100; i++) {
      const f = new FakeFrame(randomBytes(50), "video/VP9");
      await enc.encrypt(f);
      frames.push(f);
    }
    const copy = (f: FakeFrame) => f.clone();
    expect(await dec.decrypt(copy(frames[5]!))).toBe(true);
    expect(await dec.decrypt(copy(frames[5]!))).toBe(false); // exact replay
    expect(dec.stats.dropped.replay).toBe(1);
    expect(await dec.decrypt(copy(frames[3]!))).toBe(true); // reordered but new: fine
    expect(await dec.decrypt(copy(frames[1099]!))).toBe(true);
    expect(await dec.decrypt(copy(frames[10]!))).toBe(false); // > 1024 behind the newest
    expect(await dec.decrypt(copy(frames[1098]!))).toBe(true);
    expect(dec.stats.dropped.replay).toBe(2);
  });

  it("a forged frame does not advance the replay window", async () => {
    const { enc, dec } = setup();
    const f = new FakeFrame(randomBytes(50), "video/VP9");
    await enc.encrypt(f);
    const ct = f.bytes();
    const h = decodeHeader(ct);
    // Same header (same CTR), garbage body: rejected, and the genuine frame still decrypts.
    const forged = Uint8Array.from(ct);
    forged[h.bytes.length + 3] = forged[h.bytes.length + 3]! ^ 0xff;
    expect(await dec.decrypt(new FakeFrame(forged, "video/VP9"))).toBe(false);
    expect(await dec.decrypt(new FakeFrame(ct, "video/VP9"))).toBe(true);
  });

  it("binds frames to their track: audio spliced into the video stream is dropped", async () => {
    const epoch = { epoch: 3, secret: randomBytes(32) };
    const sring = new SenderKeyRing();
    sring.setEpoch(epoch);
    const rring = new ReceiverKeyRing();
    rring.addEpoch(epoch);
    const audioEnc = new FrameEncryptor(sring, { track: 1, kind: "audio" });
    const videoDec = new FrameDecryptor(rring, { track: 0, kind: "video", codec: "video/VP9" });
    const f = new FakeFrame(randomBytes(80), "audio/opus");
    await audioEnc.encrypt(f);
    expect(await videoDec.decrypt(new FakeFrame(f.bytes(), "video/VP9"))).toBe(false);
    expect(videoDec.stats.dropped.wrong_track).toBe(1);
  });

  it("one encryptor per track per realm", () => {
    const r = new SenderKeyRing();
    const release = r.claimTrack(0);
    expect(() => r.claimTrack(0)).toThrow(/already has an encryptor/);
    release();
    expect(() => r.claimTrack(0)).not.toThrow();
  });

  it("refuses to exceed the counter limit", async () => {
    const k = await SFrameKey.create({ kid: kidFor(1, 0), baseKey: randomBytes(32), direction: "encrypt", maxCtr: 2n });
    await k.encrypt(randomBytes(4));
    await k.encrypt(randomBytes(4));
    await k.encrypt(randomBytes(4));
    await expect(k.encrypt(randomBytes(4))).rejects.toMatchObject({ code: "counter_exhausted" });
  });

  it("encrypt-only and decrypt-only keys stay that way", async () => {
    const base = randomBytes(32);
    const e = await SFrameKey.create({ kid: 9n, baseKey: base, direction: "encrypt" });
    const d = await SFrameKey.create({ kid: 9n, baseKey: base, direction: "decrypt" });
    await expect(e.decrypt(await e.encrypt(randomBytes(4)))).rejects.toMatchObject({ code: "state" });
    await expect(d.encrypt(randomBytes(4))).rejects.toMatchObject({ code: "state" });
  });
});

describe("ReplayWindow", () => {
  it("accepts new counters once, in any order inside the window", () => {
    const w = new ReplayWindow(64);
    const order = [5n, 1n, 3n, 2n, 70n, 7n, 69n, 6n];
    for (const c of order) {
      const ok = w.check(c);
      if (ok) w.commit(c);
    }
    expect(w.check(5n)).toBe(false);
    expect(w.check(70n)).toBe(false);
    expect(w.check(69n)).toBe(false);
    expect(w.check(6n)).toBe(false); // 70 - 6 = 64: outside the window now
    expect(w.check(8n)).toBe(true);
    expect(w.check(71n)).toBe(true);
  });
});
