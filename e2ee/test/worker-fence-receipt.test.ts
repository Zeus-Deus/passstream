/** The page post is not the worker receipt: preserves the documented pre-receipt limitation. */
import { expect, it, vi } from "vitest";
import { installTransformWorker, type WorkerScopeLike } from "../src/worker-runtime.ts";
import { FakeFrame } from "./media.ts";
it("pre-command frame can leave after page posts a fence but before worker receives it", async () => {
  let mono = 100,
    release!: () => void,
    entered!: () => void,
    wrote!: () => void;
  const barrier = new Promise<void>((r) => (release = r)),
    start = new Promise<void>((r) => (entered = r)),
    written = new Promise<void>((r) => (wrote = r));
  const orig = crypto.subtle.encrypt.bind(crypto.subtle);
  const hook = vi.spyOn(crypto.subtle, "encrypt").mockImplementation(async (...args) => {
    entered();
    await barrier;
    return orig(...args);
  });
  const messages: unknown[] = [],
    out: { frame: FakeFrame; at: number }[] = [];
  const scope: WorkerScopeLike = { onmessage: null, onrtctransform: null, postMessage: (m) => messages.push(m) };
  const w = installTransformWorker(scope, { monotonic: () => mono });
  let controller!: ReadableStreamDefaultController<FakeFrame>;
  const readable = new ReadableStream<FakeFrame>({ start: (c) => (controller = c) });
  const writable = new WritableStream<FakeFrame>({
    write: (frame) => {
      out.push({ frame, at: mono });
      wrote();
    },
  });
  scope.onrtctransform!({ transformer: { options: { role: "encrypt", kind: "video", track: 0, codec: "video/VP9" }, readable, writable } });
  scope.onmessage!({ data: { type: "sender-epoch", epoch: 1, secret: crypto.getRandomValues(new Uint8Array(32)).buffer } });
  controller.enqueue(new FakeFrame(crypto.getRandomValues(new Uint8Array(300)), "video/VP9", "key"));
  await start;
  mono = 200;
  const pagePostAt = mono; // page posts, but the worker has not handled that queued message yet
  release();
  await written;
  mono = 201;
  scope.onmessage!({ data: { type: "sender-fence", at: pagePostAt, ms: 1000 } });
  const fence = messages.find((m): m is { type: "fence"; at: number } => typeof m === "object" && m !== null && "type" in m && m.type === "fence");
  const receipt = fence!.at;
  console.log(JSON.stringify({ scenario: "page-post-before-worker-receipt", pagePostAt, receipt, emitted: out.map((r) => r.at) }));
  expect(out).toHaveLength(1);
  expect(out[0]!.at).toBeGreaterThanOrEqual(pagePostAt);
  expect(out[0]!.at).toBeLessThan(receipt);
  controller.close();
  await Promise.all(w.piped);
  hook.mockRestore();
});
