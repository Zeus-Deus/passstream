/**
 * Safe mode (FEATURES.md "Safe mode"): wraps a capture track in a redacted
 * track. Frames flow MediaStreamTrackProcessor → frame worker → generator;
 * recognition runs in its own worker, so a slow OCR pass never stalls video.
 * Use `track` everywhere the capture would have been sent or recorded.
 */
import RedactWorker from "./redact-worker?worker";
import OcrWorker from "./ocr-worker?worker";
import type { RedactIn, RedactOut, RedactStats } from "./redact-worker";
import type { Rect } from "./engine";

export type { RedactStats };

export interface RedactorOptions {
  /** Try WebGPU for recognition (falls back to WASM). Read once, by the first redactor. */
  gpu?: boolean;
  /** Recognition threads (needs cross-origin isolation; else 1). Read once. */
  threads?: number;
  /** Tests/benchmarks: report covers and recognised text per frame. */
  test?: boolean;
  /** Benchmarks only: forward frames untouched (measures the pipeline hop itself). */
  bypass?: boolean;
}

declare const MediaStreamTrackProcessor: {
  new (init: { track: MediaStreamTrack; maxBufferSize?: number }): { readable: ReadableStream<VideoFrame> };
};
declare const MediaStreamTrackGenerator: {
  new (init: { kind: "video" }): MediaStreamTrack & { writable: WritableStream<VideoFrame> };
};

/**
 * One recognition worker for the page: loading the runtime and model and
 * warming up takes about a second, so a new capture or a safe mode toggle
 * reuses it (each frame worker takes it over through a new MessagePort).
 * The model and runtime are fetched once and transferred, not copied.
 */
let ocrWorker: Worker | null = null;
/** Redactors running now: told when the recognition worker dies. */
const live = new Set<Redactor>();
function recogniser(opts: RedactorOptions): Worker {
  if (ocrWorker) return ocrWorker;
  const w = (ocrWorker = new OcrWorker({ name: "safe-mode-ocr" }));
  // The worker failed to load or crashed: its text stays covered, and the next
  // redactor (a new capture or a safe mode toggle) starts a fresh worker.
  w.onerror = (e) => {
    e.preventDefault();
    if (ocrWorker === w) ocrWorker = null;
    w.terminate();
    for (const r of live) r.recogniserFailed(e.message || "recognition worker failed");
  };
  const get = async (u: string) => {
    const r = await fetch(u);
    if (!r.ok) throw new Error(`${u}: ${r.status}`);
    return r;
  };
  void Promise.all([
    get("/assets/redact/rec.onnx").then((r) => r.arrayBuffer()),
    get("/assets/redact/dict.txt").then((r) => r.text()),
    get("/assets/redact/ort/ort-wasm-simd-threaded.asyncify.wasm").then((r) => r.arrayBuffer()),
  ]).then(
    ([model, dict, wasmBinary]) => {
      const cores = navigator.hardwareConcurrency || 4;
      const init = { type: "init", model, dict, wasmBinary, wasm: new URL("/assets/redact/ort/", location.href).href, gpu: opts.gpu ?? true, threads: opts.threads ?? Math.max(1, Math.min(6, cores >> 1)), keepText: !!opts.test };
      w.postMessage(init, [model, wasmBinary]);
    },
    // Reported to whichever frame worker holds the port: its text stays covered.
    (e) => w.postMessage({ type: "fail", message: `safe mode assets: ${String(e)}` }),
  );
  return w;
}

export function safeModeSupported(): boolean {
  return typeof MediaStreamTrackProcessor === "function" && typeof MediaStreamTrackGenerator === "function" && typeof OffscreenCanvas === "function";
}

export class Redactor {
  readonly track: MediaStreamTrack;
  stats: RedactStats | null = null;
  private worker: Worker;
  private input: MediaStreamTrack;
  private listeners = new Set<(m: RedactOut) => void>();

  constructor(readonly source: MediaStreamTrack, opts: RedactorOptions = {}) {
    if (!safeModeSupported()) throw new Error("safe mode needs MediaStreamTrackProcessor");
    // Its own copy of the capture: the capture itself stays disabled while safe
    // mode is on, so a sender still holding it can only ever send black.
    this.input = source.clone();
    this.input.enabled = true;
    const processor = new MediaStreamTrackProcessor({ track: this.input, maxBufferSize: 2 });
    const generator = new MediaStreamTrackGenerator({ kind: "video" });
    generator.contentHint = source.contentHint;
    this.track = generator;
    const channel = new MessageChannel();
    recogniser(opts).postMessage({ type: "port", port: channel.port1 }, [channel.port1]);
    this.worker = new RedactWorker({ name: "safe-mode" });
    this.worker.onmessage = (e: MessageEvent<RedactOut>) => {
      if (e.data.type === "stats") this.stats = e.data.stats;
      for (const f of this.listeners) f(e.data);
    };
    const start: RedactIn = { type: "start", readable: processor.readable, writable: generator.writable, ocr: channel.port2, test: !!opts.test, bypass: !!opts.bypass };
    this.worker.postMessage(start, [processor.readable, generator.writable, channel.port2] as unknown as Transferable[]);
    live.add(this);
  }

  /** The shared recognition worker died: nothing can clear text any more. */
  recogniserFailed(message: string) {
    this.worker.postMessage({ type: "ocrFailed", message } satisfies RedactIn);
  }

  /** Subscribe to stats (every 500 ms) and, in test mode, per-frame covers. */
  on(f: (m: RedactOut) => void): () => void {
    this.listeners.add(f);
    return () => this.listeners.delete(f);
  }
  /** All on-screen text becomes unknown (covered) until checked again. */
  reset() {
    this.worker.postMessage({ type: "reset" } satisfies RedactIn);
  }
  stop() {
    this.worker.postMessage({ type: "stop" } satisfies RedactIn);
    this.track.stop();
    this.input.stop();
    live.delete(this);
    // The recognition worker stays for the next capture.
    setTimeout(() => this.worker.terminate(), 100);
  }
}

export type { Rect };
