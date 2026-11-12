/**
 * Recognition helpers for safe mode (pure; the ONNX session lives in
 * ocr-worker.ts). The model is PP-OCRv5's Latin recogniser: a 48-px-high
 * line in, per-timestep character probabilities out (CTC). Character
 * timesteps give x positions, so only the sensitive part of a line is hidden.
 */
import { findSensitive, type Category } from "./classify";

export const INPUT_H = 48;
/** Resized segment widths are padded up to one of these (fewer GPU shapes). */
export const BUCKETS = [96, 160, 256, 384, 512, 768, 1024, 1536];
const MAX_SEG = BUCKETS[BUCKETS.length - 1];

export interface Cover {
  /** Range in the line crop's x coordinates. */
  x0: number;
  x1: number;
  category: Category | "uncertain";
}
export interface LineResult {
  covers: Cover[];
  /** Only filled for tests and benchmarks. */
  text?: string;
}

/** One recognition input: a horizontal slice [x0, x1) of a line crop. */
export interface Segment {
  x0: number;
  x1: number;
  /** Resized (unpadded) width and its bucket. */
  rw: number;
  bucket: number;
  /**
   * Set when this segment starts at a hard cut (no word gap): segments overlap
   * there, and glyphs left of `join` belong to the previous segment.
   */
  join?: number;
}

function background(data: Uint8Array, w: number, h: number): number {
  const hist = new Uint32Array(64);
  for (let x = 0; x < w; x++) hist[data[x] >> 2]++, hist[data[(h - 1) * w + x] >> 2]++;
  for (let y = 0; y < h; y++) hist[data[y * w] >> 2]++, hist[data[y * w + w - 1] >> 2]++;
  let bg = 0;
  for (let i = 1; i < 64; i++) if (hist[i] > hist[bg]) bg = i;
  return bg * 4 + 2;
}

/**
 * Split a line crop into segments no wider than the largest bucket after
 * resizing, cutting only at blank column runs (word gaps), so a token is
 * never cut in half. A gapless overlong run (a long key or URL) is cut hard
 * with overlap, and the two readings are stitched at the cut without a space.
 * `exact` pads only to a multiple of 8 (CPU: no batching, so no shared shape).
 */
export function segments(data: Uint8Array, w: number, h: number, exact = false): Segment[] {
  const scale = INPUT_H / h;
  const maxW = Math.floor(MAX_SEG / scale);
  const bg = background(data, w, h);
  let lo = 255, hi = 0;
  for (let i = 0; i < data.length; i++) (data[i] < lo && (lo = data[i])), data[i] > hi && (hi = data[i]);
  const thr = Math.max(8, (hi - lo) * 0.3);
  const ink = new Uint8Array(w);
  for (let y = 0; y < h; y++) {
    const r = y * w;
    for (let x = 0; x < w; x++) {
      const d = data[r + x] - bg;
      if (d > thr || d < -thr) ink[x] = 1;
    }
  }
  const minGap = Math.max(2, Math.round(h * 0.3));
  const out: Segment[] = [];
  const overlap = Math.max(4, Math.round(h * 1.5));
  let start = 0;
  let join: number | undefined;
  while (start < w) {
    let end = w;
    let hard = false;
    if (w - start > maxW) {
      // The widest blank run within the allowed window, preferring later ones.
      let best = -1, bestLen = 0, run = 0;
      for (let x = start + 1; x < start + maxW; x++) {
        run = ink[x] ? 0 : run + 1;
        if (run >= minGap && run >= bestLen) (best = x - (run >> 1)), (bestLen = run);
      }
      if (best > start) end = best;
      else (end = start + maxW - overlap), (hard = true);
    }
    const x1 = hard ? end + overlap : end;
    const rw = Math.max(8, Math.round((x1 - start) * scale));
    out.push({ x0: start, x1, rw, bucket: exact ? Math.ceil(rw / 8) * 8 : (BUCKETS.find((b) => b >= rw) ?? MAX_SEG), ...(join !== undefined ? { join } : {}) });
    join = hard ? end + (overlap >> 1) : undefined;
    start = end;
  }
  return out;
}

/**
 * Write one segment into a [3, 48, bucket] slice of `dst` (bilinear resize,
 * grey replicated to the three channels, contrast-stretched to [-1, 1], zero pad).
 */
export function fill(dst: Float32Array, offset: number, data: Uint8Array, w: number, h: number, s: Segment) {
  const plane = INPUT_H * s.bucket;
  // Stretch the crop's contrast to the full range: dim text reads like bright text.
  let lo = 255, hi = 0;
  for (let i = 0; i < data.length; i++) (data[i] < lo && (lo = data[i])), data[i] > hi && (hi = data[i]);
  const k = hi > lo ? 2 / (hi - lo) : 0;
  const sx = (s.x1 - s.x0) / s.rw;
  const sy = h / INPUT_H;
  for (let y = 0; y < INPUT_H; y++) {
    const fy = Math.min(h - 1, Math.max(0, (y + 0.5) * sy - 0.5));
    const y0 = fy | 0, y1 = Math.min(h - 1, y0 + 1), wy = fy - y0;
    const r = offset + y * s.bucket;
    for (let x = 0; x < s.bucket; x++) {
      let v = 0;
      if (x < s.rw) {
        const fx = Math.min(s.x1 - s.x0 - 1, Math.max(0, (x + 0.5) * sx - 0.5)) + s.x0;
        const x0 = fx | 0, x1 = Math.min(w - 1, x0 + 1), wx = fx - x0;
        const a = data[y0 * w + x0] * (1 - wx) + data[y0 * w + x1] * wx;
        const b = data[y1 * w + x0] * (1 - wx) + data[y1 * w + x1] * wx;
        v = (a * (1 - wy) + b * wy - lo) * k - 1;
      }
      dst[r + x] = v;
      dst[r + plane + x] = v;
      dst[r + 2 * plane + x] = v;
    }
  }
}

export interface Glyph {
  c: string;
  /** Centre x in the line crop. */
  x: number;
  p: number;
}

/** Greedy CTC decode of one [T, C] probability slice into placed glyphs. */
export function decode(probs: Float32Array, T: number, C: number, chars: string[], s: Segment): Glyph[] {
  const out: Glyph[] = [];
  const step = s.bucket / T;
  const toCrop = (s.x1 - s.x0) / s.rw;
  let prev = -1;
  for (let t = 0; t < T; t++) {
    const o = t * C;
    let best = 0, bv = probs[o];
    for (let k = 1; k < C; k++) if (probs[o + k] > bv) (bv = probs[o + k]), (best = k);
    if (best !== prev && best !== 0) {
      const xr = (t + 0.5) * step;
      if (xr <= s.rw + step) out.push({ c: chars[best] ?? "", x: s.x0 + xr * toCrop, p: bv });
    }
    prev = best;
  }
  return out;
}

/**
 * Join segment glyphs and classify the whole line: a space at each word-gap
 * cut; at a hard cut, each side keeps its own glyphs up to the join point.
 */
export function classifyLine(glyphs: Glyph[][], segs: Segment[], w: number, h: number, keepText: boolean): LineResult {
  const all: Glyph[] = [];
  glyphs.forEach((g0, i) => {
    const join = segs[i]?.join;
    const next = segs[i + 1]?.join;
    const g = g0.filter((x) => (join === undefined || x.x >= join) && (next === undefined || x.x < next));
    if (i > 0 && join === undefined && g.length && all.length && all[all.length - 1].c !== " ") all.push({ c: " ", x: (all[all.length - 1].x + g[0].x) / 2, p: 1 });
    all.push(...g);
  });
  // String offsets → glyph index (a glyph may be two UTF-16 units).
  let text = "";
  const at: number[] = [];
  all.forEach((g, i) => {
    for (let k = 0; k < g.c.length; k++) at.push(i);
    text += g.c;
  });
  const covers: Cover[] = [];
  // Character pitch: median spacing between adjacent glyph centres.
  const gaps = all.slice(1).map((g, i) => g.x - all[i].x).filter((d) => d > 0).sort((a, b) => a - b);
  const pitch = gaps.length ? gaps[gaps.length >> 1] : h * 0.6;
  for (const sp of findSensitive(text)) {
    covers.push({ x0: all[at[sp.start]].x - pitch, x1: all[at[sp.end - 1]].x + pitch, category: sp.category });
  }
  // Keep the whole line hidden when the reading can't be trusted: a digit or
  // "@" read with low confidence (a misread secret), low confidence overall,
  // or far fewer glyphs than the ink's width holds (two rows squashed into one
  // reading, a font the model can't read).
  const mean = all.length ? all.reduce((s, g) => s + g.p, 0) / all.length : 1;
  const expected = w / (h * 0.55);
  if (
    all.some((g) => g.p < 0.35 && /[\d@]/.test(g.c) && !covers.some((c) => g.x >= c.x0 && g.x <= c.x1)) ||
    mean < 0.6 ||
    // Nothing read from something as wide as a few characters: unreadable, not empty.
    (!all.some((g) => g.c.trim()) && w >= h * 2) ||
    (expected >= 6 && all.filter((g) => g.c !== " ").length < expected * 0.25)
  )
    covers.push({ x0: -Infinity, x1: Infinity, category: "uncertain" });
  return keepText ? { covers, text } : { covers };
}
