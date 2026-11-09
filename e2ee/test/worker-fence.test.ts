/**
 * The privacy curtain's encoded-frame fence (review D2 #5): frames that reach
 * the encrypt transform within the fence window are dropped whenever they were
 * captured, and video resumes only from a keyframe.
 */
import { describe, expect, it, vi } from "vitest";
import { randomBytes } from "../src/bytes.ts";
import type { EncodedFrameLike } from "../src/frame-cryptor.ts";
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
function pipe(options: unknown) {
  const out: EncodedFrameLike[] = [];
  let ctl!: ReadableStreamDefaultController<EncodedFrameLike>;
  let keyframes = 0;
  const readable = new ReadableStream<EncodedFrameLike>({ start: (c) => void (ctl = c) });
  const writable = new WritableStream<EncodedFrameLike>({ write: (f) => void out.push(f) });
  const t: ScriptTransformerLike = { readable, writable, options, generateKeyFrame: async () => void keyframes++ };
  return { t, out, push: (f: EncodedFrameLike) => ctl.enqueue(f), close: () => ctl.close(), keyframes: () => keyframes };
}
const settle = () => new Promise((r) => setTimeout(r, 20));
const vp9 = (type: "key" | "delta") => new FakeFrame(randomBytes(300), "video/VP9", type);

describe("curtain fence in the sender worker", () => {
  it("drops every video and audio frame in the window, then passes video only from a keyframe", async () => {
    let mono = 10_000;
    const scope = new Scope();
    const w = installTransformWorker(scope, { monotonic: () => mono, dropReportMs: 0 });
    const video = pipe({ role: "encrypt", kind: "video", track: 0, codec: "video/VP9" });
    const low = pipe({ role: "encrypt", kind: "video", track: 2, codec: "video/VP9" });
    const audio = pipe({ role: "encrypt", kind: "audio", track: 1, codec: "audio/opus" });
    for (const p of [video, low, audio]) scope.onrtctransform!({ transformer: p.t });
    scope.send({ type: "sender-epoch", epoch: 1, secret: randomBytes(32).buffer });
    // Before the curtain: everything passes.
    video.push(vp9("delta"));
    audio.push(new FakeFrame(randomBytes(80), "audio/opus"));
    await settle();
    expect([video.out.length, audio.out.length]).toEqual([1, 1]);
    const kfBefore = video.keyframes();
    const lowBefore = low.keyframes();
    // Curtain at 10 000 for 150 ms. Frames encoded in the window (captured before the curtain or not) are dropped.
    scope.send({ type: "sender-fence", at: 10_000, ms: 150 });
    for (const t of [10_000, 10_060, 10_149]) {
      mono = t;
      video.push(vp9("key"));
      low.push(vp9("delta"));
      audio.push(new FakeFrame(randomBytes(80), "audio/opus"));
      await settle();
    }
    expect([video.out.length, low.out.length, audio.out.length]).toEqual([1, 0, 1]);
    // After the window: deltas wait for a keyframe (requested once per transform); audio resumes at once.
    mono = 10_200;
    video.push(vp9("delta"));
    low.push(vp9("delta"));
    audio.push(new FakeFrame(randomBytes(80), "audio/opus"));
    await settle();
    expect([video.out.length, low.out.length, audio.out.length]).toEqual([1, 0, 2]);
    expect(video.keyframes() - kfBefore).toBe(1);
    expect(low.keyframes() - lowBefore).toBe(1);
    video.push(vp9("delta"));
    await settle();
    expect(video.keyframes() - kfBefore).toBe(1); // not per frame...
    mono = 10_460;
    video.push(vp9("delta"));
    await settle();
    expect(video.keyframes() - kfBefore).toBe(2); // ...but again after 250 ms: a lost request can't stall viewers
    video.push(vp9("key"));
    video.push(vp9("delta"));
    low.push(vp9("key"));
    await settle();
    expect([video.out.length, low.out.length]).toEqual([3, 1]);
    const fenceDrops = scope.out.filter((m) => (m as { type: string; reason?: string }).type === "drop" && (m as { reason?: string }).reason === "fence").length;
    expect(fenceDrops).toBeGreaterThan(0);
    expect(w.fence!.until).toBe(10_150);
    for (const p of [video, low, audio]) p.close();
  });

  it("opens when the page says the curtain picture is on the wire, not at the upper bound", async () => {
    let mono = 20_000;
    const scope = new Scope();
    installTransformWorker(scope, { monotonic: () => mono, dropReportMs: 0 });
    const video = pipe({ role: "encrypt", kind: "video", track: 0, codec: "video/VP9" });
    scope.onrtctransform!({ transformer: video.t });
    scope.send({ type: "sender-epoch", epoch: 1, secret: randomBytes(32).buffer });
    scope.send({ type: "sender-fence", at: 20_000, ms: 1000 });
    mono = 20_010;
    video.push(vp9("delta")); // a pre-curtain frame still in the encoder: dropped
    await settle();
    expect(video.out.length).toBe(0);
    mono = 20_030;
    scope.send({ type: "sender-fence-open" }); // replaceTrack to the curtain picture resolved
    video.push(vp9("key")); // the curtain picture's first frame (a resolution change): passes at once
    video.push(vp9("delta"));
    await settle();
    expect(video.out.length).toBe(2);
    video.close();
  });

  it("the window runs from the earlier of the sender's time and receipt; bad fences are refused", () => {
    let mono = 5_000;
    const scope = new Scope();
    const w = installTransformWorker(scope, { monotonic: () => mono });
    scope.send({ type: "sender-fence", at: 4_990, ms: 150 }); // posted 10 ms ago
    expect(w.fence!.until).toBe(5_140);
    // The worker reports when it armed the fence: the guarantee runs from there.
    expect(scope.out.at(-1)).toEqual({ type: "fence", at: 5_000 });
    mono = 5_100;
    scope.send({ type: "sender-fence", at: 99_999, ms: 150 }); // a clock skewed into the future can't stretch it
    expect(w.fence!.until).toBe(5_250);
    scope.send({ type: "sender-fence", at: 5_100, ms: 10_000 });
    expect((scope.out.at(-1) as { type: string }).type).toBe("error");
  });
});

describe("a curtain armed while a frame is in WebCrypto (review D2 r2 #2)", () => {
  async function held(scope: Scope, video: ReturnType<typeof pipe>, during: () => void) {
    let release!: () => void, entered!: () => void;
    const wait = new Promise<void>((r) => (release = r)),
      start = new Promise<void>((r) => (entered = r));
    const orig = crypto.subtle.encrypt.bind(crypto.subtle);
    const hook = vi.spyOn(crypto.subtle, "encrypt").mockImplementation(async (...args) => {
      entered();
      await wait;
      return orig(...args);
    });
    video.push(vp9("delta"));
    await start;
    during();
    release();
    await settle();
    hook.mockRestore();
  }

  it("the reviewer's probe: fence armed during the await, still closed at its end: 0 frames emitted (was 1)", async () => {
    let mono = 10_000;
    const scope = new Scope();
    installTransformWorker(scope, { monotonic: () => mono, dropReportMs: 0 });
    const video = pipe({ role: "encrypt", kind: "video", track: 0, codec: "video/VP9" });
    scope.onrtctransform!({ transformer: video.t });
    scope.send({ type: "sender-epoch", epoch: 1, secret: randomBytes(32).buffer });
    video.push(vp9("key"));
    await settle();
    expect(video.out.length).toBe(1);
    await held(scope, video, () => {
      scope.send({ type: "sender-fence", at: mono, ms: 1000 });
      mono += 10;
    });
    expect(video.out.length).toBe(1);
    video.close();
  });

  it("fence armed and opened again during the await: the frame is dropped and video waits for a keyframe", async () => {
    let mono = 30_000;
    const scope = new Scope();
    installTransformWorker(scope, { monotonic: () => mono, dropReportMs: 0 });
    const video = pipe({ role: "encrypt", kind: "video", track: 0, codec: "video/VP9" });
    scope.onrtctransform!({ transformer: video.t });
    scope.send({ type: "sender-epoch", epoch: 1, secret: randomBytes(32).buffer });
    await held(scope, video, () => {
      scope.send({ type: "sender-fence", at: mono, ms: 1000 });
      mono += 5;
      scope.send({ type: "sender-fence-open" });
    });
    expect(video.out.length).toBe(0);
    video.push(vp9("delta")); // a delta after the fence: still held for a keyframe
    await settle();
    expect(video.out.length).toBe(0);
    video.push(vp9("key"));
    await settle();
    expect(video.out.length).toBe(1);
    video.close();
  });
});
