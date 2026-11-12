/**
 * Safe mode's per-frame logic (pure; redact-worker.ts drives it with real
 * VideoFrames). Every frame is compared exactly with the previous one in
 * tiles; only bands around changed tiles are re-scanned for text lines. A
 * line's verdict is cached by its pixel content, so unchanged and scrolled
 * lines cost nothing. A line without a verdict is covered until recognition
 * clears it: new text is never sent before it has been checked (fail-closed).
 */
import { MAX_LINE_H, contentKey, crop, cropBox, detectLines, tighten, type Box } from "./lines";
import type { Cover, LineResult } from "./ocr";
import type { OcrJob } from "./ocr-worker";

export const TILE_W = 64;
export const TILE_H = 32;
/** Band margin around changed tiles: a whole line that touches a change is re-read. */
const MARGIN = MAX_LINE_H;
const PAD = 2;
const CACHE_MAX = 20_000;
/** Lines awaiting recognition before off-screen ones are forgotten (above the recogniser's 400-job backlog). */
const INFLIGHT_MAX = 1_000;

export type CoverKind = Cover["category"] | "pending";
export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
  kind: CoverKind;
}
interface Line {
  box: Box;
  key: string;
  /** Tight box x minus crop x, for converting crop-relative covers. */
  offset: number;
}
export interface FrameReport {
  changed: boolean;
  dirtyTiles: number;
  lines: number;
  /** Lines on screen still waiting for recognition. */
  pending: number;
  ms: { diff: number; detect: number };
}

export class Engine {
  W = 0;
  H = 0;
  private prev: Uint32Array | null = null;
  private luma = new Uint8Array(0);
  /** HSL lightness, (max + min) / 2: shows saturated colours that luma hides. */
  private light = new Uint8Array(0);
  private lines: Line[] = [];
  private cache = new Map<string, LineResult>();
  private inflight = new Map<string, number>();

  constructor(
    private readonly submit: (jobs: OcrJob[]) => void,
    private readonly limits = { cache: CACHE_MAX, inflight: INFLIGHT_MAX },
  ) {}

  /** Forget everything, verdicts included: all text is unknown again. */
  reset() {
    this.prev = null;
    this.lines = [];
    this.W = this.H = 0;
    this.cache.clear();
    this.inflight.clear();
  }

  /** Recognition dropped these jobs (backlog): lines still on screen are sent again. */
  dropped(keys: string[]) {
    for (const k of keys) this.inflight.delete(k);
    this.resubmit(false);
  }

  /** Analyse one RGBX frame (little-endian u32 per pixel). `px` becomes the new baseline. */
  frame(px: Uint32Array, W: number, H: number): FrameReport {
    const t0 = performance.now();
    if (W !== this.W || H !== this.H || !this.prev) {
      this.W = W;
      this.H = H;
      this.prev = null;
      this.lines = [];
      this.luma = new Uint8Array(W * H);
      this.light = new Uint8Array(W * H);
    }
    const tilesX = Math.ceil(W / TILE_W), tilesY = Math.ceil(H / TILE_H);
    const dirty = new Uint8Array(tilesX * tilesY);
    let dirtyTiles = 0;
    const prev = this.prev;
    if (!prev) {
      dirty.fill(1);
      dirtyTiles = dirty.length;
    } else {
      for (let ty = 0; ty < tilesY; ty++) {
        const yEnd = Math.min(H, (ty + 1) * TILE_H);
        for (let tx = 0; tx < tilesX; tx++) {
          const xs = tx * TILE_W, xe = Math.min(W, xs + TILE_W);
          let diff = false;
          for (let y = ty * TILE_H; y < yEnd && !diff; y++) {
            const r = y * W;
            for (let x = xs; x < xe; x++) if (px[r + x] !== prev[r + x]) { diff = true; break; }
          }
          if (diff) dirty[ty * tilesX + tx] = 1, dirtyTiles++;
        }
      }
    }
    this.prev = px;
    const t1 = performance.now();
    if (!dirtyTiles) return { changed: false, dirtyTiles, lines: this.lines.length, pending: this.pendingCount(), ms: { diff: t1 - t0, detect: 0 } };

    // Bands: rows of changed tiles plus a margin, widened to swallow any old line they cut.
    let bands: [number, number][] = [];
    for (let ty = 0; ty < tilesY; ty++) {
      let any = false;
      for (let tx = 0; tx < tilesX && !any; tx++) any = dirty[ty * tilesX + tx] === 1;
      if (!any) continue;
      const y0 = Math.max(0, ty * TILE_H - MARGIN), y1 = Math.min(H, (ty + 1) * TILE_H + MARGIN);
      const last = bands[bands.length - 1];
      if (last && y0 <= last[1]) last[1] = Math.max(last[1], y1);
      else bands.push([y0, y1]);
    }
    for (let pass = 0; pass < 3; pass++) {
      let grew = false;
      for (const l of this.lines)
        for (const b of bands)
          if (l.box.y < b[1] && l.box.y + l.box.h > b[0] && (l.box.y < b[0] || l.box.y + l.box.h > b[1])) {
            b[0] = Math.max(0, Math.min(b[0], l.box.y - PAD));
            b[1] = Math.min(H, Math.max(b[1], l.box.y + l.box.h + PAD));
            grew = true;
          }
      bands = mergeBands(bands);
      if (!grew) break;
    }
    // Luma only for band rows; outside them it is unchanged.
    const luma = this.luma, light = this.light;
    for (const [y0, y1] of bands)
      for (let i = y0 * W, end = y1 * W; i < end; i++) {
        const v = px[i];
        const r = v & 0xff, g = (v >>> 8) & 0xff, b = (v >>> 16) & 0xff;
        luma[i] = (r * 77 + g * 150 + b * 29) >> 8;
        const hi = r > g ? (r > b ? r : b) : g > b ? g : b, lo = r < g ? (r < b ? r : b) : g < b ? g : b;
        light[i] = (hi + lo) >> 1;
      }
    const inBand = (b: Box) => bands.some(([y0, y1]) => b.y < y1 && b.y + b.h > y0);
    const kept = this.lines.filter((l) => !inBand(l.box));
    const jobs: OcrJob[] = [];
    for (const [y0, y1] of bands) {
      for (const raw of detectLines(luma, light, W, H, y0, y1)) {
        // No ink found inside a detected box: keep the whole box (fail-closed).
        const box = tighten(luma, light, W, raw) ?? raw;
        const key = contentKey(luma, light, W, box);
        const offset = box.x - cropBox(W, H, box, PAD).x;
        kept.push({ box, key, offset });
        if (!this.cache.has(key) && !this.inflight.has(key)) {
          const c = crop(luma, light, W, H, box, PAD);
          this.inflight.set(key, offset);
          jobs.push({ key, w: c.box.w, h: c.box.h, data: c.data });
        }
      }
    }
    this.lines = kept;
    // Recognition stuck or gone: forget lines no longer on screen (they are read again if they return).
    if (this.inflight.size > this.limits.inflight) {
      const visible = new Set(kept.map((l) => l.key));
      for (const k of this.inflight.keys()) if (!visible.has(k)) this.inflight.delete(k);
    }
    if (jobs.length) this.submit(jobs);
    return { changed: true, dirtyTiles, lines: kept.length, pending: this.pendingCount(), ms: { diff: t1 - t0, detect: performance.now() - t1 } };
  }

  /** Store recognition results; true if any line on screen was waiting for one. */
  results(items: { key: string; result: LineResult }[]): boolean {
    let onScreen = false;
    for (const { key, result } of items) {
      // Forgotten while recognition was stuck (see frame()): no offset to place it by.
      const offset = this.inflight.get(key);
      if (offset === undefined) continue;
      this.inflight.delete(key);
      const covers = result.covers.map((c) => ({ ...c, x0: c.x0 - offset, x1: c.x1 - offset }));
      this.cache.set(key, { covers });
      if (!onScreen && this.lines.some((l) => l.key === key)) onScreen = true;
    }
    if (this.cache.size > this.limits.cache) {
      // Oldest first, never a line on screen: an unchanged screen isn't re-read,
      // so an evicted visible line would stay covered.
      const visible = new Set(this.lines.map((l) => l.key));
      let drop = this.cache.size - this.limits.cache;
      for (const k of this.cache.keys()) {
        if (drop <= 0) break;
        if (visible.has(k)) continue;
        this.cache.delete(k);
        drop--;
      }
    }
    return onScreen;
  }

  /** Keys still pending among the lines on screen. */
  pendingKeys(): Set<string> {
    const out = new Set<string>();
    for (const l of this.lines) if (!this.cache.has(l.key)) out.add(l.key);
    return out;
  }
  pendingCount(): number {
    let n = 0;
    for (const l of this.lines) if (!this.cache.has(l.key)) n++;
    return n;
  }
  /** Send every on-screen line still waiting for a verdict again (a recognition worker failed). */
  resubmit(all = true) {
    if (all) this.inflight.clear();
    const jobs: OcrJob[] = [];
    for (const l of this.lines) {
      if (this.cache.has(l.key) || this.inflight.has(l.key)) continue;
      const c = crop(this.luma, this.light, this.W, this.H, l.box, PAD);
      this.inflight.set(l.key, l.box.x - c.box.x);
      jobs.push({ key: l.key, w: c.box.w, h: c.box.h, data: c.data });
    }
    if (jobs.length) this.submit(jobs);
  }

  /** What to hide on the current frame. */
  covers(): Rect[] {
    const out: Rect[] = [];
    for (const l of this.lines) {
      const b = l.box;
      const y = Math.max(0, b.y - PAD), h = Math.min(this.H, b.y + b.h + PAD) - y;
      const v = this.cache.get(l.key);
      if (!v) {
        out.push({ x: Math.max(0, b.x - PAD), y, w: Math.min(this.W, b.x + b.w + PAD) - Math.max(0, b.x - PAD), h, kind: "pending" });
        continue;
      }
      for (const c of v.covers) {
        const x0 = Math.max(0, b.x + Math.max(-PAD, c.x0)), x1 = Math.min(this.W, b.x + Math.min(b.w + PAD, c.x1));
        if (x1 > x0) out.push({ x: Math.floor(x0), y, w: Math.ceil(x1 - x0), h, kind: c.category });
      }
    }
    return out;
  }
}

function mergeBands(bands: [number, number][]): [number, number][] {
  bands.sort((a, b) => a[0] - b[0]);
  const out: [number, number][] = [];
  for (const b of bands) {
    const last = out[out.length - 1];
    if (last && b[0] <= last[1]) last[1] = Math.max(last[1], b[1]);
    else out.push([b[0], b[1]]);
  }
  return out;
}
