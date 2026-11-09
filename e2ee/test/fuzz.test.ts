/**
 * Fuzz-ish property tests: random and mutated inputs into every parser and
 * state machine. The only acceptable failure is a typed E2EEError.
 */
import { describe, expect, it } from "vitest";
import { decodeAda, ViewerAdmission } from "../src/admission.ts";
import { fromBase64Url, randomBytes } from "../src/bytes.ts";
import { decodeEpochKey } from "../src/channel.ts";
import { frameLayout } from "../src/codec.ts";
import { E2EEError } from "../src/errors.ts";
import { FrameDecryptor } from "../src/frame-cryptor.ts";
import { decodeSenderKey, parseFragment } from "../src/identity.ts";
import { ReceiverKeyRing } from "../src/keyring.ts";
import { decodeHeader } from "../src/sframe.ts";
import { decodeMessage, peekType } from "../src/wire.ts";
import { FakeFrame } from "./media.ts";
import { join, makeHost, viewer } from "./harness.ts";

/** Deterministic PRNG (mulberry32) so failures reproduce. */
function rng(seed: number) {
  let a = seed >>> 0;
  const next = () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
  const int = (n: number) => Math.floor(next() * n);
  const bytes = (n: number) => Uint8Array.from({ length: n }, () => int(256));
  return { next, int, bytes };
}

function mutate(r: ReturnType<typeof rng>, src: Uint8Array): Uint8Array {
  const d = Array.from(src);
  const ops = 1 + r.int(4);
  for (let i = 0; i < ops; i++) {
    const op = r.int(5);
    const at = d.length ? r.int(d.length) : 0;
    if (op === 0 && d.length) d[at] = r.int(256);
    else if (op === 1) d.splice(at, 0, r.int(256));
    else if (op === 2 && d.length) d.splice(at, 1 + r.int(4));
    else if (op === 3) d.length = r.int(d.length + 1);
    else d.push(...r.bytes(r.int(8)));
  }
  return Uint8Array.from(d);
}

function onlyTyped(fn: () => unknown): void {
  try {
    fn();
  } catch (e) {
    if (!(e instanceof E2EEError)) throw new Error(`untyped error: ${String(e)}`);
  }
}

async function onlyTypedAsync(fn: () => Promise<unknown>): Promise<void> {
  try {
    await fn();
  } catch (e) {
    if (!(e instanceof E2EEError)) throw new Error(`untyped error: ${String(e)}`);
  }
}

const N = 20_000;

describe("parsers never throw untyped errors", () => {
  it(`wire messages (${N} random + mutated valid)`, async () => {
    const r = rng(1);
    const h = await makeHost();
    const v = viewer(h);
    const seeds: Uint8Array[] = [v.start()];
    await h.host.receive("p", seeds[0]!);
    seeds.push(...h.take("p"));
    for (let i = 0; i < N; i++) {
      const input = i % 2 ? r.bytes(r.int(300)) : mutate(r, seeds[r.int(seeds.length)]!);
      onlyTyped(() => decodeMessage(input));
      onlyTyped(() => peekType(input));
    }
  });

  it(`SFrame headers (${N})`, () => {
    const r = rng(2);
    for (let i = 0; i < N; i++) onlyTyped(() => decodeHeader(r.bytes(r.int(20))));
  });

  it(`codec layouts (${N})`, () => {
    const r = rng(3);
    for (let i = 0; i < N; i++) {
      const d = r.bytes(r.int(64)).map((b) => (r.next() < 0.5 ? b % 4 : b));
      for (const c of ["vp8", "vp9", "h264", "av1", "audio"] as const) onlyTyped(() => frameLayout(c, d));
    }
  });

  it("fragments, sender keys, base64url, ADa, epoch keys", () => {
    const r = rng(4);
    const alphabet = "ABCabc019-_=+/&#ck ";
    for (let i = 0; i < N; i++) {
      const s = Array.from({ length: r.int(60) }, () => alphabet[r.int(alphabet.length)]).join("");
      onlyTyped(() => parseFragment(s));
      onlyTyped(() => decodeSenderKey(s));
      onlyTyped(() => fromBase64Url(s));
      onlyTyped(() => decodeAda(r.bytes(r.int(12))));
      onlyTyped(() => decodeEpochKey(r.bytes(r.int(48))));
    }
  });
});

describe("state machines never throw untyped errors", () => {
  it("SenderHost.receive swallows garbage and keeps working", async () => {
    const r = rng(5);
    const h = await makeHost();
    const v = viewer(h);
    const seed = v.start();
    for (let i = 0; i < 2000; i++) {
      const input = i % 2 ? r.bytes(r.int(200)) : mutate(r, seed);
      await h.host.receive(`f${i % 7}`, input); // never throws by contract
    }
    expect(h.errors.every((e) => e instanceof E2EEError)).toBe(true);
    await h.host.unlock({ keepPin: true }); // fuzzed RESPONSEs may legitimately have frozen the stream
    await join(h, "honest", viewer(h));
  });

  it("ViewerAdmission.handle on garbage", async () => {
    const r = rng(6);
    const h = await makeHost();
    const v0 = viewer(h);
    await h.host.receive("p", v0.start());
    const [challenge] = h.take("p");
    for (let i = 0; i < 2000; i++) {
      const v = viewer(h);
      v.start();
      await onlyTypedAsync(() => v.handle(i % 2 ? r.bytes(r.int(200)) : mutate(r, challenge!)));
    }
  });

  it(`FrameDecryptor drops ${N / 4} garbage frames without throwing`, async () => {
    const r = rng(7);
    const ring = new ReceiverKeyRing();
    ring.addEpoch({ epoch: 1, secret: randomBytes(32) });
    const mimes = ["video/VP8", "video/VP9", "video/H264", "video/AV1", "audio/opus", undefined];
    const decs = mimes.map((m) => new FrameDecryptor(ring, { track: 0, kind: m?.startsWith("audio") ? "audio" : "video" }));
    for (let i = 0; i < N / 4; i++) {
      const k = r.int(mimes.length);
      const d = r.bytes(r.int(120));
      if (d.length > 1 && r.next() < 0.5) d[0] = 0x80 | 16; // plausible header for KID 16 (epoch 1, track 0)
      expect(await decs[k]!.decrypt(new FakeFrame(d, mimes[k]))).toBe(false);
    }
  });

  it("ViewerAdmission constructor validates inputs with typed errors", () => {
    const r = rng(8);
    for (let i = 0; i < 500; i++) {
      onlyTyped(
        () =>
          new ViewerAdmission({
            streamId: "s",
            senderKey: r.bytes(r.int(40)),
            viewerSid: r.bytes(r.int(20)),
            linkSecret: r.bytes(r.next() < 0.5 ? 32 : r.int(40)),
            pin: r.next() < 0.3 ? null : String.fromCharCode(r.int(40)),
          }),
      );
    }
  });
});
