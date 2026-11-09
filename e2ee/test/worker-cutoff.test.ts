/**
 * Phase 2c review (HIGH): the 300 ms kick cutoff must hold even when the main
 * thread stalls. The worker gets the cutoff synchronously at kick time and
 * enforces it on its own clock; SenderKeyManager.tick() never runs here
 * (the "stalled UI thread").
 */
import { beforeEach, describe, expect, it } from "vitest";
import { randomBytes } from "../src/bytes.ts";
import type { EpochKey } from "../src/channel.ts";
import type { EncodedFrameLike } from "../src/frame-cryptor.ts";
import { SenderKeyManager } from "../src/keys.ts";
import { MemoryStorage } from "../src/storage.ts";
import { installTransformWorker, type ScriptTransformerLike, type WorkerScopeLike } from "../src/worker-runtime.ts";
import { FakeFrame } from "./media.ts";

class Scope implements WorkerScopeLike {
  onrtctransform: WorkerScopeLike["onrtctransform"] = null;
  onmessage: WorkerScopeLike["onmessage"] = null;
  out: unknown[] = [];
  postMessage(m: unknown) {
    this.out.push(m);
  }
  send(data: unknown) {
    this.onmessage!({ data });
  }
}

const live: Array<{ idle: () => boolean; outLen: () => number }> = [];
function pipe(options: unknown) {
  const out: EncodedFrameLike[] = [];
  // A pull source, so the test can tell when the worker has consumed every
  // pushed frame and is waiting for more (it pulls again only after the
  // previous frame was encrypted and written, or dropped).
  const queued: EncodedFrameLike[] = [];
  let waiting: ((f: EncodedFrameLike | undefined) => void) | null = null;
  let closed = false;
  let taken = 0;
  const readable = new ReadableStream<EncodedFrameLike>(
    {
      pull: (c) =>
        new Promise<void>((resolve) => {
          const give = (f: EncodedFrameLike | undefined) => {
            waiting = null;
            if (f) c.enqueue(f);
            else c.close();
            resolve();
          };
          if (queued.length) {
            taken++;
            give(queued.shift());
          }
          else if (closed) give(undefined);
          else waiting = give;
        }),
    },
    { highWaterMark: 0 },
  );
  const writable = new WritableStream<EncodedFrameLike>({ write: (f) => void out.push(f) });
  const t: ScriptTransformerLike = { readable, writable, options, generateKeyFrame: async () => undefined };
  live.push({ idle: () => queued.length === 0 && (waiting !== null || taken === 0 || closed), outLen: () => out.length });
  return {
    t,
    out,
    push: (f: EncodedFrameLike) => {
      if (waiting) {
        taken++;
        waiting(f);
      } else queued.push(f);
    },
    close: () => {
      closed = true;
      if (waiting) waiting(undefined);
    },
  };
}
beforeEach(() => void live.splice(0));
// Wait until every transform has consumed all pushed frames and gone back to
// waiting, with its output unchanged for a moment. Bounded, so a stuck worker
// fails the assertion that follows rather than hanging. A fixed 20 ms sleep was
// flaky under host load (encryption is async WebCrypto).
const settle = async () => {
  const until = Date.now() + 3_000;
  let last = -1;
  let stableSince = 0;
  for (;;) {
    await new Promise((r) => setTimeout(r, 5));
    const len = live.reduce((n, l) => n + l.outLen(), 0);
    const idle = live.every((l) => l.idle());
    if (len !== last) {
      last = len;
      stableSince = Date.now();
    }
    if (idle && Date.now() - stableSince >= 15) return;
    if (Date.now() > until) return;
  }
};
const epochOf = (f: EncodedFrameLike) => {
  // SFrame header: KID = epoch*16 + track (track 0), short or long form.
  const b = new Uint8Array(f.data);
  const cfg = b[0]!;
  if (!(cfg & 0x80)) return ((cfg >> 4) & 7) >> 4;
  const len = ((cfg >> 4) & 7) + 1;
  let kid = 0;
  for (let i = 0; i < len; i++) kid = kid * 256 + b[1 + i]!;
  return Math.floor(kid / 16);
};

describe("worker-enforced revocation cutoff", () => {
  it("drops frames past the cutoff with a stalled main thread, resumes on a fresh epoch", async () => {
    let mono = 1_000;
    const scope = new Scope();
    const w = installTransformWorker(scope, { monotonic: () => mono, dropReportMs: 0 });
    const tx = pipe({ role: "encrypt", kind: "video", track: 0, codec: "video/VP9" });
    scope.onrtctransform!({ transformer: tx.t });

    // The real key manager, wired like core.ts; its tick() is never called.
    let wall = 50_000;
    const keys = new SenderKeyManager({
      storage: new MemoryStorage(),
      streamId: "s",
      now: () => wall,
      send: () => undefined,
      onActivate: (k: EpochKey) => scope.send({ type: "sender-epoch", epoch: k.epoch, secret: k.secret.slice().buffer }),
      onSuspend: () => scope.send({ type: "sender-suspend" }),
      onCutoff: (c) => scope.send({ type: "sender-cutoff", at: mono + c.within, within: c.within, stale: c.stale }),
    });
    await keys.init();
    await keys.admit({ id: "kicked", sealRekey: async () => new Uint8Array([1]) } as never);
    await keys.admit({ id: "stays", sealRekey: async () => new Uint8Array([2]) } as never);
    expect(keys.activeEpoch).toBe(1);

    const kicked = keys.kick(["kicked"]);
    expect(w.cutoff).toEqual({ at: 1_300, stale: 1 });

    // Before the cutoff, epoch 1 still flows.
    mono = 1_250;
    tx.push(new FakeFrame(randomBytes(300)));
    await settle();
    expect(tx.out).toHaveLength(1);

    // UI thread stalled: no ack handled, no tick(). The worker's clock moves on.
    for (const at of [1_300, 1_425, 1_557, 1_689]) {
      mono = at;
      wall += 140;
      tx.push(new FakeFrame(randomBytes(300)));
    }
    await settle();
    expect(tx.out).toHaveLength(1);
    expect(w.senderRing.epoch).toBeNull();
    expect(scope.out.some((m) => (m as { reason?: string }).reason === "cutoff")).toBe(true);

    // A late re-post of the stale epoch is refused.
    scope.send({ type: "sender-epoch", epoch: 1, secret: randomBytes(32).buffer });
    expect(w.senderRing.epoch).toBeNull();

    // The stall ends: tick() activates the fresh epoch, the worker resumes.
    await kicked;
    await keys.tick();
    expect(keys.activeEpoch).toBe(2);
    mono = 1_900;
    tx.push(new FakeFrame(randomBytes(300)));
    await settle();
    expect(tx.out).toHaveLength(2);
    expect(epochOf(tx.out[1]!)).toBe(2);
    expect(w.cutoff).toBeNull();
    tx.close();
  });

  it("drops a frame whose encryption straddles the cutoff", async () => {
    // The before-check reads 299 (fine), the AEAD runs, the after-check reads 301.
    const reads = [0, 299, 301];
    let i = 0;
    const scope = new Scope();
    const w = installTransformWorker(scope, { monotonic: () => reads[Math.min(i++, reads.length - 1)]!, dropReportMs: 0 });
    const tx = pipe({ role: "encrypt", kind: "video", track: 0, codec: "video/VP9" });
    scope.onrtctransform!({ transformer: tx.t });
    scope.send({ type: "sender-epoch", epoch: 3, secret: randomBytes(32).buffer });
    scope.send({ type: "sender-cutoff", at: 300, within: 300, stale: 3 }); // reads[0]
    tx.push(new FakeFrame(randomBytes(300)));
    await settle();
    expect(i).toBeGreaterThanOrEqual(3);
    expect(tx.out).toHaveLength(0);
    expect(w.senderRing.epoch).toBeNull();
    tx.close();
  });

  it("uses the earlier of the absolute deadline and receipt + within", () => {
    let mono = 10_000;
    const scope = new Scope();
    const w = installTransformWorker(scope, { monotonic: () => mono });
    scope.send({ type: "sender-cutoff", at: 99_999, within: 300, stale: 0 });
    expect(w.cutoff!.at).toBe(10_300);
    mono = 10_100;
    scope.send({ type: "sender-cutoff", at: 10_200, within: 300, stale: 5 });
    expect(w.cutoff).toEqual({ at: 10_200, stale: 5 });
  });

  it("forgets finished pipes and transforms (PC replacement)", async () => {
    const scope = new Scope();
    const w = installTransformWorker(scope);
    scope.send({ type: "receiver-epoch", epoch: 1, secret: randomBytes(32).buffer });
    for (let i = 0; i < 5; i++) {
      const rx = pipe({ role: "decrypt", kind: "video", track: 0, codec: "video/VP9" });
      scope.onrtctransform!({ transformer: rx.t });
      rx.close();
    }
    await settle();
    expect(w.active).toHaveLength(0);
    expect(w.piped).toHaveLength(0);
    const live = pipe({ role: "decrypt", kind: "audio", track: 1 });
    scope.onrtctransform!({ transformer: live.t });
    expect(w.active).toHaveLength(1);
    expect(w.piped).toHaveLength(1);
    live.close();
    await settle();
    expect(w.active).toHaveLength(0);
  });
});
