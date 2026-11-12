/**
 * Safe mode's frame worker: capture VideoFrames in, redacted VideoFrames out.
 * A frame with nothing to hide passes through untouched (no copy, no
 * re-render). Otherwise covers are painted over it on an OffscreenCanvas.
 * Covers are synthetic: the background colour around the line and a soft
 * bar that reads as blurred text. Nothing is derived from the hidden pixels,
 * so nothing can be recovered from it (a real blur can leak digits).
 */
import { Engine, type Rect } from "./engine";
import type { OcrOut } from "./ocr-worker";

export type RedactIn =
  | { type: "start"; readable: ReadableStream<VideoFrame>; writable: WritableStream<VideoFrame>; ocr: MessagePort[]; holdMs: number; test: boolean; bypass?: boolean }
  | { type: "hold"; ms: number }
  | { type: "reset" }
  | { type: "stop" };
export type RedactStatus = "starting" | "protecting" | "catching_up" | "unavailable";
export interface RedactStats {
  status: RedactStatus;
  backend: string;
  frames: number;
  passThrough: number;
  refreshes: number;
  covers: number;
  pending: number;
  /** Per-frame processing time inside the worker (ms): p50 / p95 / max over the last window. */
  proc: [number, number, number];
  copy: [number, number, number];
  diff: [number, number, number];
  detect: [number, number, number];
  compose: [number, number, number];
  hold: [number, number, number];
  /** Recognition passes: lines, wall time (ms). */
  ocr: { batches: number; lines: number; ms: number; lastMs: number };
  /** Time from a change appearing to its last line being cleared (ms). */
  reveal: [number, number, number];
}
export type RedactOut =
  | { type: "stats"; stats: RedactStats }
  | { type: "frame"; at: number; covers: Rect[]; pending: number; texts?: Record<string, string>; ts?: number; inAt?: number; outAt?: number; kind?: string };

/** Recognition workers and the lines each still owes (jobs go to the least loaded). */
const ocrPorts: { port: MessagePort; owed: number; ready: boolean; failed: boolean }[] = [];
const engine = new Engine((jobs) => {
  const live = ocrPorts.filter((p) => !p.failed);
  const ready = live.filter((p) => p.ready);
  const pool = ready.length ? ready : live;
  if (!pool.length) return; // all failed: lines stay covered
  const batches = pool.map(() => [] as typeof jobs);
  // Largest first onto the least loaded worker (by pixels owed).
  for (const j of [...jobs].sort((a, b) => b.data.length - a.data.length)) {
    let k = 0;
    for (let i = 1; i < pool.length; i++) if (pool[i].owed < pool[k].owed) k = i;
    pool[k].owed += j.data.length;
    batches[k].push(j);
  }
  batches.forEach((b, i) => b.length && pool[i].port.postMessage({ type: "jobs", jobs: b }, b.map((j) => j.data.buffer as ArrayBuffer)));
});
let writer: WritableStreamDefaultWriter<VideoFrame> | null = null;
let holdMs = 0;
let test = false;
let status: RedactStatus = "starting";
let backend = "";
let stopped = false;
let busy = false;
let last: VideoFrame | null = null;
let lastTs = 0;
let lastTsAt = 0;
/**
 * A re-sent frame's timestamp: the real time elapsed since the last one. A
 * +1 µs stamp was rounded back onto the previous frame's, and WebRTC drops
 * frames that don't advance (the reveal would be lost on a static screen).
 */
function nextTs(): number {
  return lastTs + Math.max(1000, Math.round((performance.now() - lastTsAt) * 1000));
}
let canvas: OffscreenCanvas | null = null;
let ctx: OffscreenCanvasRenderingContext2D | null = null;
const buffers: Uint32Array[] = [];
let useCanvasCopy = false;
let wake: (() => void) | null = null;
/** When the oldest still-pending change appeared (for reveal timing). */
let pendingSince = 0;

const win = { proc: [] as number[], copy: [] as number[], diff: [] as number[], detect: [] as number[], compose: [] as number[], hold: [] as number[], reveal: [] as number[] };
const counts = { frames: 0, passThrough: 0, refreshes: 0, covers: 0, pending: 0 };
const ocr = { batches: 0, lines: 0, ms: 0, lastMs: 0 };

function q(a: number[]): [number, number, number] {
  if (!a.length) return [0, 0, 0];
  const s = [...a].sort((x, y) => x - y);
  const r = (v: number) => Math.round(v * 100) / 100;
  return [r(s[Math.floor(s.length * 0.5)]), r(s[Math.min(s.length - 1, Math.floor(s.length * 0.95))]), r(s[s.length - 1])];
}
function rec(k: keyof typeof win, v: number) {
  const a = win[k];
  a.push(v);
  if (a.length > 300) a.shift();
}
function post(m: RedactOut) {
  (self as unknown as Worker).postMessage(m);
}

function buffer(n: number): Uint32Array {
  // Two buffers alternate: the engine keeps the previous frame as its baseline.
  if (buffers.length < 2 || buffers[0].length !== n) buffers.splice(0, buffers.length, new Uint32Array(n), new Uint32Array(n));
  const b = buffers.shift()!;
  buffers.push(b);
  return b;
}

async function copy(frame: VideoFrame, W: number, H: number): Promise<Uint32Array> {
  const px = buffer(W * H);
  if (!useCanvasCopy) {
    try {
      await frame.copyTo(px, { format: "RGBX" } as VideoFrameCopyToOptions);
      return px;
    } catch {
      useCanvasCopy = true;
    }
  }
  const c = surface(W, H);
  c.drawImage(frame, 0, 0);
  px.set(new Uint32Array(c.getImageData(0, 0, W, H).data.buffer));
  return px;
}

function surface(W: number, H: number): OffscreenCanvasRenderingContext2D {
  if (!canvas || canvas.width !== W || canvas.height !== H) {
    canvas = new OffscreenCanvas(W, H);
    ctx = canvas.getContext("2d", { alpha: false })!;
  }
  return ctx!;
}

function background(px: Uint32Array, W: number, H: number, r: Rect): number {
  // The most common colour just outside the cover (16 samples).
  const seen = new Map<number, number>();
  const at = (x: number, y: number) => {
    if (x < 0 || y < 0 || x >= W || y >= H) return;
    const v = px[y * W + x] & 0xffffff;
    seen.set(v, (seen.get(v) ?? 0) + 1);
  };
  for (let i = 0; i < 8; i++) {
    const x = r.x + Math.round(((i + 0.5) * r.w) / 8);
    at(x, r.y - 1), at(x, r.y + r.h);
  }
  at(r.x - 2, r.y + (r.h >> 1)), at(r.x + r.w + 1, r.y + (r.h >> 1));
  let best = 0, n = -1;
  for (const [v, c] of seen) if (c > n) (best = v), (n = c);
  return best;
}

/**
 * Paint covers straight into the RGBX pixels and wrap them in a new frame
 * (the constructor copies), then restore the covered pixels so `px` stays the
 * diff baseline. No canvas: frames made from a worker canvas reached the
 * consumer one frame late, and the software VP9 encoder wants CPU memory.
 */
function paint(frame: VideoFrame, px: Uint32Array, covers: Rect[], timestamp: number): VideoFrame {
  const W = frame.visibleRect!.width, H = frame.visibleRect!.height;
  const saved: Uint32Array[] = [];
  for (const r of covers) {
    const x0 = Math.max(0, r.x - 1), x1 = Math.min(W, r.x + r.w + 1);
    const y0 = Math.max(0, r.y - 1), y1 = Math.min(H, r.y + r.h + 1);
    const keep = new Uint32Array((x1 - x0) * (y1 - y0));
    for (let y = y0; y < y1; y++) keep.set(px.subarray(y * W + x0, y * W + x1), (y - y0) * (x1 - x0));
    saved.push(keep);
    const bg = background(px, W, H, r);
    const R = bg & 0xff, G = (bg >>> 8) & 0xff, B = (bg >>> 16) & 0xff;
    // A soft bar where the text was: reads as blurred text, carries none of it.
    const light = R * 0.3 + G * 0.59 + B * 0.11 > 140;
    const [tr, tg, tb] = light ? [40, 40, 60] : [235, 235, 250];
    const by0 = r.y + r.h * 0.28, by1 = r.y + r.h * 0.72, bx0 = x0 + 3, bx1 = x1 - 3;
    const soft = Math.max(1.5, r.h * 0.16);
    for (let y = y0; y < y1; y++) {
      const dy = Math.min(y + 0.5 - by0, by1 - y - 0.5);
      const row = y * W;
      for (let x = x0; x < x1; x++) {
        const d = Math.min(dy, x + 0.5 - bx0, bx1 - x - 0.5);
        const a = d <= -soft ? 0 : d >= soft ? 0.22 : 0.22 * ((d + soft) / (2 * soft));
        px[row + x] = 0xff000000 | (Math.round(B + (tb - B) * a) << 16) | (Math.round(G + (tg - G) * a) << 8) | Math.round(R + (tr - R) * a);
      }
    }
  }
  const out = new VideoFrame(px, { format: "RGBX", codedWidth: W, codedHeight: H, timestamp, duration: frame.duration ?? undefined });
  covers.forEach((r, i) => {
    const x0 = Math.max(0, r.x - 1), x1 = Math.min(W, r.x + r.w + 1);
    const y0 = Math.max(0, r.y - 1), y1 = Math.min(H, r.y + r.h + 1);
    for (let y = y0; y < y1; y++) px.set(saved[i].subarray((y - y0) * (x1 - x0), (y - y0 + 1) * (x1 - x0)), y * W + x0);
  });
  return out;
}

/** Paint (if needed) and write; resolves with the time the frame was handed to the writer. */
async function emit(frame: VideoFrame, px: Uint32Array, timestamp: number, refresh: boolean, inAt = 0): Promise<number> {
  const t0 = performance.now();
  const covers = engine.covers();
  counts.covers = covers.length;
  counts.pending = engine.pendingCount();
  setStatus();
  let out: VideoFrame;
  if (!covers.length && !refresh) {
    out = frame.clone();
    counts.passThrough++;
  } else out = paint(frame, px, covers, timestamp);
  if (!refresh) rec("compose", performance.now() - t0);
  lastTs = timestamp;
  lastTsAt = performance.now();
  const handed = performance.now();
  // Absolute times (timeOrigin + now) so the page can line them up with its own clock.
  if (test) post({ type: "frame", at: handed, covers, pending: counts.pending, ts: timestamp, inAt: performance.timeOrigin + inAt, outAt: performance.timeOrigin + handed, kind: refresh ? "refresh" : covers.length ? "painted" : "clone" });
  await writer?.write(out);
  return handed;
}

function setStatus() {
  if (status === "unavailable") return;
  // Ready once any recognition worker is.
  status = !backend ? "starting" : counts.pending ? "catching_up" : "protecting";
}

/** All bytes zero: a failed readback (a real black screen still has X = 0xff or is rare; covering it costs nothing). */
function blank(px: Uint32Array): boolean {
  for (let i = 0; i < px.length; i += 257) if (px[i] !== 0) return false;
  for (let i = 0; i < px.length; i++) if (px[i] !== 0) return false;
  return true;
}
let neutralBuf: Uint32Array | null = null;
function neutral(n: number): Uint32Array {
  if (neutralBuf?.length !== n) neutralBuf = new Uint32Array(n).fill(0xff2a2626);
  return neutralBuf;
}
let ocrFailed = false;

async function loop(reader: ReadableStreamDefaultReader<VideoFrame>, bypass: boolean) {
  for (;;) {
    const { value: frame, done } = await reader.read();
    if (done || stopped) {
      frame?.close();
      break;
    }
    // Benchmark baseline only: the processor/generator hop with no safe mode work.
    if (bypass) {
      await writer?.write(frame);
      continue;
    }
    busy = true;
    try {
      const t0 = performance.now();
      const W = frame.visibleRect!.width, H = frame.visibleRect!.height;
      const px = await copy(frame, W, H);
      const t1 = performance.now();
      // Fail closed if the browser can't read the frame (Chromium's Vulkan mode
      // returns all-zero pixels): no text would be found and nothing hidden.
      if (blank(px)) {
        status = "unavailable";
        engine.reset();
        const out = new VideoFrame(neutral(W * H), { format: "RGBX", codedWidth: W, codedHeight: H, timestamp: frame.timestamp, duration: frame.duration ?? undefined });
        lastTs = frame.timestamp;
        lastTsAt = performance.now();
        await writer?.write(out);
        continue;
      }
      if (status === "unavailable" && !ocrFailed) status = "starting";
      const report = engine.frame(px, W, H);
      rec("copy", t1 - t0);
      rec("diff", report.ms.diff);
      if (report.changed) rec("detect", report.ms.detect);
      if (report.pending && !pendingSince) pendingSince = t0;
      // Hold briefly for small changes so typing doesn't flash covers.
      let held = 0;
      if (holdMs > 0 && report.pending && backend) {
        const deadline = t0 + holdMs;
        while (engine.pendingCount() && performance.now() < deadline) {
          await new Promise<void>((res) => {
            wake = res;
            setTimeout(res, deadline - performance.now());
          });
        }
        held = performance.now() - t1;
        rec("hold", held);
      }
      last?.close();
      last = frame.clone();
      lastPx = px;
      const handed = await emit(frame, px, frame.timestamp, false, t0);
      rec("proc", handed - t0 - held);
      counts.frames++;
    } finally {
      frame.close();
      busy = false;
    }
  }
}
let lastPx: Uint32Array | null = null;

/** Re-analyse the last frame from scratch (after a reset) and send it: a static screen sends no frames. */
async function reprocess() {
  if (!last || busy) return;
  busy = true;
  try {
    const W = last.visibleRect!.width, H = last.visibleRect!.height;
    const px = await copy(last, W, H);
    const report = engine.frame(px, W, H);
    if (report.pending && !pendingSince) pendingSince = performance.now();
    lastPx = px;
    counts.refreshes++;
    await emit(last, px, nextTs(), true);
  } finally {
    busy = false;
  }
}

function onOcr(m: OcrOut, from: (typeof ocrPorts)[number]) {
  if (m.type === "ready") {
    from.ready = true;
    backend = `${m.backend}${m.backend === "wasm" ? `×${m.threads}` : ""}${ocrPorts.length > 1 ? ` ×${ocrPorts.length} workers` : ""}`;
    setStatus();
  } else if (m.type === "error") {
    console.error("safe mode recogniser", m.message);
    from.failed = true;
    if (ocrPorts.every((p) => p.failed)) {
      // Nothing can clear a line any more: everything stays covered.
      status = "unavailable";
      ocrFailed = true;
    } else engine.resubmit();
  } else if (m.type === "results") {
    from.owed = Math.max(0, from.owed - m.bytes);
    ocr.batches++;
    ocr.lines += m.items.length;
    ocr.ms += m.ms;
    ocr.lastMs = m.ms;
    const onScreen = engine.results(m.items);
    if (test) post({ type: "frame", at: performance.now(), covers: [], pending: engine.pendingCount(), texts: Object.fromEntries(m.items.map((i) => [i.key, i.result.text ?? ""])) });
    if (!engine.pendingCount() && pendingSince) {
      rec("reveal", performance.now() - pendingSince);
      pendingSince = 0;
    }
    wake?.();
    // A static screen sends no new frames: re-send the last one with the new covers.
    if (onScreen && !busy && last && lastPx) {
      counts.refreshes++;
      void emit(last, lastPx, nextTs(), true);
    }
  }
}

self.onmessage = (e: MessageEvent<RedactIn>) => {
  const m = e.data;
  if (m.type === "start") {
    holdMs = m.holdMs;
    test = m.test;
    for (const port of m.ocr) {
      const entry = { port, owed: 0, ready: false, failed: false };
      ocrPorts.push(entry);
      port.onmessage = (ev) => onOcr(ev.data as OcrOut, entry);
    }
    writer = m.writable.getWriter();
    void loop(m.readable.getReader(), !!m.bypass).finally(() => void writer?.close().catch(() => undefined));
    setInterval(() => {
      post({
        type: "stats",
        stats: {
          status, backend, ...counts, ocr: { ...ocr },
          proc: q(win.proc), copy: q(win.copy), diff: q(win.diff), detect: q(win.detect), compose: q(win.compose), hold: q(win.hold), reveal: q(win.reveal),
        },
      });
    }, 500);
  } else if (m.type === "hold") holdMs = m.ms;
  else if (m.type === "reset") {
    engine.reset();
    void reprocess();
  }
  else if (m.type === "stop") {
    stopped = true;
    last?.close();
    last = null;
  }
};
