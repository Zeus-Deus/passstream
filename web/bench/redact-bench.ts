/**
 * Safe mode benchmark (dev server only: `npx vite`, then /bench/redact.html,
 * driven by test/redact/bench.mjs). The page captures itself, runs the
 * redactor, and reads the raw and redacted tracks side by side, so added
 * latency is measured per frame by matching capture timestamps.
 */
import { Redactor, type RedactStats } from "../src/redact/redactor";
import type { Rect } from "../src/redact/engine";
import MeterWorker from "./meter-worker?worker";

declare const MediaStreamTrackProcessor: {
  new (init: { track: MediaStreamTrack; maxBufferSize?: number }): { readable: ReadableStream<VideoFrame> };
};

const names = ["Mia Clarke", "Omar Haddad", "Lena Vogel", "Ravi Shah", "Zoe Martin", "Ivan Petrov", "Sara Lind", "Kai Tanaka", "Nora Quinn", "Luis Ortega", "Emma Brooks", "Yusuf Kaya"];
const list = document.querySelector(".list")!;
for (let i = 0; i < 36; i++) {
  const n = names[i % names.length];
  const d = document.createElement("div");
  d.innerHTML = `<b>${n}</b>${n.toLowerCase().replace(" ", ".")}${i}@example-mail.com<br>${i % 3 ? "Weekly sync notes" : `Refund of $${(i * 37.5).toFixed(2)}`}`;
  list.appendChild(d);
}

// Planted sensitive strings (independent of the classifier's rules).
const NEEDLES = [
  /[\w.+-]+@[\w-]+(?:\.[\w-]+)+/g,
  /\b\d{4} \d{4} \d{4} \d{4}\b/g,
  /\b[A-Z]{2}\d{2}(?: [A-Z0-9]{4}){3,}(?: \d{1,2})?/g,
  /\+\d[\d ()-]{8,}\d|07700 900461|\(415\) 555-0132/g,
  /[-+]?[$€£]\d[\d,.]*\d/g,
  /sk_live_\w+|ghp_\w+/g,
];
/** Is the text's own element on top at the rect's centre (not under the popup, not clipped)? */
function visible(r: DOMRect, owner: Element) {
  const x = r.left + r.width / 2, y = r.top + r.height / 2;
  const el = document.elementFromPoint(x, y);
  if (!el || !(owner.contains(el) || el.contains(owner))) return false;
  for (let a: Element | null = owner; a; a = a.parentElement) {
    if (getComputedStyle(a).overflow === "visible") continue;
    const c = a.getBoundingClientRect();
    if (y < c.top || y > c.bottom || x < c.left || x > c.right) return false;
  }
  return true;
}

let redactor: Redactor;
let lastPending = -1;
let lastPendingAt = 0;
let imageCanvas: CanvasRenderingContext2D | null = null;
const capture = { width: 1920, height: 1080 };
// eslint-disable-next-line @typescript-eslint/no-explicit-any
let meterCall: (msg: unknown, want: string) => Promise<any> = async () => ({});
let frames: { at: number; covers: Rect[]; pending: number }[] = [];
/** Per stage: raw arrival on the page → worker read, worker → handed to generator, handed → page arrival. */
let stages: { queue: number[]; work: number[]; out: number[] } = { queue: [], work: [], out: [] };
const workerTimes = new Map<number, { inAt: number; outAt: number; kind?: string }>();
const log: unknown[] = [];
const texts: Record<string, string> = {};


function q(a: number[]) {
  const s = [...a].sort((x, y) => x - y);
  const at = (p: number) => (s.length ? Math.round(s[Math.min(s.length - 1, Math.floor(s.length * p))] * 100) / 100 : 0);
  return { n: s.length, p50: at(0.5), p95: at(0.95), max: at(1) };
}

function toUrl(f: { w: number; h: number; data: Uint8ClampedArray } | null): string {
  if (!f) return "";
  const c = document.createElement("canvas");
  c.width = f.w;
  c.height = f.h;
  c.getContext("2d")!.putImageData(new ImageData(f.data, f.w, f.h), 0, 0);
  return c.toDataURL("image/png");
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function waitClear(timeout = 10_000): Promise<number> {
  const t0 = performance.now();
  // Allow the change to be captured and analysed first.
  await sleep(50);
  while (performance.now() - t0 < timeout) {
    // Per-message pending count (stats are only posted every 500 ms).
    if (redactor.stats?.backend && lastPending === 0 && lastPendingAt > t0) return lastPendingAt - t0;
    await sleep(2);
  }
  return -1;
}

const api = {
  async start(opts: { gpu: boolean; threads?: number; bypass?: boolean }) {
    const stream = await navigator.mediaDevices.getDisplayMedia({
      video: { frameRate: 30 },
      preferCurrentTab: true,
    } as DisplayMediaStreamOptions);
    return api.startWith(stream.getVideoTracks()[0], opts);
  },
  async startWith(source: MediaStreamTrack, opts: { gpu: boolean; threads?: number; bypass?: boolean }) {
    source.contentHint = "text";
    capture.width = source.getSettings().width ?? 1920;
    capture.height = source.getSettings().height ?? 1080;
    redactor = new Redactor(source, { ...opts, test: true });
    redactor.on((m) => {
      if (m.type !== "frame") return;
      lastPending = m.pending;
      lastPendingAt = performance.now();
      if (m.texts) Object.assign(texts, m.texts);
      else {
        frames.push({ at: m.at, covers: m.covers, pending: m.pending });
        if (m.ts !== undefined) workerTimes.set(m.ts, { inAt: m.inAt!, outAt: m.outAt!, kind: m.kind });
        log.push({ w: m.kind, ts: m.ts, outAt: m.outAt });
      }
    });
    const meter = new MeterWorker();
    meterCall = (msg, want) =>
      new Promise((res) => {
        const h = (e: MessageEvent) => {
          if (e.data.type === want) (meter.removeEventListener("message", h), res(e.data));
        };
        meter.addEventListener("message", h);
        meter.postMessage(msg);
      });
    meter.addEventListener("message", (e) => {
      if (e.data.type !== "arrive") return;
      log.push({ arrive: e.data.ts, at: e.data.at });
      if (e.data.rawAt === undefined) return;
      const w = workerTimes.get(e.data.ts);
      if (w) {
        stages.queue.push(w.inAt - e.data.rawAt);
        stages.work.push(w.outAt - w.inAt);
        stages.out.push(e.data.at - w.outAt);
      }
    });
    const rawRead = new MediaStreamTrackProcessor({ track: source.clone(), maxBufferSize: 4 }).readable;
    const redRead = new MediaStreamTrackProcessor({ track: redactor.track, maxBufferSize: 4 }).readable;
    meter.postMessage({ type: "start", raw: rawRead, red: redRead }, [rawRead, redRead] as unknown as Transferable[]);
    const t0 = performance.now();
    while (!opts.bypass && !redactor.stats?.backend && performance.now() - t0 < 30_000) await sleep(20);
    return { settings: source.getSettings(), stats: redactor.stats, readyMs: performance.now() - t0 };
  },
  waitClear,
  async snapshot() {
    const s = await meterCall({ type: "snapshot" }, "snapshot");
    return { before: toUrl(s.before), after: toUrl(s.after) };
  },
  /** Reset per-scenario measurements. */
  mark() {
    void meterCall({ type: "mark" }, "mark-never-answered");
    frames = [];
    stages = { queue: [], work: [], out: [] };
  },
  async measure() {
    const { deltas } = await meterCall({ type: "deltas" }, "deltas");
    const covered = frames.filter((f) => f.covers.some((c) => c.kind === "pending")).length;
    return { added: q(deltas), stages: { queue: q(stages.queue), work: q(stages.work), out: q(stages.out) }, frames: frames.length, framesWithPendingCovers: covered, stats: redactor.stats as RedactStats };
  },
  texts: () => Object.values(texts),
  log: () => log.slice(-80),
  /**
   * Ground truth: every planted sensitive string on screen and how much of it
   * the latest output frame covers (sampled on a 12×3 grid per rect).
   */
  /** Planted strings' rects (CSS px) for replaying a screenshot as an image source. */
  needles() {
    const out: { text: string; rects: { x: number; y: number; w: number; h: number }[] }[] = [];
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const t = n.textContent ?? "";
      for (const re of NEEDLES)
        for (const m of t.matchAll(re)) {
          const r = document.createRange();
          r.setStart(n, m.index);
          r.setEnd(n, m.index + m[0].length);
          const rects = [...r.getClientRects()].filter((rect) => rect.bottom > 0 && rect.top < innerHeight && visible(rect, n.parentElement!)).map((q) => ({ x: q.left, y: q.top, w: q.width, h: q.height }));
          if (rects.length) out.push({ text: m[0], rects });
        }
    }
    return out;
  },
  /** Recall against saved rects (image-source runs). */
  recallSaved(needles: { text: string; rects: { x: number; y: number; w: number; h: number }[] }[]) {
    const covers = frames[frames.length - 1]?.covers ?? [];
    const hidden = (x: number, y: number) => covers.some((c) => x >= c.x && x <= c.x + c.w && y >= c.y && y <= c.y + c.h);
    return needles.map((nd) => {
      let inside = 0, total = 0;
      for (const r of nd.rects) for (let i = 0; i < 12; i++) for (let j = 0; j < 3; j++) (total++, hidden(r.x + ((i + 0.5) * r.w) / 12, r.y + r.h * (0.35 + 0.15 * j)) && inside++);
      return { text: nd.text, covered: Math.round((100 * inside) / total) };
    });
  },
  /** Image source: a canvas showing screenshots (headed GPU runs, where tab capture is blank). */
  async startImage(opts: { gpu: boolean; threads?: number }, url: string) {
    const c = document.createElement("canvas");
    c.width = 1920;
    c.height = 1080;
    imageCanvas = c.getContext("2d")!;
    await api.showImage(url);
    const source = c.captureStream(30).getVideoTracks()[0];
    // Keep frames flowing like a live screen would on change.
    return api.startWith(source, opts);
  },
  async showImage(url: string) {
    const img = new Image();
    img.src = url;
    await img.decode();
    imageCanvas!.drawImage(img, 0, 0);
  },
  recall() {
    const covers = frames[frames.length - 1]?.covers ?? [];
    // A tiled (headed) window is captured scaled: map CSS px to capture px.
    const sx = capture.width / innerWidth, sy = capture.height / innerHeight;
    const hidden = (x: number, y: number) => covers.some((c) => x * sx >= c.x && x * sx <= c.x + c.w && y * sy >= c.y && y * sy <= c.y + c.h);
    const out: { text: string; covered: number }[] = [];
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const t = n.textContent ?? "";
      for (const re of NEEDLES) {
        for (const m of t.matchAll(re)) {
          const r = document.createRange();
          r.setStart(n, m.index);
          r.setEnd(n, m.index + m[0].length);
          let inside = 0, total = 0;
          for (const rect of r.getClientRects()) {
            if (rect.bottom < 0 || rect.top > innerHeight || rect.right < 0 || rect.left > innerWidth || !visible(rect, n.parentElement!)) continue;
            for (let i = 0; i < 12; i++) for (let j = 0; j < 3; j++) {
              total++;
              if (hidden(rect.left + ((i + 0.5) * rect.width) / 12, rect.top + rect.height * (0.35 + 0.15 * j))) inside++;
            }
          }
          if (total) out.push({ text: m[0], covered: Math.round((100 * inside) / total) });
        }
      }
    }
    return out;
  },
  covers: () => frames[frames.length - 1]?.covers ?? [],
  reset: () => redactor.reset(),
  async type(text: string, every = 120) {
    const el = document.getElementById("typed")!;
    for (const ch of text) {
      el.textContent += ch;
      await sleep(every);
    }
  },
  async scroll(steps: number, px = 24, every = 100) {
    for (let i = 0; i < steps; i++) {
      list.scrollTop += px;
      await sleep(every);
    }
  },
  popup(on: boolean) {
    document.getElementById("popup")!.style.display = on ? "block" : "none";
  },
};
(window as unknown as { bench: typeof api }).bench = api;
