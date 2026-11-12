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
  /** Hold a frame up to this long for small changes to be checked (0 = never hold). */
  holdMs?: number;
  /** Try WebGPU for recognition (falls back to WASM). */
  gpu?: boolean;
  /** Recognition workers, and threads in each (threads need cross-origin isolation; else 1). */
  workers?: number;
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

/** Recognition workers by core count (tuned in test/redact/bench.mjs). */
const DEFAULT_WORKERS = (_cores: number) => 1; // more workers measured no faster: OCR is CPU-bound

/** Model, dictionary and WASM runtime, fetched once per page (the pages are served no-store). */
let assetsPromise: Promise<{ model: ArrayBuffer; dict: string; wasmBinary: ArrayBuffer }> | null = null;
function assets() {
  const get = async (u: string) => {
    const r = await fetch(u);
    if (!r.ok) throw new Error(`${u}: ${r.status}`);
    return r;
  };
  assetsPromise ??= Promise.all([
    get("/assets/redact/rec.onnx").then((r) => r.arrayBuffer()),
    get("/assets/redact/dict.txt").then((r) => r.text()),
    get("/assets/redact/ort/ort-wasm-simd-threaded.asyncify.wasm").then((r) => r.arrayBuffer()),
  ]).then(([model, dict, wasmBinary]) => ({ model, dict, wasmBinary }));
  assetsPromise.catch(() => (assetsPromise = null));
  return assetsPromise;
}

export function safeModeSupported(): boolean {
  return typeof MediaStreamTrackProcessor === "function" && typeof MediaStreamTrackGenerator === "function" && typeof OffscreenCanvas === "function";
}

export class Redactor {
  readonly track: MediaStreamTrack;
  stats: RedactStats | null = null;
  private worker: Worker;
  private ocr: Worker[] = [];
  private listeners = new Set<(m: RedactOut) => void>();

  constructor(readonly source: MediaStreamTrack, opts: RedactorOptions = {}) {
    if (!safeModeSupported()) throw new Error("safe mode needs MediaStreamTrackProcessor");
    const processor = new MediaStreamTrackProcessor({ track: source, maxBufferSize: 2 });
    const generator = new MediaStreamTrackGenerator({ kind: "video" });
    if (!new URLSearchParams(location.search).has("nohint")) generator.contentHint = source.contentHint;
    this.track = generator;
    const cores = navigator.hardwareConcurrency || 4;
    const workers = opts.workers ?? DEFAULT_WORKERS(cores);
    const threads = opts.threads ?? Math.max(1, Math.floor(cores / 2 / workers));
    const ports: MessagePort[] = [];
    for (let i = 0; i < workers; i++) {
      const channel = new MessageChannel();
      const w = new OcrWorker({ name: `safe-mode-ocr-${i}` });
      w.postMessage({ type: "port", port: channel.port1 }, [channel.port1]);
      void assets().then(
        (a) => w.postMessage({ type: "init", ...a, wasm: new URL("/assets/redact/ort/", location.href).href, gpu: opts.gpu ?? true, threads, keepText: !!opts.test }),
        (e) => console.error("safe mode assets", String(e)),
      );
      this.ocr.push(w);
      ports.push(channel.port2);
    }
    this.worker = new RedactWorker({ name: "safe-mode" });
    this.worker.onmessage = (e: MessageEvent<RedactOut>) => {
      if (e.data.type === "stats") this.stats = e.data.stats;
      for (const f of this.listeners) f(e.data);
    };
    const start: RedactIn = { type: "start", readable: processor.readable, writable: generator.writable, ocr: ports, holdMs: opts.holdMs ?? 0, test: !!opts.test, bypass: !!opts.bypass };
    this.worker.postMessage(start, [processor.readable, generator.writable, ...ports] as unknown as Transferable[]);
  }

  /** Subscribe to stats (every 500 ms) and, in test mode, per-frame covers. */
  on(f: (m: RedactOut) => void): () => void {
    this.listeners.add(f);
    return () => this.listeners.delete(f);
  }
  setHold(ms: number) {
    this.worker.postMessage({ type: "hold", ms } satisfies RedactIn);
  }
  /** All on-screen text becomes unknown (covered) until checked again. */
  reset() {
    this.worker.postMessage({ type: "reset" } satisfies RedactIn);
  }
  stop() {
    this.worker.postMessage({ type: "stop" } satisfies RedactIn);
    this.track.stop();
    setTimeout(() => {
      this.worker.terminate();
      this.ocr.forEach((w) => w.terminate());
    }, 100);
  }
}

export type { Rect };
