/**
 * Safe mode's recognition worker: line crops in, covers out. Runs PP-OCRv5
 * recognition through ONNX Runtime Web, on WebGPU when the browser exposes a
 * GPU adapter and on threaded WASM otherwise. Segments are batched by width
 * bucket, so one GPU dispatch serves many lines.
 */
import * as ort from "onnxruntime-web/webgpu";
import { BUCKETS, INPUT_H, classifyLine, decode, fill, segments, type Glyph, type LineResult, type Segment } from "./ocr";

export interface OcrJob {
  key: string;
  w: number;
  h: number;
  data: Uint8Array;
}
export type OcrIn =
  | { type: "init"; model: ArrayBuffer; dict: string; wasmBinary: ArrayBuffer; wasm: string; gpu: boolean; threads: number; keepText: boolean }
  | { type: "jobs"; jobs: OcrJob[] };
export type OcrOut =
  | { type: "ready"; backend: "webgpu" | "wasm"; threads: number; loadMs: number }
  | { type: "results"; items: { key: string; result: LineResult }[]; ms: number; segments: number; bytes: number }
  | { type: "error"; message: string };

/** GPU: one dispatch serves many lines. CPU: padding to a shared width only wastes work. */
let maxBatch = 32;
let exact = false;
/** Lines per pass (results are posted after each): GPU batches many; CPU posts often so text clears progressively. */
let maxLines = 48;

let session: ort.InferenceSession | null = null;
let chars: string[] = [];
let keepText = false;
const queue: OcrJob[] = [];
let busy = false;
let port: { postMessage(m: OcrOut): void } = self as unknown as { postMessage(m: OcrOut): void };

async function init(m: Extract<OcrIn, { type: "init" }>) {
  const t0 = performance.now();
  keepText = m.keepText;
  ort.env.wasm.wasmPaths = m.wasm;
  ort.env.wasm.numThreads = crossOriginIsolated ? m.threads : 1;
  ort.env.logLevel = "error";
  // The runtime and model arrive with the message: fetched once for all workers.
  ort.env.wasm.wasmBinary = m.wasmBinary;
  const dict = m.dict.split("\n");
  if (dict[dict.length - 1] === "") dict.pop();
  chars = ["", ...dict, " "];
  let backend: "webgpu" | "wasm" = "wasm";
  const gpu = (navigator as Navigator & { gpu?: { requestAdapter(): Promise<unknown> } }).gpu;
  if (m.gpu && gpu && (await gpu.requestAdapter().catch(() => null))) backend = "webgpu";
  const model = new Uint8Array(m.model);
  try {
    session = await ort.InferenceSession.create(model, { executionProviders: [backend], graphOptimizationLevel: "all" });
  } catch (e) {
    if (backend !== "webgpu") throw e;
    backend = "wasm";
    session = await ort.InferenceSession.create(model, { executionProviders: ["wasm"], graphOptimizationLevel: "all" });
  }
  if (backend === "wasm") (maxBatch = 1), (exact = true), (maxLines = 8);
  // Warm the common shapes so the first real change isn't paying for shader/kernel setup.
  for (const b of [BUCKETS[2], BUCKETS[4]]) await run(new Float32Array(3 * INPUT_H * b), 1, b);
  port.postMessage({ type: "ready", backend, threads: ort.env.wasm.numThreads ?? 1, loadMs: Math.round(performance.now() - t0) });
}

async function run(input: Float32Array, n: number, bucket: number) {
  const s = session!;
  const out = await s.run({ [s.inputNames[0]]: new ort.Tensor("float32", input, [n, 3, INPUT_H, bucket]) });
  const t = out[s.outputNames[0]];
  const data = (await t.getData()) as Float32Array;
  return { data, T: t.dims[1], C: t.dims[2] };
}

async function pump() {
  if (busy || !session) return;
  busy = true;
  try {
    while (queue.length) {
      const t0 = performance.now();
      const jobs = queue.splice(Math.max(0, queue.length - maxLines));
      // Newest first: what is on screen now matters more than what scrolled past.
      const segs: { job: number; k: number; seg: Segment }[] = [];
      jobs.forEach((j, i) => segments(j.data, j.w, j.h, exact).forEach((seg, k) => segs.push({ job: i, k, seg })));
      const glyphs: Glyph[][][] = jobs.map(() => []);
      const byBucket = new Map<number, number[]>();
      segs.forEach((s, i) => byBucket.set(s.seg.bucket, [...(byBucket.get(s.seg.bucket) ?? []), i]));
      for (const [bucket, idx] of byBucket) {
        for (let k = 0; k < idx.length; k += maxBatch) {
          const part = idx.slice(k, k + maxBatch);
          const plane = 3 * INPUT_H * bucket;
          const input = new Float32Array(part.length * plane);
          part.forEach((si, n) => {
            const { job, seg } = segs[si];
            fill(input, n * plane, jobs[job].data, jobs[job].w, jobs[job].h, seg);
          });
          const { data, T, C } = await run(input, part.length, bucket);
          part.forEach((si, n) => {
            const { job, k: at, seg } = segs[si];
            glyphs[job][at] = decode(data.subarray(n * T * C, (n + 1) * T * C), T, C, chars, seg);
          });
        }
      }
      const items = jobs.map((j, i) => ({ key: j.key, result: classifyLine(glyphs[i], segs.filter((x) => x.job === i).map((x) => x.seg), j.h, keepText) }));
      port.postMessage({ type: "results", items, ms: performance.now() - t0, segments: segs.length, bytes: jobs.reduce((n, j) => n + j.data.length, 0) });
    }
  } catch (e) {
    port.postMessage({ type: "error", message: String(e) });
  } finally {
    busy = false;
  }
}

function onMessage(m: OcrIn) {
  if (m.type === "init") void init(m).then(pump, (e) => port.postMessage({ type: "error", message: String(e) }));
  else {
    queue.push(...m.jobs);
    void pump();
  }
}

// The engine worker talks to this worker over a MessagePort handed over once.
self.onmessage = (e: MessageEvent) => {
  if (e.data?.type === "port") {
    const p = e.data.port as MessagePort;
    port = p as unknown as typeof port;
    p.onmessage = (ev) => onMessage(ev.data as OcrIn);
    return;
  }
  onMessage(e.data as OcrIn);
};
